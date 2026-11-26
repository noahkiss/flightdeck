package main

// A group move and its lane (groups.md 5.7 steps 1 and 2): the lane takes no new item while
// the group moves, a run before its push stops, a land past its push finishes first, at most
// [groups] move_drain, and the old home's lane worktree goes once no group here needs it. The
// new hab is a fake that takes group.adopt and group.adopt_commit.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
)

// fakeMoveHab is the new home: it keeps the record each adopt hands over. As a group's home
// for a move queued on the fixture's hab, it answers group.move with draining.
type fakeMoveHab struct {
	mu      sync.Mutex
	adopted *api.Group
	commits int
	moves   int
}

func (h *fakeMoveHab) Call(ctx context.Context, method string, params, result any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "group.adopt":
		rec := params.(api.GroupAdoptParams).Record
		h.adopted = &rec
	case "group.adopt_commit":
		h.commits++
	case "group.move":
		h.moves++
		*result.(*api.GroupMoveResult) = api.GroupMoveResult{State: "draining", Home: "dev-me", Text: "draining"}
	default:
		return fmt.Errorf("unexpected %s", method)
	}
	return nil
}

func (h *fakeMoveHab) record() *api.Group {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.adopted
}

// moveFix is a lane fixture with a linked second hab, and a lane script that writes started
// and then waits until release exists (exit 0) or forever when release is "".
func moveFix(t *testing.T, step string, keys groups.Keys) (f *laneFix, started, release string) {
	t.Helper()
	d := t.TempDir()
	script, started, release := filepath.Join(d, "step.sh"), filepath.Join(d, "started"), filepath.Join(d, "release")
	writeFile(t, script, fmt.Sprintf("#!/bin/sh\ntouch %q\nwhile [ ! -e %q ]; do sleep 0.05; done\n", started, release))
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}
	f = newLaneFix(t, fmt.Sprintf(`{"lane":{%q:%q}}`, step, script), keys)
	f.peer = &fakeMoveHab{}
	return f, started, release
}

func (f *laneFix) tomb(id string) *api.Group {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if t := f.svc.tombs[id]; t != nil {
		c := *t
		return &c
	}
	return nil
}

func (f *laneFix) moveTo(id string) api.GroupMoveResult {
	f.t.Helper()
	res, err := f.svc.Move(context.Background(), api.GroupMoveParams{Group: id, Device: "dev-new"})
	if err != nil {
		f.t.Fatalf("move: %v", err)
	}
	return res
}

func (f *laneFix) laneWorktrees() []string {
	m, _ := filepath.Glob(filepath.Join(f.laneDir, "*"))
	return m
}

// TestLaneMoveStopsRunBeforePush: a move while the item's checks run stops the run before its
// push: the move commits at once, the item goes to the new home ready at the front of the
// queue, nothing lands, and the old home's lane worktree is removed.
func TestLaneMoveStopsRunBeforePush(t *testing.T) {
	f, started, _ := moveFix(t, "checks", groups.DefaultKeys)
	a := f.branch("group/m1/a", "origin/main", "a.txt")
	b := f.branch("group/m1/b", "origin/main", "b.txt")
	id := "a1a1a1a1a1a1a1a1"
	f.group(id, laneItem{id: "a", head: a}, laneItem{id: "b", head: b})
	waitUntil(t, "the checks to start", func() bool { return exists(started) })
	if len(f.laneWorktrees()) != 1 {
		t.Fatalf("lane worktrees %v", f.laneWorktrees())
	}
	if res := f.moveTo(id); res.State != "moved" {
		t.Fatalf("move %+v", res)
	}
	rec := f.peer.record()
	if rec == nil {
		t.Fatal("no adopt")
	}
	if it := rec.Items[groups.ItemIndex(*rec, "a")]; it.State != groups.ItemReady || rec.Lane.Landing != "" || !slices.Equal(rec.Lane.Queue, []string{"a", "b"}) {
		t.Fatalf("adopted item %+v, lane %+v", it, rec.Lane)
	}
	if tb := f.tomb(id); tb == nil || tb.Moved == nil {
		t.Fatalf("no tombstone: %+v", tb)
	}
	if got := f.subjects(); got != "first" {
		t.Fatalf("main moved: %s", got)
	}
	waitUntil(t, "the lane worktree to go", func() bool { return len(f.laneWorktrees()) == 0 })
	if out := lgit(t, f.src, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Fatalf("worktrees left: %s", out)
	}
}

// TestLaneMoveDrainsLand: a move while an item's CI runs, past its push, answers draining; the
// land finishes, the lane takes no next item, and the group moves with the item landed.
func TestLaneMoveDrainsLand(t *testing.T) {
	f, started, release := moveFix(t, "ci", groups.DefaultKeys)
	a := f.branch("group/m2/a", "origin/main", "a.txt")
	b := f.branch("group/m2/b", "origin/main", "b.txt")
	id := "a2a2a2a2a2a2a2a2"
	f.group(id, laneItem{id: "a", head: a}, laneItem{id: "b", head: b})
	waitUntil(t, "CI to start", func() bool { return exists(started) })
	res := f.moveTo(id)
	if res.State != "draining" || res.Home != "dev-new" {
		t.Fatalf("move %+v", res)
	}
	if g := f.get(id); g.Moving == nil || f.peer.record() != nil {
		t.Fatalf("while draining: moving %+v, adopted %v", g.Moving, f.peer.record() != nil)
	}
	os.Remove(started)
	writeFile(t, release, "")
	waitUntil(t, "the tombstone", func() bool { tb := f.tomb(id); return tb != nil && tb.Moved != nil })
	rec := f.peer.record()
	ia, ib := rec.Items[groups.ItemIndex(*rec, "a")], rec.Items[groups.ItemIndex(*rec, "b")]
	if ia.State != groups.ItemLanded || ib.State != groups.ItemReady || rec.Lane.Landing != "" {
		t.Fatalf("adopted a %+v, b %+v, lane %+v", ia, ib, rec.Lane)
	}
	if exists(started) {
		t.Fatal("the lane started item b while the group moved")
	}
	if got := f.subjects(); got != "change a.txt,first" {
		t.Fatalf("main: %s", got)
	}
}

// TestLaneMoveDrainLimit: a land past its push that outlasts move_drain is stopped; its item
// moves ready at the front of the queue, and a lane worktree another group here uses stays.
func TestLaneMoveDrainLimit(t *testing.T) {
	keys := groups.DefaultKeys
	keys.MoveDrain = 300 * time.Millisecond
	f, started, _ := moveFix(t, "ci", keys)
	a := f.branch("group/m3/a", "origin/main", "a.txt")
	other := makeGroup("b3b3b3b3b3b3b3b3", laneItem{id: "x", head: a, state: groups.ItemWorking})
	other.State = groups.StatePaused
	f.put(other) // a live group in the same repository
	id := "a3a3a3a3a3a3a3a3"
	f.group(id, laneItem{id: "a", head: a})
	waitUntil(t, "CI to start", func() bool { return exists(started) })
	if res := f.moveTo(id); res.State != "draining" {
		t.Fatalf("move %+v", res)
	}
	waitUntil(t, "the tombstone", func() bool { tb := f.tomb(id); return tb != nil && tb.Moved != nil })
	rec := f.peer.record()
	if it := rec.Items[0]; it.State != groups.ItemReady || rec.Lane.Landing != "" || !slices.Equal(rec.Lane.Queue, []string{"a"}) {
		t.Fatalf("adopted item %+v, lane %+v", it, rec.Lane)
	}
	if got := f.subjects(); got != "first" {
		t.Fatalf("main moved: %s", got)
	}
	f.svc.Stop() // waits for any removal
	if len(f.laneWorktrees()) != 1 {
		t.Fatalf("the shared lane worktree went: %v", f.laneWorktrees())
	}
}

// TestLaneMoveDrainRefusesClose: while a land drains, close and pause answer unavailable, so
// no end splits the group; the move then commits with the group running and the item landed.
func TestLaneMoveDrainRefusesClose(t *testing.T) {
	f, started, release := moveFix(t, "ci", groups.DefaultKeys)
	a := f.branch("group/m4/a", "origin/main", "a.txt")
	id := "a4a4a4a4a4a4a4a4"
	f.group(id, laneItem{id: "a", head: a})
	waitUntil(t, "CI to start", func() bool { return exists(started) })
	if res := f.moveTo(id); res.State != "draining" {
		t.Fatalf("move %+v", res)
	}
	if _, err := f.svc.Close(context.Background(), id); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("close while draining: %v", err)
	}
	if _, err := f.svc.end(id, "", false); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("end while draining: %v", err)
	}
	if _, err := f.svc.Pause(id); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("pause while draining: %v", err)
	}
	if g := f.get(id); g.State != groups.StateRunning || g.Moving == nil {
		t.Fatalf("while draining: %+v", g)
	}
	writeFile(t, release, "")
	waitUntil(t, "the tombstone", func() bool { tb := f.tomb(id); return tb != nil && tb.Moved != nil })
	rec := f.peer.record()
	if rec.State != groups.StateRunning || rec.Items[0].State != groups.ItemLanded {
		t.Fatalf("adopted %s, item %+v", rec.State, rec.Items[0])
	}
}

// TestLaneMoveStateChangedBeforeAdopt: a group that is no longer running or paused when the
// drain ends does not move; moving clears and the card says why.
func TestLaneMoveStateChangedBeforeAdopt(t *testing.T) {
	f, started, release := moveFix(t, "ci", groups.DefaultKeys)
	a := f.branch("group/m5/a", "origin/main", "a.txt")
	id := "a5a5a5a5a5a5a5a5"
	f.group(id, laneItem{id: "a", head: a})
	waitUntil(t, "CI to start", func() bool { return exists(started) })
	if res := f.moveTo(id); res.State != "draining" {
		t.Fatalf("move %+v", res)
	}
	f.svc.mu.Lock()
	f.svc.groups[id].State = groups.StateEnding // as a path that skips the guard would
	f.svc.mu.Unlock()
	writeFile(t, release, "")
	waitUntil(t, "the move to fail", func() bool { g := f.get(id); return g.Moving == nil && g.MoveFailed != "" })
	if g := f.get(id); !strings.Contains(g.MoveFailed, "became ending") || f.peer.record() != nil || f.tomb(id) != nil {
		t.Fatalf("move_failed %q, adopted %v", g.MoveFailed, f.peer.record() != nil)
	}
}

// TestLaneMoveRestartMidDrain: a hab that stops while a land drains keeps the group; its next
// start clears moving and the card says why. Nothing reached the new home.
func TestLaneMoveRestartMidDrain(t *testing.T) {
	f, started, _ := moveFix(t, "ci", groups.DefaultKeys)
	a := f.branch("group/m6/a", "origin/main", "a.txt")
	id := "a6a6a6a6a6a6a6a6"
	f.group(id, laneItem{id: "a", head: a})
	waitUntil(t, "CI to start", func() bool { return exists(started) })
	if res := f.moveTo(id); res.State != "draining" {
		t.Fatalf("move %+v", res)
	}
	f.svc.Stop()
	f.start()
	g := f.get(id)
	if g.Moving != nil || !strings.Contains(g.MoveFailed, "restarted during it") {
		t.Fatalf("after the restart: moving %+v, move_failed %q", g.Moving, g.MoveFailed)
	}
	if f.peer.record() != nil || f.tomb(id) != nil {
		t.Fatal("the group moved")
	}
}

// TestLaneMoveLockWait: a run that waits for the repository's lane lock, behind another
// group's run, leaves the wait at once when its group moves; the move commits with the item
// ready at the front, and the worktree the other group uses stays.
func TestLaneMoveLockWait(t *testing.T) {
	f, started, _ := moveFix(t, "ci", groups.DefaultKeys)
	x := f.branch("group/m7/x", "origin/main", "x.txt")
	a := f.branch("group/m7/a", "origin/main", "a.txt")
	f.group("b7b7b7b7b7b7b7b7", laneItem{id: "x", head: x})
	waitUntil(t, "the other group's CI to start", func() bool { return exists(started) })
	id := "a7a7a7a7a7a7a7a7"
	f.group(id, laneItem{id: "a", head: a})
	f.waitItem(id, "a", groups.ItemLanding)
	done := make(chan api.GroupMoveResult, 1)
	go func() { done <- f.moveTo(id) }()
	select {
	case res := <-done:
		if res.State != "moved" {
			t.Fatalf("move %+v", res)
		}
	case <-time.After(scaleTimeout(10 * time.Second)):
		t.Fatal("the move waited behind the other group's run")
	}
	rec := f.peer.record()
	if it := rec.Items[0]; it.State != groups.ItemReady || len(it.Attempts) != 0 || !slices.Equal(rec.Lane.Queue, []string{"a"}) {
		t.Fatalf("adopted item %+v, lane %+v", it, rec.Lane)
	}
	if len(f.laneWorktrees()) != 1 {
		t.Fatalf("lane worktrees %v", f.laneWorktrees())
	}
}

// TestLaneMoveQueuedDraining: a queued move the home answers with draining stays, marked, and
// is not sent again; while the home's catalog copy shows the move, it waits; once the drain's
// time has passed and the home kept the group, it goes with one notice. queued_move_days
// does not expire it meanwhile.
func TestLaneMoveQueuedDraining(t *testing.T) {
	f, _, _ := moveFix(t, "ci", groups.DefaultKeys)
	id := "a8a8a8a8a8a8a8a8"
	if _, err := f.svc.queue(id, groups.Where{Device: "dev-new", Entry: api.CatalogGroup{ID: id, Title: "Shop"}}, "new"); err != nil {
		t.Fatal(err)
	}
	queued := func() []groups.Queued {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return slices.Clone(f.svc.queued)
	}
	f.svc.moveTick()
	waitUntil(t, "the draining mark", func() bool { q := queued(); return len(q) == 1 && q[0].Draining != "" })
	loaded, err := f.svc.st.LoadQueued(time.Now())
	if err != nil || len(loaded) != 1 || loaded[0].Draining == "" {
		t.Fatalf("saved %+v %v", loaded, err)
	}
	for range 3 {
		f.svc.moveTick()
	}
	time.Sleep(100 * time.Millisecond)
	f.peer.mu.Lock()
	moves := f.peer.moves
	f.peer.mu.Unlock()
	if moves != 1 {
		t.Fatalf("group.move sent %d times", moves)
	}
	// The drain's time has passed, and so has queued_move_days: still moving there, it waits,
	// with no "dropped" notice.
	f.svc.mu.Lock()
	f.svc.queued[0].Draining = stampT(time.Now().Add(-time.Hour))
	f.svc.queued[0].At = stampT(time.Now().Add(-f.keys.QueuedMove - time.Hour))
	f.svc.mu.Unlock()
	f.mu.Lock()
	f.peerGroups = []api.CatalogGroup{{ID: id, State: "running", Home: "dev-new", Generation: 1, Moving: &api.GroupMoveRef{To: "dev-me", Generation: 2}}}
	f.mu.Unlock()
	f.svc.moveTick()
	if q := queued(); len(q) != 1 {
		t.Fatalf("dropped while the home still moves it: %+v", q)
	}
	// The home kept the group: the request goes, with one notice.
	f.mu.Lock()
	f.peerGroups[0].Moving = nil
	f.mu.Unlock()
	f.svc.moveTick()
	if q := queued(); len(q) != 0 {
		t.Fatalf("kept %+v", q)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.notices, []string{"Group move failed"}) {
		t.Fatalf("notices %v", f.notices)
	}
}

// TestLaneSweepAtStart: a lane worktree that no group here lands in, left by a removal a stop
// cut short, goes at the next start.
func TestLaneSweepAtStart(t *testing.T) {
	f, started, _ := moveFix(t, "checks", groups.DefaultKeys)
	a := f.branch("group/m9/a", "origin/main", "a.txt")
	id := "a9a9a9a9a9a9a9a9"
	f.group(id, laneItem{id: "a", head: a})
	waitUntil(t, "the checks to start", func() bool { return exists(started) })
	f.svc.Stop()
	if len(f.laneWorktrees()) != 1 {
		t.Fatalf("lane worktrees %v", f.laneWorktrees())
	}
	if err := f.svc.st.Remove(id); err != nil { // as if the group had moved away
		t.Fatal(err)
	}
	f.start()
	waitUntil(t, "the lane worktree to go", func() bool { return len(f.laneWorktrees()) == 0 })
	if out := lgit(t, f.src, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Fatalf("worktrees left: %s", out)
	}
}
