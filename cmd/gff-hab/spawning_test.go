package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestPaneLiveSpawning: a token's pane is live while its spawn is in flight, and only then. The
// link's answer wins over the set: a pane the link reports lost or gone is refused even while
// its uuid is in the set.
func TestPaneLiveSpawning(t *testing.T) {
	u := newUUID()
	known := map[hp.UUID]link.PaneStatus{}
	lookup := func(x hp.UUID) (link.PaneStatus, bool) { st, ok := known[x]; return st, ok }
	sp := newSpawning()
	live := paneLive(lookup, sp)
	is := func(what string, want bool) {
		t.Helper()
		if pane, ok := live(u); ok != want || (ok && pane != u.String()) {
			t.Fatalf("%s: live %q %v, want %v", what, pane, ok, want)
		}
	}
	is("unknown", false)
	sp.add(u)
	is("(a) after add", true)
	sp.add(u)
	sp.done(u)
	is("(c) added twice, done once", true)
	sp.done(u)
	is("(b) after done", false)

	sp.add(u)
	for _, st := range []link.PaneStatus{{UUID: u, Lost: true}, {UUID: u, Gone: true}} {
		known[u] = st
		is(fmt.Sprintf("(d) link reports lost %v gone %v, uuid in the set", st.Lost, st.Gone), false)
	}
	known[u] = link.PaneStatus{UUID: u}
	is("followed by the link", true)
	sp.done(u)
	is("followed by the link, set empty", true)

	delete(known, u)
	nilSet := paneLive(lookup, nil)
	if _, ok := nilSet(u); ok {
		t.Fatal("(e) nil set: an unknown pane is live")
	}
	known[u] = link.PaneStatus{UUID: u}
	if _, ok := nilSet(u); !ok {
		t.Fatal("(e) nil set: a followed pane is refused")
	}
	if _, ok := livePane(nil, sp)(u); ok {
		t.Fatal("no link: a pane is live")
	}
}

// fakeActions returns actions over a real store with a fake holder, whose spawns run is.
func fakeActions(t *testing.T, run func(context.Context, hp.Spawn) (link.PaneStatus, error), has func(hp.UUID) bool) (*actions, *structure.Store) {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	sp := &spawner{tokens: hook.NewTokens(make([]byte, 32), nil), hookSock: "/nonexistent/hook.sock", spawning: newSpawning(), run: run, has: has}
	return &actions{sp: sp, store: st, getenv: func(string) string { return "" }, environ: func() []string { return nil }, home: t.TempDir()}, st
}

func spawned(_ context.Context, s hp.Spawn) (link.PaneStatus, error) {
	return link.PaneStatus{UUID: s.UUID}, nil
}

// TestCreateSpawnErrors: after a spawn error the session stays only when the program may run:
// the answer was lost (a timeout, a closed link) and the pane exists. A definite refusal closes
// it even then, and a lost answer with no pane closes it too. A kept session's pane is attached
// by the same spawn again, with a deadline of its own, when the current holder lists it; a pane
// on another holder is not spawned again (that would start a second program).
func TestCreateSpawnErrors(t *testing.T) {
	refused := &holderclient.RequestError{Ref: hp.KindSpawn, Code: hp.CodeDraining, Message: "holder is draining"}
	for _, c := range []struct {
		name      string
		err       error
		exists    bool
		onCurrent bool
		keep      bool
		calls     int
	}{
		{"timeout, pane exists", context.DeadlineExceeded, true, true, true, 2},
		{"link closed, pane exists", link.ErrClosed, true, true, true, 2},
		{"timeout, pane on another holder", context.DeadlineExceeded, true, false, true, 1},
		{"timeout, no pane", context.DeadlineExceeded, false, false, false, 1},
		{"spawn-failed", refused, true, true, false, 1},
		{"no holder", holderclient.ErrNoHolder, true, true, false, 1},
		{"uuid used", link.ErrUUIDUsed, true, true, false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The caller's context is over by the time the first answer is lost.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []hp.Spawn
			var retryErr error
			retryDeadline := false
			a, st := fakeActions(t,
				func(x context.Context, s hp.Spawn) (link.PaneStatus, error) {
					calls = append(calls, s)
					if len(calls) == 1 {
						cancel()
						return link.PaneStatus{}, c.err
					}
					_, retryDeadline = x.Deadline()
					retryErr = x.Err()
					return link.PaneStatus{UUID: s.UUID}, nil
				},
				func(u hp.UUID) bool { return c.exists && len(calls) > 0 && u == calls[0].UUID })
			a.sp.current = func(u hp.UUID) bool { return c.onCurrent && len(calls) > 0 && u == calls[0].UUID }
			session, pane, err := a.CreateSession(ctx, api.SessionCreateParams{Name: "s", Argv: []string{"/bin/true"}})
			_, have := st.SessionByName("s")
			if len(calls) != c.calls {
				t.Fatalf("%d spawns, want %d", len(calls), c.calls)
			}
			if c.calls == 2 && (calls[1].UUID != calls[0].UUID || !slices.Equal(calls[1].Argv, calls[0].Argv) || retryErr != nil || !retryDeadline) {
				t.Fatalf("the retry: %+v after %+v, its context %v, deadline %v", calls[1], calls[0], retryErr, retryDeadline)
			}
			if c.keep {
				if err != nil || session == "" || pane != calls[0].UUID.String() || !have {
					t.Fatalf("kept: %q %q %v, session there %v", session, pane, err, have)
				}
				return
			}
			if err == nil || have {
				t.Fatalf("closed: err %v, session there %v", err, have)
			}
		})
	}
}

// TestUnnamedCreatesAtOnce: two unnamed creates of the same program pick the same free name
// at once; the second to file it picks again and gets the next name, not conflict.
func TestUnnamedCreatesAtOnce(t *testing.T) {
	a, st := fakeActions(t, spawned, nil)
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	a.peerName = func(name string) string {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(both)
		}
		first := arrived <= 2
		mu.Unlock()
		if first { // hold the first two lookups until both creates picked a name
			select {
			case <-both:
			case <-time.After(10 * time.Second):
			}
		}
		return ""
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = a.CreateSession(context.Background(), api.SessionCreateParams{Argv: []string{"/bin/sh"}})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	for _, name := range []string{"sh", "sh-2"} {
		if _, ok := st.SessionByName(name); !ok {
			t.Errorf("no session %s", name)
		}
	}
}
