package structure

import (
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// A session with no pane (the Capcom service session) is filed, survives a reopen, and is found
// through its route (capcom-service.md 2.1, 3.3).
func TestNoPaneSessionAndRoute(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder()
	s, _ := open(t, dir, h)
	if _, err := s.CreateSession(SessionSpec{Name: "x", NoPane: true, Pane: "p1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a pane with NoPane: %v", err)
	}
	ss, err := s.CreateSession(SessionSpec{Name: "capcom", NoPane: true, Kind: "capcom"})
	if err != nil || ss.Pane != "" || ss.State != api.SessionStateRunning {
		t.Fatalf("%v %+v", err, ss)
	}
	if _, err := s.CreateSession(SessionSpec{Name: "capcom-2", NoPane: true}); err != nil {
		t.Fatalf("a second pane-less session: %v", err)
	}
	if _, ok := s.SessionByPane("capcom-route"); ok {
		t.Fatal("a route found before it was set")
	}
	s.SetRoute("capcom-route", ss.ID)
	if got, ok := s.SessionByPane("capcom-route"); !ok || got.ID != ss.ID {
		t.Fatal("the route does not find the session")
	}
	s.SetRoute("capcom-route", "")
	if _, ok := s.SessionByPane("capcom-route"); ok {
		t.Fatal("a removed route still finds the session")
	}
	s.Close()
	s2, _ := open(t, dir, h)
	if got, ok := s2.Session(ss.ID); !ok || got.State != api.SessionStateRunning || got.Kind != "capcom" {
		t.Fatalf("after a reopen: %v %+v", ok, got)
	}
}
