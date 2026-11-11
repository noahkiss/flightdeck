package groups

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func hostWith(dev string, gs ...api.CatalogGroup) api.CatalogEntry {
	return api.CatalogEntry{Catalog: api.Catalog{DeviceID: dev, Groups: gs}}
}

// TestFind: the highest generation wins; a tombstone counts as its move's generation and points
// at its to; at a tie the live entry wins (groups.md 5.7).
func TestFind(t *testing.T) {
	old := api.CatalogGroup{ID: "g", Generation: 1, Home: "A"}
	tomb := api.CatalogGroup{ID: "g", Generation: 1, Home: "A", Moved: &api.GroupMoveRef{To: "B", Generation: 2}}
	live := api.CatalogGroup{ID: "g", Generation: 2, Home: "B", Title: "live"}
	cases := []struct {
		name  string
		hosts []api.CatalogEntry
		dev   string
		live  bool
	}{
		{"old copy only", []api.CatalogEntry{hostWith("A", old)}, "A", true},
		{"tombstone only", []api.CatalogEntry{hostWith("A", tomb)}, "B", false},
		{"tombstone and the old copy of it", []api.CatalogEntry{hostWith("C", old), hostWith("A", tomb)}, "B", false},
		{"live at the tombstone's generation", []api.CatalogEntry{hostWith("A", tomb), hostWith("B", live)}, "B", true},
		{"live first", []api.CatalogEntry{hostWith("B", live), hostWith("A", tomb)}, "B", true},
		{"a later move", []api.CatalogEntry{hostWith("B", api.CatalogGroup{ID: "g", Generation: 2, Moved: &api.GroupMoveRef{To: "C", Generation: 3}}), hostWith("A", tomb)}, "C", false},
	}
	for _, c := range cases {
		w, ok := Find(api.CatalogList{Hosts: c.hosts}, "g")
		if !ok || w.Device != c.dev || (w.Entry.Moved == nil) != c.live {
			t.Errorf("%s: %+v %v", c.name, w, ok)
		}
	}
	if _, ok := Find(api.CatalogList{Hosts: []api.CatalogEntry{hostWith("A", old)}}, "other"); ok {
		t.Error("found a group that is not there")
	}
}

// TestMovableAndDrain: only a running or paused group that is not moving moves; a land before
// its push goes back to the front of the queue.
func TestMovableAndDrain(t *testing.T) {
	g := api.Group{ID: "g", State: StateRunning}
	if why := Movable(g); why != "" {
		t.Fatal(why)
	}
	for _, st := range []api.GroupState{StateConfirming, StateEnding, StateEnded, StateDropped} {
		g.State = st
		if Movable(g) == "" {
			t.Errorf("%s moves", st)
		}
	}
	g.State, g.Moving = StatePaused, &api.GroupMoveRef{To: "B", Label: "bee"}
	if Movable(g) == "" {
		t.Error("a moving group moves again")
	}
	g = api.Group{Items: []api.GroupItem{{ID: "a", State: ItemReady}, {ID: "b", State: ItemLanding}},
		Lane: api.GroupLaneState{Queue: []string{"a"}, Landing: "b"}}
	if !Drain(&g) || g.Items[1].State != ItemReady || g.Lane.Landing != "" || len(g.Lane.Queue) != 2 || g.Lane.Queue[0] != "b" {
		t.Fatalf("drained %+v", g)
	}
	if Drain(&g) {
		t.Fatal("a second drain changed something")
	}
}

// TestMoveKeys: the [groups] keys of the movable home, their ranges and defaults.
func TestMoveKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(p, []byte("[groups]\nmove_answer = \"10s\"\nmove_drain = \"5s\"\nmove_retry = 3\ntombstone_keep = 6\nqueued_move_days = 2\n"), 0o600)
	var logs int
	k := LoadKeys(p, func(string, ...any) { logs++ })
	if k.MoveAnswer != 10*time.Second || k.MoveDrain != DefaultKeys.MoveDrain || k.MoveRetry != DefaultKeys.MoveRetry ||
		k.TombstoneKeep != DefaultKeys.TombstoneKeep || k.QueuedMove != 48*time.Hour {
		t.Fatalf("keys %+v", k)
	}
	if logs != 3 {
		t.Fatalf("logged %d lines, want move_drain, move_retry and tombstone_keep", logs)
	}
}

// TestQueuedRoundTrip: the queued moves file, and one that does not parse set aside.
func TestQueuedRoundTrip(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if q, err := st.LoadQueued(time.Now()); err != nil || q != nil {
		t.Fatalf("no file: %v %v", q, err)
	}
	want := []Queued{{Group: "0123456789abcdef", Home: "A", At: "2026-11-07T00:00:00Z"}}
	if err := st.SaveQueued(want); err != nil {
		t.Fatal(err)
	}
	if q, err := st.LoadQueued(time.Now()); err != nil || len(q) != 1 || q[0] != want[0] {
		t.Fatalf("read back %v %v", q, err)
	}
	if gs, problems := st.Load(time.Now()); len(gs) != 0 || len(problems) != 0 {
		t.Fatalf("the queued file read as a record: %v %v", gs, problems)
	}
	os.WriteFile(filepath.Join(st.dir, queuedFile), []byte("{"), 0o600)
	if _, err := st.LoadQueued(time.Now()); err == nil {
		t.Fatal("a broken file read")
	}
}

// TestShortCopiesMoveRefs: the catalog form holds its own move refs, so the old home can mark
// its tombstone acknowledged while a peer's catalog.put is being marshalled.
func TestShortCopiesMoveRefs(t *testing.T) {
	g := api.Group{ID: "0123456789abcdef", Moving: &api.GroupMoveRef{To: "a"}, Moved: &api.GroupMoveRef{To: "b"}}
	c := Short(g)
	if c.Moving == g.Moving || c.Moved == g.Moved {
		t.Fatal("Short shares a move ref with the group")
	}
	g.Moved.Acked = true
	if c.Moved.Acked || c.Moved.To != "b" || c.Moving.To != "a" {
		t.Fatalf("catalog refs = %+v, %+v", c.Moving, c.Moved)
	}
	if c := Short(api.Group{}); c.Moving != nil || c.Moved != nil {
		t.Fatal("Short invents a move ref")
	}
}
