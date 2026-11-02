package lbsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

func setBefore(c *caller, f func(method string) error) { c.before.Store(&f) }
func setAfter(c *caller, f func(method string))        { c.after.Store(&f) }

// slowLink is a link whose bytes take time: request and answer each move at rate bytes a
// millisecond, one call at a time, the way a slow ssh link carries the control connection.
// Two calls on the way at once count as an overlap: on a real link the second answer would
// queue behind the first in the dialed hab's backlog.
type slowLink struct {
	inner    *caller
	rate     int // bytes per millisecond
	mu       sync.Mutex
	busy     bool
	overlaps atomic.Int32
	pulls    atomic.Int32
	minAsked atomic.Int64 // the smallest max_bytes a pull asked for
}

func (l *slowLink) Call(ctx context.Context, method string, params, result any) error {
	l.mu.Lock()
	if l.busy {
		l.overlaps.Add(1)
	}
	l.busy = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.busy = false
		l.mu.Unlock()
	}()
	if p, ok := params.(api.LogbookPullParams); ok {
		l.pulls.Add(1)
		if n := int64(p.MaxBytes); n > 0 && (l.minAsked.Load() == 0 || n < l.minAsked.Load()) {
			l.minAsked.Store(n)
		}
	}
	var raw json.RawMessage
	if err := l.inner.Call(ctx, method, params, &raw); err != nil {
		return err
	}
	req, _ := json.Marshal(params)
	d := time.Duration(len(req)+len(raw)) * time.Millisecond / time.Duration(l.rate)
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return ctx.Err()
	}
	return json.Unmarshal(raw, result)
}

// A slow link: a page that takes too long is given up on, the page size halves until pages fit
// the time, the answer given up on is waited for before the next call, so two answers are
// never on the way at once, and every op arrives.
func TestSlowLink(t *testing.T) {
	a := newHab(t, "a", func(_ *hab, c *Config) {
		c.PageTime = 300 * time.Millisecond
		c.MinBackoff = 50 * time.Millisecond
	})
	b := newHab(t, "b")
	for i := range 40 {
		if _, err := b.store.Write(context.Background(), logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask,
			map[string]any{"title": fmt.Sprintf("big %d", i), "body": strings.Repeat("y", 20<<10)})}}); err != nil {
			t.Fatal(err)
		}
	}
	link := &slowLink{inner: &caller{from: a, to: b}, rate: 2 << 10}
	a.mu.Lock()
	a.links = append(a.links, Link{Label: "b", DeviceID: b.id, Online: true, Caps: []string{Capability}, Caller: link})
	a.mu.Unlock()
	a.svc.Kick()
	waitFor(t, "every op over the slow link", func() bool {
		if seqOf(t, a, b.id) == 40 {
			return true
		}
		a.svc.Kick()
		time.Sleep(50 * time.Millisecond)
		return false
	})
	if n := link.overlaps.Load(); n != 0 {
		t.Fatalf("%d calls went out while an earlier answer was on the way", n)
	}
	if asked := link.minAsked.Load(); asked >= logbook.PageBytes || asked <= 0 {
		t.Fatalf("the smallest page asked for was %d bytes: the page size never shrank", asked)
	}
	if n := a.svc.pageBytes("b"); n >= logbook.PageBytes {
		t.Fatalf("page size %d after slow pages", n)
	}
}

// A wake-up during an exchange that then fails runs the next one at once, past the backoff;
// and a wake-up during the backoff wait ends the wait.
func TestWakeDuringFailingExchange(t *testing.T) {
	a := newHab(t, "a", func(_ *hab, c *Config) {
		c.MinBackoff = time.Hour
		c.Path = "" // only the test's calls start exchanges
	})
	b := newHab(t, "b")
	l := dial(a, b)
	idle := func() {
		waitFor(t, "a's exchanges end", func() bool {
			a.svc.mu.Lock()
			defer a.svc.mu.Unlock()
			return len(a.svc.running) == 0
		})
	}
	waitFor(t, "first exchange", func() bool { _, ok := a.svc.LinkStats()["b"]; return ok })

	// blockOnce makes the next pull wait for release and then fail; later calls go through.
	blockOnce := func() (entered, release chan struct{}) {
		entered, release = make(chan struct{}), make(chan struct{})
		var once atomic.Bool
		setBefore(l, func(method string) error {
			if method != "logbook.pull" || !once.CompareAndSwap(false, true) {
				return nil
			}
			close(entered)
			<-release
			return errors.New("the link failed")
		})
		return entered, release
	}

	// 1. A wake-up during the failing exchange.
	idle()
	entered, release := blockOnce()
	write(t, a, "one")
	a.svc.Woken("b")
	<-entered
	a.svc.Woken("b")
	close(release)
	waitFor(t, "b gets the op past the backoff", func() bool { return seqOf(t, b, a.id) == 1 })

	// 2. A change during the failing exchange waits out the backoff; a wake-up ends the wait.
	idle()
	entered, release = blockOnce()
	a.svc.Woken("b")
	<-entered
	write(t, a, "two")
	a.svc.Changed()
	waitFor(t, "the change asks for another exchange", func() bool {
		a.svc.mu.Lock()
		defer a.svc.mu.Unlock()
		return a.svc.again["b"]
	})
	close(release)
	waitFor(t, "the failure", func() bool { return a.svc.LinkStats()["b"].Err != "" })
	time.Sleep(50 * time.Millisecond)
	if seqOf(t, b, a.id) != 1 {
		t.Fatal("the change ran past the backoff")
	}
	a.svc.Woken("b")
	waitFor(t, "b gets the op after the wake-up ends the wait", func() bool { return seqOf(t, b, a.id) == 2 })
}

// A hab that pushes is never woken for its own pushed ops, and a hab's have counts the page it
// was served.
func TestNoEchoWake(t *testing.T) {
	a, b := newHab(t, "a"), newHab(t, "b")
	write(t, b, "on b")
	dial(a, b)
	waitFor(t, "a's first pull", func() bool { return seqOf(t, a, b.id) == 1 })
	b.svc.mu.Lock()
	served := b.svc.have[a.id][b.id]
	b.svc.mu.Unlock()
	if served != 1 {
		t.Fatalf("b's have of a holds b:%d after serving op 1", served)
	}
	for i := range 3 {
		write(t, a, fmt.Sprintf("on a %d", i))
		a.svc.Changed()
		waitFor(t, "b gets it", func() bool { return seqOf(t, b, a.id) == uint64(i+1) })
	}
	time.Sleep(150 * time.Millisecond)
	if n := b.wakesOf(a.id); n != 0 {
		t.Fatalf("b woke a %d times for a's own pushes", n)
	}
}

// Ops pulled from one peer go on to the others at once: a hab with no file check (Path empty)
// pulls from b and pushes to c without waiting for the poll.
func TestPulledOpsGoOn(t *testing.T) {
	a := newHab(t, "a", func(_ *hab, c *Config) { c.Path = "" })
	b, c := newHab(t, "b"), newHab(t, "c")
	dial(a, c)
	dial(a, b)
	waitFor(t, "first exchanges", func() bool {
		st := a.svc.LinkStats()
		_, okb := st["b"]
		_, okc := st["c"]
		return okb && okc
	})
	write(t, b, "on b")
	a.svc.Woken("b")
	waitFor(t, "c gets b's op through a", func() bool { return seqOf(t, c, b.id) == 1 })
}

// A request while an exchange runs makes it run once more: a wake-up that comes after the
// exchange's pull was answered is not lost.
func TestRequestDuringExchange(t *testing.T) {
	a := newHab(t, "a", func(_ *hab, c *Config) { c.Path = "" })
	b := newHab(t, "b")
	l := dial(a, b)
	waitFor(t, "first exchange", func() bool { _, ok := a.svc.LinkStats()["b"]; return ok })
	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	setAfter(l, func(method string) {
		if method == "logbook.pull" && once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	})
	a.svc.Woken("b")
	<-entered // b answered the pull; the answer is on its way back
	write(t, b, "after the answer")
	a.svc.Woken("b")
	close(release)
	waitFor(t, "a gets the op in the exchange run once more", func() bool { return seqOf(t, a, b.id) == 1 })
}

// The file check looks once more after a change: a commit seen half written, whose last write
// moves neither size nor time, still goes out.
func TestRecheckAfterFileChange(t *testing.T) {
	var arm atomic.Bool
	var store atomic.Pointer[logbook.Store]
	dummy := filepath.Join(t.TempDir(), "watched")
	if err := os.WriteFile(dummy, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newHab(t, "a", func(h *hab, c *Config) {
		c.Path = dummy // the check watches this file; the store is elsewhere
		c.checked = func() {
			if arm.CompareAndSwap(true, false) {
				// The look that saw the file change found no new op yet; the commit lands now,
				// before the next look, and moves nothing the check sees.
				if _, err := store.Load().Write(context.Background(), logbook.Batch{Steps: []logbook.Step{
					logbook.Create(logbook.TypeTask, map[string]any{"title": "late commit"})}}); err != nil {
					t.Error(err)
				}
			}
		}
	})
	store.Store(a.store)
	b := newHab(t, "b")
	dial(a, b)
	waitFor(t, "first exchange", func() bool { _, ok := a.svc.LinkStats()["b"]; return ok })
	arm.Store(true)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dummy, future, future); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b gets the late commit", func() bool { return seqOf(t, b, a.id) == 1 })
}

// The start hold: with a peer listed, a write waits until the first exchange ends; with none,
// nothing waits; with only an offline peer, the hold runs out.
func TestStartHold(t *testing.T) {
	b := newHab(t, "b")
	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	var l *caller
	a := newHab(t, "a", func(h *hab, c *Config) {
		l = &caller{from: h, to: b}
		setBefore(l, func(method string) error {
			if once.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			return nil
		})
		h.links = []Link{{Label: "b", DeviceID: b.id, Online: true, Caps: []string{Capability}, Caller: l}}
		c.StartHold = 5 * time.Second
	})
	<-entered
	done := make(chan error, 1)
	go func() {
		_, err := a.store.Write(context.Background(), logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "held"})}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a write did not wait for the first exchange: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the write still waits after the first exchange")
	}

	// No peers: no hold.
	c := newHab(t, "c")
	start := time.Now()
	write(t, c, "alone")
	if time.Since(start) > time.Second {
		t.Fatal("a mesh of one waited")
	}
	// Only an offline peer: the hold runs out.
	d := newHab(t, "d", func(h *hab, cfg *Config) {
		h.links = []Link{{Label: "b", DeviceID: b.id, Online: false, Caps: []string{Capability}}}
		cfg.StartHold = 300 * time.Millisecond
	})
	start = time.Now()
	write(t, d, "offline peers")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the write waited %v", took)
	}
}
