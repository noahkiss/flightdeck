package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestSessionActionSeedFrom: a recreate's follow-up names Capcom at its home: this hab's host for
// Capcom's own route, the Capcom home's peer label for its forward (hab-logic-api.md 3.13).
func TestSessionActionSeedFrom(t *testing.T) {
	t.Parallel()
	dr, ar := doorRig(t)
	dr.peer.DeviceID = "home-device" // the peer "bee" holds the Capcom mark
	dr.s.capcomHome = func() string { return "home-device" }
	got := make(chan string, 4)
	ar.acts.recreate = func(_ context.Context, _ api.Session, _ api.AgentState, _, seedFrom string) (string, error) {
		got <- seedFrom
		return "new", nil
	}
	ar.acts.recreatable = nil
	ctx := context.Background()
	dr.setRule(readPanesAll)
	for _, c := range []struct {
		caller  paneCaller
		session string
		want    string
	}{{fromCap, "other", "here/capcom"}, {fromCapPeer, "mate", "bee/capcom"}} {
		ar.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
		ar.agents.set(paneMate, api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
		if _, err := dr.s.Action(ctx, c.caller, api.SessionActionParams{Session: c.session, Action: "recreate", FollowUp: "carry on"}); err != nil {
			t.Fatal(err)
		}
		if s := <-got; s != c.want {
			t.Errorf("seed from %q, want %q", s, c.want)
		}
	}
	// The owner's recreate names no author: the follow-up is the owner's own.
	if a := dr.s.capcomAddress(fromOwner); a != "" {
		t.Errorf("owner: %q", a)
	}
}

// TestSessionActionRecheckPane: the recheck refuses once the session's pane is no longer the one
// the action was authorised for.
func TestSessionActionRecheckPane(t *testing.T) {
	t.Parallel()
	dr, _ := doorRig(t)
	dr.setRule(readPanesAll)
	ss, _ := dr.s.store.SessionByName("other")
	if err := dr.s.recheck(fromCap, ss.ID, ss.Pane)(); err != nil {
		t.Fatalf("same pane: %v", err)
	}
	err := dr.s.recheck(fromCap, ss.ID, paneMate)()
	if paneErrKind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "pane changed") {
		t.Fatalf("another pane: %v", err)
	}
	dr.setRule(readPanesOff)
	if err := dr.s.recheck(fromCap, ss.ID, ss.Pane)(); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("rule off: %v", err)
	}
}

// TestRecreateGroupMember: a group member's recreate is the group swap, with Capcom as the
// seed's author and the model the member was started with when no group record is at hand.
func TestRecreateGroupMember(t *testing.T) {
	t.Parallel()
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
	var made api.SessionCreateParams
	w := &memberSwap{store: st, move: calls.move, closePane: calls.closePane, logf: t.Logf,
		hold: func(context.Context, string, string) (func(), error) { return func() {}, nil },
		rename: func(id, name string) error {
			_, err := st.RenameSession(id, name)
			return err
		},
		folder: func(context.Context, string) string { return "/work/a" },
		start:  func(string) (resumeEntry, bool) { return resumeEntry{Harness: "claude", Model: "opus"}, true },
		create: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			made = p
			s, err := st.CreateSession(structure.SessionSpec{Name: p.Name, Pane: "p2", Index: -1, Group: p.Group})
			return api.SessionCreated{Session: s.ID}, err
		}}
	if _, err := w.recreate(context.Background(), old, api.AgentState{Harness: "claude"}, "carry on", "home/capcom"); err != nil {
		t.Fatal(err)
	}
	if made.SeedFrom != "home/capcom" || made.Model != "opus" || made.Seed != "carry on" || made.Group == nil {
		t.Fatalf("created %+v", made)
	}
}

// TestRecreateCutShort: a recreate a stop cut short leaves its swap mark with no group; the next
// start undoes it (the old name back, no group set) when no fresh session runs, and finishes it
// when one does.
func TestRecreateCutShort(t *testing.T) {
	t.Parallel()
	for _, fresh := range []bool{false, true} {
		st := swapStore(t, filepath.Join(t.TempDir(), "structure"))
		old, err := st.CreateSession(structure.SessionSpec{Name: "keep-1", Pane: "p1", Index: -1})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetSwap(old.ID, &structure.SwapMark{Name: "keep", Aside: "keep-1"}); err != nil {
			t.Fatal(err)
		}
		var freshID string
		if fresh {
			s2, err := st.CreateSession(structure.SessionSpec{Name: "keep", Pane: "p2", Index: -1})
			if err != nil {
				t.Fatal(err)
			}
			freshID = s2.ID
		}
		finish := undoSwaps(st, t.Logf)
		o, _ := st.Session(old.ID)
		switch {
		case fresh && (finish[old.ID] != freshID || o.Name != "keep-1"):
			t.Errorf("fresh: finish %v, old %+v", finish, o)
		case !fresh && (len(finish) != 0 || o.Name != "keep" || o.Group != nil || len(st.Swaps()) != 0):
			t.Errorf("undo: finish %v, old %+v, marks %v", finish, o, st.Swaps())
		}
		st.Close()
	}
}
