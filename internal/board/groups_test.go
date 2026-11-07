package board

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

func group(id, title, state, created string) api.Group {
	return api.Group{ID: id, Title: title, State: api.GroupState(state), Created: created, Updated: created,
		Coordinator: api.GroupCoordinator{Name: "coord", Session: "s1"}}
}

// TestGroupOrder: groups of two hosts in one list, confirming, then running newest first, then
// paused, then ended; counts, pause reasons and items from the record.
func TestGroupOrder(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	run := group("g1", "older", "running", "2026-01-02T09:00:00Z")
	run.Items = []api.GroupItem{{ID: "a", Title: "API", State: "landed"}, {ID: "b", Title: "UI", State: "working", Lead: "lead-2"},
		{ID: "c", Title: "Docs", State: "dropped"}}
	run.Members = []api.GroupMember{{Name: "lead-2", Session: "s2"}, {Name: "rev-1", Session: "s3"}, {Name: "planned"}}
	run.Waits = []string{"disk on b"}
	paused := group("g2", "held", "paused", "2026-01-02T11:00:00Z")
	paused.Pause = api.GroupPause{User: "2026-01-02T11:30:00Z", Usage: "2026-01-02T11:40:00Z"}
	a := hosts.Host{Label: "a", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "A"}, Structure: api.Structure{Groups: []api.Group{
		run, paused, group("g3", "done", "ended", "2026-01-02T08:00:00Z"),
	}}}
	b := hosts.Host{Label: "b", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "B"}, Structure: api.Structure{Groups: []api.Group{
		group("g4", "newer", "running", "2026-01-02T10:00:00Z"), group("g5", "asks", "confirming", "2026-01-01T10:00:00Z"),
	}}}
	rows := GroupRows([]hosts.Host{a, b}, nil, now)
	var got []string
	for _, r := range rows {
		got = append(got, r.Host+"/"+r.Title+"/"+r.State)
	}
	want := "b/asks/confirming b/newer/running a/older/running a/held/paused a/done/ended"
	if strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
	o := rows[2]
	if o.Items != 2 || o.Landed != 1 || o.Members != 2 || o.Coordinator != "coord" || len(o.ItemRows) != 3 ||
		o.ItemRows[1].Lead != "lead-2" || o.Waits[0] != "disk on b" || o.Stale {
		t.Fatalf("running row %+v", o)
	}
	if p := rows[3].PausedBy; strings.Join(p, ",") != "you,usage" {
		t.Fatalf("pause reasons %v", p)
	}
	if e := rows[4].Ended; !e.Equal(time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("an ended record with no end time takes its last update: %v", e)
	}
}

// TestGroupsOverLeave: an ended or dropped group leaves the board a day after its end.
func TestGroupsOverLeave(t *testing.T) {
	g := group("g1", "done", "dropped", "2026-01-01T08:00:00Z")
	g.Ended = "2026-01-01T09:00:00Z"
	h := hosts.Host{Label: "a", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "A"}, Structure: api.Structure{Groups: []api.Group{g}}}
	end := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if n := len(GroupRows([]hosts.Host{h}, nil, end.Add(GroupOverShown-time.Minute))); n != 1 {
		t.Fatalf("rows within the day: %d", n)
	}
	if n := len(GroupRows([]hosts.Host{h}, nil, end.Add(GroupOverShown))); n != 0 {
		t.Fatalf("rows after the day: %d", n)
	}
}

// TestGroupsStale: an offline home host keeps its last records, stale; a catalog copy
// confirmed after the drop wins; a host never reached shows the copy by its pinned id; a
// failed host shows none.
func TestGroupsStale(t *testing.T) {
	drop := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	now := drop.Add(time.Hour)
	held := hosts.Host{Label: "a", Status: hosts.Offline, Since: drop, Welcome: api.Welcome{DeviceID: "A"},
		Structure: api.Structure{Groups: []api.Group{group("g1", "mine", "running", "2026-01-01T09:00:00Z")}}}
	cp := GroupCopy{Groups: []api.CatalogGroup{{ID: "g1", Title: "theirs", State: "paused", Items: 3, Landed: 1, Members: 4,
		Coordinator: &api.CatalogMember{Name: "coord"}, Pause: &api.GroupPause{Usage: "2026-01-01T10:30:00Z"}}}}
	cats := func(at time.Time) GroupCatalogs {
		return func(id string) (GroupCopy, bool) {
			c := cp
			c.SeenAt = at
			return c, id == "A"
		}
	}
	one := func(rows []GroupRow) GroupRow {
		if len(rows) != 1 || !rows[0].Stale {
			t.Fatalf("rows %+v", rows)
		}
		return rows[0]
	}
	if r := one(GroupRows([]hosts.Host{held}, nil, now)); r.Title != "mine" || !r.Since.Equal(drop) {
		t.Fatalf("no catalog: %+v", r)
	}
	if r := one(GroupRows([]hosts.Host{held}, cats(drop.Add(CatalogSkew)), now)); r.Title != "mine" {
		t.Fatalf("a copy inside the skew margin won: %+v", r)
	}
	r := one(GroupRows([]hosts.Host{held}, cats(drop.Add(time.Minute)), now))
	if r.Title != "theirs" || r.Items != 3 || r.Landed != 1 || r.Members != 4 || r.Coordinator != "coord" ||
		strings.Join(r.PausedBy, ",") != "usage" || len(r.ItemRows) != 0 {
		t.Fatalf("newer copy: %+v", r)
	}
	never := hosts.Host{Label: "a", Status: hosts.Connecting, Pinned: "A"}
	if r := one(GroupRows([]hosts.Host{never}, cats(drop), now)); r.Title != "theirs" {
		t.Fatalf("never reached: %+v", r)
	}
	failed := held
	failed.Status = hosts.Failed
	if n := len(GroupRows([]hosts.Host{failed}, cats(now), now)); n != 0 {
		t.Fatalf("a failed host shows %d groups", n)
	}
}

// TestMemberMark: a member's session row carries its group's title, from the home host's
// snapshot on another host; a group no host holds reads "group".
func TestMemberMark(t *testing.T) {
	home := hosts.Host{Label: "a", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "A"},
		Structure: api.Structure{Groups: []api.Group{group("g1", "Split the parser", "running", "2026-01-01T09:00:00Z")}}}
	peer := hosts.Host{Label: "b", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "B"}, Structure: api.Structure{Sessions: []api.Session{
		{ID: "s1", Name: "lead", Group: &api.SessionGroup{ID: "g1", Role: "lead", Item: "api"}},
		{ID: "s2", Name: "lost", Group: &api.SessionGroup{ID: "gx", Role: "worker"}},
		{ID: "s3", Name: "plain"},
	}}}
	marks := map[string]string{}
	for _, r := range Rows([]hosts.Host{home, peer}, nil) {
		marks[r.Name] = r.Group
	}
	if marks["lead"] != "Split the parser" || marks["lost"] != "group" || marks["plain"] != "" {
		t.Fatalf("marks %v", marks)
	}
}

// TestGroupCountsMatchTheCatalog: a live row and the catalog's short form of one record count
// alike, so a card keeps its numbers when its home host goes offline.
func TestGroupCountsMatchTheCatalog(t *testing.T) {
	g := group("g1", "Split the parser", "running", "2026-01-01T09:00:00Z")
	g.Items = []api.GroupItem{{ID: "a", State: "landed"}, {ID: "b", State: "working"}, {ID: "c", State: "dropped"}}
	g.Members = []api.GroupMember{{Name: "lead-1", Session: "s1"}, {Name: "lead-2", Session: "s2"}, {Name: "rev-1"}}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	live := GroupRows([]hosts.Host{{Label: "a", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "A"},
		Structure: api.Structure{Groups: []api.Group{g}}}}, nil, now)
	cats := func(string) (GroupCopy, bool) {
		return GroupCopy{Groups: []api.CatalogGroup{groups.Short(g)}, SeenAt: now}, true
	}
	copied := GroupRows([]hosts.Host{{Label: "a", Status: hosts.Offline, Since: now.Add(-time.Hour), Welcome: api.Welcome{DeviceID: "A"}}}, cats, now)
	if len(live) != 1 || len(copied) != 1 {
		t.Fatalf("rows %+v %+v", live, copied)
	}
	l, c := live[0], copied[0]
	if l.Items != 2 || l.Landed != 1 || l.Members != 2 {
		t.Fatalf("live counts %d/%d, %d", l.Landed, l.Items, l.Members)
	}
	if got, want := []int{c.Items, c.Landed, c.Members}, []int{l.Items, l.Landed, l.Members}; !slices.Equal(got, want) {
		t.Fatalf("catalog counts %v, live %v", got, want)
	}
}
