package control

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakeRounds records calls made on the server's goroutines; mu orders them against the test's
// reads (a reply over the socket is no happens-before edge for the race detector).
type fakeRounds struct {
	mu  sync.Mutex
	err error
	got []string
}

// note records one call and returns the error to answer with.
func (f *fakeRounds) note(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, call)
	return f.err
}

func (f *fakeRounds) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func (f *fakeRounds) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeRounds) List() []api.Round { return []api.Round{{ID: "round-1"}} }
func (f *fakeRounds) Get(r api.RoundRef) (api.Round, error) {
	err := f.note("get " + r.Name)
	return api.Round{ID: "round-1"}, err
}
func (f *fakeRounds) Create(_ context.Context, p api.RoundCreateParams, by string) (api.RoundCreated, error) {
	err := f.note("create " + p.Round.Name + " by " + by)
	return api.RoundCreated{Round: api.Round{ID: "round-1"}}, err
}
func (f *fakeRounds) Update(_ context.Context, p api.RoundUpdateParams) (api.Round, error) {
	err := f.note("update " + p.ID)
	return api.Round{ID: p.ID}, err
}
func (f *fakeRounds) Delete(r api.RoundRef) error {
	err := f.note("delete " + r.ID)
	return err
}
func (f *fakeRounds) Pause(r api.RoundRef) (api.Round, error) {
	err := f.note("pause " + r.ID)
	return api.Round{}, err
}
func (f *fakeRounds) Unpause(r api.RoundRef) (api.Round, error) {
	err := f.note("unpause " + r.ID)
	return api.Round{}, err
}
func (f *fakeRounds) Run(r api.RoundRef) (api.RoundRun, error) {
	err := f.note("run " + r.ID)
	return api.RoundRun{ID: "run-1"}, err
}
func (f *fakeRounds) Runs(p api.RoundRunsParams) (api.RoundRuns, error) {
	err := f.note("runs " + p.ID)
	return api.RoundRuns{}, err
}

// TestRoundMethods: each round method reaches Rounds with its params; the control connection
// creates as a user (no created_by); an *Error keeps its kind; with no Rounds every method is
// unavailable; round events reach host scopes only.
func TestRoundMethods(t *testing.T) {
	fr := &fakeRounds{}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Rounds = fr })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	if _, err := cl.RoundCreate(ctx(t), api.RoundCreateParams{Round: api.RoundSpec{Name: "a"}}); err != nil {
		t.Fatal(err)
	}
	cl.RoundGet(ctx(t), api.RoundRef{Name: "a"})
	cl.RoundUpdate(ctx(t), api.RoundUpdateParams{ID: "round-1"})
	cl.RoundPause(ctx(t), api.RoundRef{ID: "round-1"})
	cl.RoundUnpause(ctx(t), api.RoundRef{ID: "round-1"})
	cl.RoundRun(ctx(t), api.RoundRef{ID: "round-1"})
	cl.RoundRuns(ctx(t), api.RoundRunsParams{RoundRef: api.RoundRef{ID: "round-1"}})
	cl.RoundDelete(ctx(t), api.RoundRef{ID: "round-1"})
	if l, err := cl.RoundList(ctx(t), api.RoundListParams{}); err != nil || len(l.Rounds) != 1 {
		t.Fatalf("list: %+v %v", l, err)
	}
	want := []string{"create a by ", "get a", "update round-1", "pause round-1", "unpause round-1", "run round-1", "runs round-1", "delete round-1"}
	got := fr.calls()
	if len(got) != len(want) {
		t.Fatalf("calls %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls %q, want %q", got, want)
		}
	}
	fr.setErr(Errorf(api.ErrorKindConflict, "live"))
	var ce *Error
	if _, err := cl.RoundRun(ctx(t), api.RoundRef{ID: "round-1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
		t.Errorf("conflict: %v", err)
	}

	bare := startHab(t, "", socketPath(t), nil)
	bc := dial(t, bare.path)
	hello(t, bc, "")
	if _, err := (api.Client{Caller: bc}).RoundList(ctx(t), api.RoundListParams{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no rounds: %v", err)
	}

	for _, typ := range []api.EventType{EventRoundUpdated, EventRoundRunUpdated} {
		e := structure.Event{Type: typ, ID: "round-1", Data: api.Round{}}
		if !newFilter(api.Scope{Kind: api.ScopeKindHost}).pass(e) {
			t.Errorf("%s: host scope dropped it", typ)
		}
		if newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"round-1"}}).pass(e) {
			t.Errorf("%s: a sessions scope got it", typ)
		}
	}
	if !structure.Ephemeral(EventRoundRunUpdated) || structure.Ephemeral(EventRoundUpdated) {
		t.Error("round.run_updated is ephemeral; round.updated is stored")
	}
}
