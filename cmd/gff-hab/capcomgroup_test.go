package main

import (
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
)

type fakeGroupsCapcom struct{ session string }

func (f *fakeGroupsCapcom) IsService(s string) bool  { return s != "" && s == f.session }
func (f *fakeGroupsCapcom) Session() string          { return f.session }
func (f *fakeGroupsCapcom) GroupCard(string, string) {}

// A Capcom group follows the service session that runs now: a mark that left this hab and came
// back files a new record, and the new record coordinates (groups.md 12.1). Other groups keep
// their session.
func TestCapcomCoordinatorFollowsService(t *testing.T) {
	cc := &fakeGroupsCapcom{session: "s-old"}
	s := &groupsService{cfg: groupsConfig{capcom: cc}}
	g := api.Group{Coordinator: api.GroupCoordinator{Session: "s-old", Name: "capcom", Address: groups.CapcomAddress}}
	if !s.CoordinatedBy(g, "s-old") {
		t.Fatal("the service session does not coordinate")
	}
	cc.session = ""
	if s.CoordinatedBy(g, "s-old") || s.coordinatorOf(g) != "" {
		t.Fatal("the renamed record still coordinates while no service runs")
	}
	cc.session = "s-new"
	if !s.CoordinatedBy(g, "s-new") || s.CoordinatedBy(g, "s-old") {
		t.Fatal("the new service session does not coordinate")
	}
	if !groups.Short(g).Coordinator.Capcom {
		t.Fatal("the catalog form lacks the Capcom mark")
	}
	plain := api.Group{Coordinator: api.GroupCoordinator{Session: "s-alice", Name: "alice", Address: "hab/alice"}}
	if !s.CoordinatedBy(plain, "s-alice") || s.CoordinatedBy(plain, "s-new") || groups.Short(plain).Coordinator.Capcom {
		t.Fatal("a plain coordinator")
	}
}
