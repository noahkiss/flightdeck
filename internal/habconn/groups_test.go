package habconn

import (
	"encoding/json"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// Groups in the copy (groups.md 15.1): the snapshot's records, then group.updated by revision.
func TestGroupsKept(t *testing.T) {
	st := newState()
	st.reset(api.Structure{Groups: []api.Group{{ID: "g", Revision: 2, Title: "Split the parser", State: "running"}}})
	at := func(rev uint64, v api.Group) bool {
		b, _ := json.Marshal(v)
		return st.apply(api.Event{Type: api.EventGroupUpdated, ID: v.ID, Data: b, Revision: &rev})
	}
	if at(2, api.Group{ID: "g", Revision: 2, State: "ended"}) {
		t.Fatal("a group.updated at the held revision applied")
	}
	if !at(3, api.Group{ID: "g", Revision: 3, Title: "Split the parser", State: "paused"}) {
		t.Fatal("a newer group.updated was ignored")
	}
	if !at(1, api.Group{ID: "h", Revision: 1, State: "confirming"}) {
		t.Fatal("a new group was ignored")
	}
	got := st.structure().Groups
	if len(got) != 2 || got[0].ID != "g" || got[0].State != "paused" || got[1].ID != "h" {
		t.Fatalf("structure groups: %+v", got)
	}
	st.reset(api.Structure{})
	if len(st.structure().Groups) != 0 {
		t.Fatal("a snapshot with no groups kept the old ones")
	}
}
