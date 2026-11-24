package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestSessionCloseRefusesALiveProgram: session.close removes only the record of a program known
// to have ended (hab-logic-api.md 3.9). A running session, a spawning one, and one that shows
// exited only because its holder was not reached are refused with conflict and stay.
func TestSessionCloseRefusesALiveProgram(t *testing.T) {
	t.Parallel()
	h := structure.NewFakeHolder(
		structure.HolderPane{ID: "run"},
		structure.HolderPane{ID: "lost"},
		structure.HolderPane{ID: "done"},
		structure.HolderPane{ID: "closed"},
	)
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	mk := func(name, pane string, fresh bool) api.Session {
		t.Helper()
		ss, err := st.CreateSession(structure.SessionSpec{Name: name, Pane: pane, NewPane: fresh})
		if err != nil {
			t.Fatalf("session %s: %v", name, err)
		}
		return ss
	}
	running := mk("running", "run", false)
	lost := mk("lost", "lost", false)
	done := mk("done", "done", false)
	closed := mk("closed", "closed", false)
	spawning := mk("spawning", "not-yet", true)
	a := &actions{store: st}

	refused := func(what string, ss api.Session) {
		t.Helper()
		if err := a.CloseSession(ss.ID); errKind(err) != api.ErrorKindConflict {
			t.Errorf("close %s: %v, want conflict", what, err)
		}
		if _, ok := st.Session(ss.ID); !ok {
			t.Errorf("the %s session's record went", what)
		}
	}
	closes := func(what string, ss api.Session) {
		t.Helper()
		if err := a.CloseSession(ss.ID); err != nil {
			t.Errorf("close %s: %v", what, err)
		}
		if _, ok := st.Session(ss.ID); ok {
			t.Errorf("the %s session's record stays", what)
		}
	}

	refused("a running", running)
	refused("a spawning", spawning)

	// A holder not reached: its pane looks gone, the session exited with no status. Its program
	// may still run.
	h.SetPartial(true)
	h.Remove("lost")
	if ss, _ := st.Session(lost.ID); ss.State != api.SessionStateExited || ss.Exit != nil {
		t.Fatalf("the lost session: %+v", ss)
	}
	refused("an unreached holder's", lost)
	if err := a.CloseSession(lost.ID); err == nil || !strings.Contains(err.Error(), "holder is not reached") {
		t.Errorf("the unreached holder's refusal says why: %v", err)
	}
	h.SetPartial(false)
	closes("a gone pane's", lost) // every holder reached: the pane is gone for good

	// An exit status ends the program: the record goes, and the exited pane stays, unfiled.
	h.Exit("done", api.ExitStatus{})
	closes("an exited", done)
	// pane.close leaves no status; the pane is gone with every holder reached.
	h.Remove("closed")
	closes("a closed pane's", closed)

	if err := a.CloseSession("nope"); errKind(err) != api.ErrorKindNotFound {
		t.Errorf("close an unknown session: %v, want not_found", err)
	}
}
