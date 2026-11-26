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

// fakeMoveHab is the new home: it keeps the record each adopt hands over.
type fakeMoveHab struct {
	mu      sync.Mutex
	adopted *api.Group
	commits int
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
	os.Chmod(script, 0o700)
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
