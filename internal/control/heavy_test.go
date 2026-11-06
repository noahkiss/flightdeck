package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeHeavy holds one ticket; grant moves it to holding.
type fakeHeavy struct {
	mu      sync.Mutex
	owner   context.Context
	granted chan struct{}
	release []string
}

func (f *fakeHeavy) Take(owner context.Context, p api.HeavyTakeParams) (api.HeavyTaken, error) {
	f.mu.Lock()
	f.owner = owner
	f.mu.Unlock()
	if p.Ticket == "bad" {
		return api.HeavyTaken{}, Errorf(api.ErrorKindInvalid, "bad ticket")
	}
	return api.HeavyTaken{Ticket: "t1", State: "waiting", Ahead: 1}, nil
}

func (f *fakeHeavy) Wait(ctx context.Context, ticket string) (api.HeavyTaken, error) {
	select {
	case <-f.granted:
		return api.HeavyTaken{Ticket: ticket, State: "holding"}, nil
	case <-ctx.Done():
		return api.HeavyTaken{Ticket: ticket, State: "waiting", Ahead: 1}, nil
	}
}

func (f *fakeHeavy) Release(p api.HeavyReleaseParams) error {
	f.mu.Lock()
	f.release = append(f.release, p.Ticket)
	f.mu.Unlock()
	return nil
}

func (f *fakeHeavy) Status() api.HeavyStatus { return api.HeavyStatus{Slots: 1} }

func (f *fakeHeavy) ownerCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owner
}

// TestHeavyMethods: heavy.take answers at once with wait_ms 0, waits for the grant with
// wait_ms, and answers waiting when its wait ends; the ticket's owner is the connection, so
// closing it ends the owner's context; release and status reach Heavy; an *Error keeps its
// kind; with no Heavy every method is unavailable.
func TestHeavyMethods(t *testing.T) {
	fh := &fakeHeavy{granted: make(chan struct{})}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Heavy = fh })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	r, err := cl.HeavyTake(ctx(t), api.HeavyTakeParams{})
	if err != nil || r.State != "waiting" || r.Ahead != 1 {
		t.Fatalf("take: %+v %v", r, err)
	}
	r, err = cl.HeavyTake(ctx(t), api.HeavyTakeParams{Ticket: "t1", WaitMS: 20})
	if err != nil || r.State != "waiting" {
		t.Fatalf("take past its wait: %+v %v", r, err)
	}
	done := make(chan api.HeavyTaken, 1)
	go func() {
		r, _ := cl.HeavyTake(context.Background(), api.HeavyTakeParams{Ticket: "t1", WaitMS: 60000})
		done <- r
	}()
	// A status call answers while a take waits: the wait is off the read loop.
	if s, err := cl.HeavyStatus(ctx(t), api.HeavyStatusParams{}); err != nil || s.Slots != 1 {
		t.Fatalf("status: %+v %v", s, err)
	}
	close(fh.granted)
	select {
	case r := <-done:
		if r.State != "holding" {
			t.Fatalf("take with a wait: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting take did not answer")
	}
	if _, err := cl.HeavyRelease(ctx(t), api.HeavyReleaseParams{Ticket: "t1"}); err != nil {
		t.Fatal(err)
	}
	var ce *Error
	if _, err := cl.HeavyTake(ctx(t), api.HeavyTakeParams{Ticket: "bad"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("bad ticket: %v", err)
	}
	owner := fh.ownerCtx()
	if owner.Err() != nil {
		t.Fatal("the owner ended while the connection is open")
	}
	c.Close()
	select {
	case <-owner.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing the connection did not end the ticket's owner")
	}

	bare := startHab(t, "", socketPath(t), nil)
	bc := dial(t, bare.path)
	hello(t, bc, "")
	if _, err := (api.Client{Caller: bc}).HeavyStatus(ctx(t), api.HeavyStatusParams{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no heavy: %v", err)
	}
	if err := bc.Call(ctx(t), "heavy.nothing", struct{}{}, nil); err == nil {
		t.Error("an unknown heavy method answered")
	}
}
