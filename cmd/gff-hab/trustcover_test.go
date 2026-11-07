package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// coverRig is the trust service over a fake pane whose start carried a group's trust cover
// (groups.md 4.2): why is what the cover check answers ("" for a match).
type coverRig struct {
	ts     *trustService
	agents *harness.Store
	f      *fakeTrustPane
	mu     sync.Mutex
	shown  []time.Time // each agent.state that showed blocked:trust
	notes  []string
}

func newCoverRig(t *testing.T, why string, hold time.Duration) *coverRig {
	t.Helper()
	shortTrust(t)
	r := &coverRig{f: &fakeTrustPane{}}
	r.agents = harness.NewStore(harness.Config{Publish: func(e structure.Event) {
		if st, ok := e.Data.(api.AgentState); ok && st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindTrust {
			r.mu.Lock()
			r.shown = append(r.shown, time.Now())
			r.mu.Unlock()
		}
	}})
	r.ts = newTrustService(r.agents, func(string) ([]string, error) { return r.f.rows() }, r.f.write, r.f.live, func(string, ...any) {})
	r.ts.hold = hold
	r.ts.covers = func(_ context.Context, harness string, pc paneCover) string {
		if harness != "claude" || pc.folder != "/work/a" || pc.cover.Group != "g1" {
			t.Errorf("cover check got %s %+v", harness, pc)
		}
		return why
	}
	r.ts.tell = func(pane, text string) {
		r.mu.Lock()
		r.notes = append(r.notes, text)
		r.mu.Unlock()
	}
	t.Cleanup(r.ts.Close)
	return r
}

var testCover = &api.TrustCover{Group: "g1", Confirmation: "none", Harness: "claude", BranchPrefix: "group/g1/"}

func (r *coverRig) shownAt() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.shown...)
}

func (r *coverRig) notesNow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notes...)
}

// TestCoverAnswered: a covered prompt is answered by the hab with Down and Enter, each its own
// write, and never shows: no blocked:trust report, so no board row and no push. While held, a
// message waits as busy.
func TestCoverAnswered(t *testing.T) {
	r := newCoverRig(t, "", 2*time.Second)
	r.f.block = make(chan struct{})
	r.ts.watch("p", "claude", "/work/a", testCover)
	waitUntil(t, "the prompt held back", func() bool { _, pending := r.ts.Declined("p"); return pending })
	close(r.f.block)
	waitUntil(t, "the answer", func() bool { return len(r.f.sent()) == 2 })
	if got := r.f.sent(); got[0] != "\x1b[B" || got[1] != "\r" {
		t.Fatalf("writes %q", got)
	}
	waitUntil(t, "the hold over", func() bool { _, pending := r.ts.Declined("p"); return !pending })
	time.Sleep(10 * trustPoll)
	if s := r.shownAt(); len(s) != 0 {
		t.Fatalf("the prompt showed %d times", len(s))
	}
	if n := r.notesNow(); len(n) != 0 {
		t.Fatalf("notes %q", n)
	}
}

// TestCoverMismatchShowsCard: a folder the cover does not answer shows the card at once, with
// no keys sent and no hold.
func TestCoverMismatchShowsCard(t *testing.T) {
	r := newCoverRig(t, "/work/a is the project's main checkout, not a group worktree", 5*time.Second)
	start := time.Now()
	r.ts.watch("p", "claude", "/work/a", testCover)
	waitUntil(t, "the card", func() bool { return len(r.shownAt()) > 0 })
	if d := r.shownAt()[0].Sub(start); d > 2*time.Second {
		t.Errorf("the card waited %s", d)
	}
	if len(r.f.sent()) != 0 {
		t.Errorf("writes %q", r.f.sent())
	}
}

// TestCoverNoCoverShowsCard: a start with no cover (a spawn outside a group) shows the card as
// before.
func TestCoverNoCoverShowsCard(t *testing.T) {
	r := newCoverRig(t, "", 5*time.Second)
	r.ts.watch("p", "claude", "/work/a", nil)
	waitUntil(t, "the card", func() bool { return len(r.shownAt()) > 0 })
	if len(r.f.sent()) != 0 {
		t.Errorf("writes %q", r.f.sent())
	}
}

// TestCoverInputLeavesCard: client input in the last second stops the hab's answer: nothing is
// sent, and the card shows when the hold ends; the spawner is told once.
func TestCoverInputLeavesCard(t *testing.T) {
	hold := 600 * time.Millisecond
	r := newCoverRig(t, "", hold)
	r.ts.inputStart("p")
	r.ts.inputEnd("p")
	start := time.Now()
	r.ts.watch("p", "claude", "/work/a", testCover)
	waitUntil(t, "the card", func() bool { return len(r.shownAt()) > 0 })
	if d := r.shownAt()[0].Sub(start); d < hold-trustPoll {
		t.Errorf("the card showed after %s, before the hold of %s", d, hold)
	}
	if len(r.f.sent()) != 0 {
		t.Errorf("writes %q", r.f.sent())
	}
	waitUntil(t, "the note", func() bool { return len(r.notesNow()) == 1 })
	if n := r.notesNow()[0]; !strings.Contains(n, "group g1") || !strings.Contains(n, "typed") {
		t.Errorf("note %q", n)
	}
	time.Sleep(10 * trustPoll)
	if n := r.notesNow(); len(n) != 1 {
		t.Errorf("notes %q", n)
	}
}

// TestCoverFailedAnswerAfterHold: an answer that fails (the keys reach nothing, so the cursor
// never reaches the trust row and no Enter goes) is tried again until the hold ends, then the
// card shows; Enter is never sent.
func TestCoverFailedAnswerAfterHold(t *testing.T) {
	hold := 900 * time.Millisecond
	r := newCoverRig(t, "", hold)
	r.f.stuck = true
	start := time.Now()
	r.ts.watch("p", "claude", "/work/a", testCover)
	waitUntil(t, "the card", func() bool { return len(r.shownAt()) > 0 })
	if d := r.shownAt()[0].Sub(start); d < hold-trustPoll {
		t.Errorf("the card showed after %s, before the hold of %s", d, hold)
	}
	for _, k := range r.f.sent() {
		if k == "\r" {
			t.Fatalf("Enter sent: %q", r.f.sent())
		}
	}
	waitUntil(t, "the note", func() bool { return len(r.notesNow()) == 1 })
	if st, ok := r.agents.Trusting("p"); !ok || st.Trust == nil || st.Trust.Folder != "/work/a" {
		t.Errorf("card state %+v", st)
	}
}
