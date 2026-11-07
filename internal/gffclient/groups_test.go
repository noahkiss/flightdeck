package gffclient

import (
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

func testGroup(id, title, state string) api.Group {
	at := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	return api.Group{ID: id, Title: title, State: api.GroupState(state), Created: at, Updated: at,
		Coordinator: api.GroupCoordinator{Name: "coord", Session: "s1"},
		Items: []api.GroupItem{{ID: "api", Title: "Parser API", State: "working", Lead: "claude-3fa9c2-lead-1"},
			{ID: "ui", Title: "Parser UI", State: "queued", Reason: "waits for api"}},
		Members: []api.GroupMember{{Name: "claude-3fa9c2-lead-1"}}}
}

// TestBoardGroups: the status board shows each group below the sessions, live from its home
// host's snapshot, then group.updated; a host that goes down keeps its groups, dimmed, with the
// time since; a host gff never reached shows the group another hab's catalog holds.
func TestBoardGroups(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.Capabilities = []string{"catalog"}
	a.Stop()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	a.PutGroup(testGroup("g1", "Split the parser", "running"))
	c := fakeHab(t, d, "c")
	c.Stop() // never reached
	a.SetCatalog(api.CatalogList{Hosts: []api.CatalogEntry{
		{Own: true, SeenAt: time.Now().UTC().Format(time.RFC3339), Catalog: api.Catalog{DeviceID: a.DeviceID, Host: "a"}},
		{Label: "c", Stale: true, SeenAt: time.Now().UTC().Format(time.RFC3339), Catalog: api.Catalog{DeviceID: c.DeviceID, Host: "c",
			Groups: []api.CatalogGroup{{ID: "g9", Title: "Remote work", State: "paused", Items: 3, Landed: 1, Members: 4,
				Pause: &api.GroupPause{User: "2026-01-01T00:00:00Z"}}}}},
	}})
	sock := func(h string) []hosts.Endpoint { return []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: h}} }
	hst := newHost(t, 160, 30, false)
	startWorkspaceSpecs(t, hst,
		hosts.Spec{Label: "one", Endpoints: sock(a.Sock)},
		hosts.Spec{Label: "three", Endpoints: sock(c.Sock), DeviceID: c.DeviceID})
	hst.typ(cmdKey + "g")
	row := func(s, name string) string {
		if l := line(s, "  "+name+" "); l >= 0 {
			return strings.Split(s, "\n")[l]
		}
		return ""
	}
	hst.waitScreen("the live group with its items, and the catalog's group", func(s string) bool {
		g := row(s, "Split the parser")
		r := row(s, "Remote work")
		return line(s, " Groups") >= 0 && strings.Contains(g, "running") && strings.Contains(g, "0/2 landed · 0 started · 1h") &&
			strings.Contains(g, "coordinator coord") && strings.Contains(s, "· Parser UI") && strings.Contains(s, "waits for api") &&
			strings.Contains(r, "three") && strings.Contains(r, "1/3 landed · 4 started") && strings.Contains(r, "paused by you") &&
			strings.Contains(r, "offline")
	})

	// group.updated changes the card.
	g := testGroup("g1", "Split the parser", "paused")
	g.Pause = api.GroupPause{User: time.Now().UTC().Format(time.RFC3339)}
	g.Waits = []string{"disk on one"}
	a.PutGroup(g)
	hst.waitScreen("paused, waiting", func(s string) bool {
		r := row(s, "Split the parser")
		return strings.Contains(r, "paused by you") && strings.Contains(r, "waiting: disk on one")
	})

	// A home host that goes down keeps its group, offline, and its items fold away.
	a.Stop()
	hst.waitScreen("one's group offline", func(s string) bool {
		return strings.Contains(row(s, "Split the parser"), "offline") && !strings.Contains(s, "· Parser UI")
	})
}

// TestGroupText: the header line's parts, an ended group's elapsed time to its end, and the
// filter by title or host.
func TestGroupText(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	g := board.GroupRow{Host: "one", Title: "Split the parser", State: "ended", Items: 2, Landed: 2, Members: 3,
		Created: now.Add(-5 * time.Hour), Ended: now.Add(-3 * time.Hour)}
	got := groupText(now, g)
	if !strings.Contains(got, "Split the parser") || !strings.Contains(got, "ended") || !strings.Contains(got, "2/2 landed · 3 started · 2h") {
		t.Fatalf("text %q", got)
	}
	u := &workspaceUI{groups: []board.GroupRow{g, {Host: "two", Title: "Other", State: "running"}}}
	u.filter = "two"
	if l := u.groupLines(now); len(l) != 1 || !strings.Contains(l[0].text, "Other") {
		t.Fatalf("filtered lines %+v", l)
	}
	u.filter = ""
	if l := u.groupLines(now); len(l) != 2 || l[0].st != dimStyle {
		t.Fatalf("an ended group is one dim line: %+v", l)
	}
}
