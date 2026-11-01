package control

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// countingActions counts creates and hands out a new session per call. While gate is set, each
// create waits for it to close.
type countingActions struct {
	mu      sync.Mutex
	n       int
	err     error
	gate    chan struct{}
	started chan struct{}
}

func (f *countingActions) CreateSession(ctx context.Context, p api.SessionCreateParams) (string, string, error) {
	f.mu.Lock()
	f.n++
	n, err, gate, started := f.n, f.err, f.gate, f.started
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if err != nil {
		return "", "", err
	}
	return "s-" + string(rune('0'+n)), "p-" + string(rune('0'+n)), nil
}

func (f *countingActions) ClosePane(context.Context, string) error { return nil }

func (f *countingActions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// countingAgents counts sends and answers with err when set.
type countingAgents struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (f *countingAgents) Send(_ context.Context, _ string, p api.SessionSendParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, p.Text)
	return nil
}

func (f *countingAgents) Answer(context.Context, string, api.ApprovalAnswerParams) error { return nil }

func (f *countingAgents) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func errKind(err error) api.ErrorKind {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

// TestIdempotentCreate: a repeated session.create with the same key starts one session and
// answers with the first ids, on the same connection and on a new one. Another key, or none,
// starts another. A key reused with other params is refused with conflict.
func TestIdempotentCreate(t *testing.T) {
	fa := &countingActions{}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Actions = fa })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	p := api.SessionCreateParams{Name: "n", Argv: []string{"/bin/true"}, IdempotencyKey: "k1"}

	first, err := cl.SessionCreate(ctx(t), p)
	if err != nil || first.Session != "s-1" || first.Pane != "p-1" {
		t.Fatalf("first create: %+v, %v", first, err)
	}
	again, err := cl.SessionCreate(ctx(t), p)
	if err != nil || again.Session != first.Session || again.Pane != first.Pane {
		t.Fatalf("repeat: %+v, %v", again, err)
	}
	c2 := dial(t, h.path) // a client that reconnected
	hello(t, c2, "")
	other, err := (api.Client{Caller: c2}).SessionCreate(ctx(t), p)
	if err != nil || other.Session != first.Session {
		t.Fatalf("repeat on a new connection: %+v, %v", other, err)
	}
	if n := fa.count(); n != 1 {
		t.Fatalf("%d creates ran, want 1", n)
	}

	p2 := p
	p2.Name = "m"
	if _, err := cl.SessionCreate(ctx(t), p2); errKind(err) != api.ErrorKindConflict {
		t.Errorf("key reused with another name: %v, want conflict", err)
	}
	p2.IdempotencyKey = "k2"
	if got, err := cl.SessionCreate(ctx(t), p2); err != nil || got.Session != "s-2" {
		t.Errorf("another key: %+v, %v", got, err)
	}
	p2.IdempotencyKey = ""
	for range 2 {
		if _, err := cl.SessionCreate(ctx(t), p2); err != nil {
			t.Fatal(err)
		}
	}
	if n := fa.count(); n != 4 {
		t.Errorf("%d creates ran, want 4", n)
	}

	long := p
	long.IdempotencyKey = strings.Repeat("x", MaxIdempotencyKey+1)
	if _, err := cl.SessionCreate(ctx(t), long); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("key of %d bytes: %v, want invalid", len(long.IdempotencyKey), err)
	}
	long.IdempotencyKey = strings.Repeat("x", MaxIdempotencyKey)
	if _, err := cl.SessionCreate(ctx(t), long); err != nil {
		t.Errorf("key of %d bytes: %v", MaxIdempotencyKey, err)
	}
}

// TestIdempotentCreateFailed: a create that failed is forgotten, so a retry with the same key
// runs again.
func TestIdempotentCreateFailed(t *testing.T) {
	fa := &countingActions{err: Errorf(api.ErrorKindUnavailable, "no holder")}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Actions = fa })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	p := api.SessionCreateParams{IdempotencyKey: "k"}
	if _, err := cl.SessionCreate(ctx(t), p); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("first create: %v", err)
	}
	fa.mu.Lock()
	fa.err = nil
	fa.mu.Unlock()
	if got, err := cl.SessionCreate(ctx(t), p); err != nil || got.Session != "s-2" {
		t.Fatalf("retry: %+v, %v", got, err)
	}
}

// TestIdempotentCreateInFlight: a repeat that arrives on another connection while the first
// create still runs waits for it and gets its ids; only one create runs.
func TestIdempotentCreateInFlight(t *testing.T) {
	fa := &countingActions{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Actions = fa })
	p := api.SessionCreateParams{IdempotencyKey: "k"}

	type answer struct {
		got api.SessionCreated
		err error
	}
	answers := make(chan answer, 2)
	call := func() {
		c := dial(t, h.path)
		hello(t, c, "")
		got, err := (api.Client{Caller: c}).SessionCreate(ctx(t), p)
		answers <- answer{got, err}
	}
	go call()
	<-fa.started // the first create is running
	go call()
	time.Sleep(50 * time.Millisecond) // let the repeat reach the hab and wait
	close(fa.gate)
	for range 2 {
		a := <-answers
		if a.err != nil || a.got.Session != "s-1" {
			t.Errorf("answer: %+v, %v", a.got, a.err)
		}
	}
	if n := fa.count(); n != 1 {
		t.Errorf("%d creates ran, want 1", n)
	}
}

// TestIdempotentSend: a repeated session.send with the same key writes once and answers
// success; a failed send is forgotten; other text under the same key is refused with conflict.
func TestIdempotentSend(t *testing.T) {
	fg := &countingAgents{}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Agents = fg })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	p := api.SessionSendParams{Session: "s", Text: "hello", Origin: api.SendOriginTyped, IdempotencyKey: "m1"}

	for i := range 3 {
		if _, err := cl.SessionSend(ctx(t), p); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if n := fg.count(); n != 1 {
		t.Fatalf("%d sends ran, want 1", n)
	}
	p2 := p
	p2.Text = "other"
	if _, err := cl.SessionSend(ctx(t), p2); errKind(err) != api.ErrorKindConflict {
		t.Errorf("key reused with other text: %v, want conflict", err)
	}
	// The same key on session.create is a separate key.
	if _, err := cl.SessionCreate(ctx(t), api.SessionCreateParams{IdempotencyKey: "m1"}); errKind(err) != api.ErrorKindUnavailable {
		t.Errorf("create with a send's key and no Actions: %v, want unavailable", err)
	}

	fg.mu.Lock()
	fg.err = Errorf(api.ErrorKindConflict, "the agent is busy")
	fg.mu.Unlock()
	p3 := p
	p3.IdempotencyKey = "m2"
	if _, err := cl.SessionSend(ctx(t), p3); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("busy send: %v", err)
	}
	fg.mu.Lock()
	fg.err = nil
	fg.mu.Unlock()
	if _, err := cl.SessionSend(ctx(t), p3); err != nil {
		t.Fatalf("retry after the busy send: %v", err)
	}
	if n := fg.count(); n != 2 {
		t.Errorf("%d sends ran, want 2", n)
	}
	p3.IdempotencyKey = strings.Repeat("x", MaxIdempotencyKey+1)
	if _, err := cl.SessionSend(ctx(t), p3); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("long key: %v, want invalid", err)
	}
}

// TestIdempotentKeysExpire: a key is forgotten after idemKeep, and the oldest goes first past
// idemMax.
func TestIdempotentKeysExpire(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	s := New(Config{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	runs := 0
	run := func(key string) {
		t.Helper()
		if _, e := s.idem(context.Background(), "m", key, key, func() (any, *Error) { runs++; return nil, nil }); e != nil {
			t.Fatal(e)
		}
	}
	run("a")
	run("a")
	if runs != 1 {
		t.Fatalf("runs %d, want 1", runs)
	}
	mu.Lock()
	now = now.Add(idemKeep)
	mu.Unlock()
	run("a")
	if runs != 2 {
		t.Fatalf("after idemKeep: runs %d, want 2", runs)
	}
	for i := range idemMax {
		run(string(rune(0x1000 + i)))
	}
	runs = 0
	run("a") // the oldest, pushed out
	run(string(rune(0x1000 + idemMax - 1)))
	if runs != 1 {
		t.Errorf("runs %d, want 1 (a forgotten, the newest kept)", runs)
	}
	if n := len(s.idemKeys.calls); n > idemMax {
		t.Errorf("%d keys held, want at most %d", n, idemMax)
	}
}
