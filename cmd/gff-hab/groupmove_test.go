package main

import (
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestPendingVerdict (groups.md 5.7 steps 6 and 7): a pending copy commits from a tombstone that
// names this hab and its generation, and goes when a copy shows the move failed: the old home
// live without moving in a copy newer than the one held at the adopt, another hab live at its
// generation or a later one, or a tombstone of another move. An old copy of the old home, from
// before the freeze, changes nothing.
func TestPendingVerdict(t *testing.T) {
	s := &groupsService{cfg: groupsConfig{self: "B"}}
	pd := &api.Group{ID: "g", Generation: 3, Pending: true, From: &api.GroupFrom{Device: "A", Catalog: 10}}
	host := func(dev string, gen uint64, gs ...api.CatalogGroup) api.CatalogEntry {
		return api.CatalogEntry{Catalog: api.Catalog{DeviceID: dev, Generation: gen, Groups: gs}}
	}
	liveA := api.CatalogGroup{ID: "g", Generation: 1}
	movingA := api.CatalogGroup{ID: "g", Generation: 1, Moving: &api.GroupMoveRef{To: "B", Generation: 3}}
	cases := []struct {
		name  string
		hosts []api.CatalogEntry
		want  string
	}{
		{"the old home's copy from before the freeze", []api.CatalogEntry{host("A", 10, liveA)}, ""},
		{"the old home moving here", []api.CatalogEntry{host("A", 11, movingA)}, ""},
		{"the tombstone", []api.CatalogEntry{host("A", 12, api.CatalogGroup{ID: "g", Moved: &api.GroupMoveRef{To: "B", Generation: 3}})}, "commit"},
		{"a copy built before the freeze, seen late", []api.CatalogEntry{host("A", 12, liveA)}, ""},
		{"the old home kept it", []api.CatalogEntry{host("A", 12, api.CatalogGroup{ID: "g", Generation: 1, Tried: 3})}, "stale"},
		{"the old home moving elsewhere", []api.CatalogEntry{host("A", 12, api.CatalogGroup{ID: "g", Generation: 1, Tried: 4, Moving: &api.GroupMoveRef{To: "C", Generation: 4}})}, "stale"},
		{"a tombstone to another hab", []api.CatalogEntry{host("A", 12, api.CatalogGroup{ID: "g", Moved: &api.GroupMoveRef{To: "C", Generation: 4}})}, "stale"},
		{"live on another hab", []api.CatalogEntry{host("A", 10, liveA), host("C", 5, api.CatalogGroup{ID: "g", Generation: 4})}, "stale"},
		{"an older home's old copy", []api.CatalogEntry{host("Z", 5, api.CatalogGroup{ID: "g", Generation: 1})}, ""},
		{"an old tombstone elsewhere", []api.CatalogEntry{host("Z", 5, api.CatalogGroup{ID: "g", Moved: &api.GroupMoveRef{To: "A", Generation: 1}})}, ""},
	}
	for _, c := range cases {
		if got := s.pendingVerdict(api.CatalogList{Hosts: c.hosts}, pd); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
