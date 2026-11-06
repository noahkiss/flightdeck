package logbook

import (
	"context"
	"testing"
	"time"
)

// Fold numbers (rounds.md 4.3, folded since): a peer's op that folds into the middle of a
// record's history by its stamp is still after the last fold number a reader handled, so it is
// read once; a rebuild keeps every number.
func TestFoldedAfter(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "a"), newReplica(t, "b")
	a.write(t, &Actor{Kind: "person"}, create("task-abc1", "task", map[string]any{"title": "ship it", "status": "open"}))
	link(t, a, b)
	// b writes first by the clock, a later; b's op reaches a after a's own.
	b.write(t, &Actor{Kind: "agent", Session: "s1"}, set("task-abc1", "status", "doing"))
	a.clock.Advance(time.Hour)
	a.write(t, &Actor{Kind: "person"}, set("task-abc1", "title", "ship it now"))

	last, err := a.LastFold(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := a.FoldedAfter(ctx, "task-abc1", 0)
	if err != nil || len(ops) != 2 || ops[1].Fold != last {
		t.Fatalf("before the peer's op: %+v %v (last %d)", ops, err, last)
	}
	push(t, b, a)
	got, err := a.FoldedAfter(ctx, "task-abc1", last)
	if err != nil || len(got) != 1 || got[0].Field != "status" || got[0].Origin != "SHA256:b" {
		t.Fatalf("after the peer's op: %+v %v", got, err)
	}
	// The peer's op is in the middle of the history by stamp, but after `last` by fold.
	hist, err := a.History(ctx, "task-abc1")
	if err != nil || len(hist) != 3 || hist[1].Field != "status" {
		t.Fatalf("history order: %+v", hist)
	}
	after := got[0].Fold
	if again, _ := a.FoldedAfter(ctx, "task-abc1", after); len(again) != 0 {
		t.Fatalf("read twice: %+v", again)
	}
	if err := a.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := a.FoldedAfter(ctx, "task-abc1", after); len(again) != 0 {
		t.Fatalf("a rebuild gave new numbers: %+v", again)
	}
	if n, _ := a.LastFold(ctx); n != after {
		t.Fatalf("last fold %d after a rebuild, want %d", n, after)
	}
}
