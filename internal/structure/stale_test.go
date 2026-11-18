package structure

import (
	"slices"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestSetStale: the flag shows on the session and raises its revision once per change; a resume
// clears it; it is never saved.
func TestSetStale(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"))
	s, rec := open(t, dir, h)
	a, err := s.CreateSession(SessionSpec{Name: "a", Pane: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	rec.take()
	if !s.SetStale(a.ID, []string{"instructions"}) {
		t.Fatal("no change")
	}
	if s.SetStale(a.ID, []string{"instructions"}) || s.SetStale("nope", []string{"instructions"}) {
		t.Fatal("the same flag, or an unknown session, changed a record")
	}
	mustEvents(t, rec.take(), "session.updated:"+a.ID)
	got, _ := s.Session(a.ID)
	if !slices.Equal(got.Stale, []string{"instructions"}) || got.Revision != a.Revision+1 {
		t.Fatalf("session %+v", got)
	}
	s.Close()
	s, _ = open(t, dir, h)
	if got, _ := s.Session(a.ID); got.Stale != nil {
		t.Fatalf("saved: %+v", got)
	}
	s.SetStale(a.ID, []string{"instructions"})
	h.Exit("p1", api.ExitStatus{})
	if _, err := s.ResumeSession(a.ID, "p2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Session(a.ID); got.Stale != nil {
		t.Fatalf("after a resume: %+v", got)
	}
}
