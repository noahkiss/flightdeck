package structure

import (
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestResumeSession: an exited session points at a new pane and runs again with its id, name,
// kind and folder; the old pane, still live, becomes unfiled. A running session, a used pane
// and an unknown session are refused. A failed spawn leaves it exited with no pane. A gff-hab
// that stops before the resumed pane shows keeps the record, exited, even with every holder
// reached.
func TestResumeSession(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"))
	s, rec := open(t, dir, h)
	a, err := s.CreateSession(SessionSpec{Name: "capcom", Pane: "p1", Kind: "capcom", Dir: "/state/capcom/c1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != api.SessionKindCapcom || a.Dir != "/state/capcom/c1" {
		t.Fatalf("kind and folder %+v", a)
	}
	if _, err := s.ResumeSession(a.ID, "p2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("resume of a running session: %v", err)
	}
	h.Exit("p1", api.ExitStatus{})
	rec.take()

	for _, bad := range []struct {
		id, pane string
		want     error
	}{
		{"nope", "p2", ErrNotFound},
		{a.ID, "", ErrInvalid},
		{a.ID, "p1", ErrConflict}, // live
	} {
		if _, err := s.ResumeSession(bad.id, bad.pane); !errors.Is(err, bad.want) {
			t.Errorf("resume %s into %q: %v, want %v", bad.id, bad.pane, err, bad.want)
		}
	}
	got, err := s.ResumeSession(a.ID, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID || got.Pane != "p2" || got.State != api.SessionStateRunning || got.Exit != nil || got.Kind != api.SessionKindCapcom || got.Revision <= a.Revision {
		t.Fatalf("resumed %+v", got)
	}
	mustEvents(t, rec.take(), "session.updated:"+a.ID, "pane.updated:p1")
	if u := s.Unfiled(); len(u) != 1 || u[0] != "p1" {
		t.Fatalf("unfiled %v", u)
	}
	h.Add(pane("p2"))
	if ss, _ := s.Session(a.ID); ss.State != api.SessionStateRunning {
		t.Fatalf("after the pane showed: %+v", ss)
	}

	// The spawn failed: exited again, no pane.
	h.Exit("p2", api.ExitStatus{})
	if _, err := s.ResumeSession(a.ID, "p3"); err != nil {
		t.Fatal(err)
	}
	s.ResumeFailed(a.ID, "p3")
	if ss, _ := s.Session(a.ID); ss.State != api.SessionStateExited || ss.Pane != "" {
		t.Fatalf("after a failed spawn: %+v", ss)
	}
	s.ResumeFailed(a.ID, "p3") // no longer its pane: nothing

	// Stopped before the spawn: the record stays, exited.
	if _, err := s.ResumeSession(a.ID, "p4"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, _ := open(t, dir, h)
	ss, ok := s2.Session(a.ID)
	if !ok || ss.State != api.SessionStateExited || ss.Kind != api.SessionKindCapcom || ss.Dir != "/state/capcom/c1" {
		t.Fatalf("after a restart before the spawn: %+v %v", ss, ok)
	}
	if _, err := s2.ResumeSession(a.ID, "p5"); err != nil {
		t.Fatalf("resume after the restart: %v", err)
	}
}
