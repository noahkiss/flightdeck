package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

// fakeRunner returns the errors a test queues, one per call; with none queued a call succeeds.
type fakeRunner struct {
	mu     sync.Mutex
	errs   []error
	calls  int
	closed bool
}

func (f *fakeRunner) next() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeRunner) Manifest() *modules.Manifest { return &modules.Manifest{ID: "fake"} }
func (f *fakeRunner) Has(string) bool             { return true }
func (f *fakeRunner) HeapGrowth() int64           { return 0 }
func (f *fakeRunner) Refresh(context.Context, Call) error {
	return f.next()
}
func (f *fakeRunner) Render(_ context.Context, c Call) ([]byte, error) {
	if err := f.next(); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"type":"text","text":%q}`, c.Place)), nil
}
func (f *fakeRunner) Act(context.Context, string, json.RawMessage, Call) error { return f.next() }
func (f *fakeRunner) Close()                                                   { f.closed = true }

// fakeLoader hands out fakeRunners, each with the errors queued for it, and records each load.
type fakeLoader struct {
	runners []*fakeRunner
	loadErr []error
	log     func(string)
}

func (l *fakeLoader) load(_ context.Context, log func(string)) (Runner, error) {
	l.log = log
	if len(l.loadErr) > 0 {
		err := l.loadErr[0]
		l.loadErr = l.loadErr[1:]
		if err != nil {
			return nil, err
		}
	}
	r := &fakeRunner{}
	if len(l.runners) > 0 {
		r = l.runners[0]
		l.runners = l.runners[1:]
	}
	return r, nil
}

var boardCall = Call{Host: "box", Place: "board"}

func scriptErr(msg string) error { return &ScriptError{Func: "render", Name: "Error", Message: msg} }

// TestManagedPause: three failures in a row pause the module (modules.md 8.3); a success
// resets the count; a paused module runs nothing and keeps the last good tree; Retry runs
// again.
func TestManagedPause(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	l := &fakeLoader{runners: []*fakeRunner{r}}
	changes := 0
	m := NewManaged(l.load, ManagedOptions{Changed: func() { changes++ }})
	defer m.Close()

	good, err := m.Render(ctx, boardCall)
	if err != nil || good.Revision != 1 || string(good.JSON) == "" || good.Made.IsZero() {
		t.Fatalf("first render = %+v, %v", good, err)
	}
	r.errs = []error{scriptErr("a"), scriptErr("b")}
	for i := 1; i <= 2; i++ {
		got, err := m.Render(ctx, boardCall)
		if err == nil || got.Revision != 1 || string(got.JSON) != string(good.JSON) {
			t.Fatalf("failure %d = %+v, %v; want the last good tree and the error", i, got, err)
		}
		if s := m.Status(); s.Failures != i || s.Paused || s.Err == nil {
			t.Fatalf("status after %d = %+v", i, s)
		}
	}
	// A success resets the count.
	if _, err := m.Render(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.Failures != 0 || s.Err != nil {
		t.Fatalf("status after a success = %+v", s)
	}
	// A refresh failure counts the same as a render failure.
	r.errs = []error{scriptErr("c"), &TimeoutError{Func: "refresh", Limit: time.Second}, scriptErr("d")}
	_ = m.Refresh(ctx, boardCall)
	_ = m.Refresh(ctx, boardCall)
	_, _ = m.Render(ctx, boardCall)
	s := m.Status()
	if !s.Paused || s.Failures != 3 {
		t.Fatalf("status after three = %+v, want paused", s)
	}
	calls := r.calls
	got, err := m.Render(ctx, boardCall)
	var pe *PausedError
	if !errors.As(err, &pe) || !errors.Is(err, ErrPaused) || pe.Failures != 3 || !strings.Contains(err.Error(), "Paused after errors: ") {
		t.Fatalf("render while paused: %v", err)
	}
	if got.Revision != 2 {
		t.Errorf("render while paused kept tree revision %d, want 2", got.Revision)
	}
	if err := m.Act(ctx, "x", nil, boardCall); !errors.Is(err, ErrPaused) {
		t.Errorf("act while paused: %v", err)
	}
	if err := m.Refresh(ctx, boardCall); !errors.Is(err, ErrPaused) {
		t.Errorf("refresh while paused: %v", err)
	}
	if r.calls != calls {
		t.Errorf("a paused module ran %d calls", r.calls-calls)
	}
	m.Retry()
	if s := m.Status(); s.Paused || s.Failures != 0 {
		t.Fatalf("status after Retry = %+v", s)
	}
	if got, err := m.Render(ctx, boardCall); err != nil || got.Revision != 3 {
		t.Fatalf("render after Retry = %+v, %v", got, err)
	}
	if changes == 0 {
		t.Error("Changed never called")
	}
	logs := strings.Join(m.Logs(), "\n")
	for _, want := range []string{"render failed (1 in a row)", "paused after 3 failures in a row", "retry after the pause"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log tail lacks %q:\n%s", want, logs)
		}
	}
}

// TestManagedWhatCounts: the errors that are not the module's fault count nothing, and an
// act counts only what stops the module.
func TestManagedWhatCounts(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		fn    string
		err   error
		count bool
		stops bool
	}{
		{"heap pressure", ctx, "render", ErrHeapPressure, false, false},
		{"client too old", ctx, "render", &ClientTooOld{Vocabulary: 1, Blocks: 2}, false, false},
		{"host down", ctx, "render", &HostError{Func: "render", Reason: "exited"}, false, false},
		{"host closed", ctx, "refresh", ErrHostClosed, false, false},
		{"caller cancel", cancelled, "render", context.Canceled, false, false},
		{"culprit", ctx, "render", &HostError{Func: "render", Reason: "killed", Culprit: true}, true, false},
		{"tree", ctx, "render", &TreeError{}, true, false},
		{"timeout", ctx, "refresh", &TimeoutError{Func: "refresh"}, true, false},
		{"stuck after cancel", cancelled, "render", &TimeoutError{Func: "render", Stuck: true}, true, true},
		{"memory", ctx, "refresh", &MemoryError{Func: "refresh"}, true, true},
		{"internal", ctx, "render", &InternalError{Func: "render"}, true, true},
		{"stopped", ctx, "render", ErrStopped, true, true},
		{"act script", ctx, "act", &ScriptError{Func: "act"}, false, false},
		{"act timeout", ctx, "act", &TimeoutError{Func: "act"}, false, false},
		{"act no act", ctx, "act", ErrNoAct, false, false},
		{"act culprit", ctx, "act", &HostError{Func: "act", Culprit: true}, true, false},
		{"act memory", ctx, "act", &MemoryError{Func: "act"}, true, true},
		{"act stuck", ctx, "act", &TimeoutError{Func: "act", Stuck: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &fakeRunner{errs: []error{tc.err}}
			l := &fakeLoader{runners: []*fakeRunner{first}}
			m := NewManaged(l.load, ManagedOptions{})
			defer m.Close()
			var err error
			switch tc.fn {
			case "render":
				_, err = m.Render(tc.ctx, boardCall)
			case "refresh":
				err = m.Refresh(tc.ctx, boardCall)
			case "act":
				err = m.Act(tc.ctx, "x", nil, boardCall)
			}
			if !errors.Is(err, tc.err) && err != tc.err {
				t.Fatalf("err = %v, want %v passed on", err, tc.err)
			}
			if n := m.Status().Failures; (n == 1) != tc.count {
				t.Errorf("failures = %d, count = %v", n, tc.count)
			}
			if first.closed != tc.stops {
				t.Errorf("closed = %v, want %v", first.closed, tc.stops)
			}
			// The next call runs on the same runner, or on a new one after a stop.
			if _, err := m.Render(ctx, boardCall); err != nil {
				t.Fatal(err)
			}
			if again := first.calls == 2; again == tc.stops {
				t.Errorf("after: first runner calls = %d, stops = %v", first.calls, tc.stops)
			}
		})
	}
}

// TestManagedGoodActNoReset: a good act leaves the count alone; the refresh or render after it
// decides.
func TestManagedGoodActNoReset(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{errs: []error{scriptErr("a")}}
	m := NewManaged((&fakeLoader{runners: []*fakeRunner{r}}).load, ManagedOptions{})
	defer m.Close()
	_, _ = m.Render(ctx, boardCall)
	if err := m.Act(ctx, "x", nil, boardCall); err != nil {
		t.Fatal(err)
	}
	if n := m.Status().Failures; n != 1 {
		t.Fatalf("failures after a good act = %d, want 1", n)
	}
}

// TestManagedLoadFailure: a load that fails counts, three pause, and a later call loads again.
func TestManagedLoadFailure(t *testing.T) {
	ctx := context.Background()
	bad := &ScriptError{Func: "load", Name: "SyntaxError", Message: "x"}
	l := &fakeLoader{loadErr: []error{bad, &HostError{Func: "load", Reason: "down"}, nil}}
	m := NewManaged(l.load, ManagedOptions{})
	defer m.Close()
	if err := m.Act(ctx, "x", nil, boardCall); !errors.Is(err, bad) && err != bad {
		t.Fatalf("act = %v", err)
	}
	if _, err := m.Render(ctx, boardCall); !errors.Is(err, ErrHostDown) {
		t.Fatalf("render = %v", err)
	}
	if n := m.Status().Failures; n != 1 {
		t.Fatalf("failures = %d, want 1 (the load failure, not the host down)", n)
	}
	if _, err := m.Render(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
}

// TestManagedTrees: one last good tree per place and vocabulary, a revision over all of them,
// and a failure for one place returns that place's tree.
func TestManagedTrees(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	now := time.Unix(1000, 0)
	m := NewManaged((&fakeLoader{runners: []*fakeRunner{r}}).load, ManagedOptions{Now: func() time.Time { return now }})
	defer m.Close()
	host := Call{Place: "host"}
	b, _ := m.Render(ctx, boardCall)
	h, _ := m.Render(ctx, host)
	if b.Revision != 1 || h.Revision != 2 || !b.Made.Equal(now) {
		t.Fatalf("revisions %d %d, made %v", b.Revision, h.Revision, b.Made)
	}
	r.errs = []error{scriptErr("x")}
	got, err := m.Render(ctx, boardCall)
	if err == nil || string(got.JSON) != string(b.JSON) {
		t.Fatalf("failure for board = %s, %v", got.JSON, err)
	}
	// Vocabulary 0 and the newest are one key; an unknown place has no tree.
	if t1, ok := m.Tree(Call{Place: "board", Vocabulary: modules.Version}); !ok || t1.Revision != 1 {
		t.Errorf("Tree(board, newest) = %+v, %v", t1, ok)
	}
	if _, ok := m.Tree(Call{Place: "sheet"}); ok {
		t.Error("a tree for a place never drawn")
	}
}

// TestManagedLogTail: the tail keeps the last 200 lines across a reload, the module's fd.log
// lines through the Loader's log function among them.
func TestManagedLogTail(t *testing.T) {
	ctx := context.Background()
	l := &fakeLoader{}
	var seen []string
	m := NewManaged(l.load, ManagedOptions{Log: func(s string) { seen = append(seen, s) }})
	defer m.Close()
	if err := m.Refresh(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 150; i++ {
		l.log(fmt.Sprintf("line %d", i))
	}
	m.Reload()
	if err := m.Refresh(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
	for i := 150; i < 250; i++ {
		l.log(fmt.Sprintf("line %d", i))
	}
	logs := m.Logs()
	if len(logs) != DefaultLogLines || logs[0] != "line 50" || logs[len(logs)-1] != "line 249" {
		t.Fatalf("tail = %d lines, %q .. %q", len(logs), logs[0], logs[len(logs)-1])
	}
	if len(seen) != 250 {
		t.Errorf("Log option saw %d lines", len(seen))
	}
}

// TestManagedReload: Reload loads the module again at the next call and ends a pause.
func TestManagedReload(t *testing.T) {
	ctx := context.Background()
	first := &fakeRunner{errs: []error{scriptErr("a"), scriptErr("b"), scriptErr("c")}}
	l := &fakeLoader{runners: []*fakeRunner{first}}
	m := NewManaged(l.load, ManagedOptions{})
	for i := 0; i < 3; i++ {
		_, _ = m.Render(ctx, boardCall)
	}
	if !m.Status().Paused {
		t.Fatal("not paused")
	}
	m.Reload()
	if s := m.Status(); s.Paused || s.Failures != 0 {
		t.Fatalf("after Reload: %+v", s)
	}
	if _, err := m.Render(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
	if !first.closed {
		t.Error("the old runner was not closed")
	}
	m.Close()
	if !errors.Is(m.Refresh(ctx, boardCall), ErrHostClosed) {
		t.Error("a call after Close")
	}
}

// TestManagedHostCrash: through the module host, a module that crashes it counts one failure
// as the culprit, its neighbour counts none, and the next good render resets the count.
func TestManagedHostCrash(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{BackoffMin: 100 * time.Millisecond})
	managed := func(id, js string, o Options) *Managed {
		b := writeBundle(t, id, "", js)
		m := NewManaged(func(_ context.Context, log func(string)) (Runner, error) {
			o.Log = log
			return h.New(b, o)
		}, ManagedOptions{})
		t.Cleanup(m.Close)
		return m
	}
	ctx := context.Background()
	crasher := managed("crasher", crashJS, Options{Limits: Limits{Act: 20 * time.Second}})
	other := managed("bystander", `exports.render = function () { fd.log("drew"); return { type: "text", text: "b" }; };`, Options{})
	if _, err := crasher.Render(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Render(ctx, boardCall); err != nil {
		t.Fatal(err)
	}
	if err := crasher.Act(ctx, "crash", nil, boardCall); !errors.Is(err, ErrHostDown) {
		t.Fatalf("crash = %v", err)
	}
	if n := crasher.Status().Failures; n != 1 {
		t.Fatalf("crasher failures = %d, want 1", n)
	}
	_, _ = other.Render(ctx, boardCall) // during the backoff: no culprit
	if n := other.Status().Failures; n != 0 {
		t.Fatalf("bystander failures = %d, want 0", n)
	}
	waitFor(t, 5*time.Second, func() bool { return h.Pid() != 0 })
	for _, m := range []*Managed{crasher, other} {
		if _, err := m.Render(ctx, boardCall); err != nil {
			t.Fatal(err)
		}
		if s := m.Status(); s.Failures != 0 {
			t.Errorf("after the restart: %+v", s)
		}
	}
	if logs := strings.Join(other.Logs(), "\n"); !strings.Contains(logs, "drew") {
		t.Errorf("bystander's tail lacks its fd.log line:\n%s", logs)
	}
}
