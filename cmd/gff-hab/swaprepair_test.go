package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// The swap's crash and timeout safety (groups.md 9.5, review M6).

func swapStore(t *testing.T, dir string) *structure.Store {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: dir,
		Holders: structure.NewFakeHolder(structure.HolderPane{ID: "p1"}, structure.HolderPane{ID: "p2"})})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var swapGroup = api.SessionGroup{ID: "g1", Role: groups.RoleLead, Item: "a"}

// grouped lists the sessions that are members of g1.
func grouped(st *structure.Store) []api.Session {
	var out []api.Session
	for _, ss := range st.Members() {
		if ss.Group != nil && ss.Group.ID == swapGroup.ID {
			out = append(out, ss)
		}
	}
	return out
}

type swapCalls struct {
	mu     sync.Mutex
	moves  []string
	closed []string
	unheld []string
}

func (c *swapCalls) move(from, to string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moves = append(c.moves, from+">"+to)
	return 1, nil
}

func (c *swapCalls) closePane(_ context.Context, pane string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, pane)
	return nil
}

func (c *swapCalls) unhold(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unheld = append(c.unheld, id)
}

// TestSwapCreateTimeoutKeepsSuccessor: a create that answers an error after it made the fresh
// session does not undo the swap: the group keeps exactly one session with the member's name.
func TestSwapCreateTimeoutKeepsSuccessor(t *testing.T) {
	st := swapStore(t, filepath.Join(t.TempDir(), "structure"))
	defer st.Close()
	old, err := st.CreateSession(structure.SessionSpec{Name: "lead-a", Pane: "p1", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	g := swapGroup
	if err := st.SetGroup(old.ID, &g, nil); err != nil {
		t.Fatal(err)
	}
	old, _ = st.Session(old.ID)
	calls := &swapCalls{}
	w := &memberSwap{store: st, move: calls.move, closePane: calls.closePane, logf: t.Logf,
		hold: func(context.Context, string, string) (func(), error) { return func() {}, nil },
		rename: func(id, name string) error {
			_, err := st.RenameSession(id, name)
			return err
		},
		folder: func(context.Context, string) string { return "/work/a" },
		create: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			if _, err := st.CreateSession(structure.SessionSpec{Name: p.Name, Pane: "p2", Index: -1, Group: p.Group}); err != nil {
				t.Errorf("create: %v", err)
			}
			return api.SessionCreated{}, context.DeadlineExceeded
		}}
	id, err := w.swap(context.Background(), old, api.AgentState{Harness: "claude"}, nil, "seed")
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	members := grouped(st)
	if len(members) != 1 || members[0].ID != id || members[0].Name != "lead-a" {
		t.Fatalf("grouped sessions %+v, want only the fresh one %s", members, id)
	}
	o, _ := st.Session(old.ID)
	if o.Name != "lead-a-1" || o.Group != nil {
		t.Fatalf("old session %+v", o)
	}
	if n := len(st.Swaps()); n != 0 {
		t.Fatalf("%d swap marks left", n)
	}
	if len(calls.moves) != 1 || calls.moves[0] != old.ID+">"+id || len(calls.closed) != 1 || calls.closed[0] != "p1" {
		t.Fatalf("moves %q closed %q", calls.moves, calls.closed)
	}
}

// swapCutShort leaves the state of a gff-hab killed mid-swap: the old session renamed, out of
// the group, its mark on disk; with fresh, the fresh session was made too.
func swapCutShort(t *testing.T, dir string, fresh bool) (oldID, freshID string) {
	st := swapStore(t, dir)
	defer st.Close()
	old, err := st.CreateSession(structure.SessionSpec{Name: "lead-a-1", Pane: "p1", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSwap(old.ID, &structure.SwapMark{Name: "lead-a", Aside: "lead-a-1", Group: swapGroup}); err != nil {
		t.Fatal(err)
	}
	if fresh {
		g := swapGroup
		s2, err := st.CreateSession(structure.SessionSpec{Name: "lead-a", Pane: "p2", Index: -1, Group: &g})
		if err != nil {
			t.Fatal(err)
		}
		freshID = s2.ID
	}
	return old.ID, freshID
}

// TestSwapRepairUndo: a swap cut short before the fresh session was made is undone at start:
// the old session has its name and group back, and the mark is gone.
func TestSwapRepairUndo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "structure")
	oldID, _ := swapCutShort(t, dir, false)
	st := swapStore(t, dir)
	defer st.Close()
	if finish := undoSwaps(st, t.Logf); len(finish) != 0 {
		t.Fatalf("to finish %v", finish)
	}
	members := grouped(st)
	if len(members) != 1 || members[0].ID != oldID || members[0].Name != "lead-a" {
		t.Fatalf("grouped sessions %+v", members)
	}
	if n := len(st.Swaps()); n != 0 {
		t.Fatalf("%d swap marks left", n)
	}
}

// TestSwapRepairFinish: a swap cut short after the fresh session was made is finished at start:
// the old session's deliveries are held until its messages moved, its pane closes, and exactly
// one grouped session holds the name.
func TestSwapRepairFinish(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "structure")
	oldID, freshID := swapCutShort(t, dir, true)
	st := swapStore(t, dir)
	defer st.Close()
	finish := undoSwaps(st, t.Logf)
	if len(finish) != 1 || finish[oldID] != freshID {
		t.Fatalf("to finish %v, want %s to %s", finish, oldID, freshID)
	}
	calls := &swapCalls{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finishSwaps(ctx, st, finish, calls.move, calls.unhold, calls.closePane, t.Logf)
	deadline := time.Now().Add(5 * time.Second)
	for len(st.Swaps()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the swap mark stayed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.moves) != 1 || calls.moves[0] != oldID+">"+freshID || len(calls.closed) != 1 || calls.closed[0] != "p1" {
		t.Fatalf("moves %q closed %q", calls.moves, calls.closed)
	}
	if len(calls.unheld) != 1 || calls.unheld[0] != oldID {
		t.Fatalf("unheld %q", calls.unheld)
	}
	members := grouped(st)
	if len(members) != 1 || members[0].ID != freshID || members[0].Replaced != oldID {
		t.Fatalf("grouped sessions %+v", members)
	}
}
