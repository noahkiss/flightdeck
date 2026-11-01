package main

import (
	"context"
	"fmt"
	"path/filepath"
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
// it even then, and a lost answer with no pane closes it too.
func TestCreateSpawnErrors(t *testing.T) {
	refused := &holderclient.RequestError{Ref: hp.KindSpawn, Code: hp.CodeDraining, Message: "holder is draining"}
	for _, c := range []struct {
		name   string
		err    error
		exists bool
		keep   bool
	}{
		{"timeout, pane exists", context.DeadlineExceeded, true, true},
		{"link closed, pane exists", link.ErrClosed, true, true},
		{"timeout, no pane", context.DeadlineExceeded, false, false},
		{"spawn-failed", refused, true, false},
		{"no holder", holderclient.ErrNoHolder, true, false},
		{"uuid used", link.ErrUUIDUsed, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked hp.UUID
			a, st := fakeActions(t,
				func(_ context.Context, s hp.Spawn) (link.PaneStatus, error) {
					asked = s.UUID
					return link.PaneStatus{}, c.err
				},
				func(u hp.UUID) bool { return c.exists && u == asked })
			session, pane, err := a.CreateSession(context.Background(), api.SessionCreateParams{Name: "s", Argv: []string{"/bin/true"}})
			_, have := st.SessionByName("s")
			if c.keep {
				if err != nil || session == "" || pane != asked.String() || !have {
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
