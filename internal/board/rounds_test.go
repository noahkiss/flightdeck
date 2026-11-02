package board

import (
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

func round(id, name, next string, paused, completed bool) api.Round {
	return api.Round{ID: id, RoundSpec: api.RoundSpec{Name: name, Mode: api.RoundModeFresh, Paused: paused,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: "0 * * * *", Zone: "UTC"}},
		NextSlot: next, Completed: completed}
}

// TestRoundOrder: rounds of two hosts in one list, the soonest next slot first across hosts,
// then by hand, then paused, then completed; the trigger in the catalog's words.
func TestRoundOrder(t *testing.T) {
	a := hosts.Host{Label: "a", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "A"}, Structure: api.Structure{Rounds: []api.Round{
		round("r1", "late", "2026-01-01T12:00:00Z", false, false),
		round("r2", "done", "", false, true),
		{ID: "r3", RoundSpec: api.RoundSpec{Name: "manual", Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}}},
	}}}
	b := hosts.Host{Label: "b", Status: hosts.Online, Welcome: api.Welcome{DeviceID: "B"}, Structure: api.Structure{Rounds: []api.Round{
		round("r1", "early", "2026-01-01T09:00:00Z", false, false),
		round("r9", "held", "", true, false),
	}}}
	rows := RoundRows([]hosts.Host{a, b}, nil)
	SortRounds(rows)
	var got []string
	for _, r := range rows {
		got = append(got, r.Host+"/"+r.Name+"/"+r.Mark())
	}
	want := "b/early/no runs yet a/late/no runs yet a/manual/no runs yet b/held/paused a/done/completed"
	if strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
	if rows[0].Trigger != "every 1h in UTC" || rows[2].Trigger != "by hand" {
		t.Fatalf("trigger words %q, %q", rows[0].Trigger, rows[2].Trigger)
	}
	if rows[0].Stale {
		t.Fatal("an online host's round is stale")
	}
}

// TestRoundsStale: an offline host keeps its last rounds, stale; a catalog copy confirmed after
// the drop wins; one confirmed before it does not; a host never reached shows the copy found
// by its pinned id; a failed host shows none.
func TestRoundsStale(t *testing.T) {
	drop := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	held := hosts.Host{Label: "a", Status: hosts.Offline, Since: drop, Welcome: api.Welcome{DeviceID: "A"},
		Structure: api.Structure{Rounds: []api.Round{round("r1", "mine", "2026-01-01T12:00:00Z", false, false)}}}
	cp := CatalogCopy{Rounds: []api.CatalogRound{{ID: "r1", Name: "theirs", Trigger: "every 1h", NextSlot: "2026-01-01T13:00:00Z",
		LastRun: api.RoundRunStateFailed}}}
	cats := func(at time.Time) Catalogs {
		return func(id string) (CatalogCopy, bool) {
			c := cp
			c.SeenAt = at
			return c, id == "A"
		}
	}
	name := func(rows []RoundRow) string {
		if len(rows) != 1 {
			t.Fatalf("rows %+v", rows)
		}
		if !rows[0].Stale {
			t.Fatalf("row not stale: %+v", rows[0])
		}
		return rows[0].Name
	}
	if n := name(RoundRows([]hosts.Host{held}, nil)); n != "mine" {
		t.Fatalf("no catalog: %s", n)
	}
	if n := name(RoundRows([]hosts.Host{held}, cats(drop.Add(-time.Minute)))); n != "mine" {
		t.Fatalf("older catalog: %s", n)
	}
	rows := RoundRows([]hosts.Host{held}, cats(drop.Add(time.Minute)))
	if n := name(rows); n != "theirs" || rows[0].Mark() != "failed" || !rows[0].Since.Equal(drop.Add(time.Minute)) {
		t.Fatalf("newer catalog: %+v", rows[0])
	}
	never := hosts.Host{Label: "a", Status: hosts.Connecting, Pinned: "A"}
	if n := name(RoundRows([]hosts.Host{never}, cats(drop))); n != "theirs" {
		t.Fatalf("never reached: %s", n)
	}
	failed := held
	failed.Status = hosts.Failed
	if rows := RoundRows([]hosts.Host{failed}, cats(drop.Add(time.Minute))); len(rows) != 0 {
		t.Fatalf("failed host: %+v", rows)
	}
}

// TestRunSessionFilter: the filter hides run sessions but those that need the owner.
func TestRunSessionFilter(t *testing.T) {
	h := hosts.Host{Label: "a", Status: hosts.Online, Structure: api.Structure{
		Rounds: []api.Round{round("r1", "nightly", "", false, false)},
		Sessions: []api.Session{
			{ID: "s1", Name: "plain", Pane: "p1"},
			{ID: "s2", Name: "run", Pane: "p2", Round: &api.SessionRound{Round: "r1", Run: "x"}},
			{ID: "s3", Name: "asks", Pane: "p3", Round: &api.SessionRound{Round: "gone", Run: "y"}},
		},
		Agents: []api.AgentState{{Pane: "p3", State: api.AgentStatusBlocked}},
	}}
	shown := map[string]bool{}
	names := map[string]string{}
	for _, r := range Rows([]hosts.Host{h}, nil) {
		shown[r.Name] = ShowRuns(r, false)
		names[r.Name] = r.RoundName
	}
	if !shown["plain"] || shown["run"] || !shown["asks"] {
		t.Fatalf("hidden filter: %v", shown)
	}
	if names["run"] != "nightly" || names["asks"] != "round" || names["plain"] != "" {
		t.Fatalf("round names: %v", names)
	}
}

// TestRunSessionFilterTrustDialog: a run blocked on Claude Code's trust dialog has no blocked
// agent state, yet its session stays shown while the filter hides run sessions. An older run
// of the same round that is not the blocked one stays hidden.
func TestRunSessionFilterTrustDialog(t *testing.T) {
	r1 := round("r1", "nightly", "", false, false)
	r1.LastRun = &api.RoundRun{ID: "x2", Round: "r1", State: api.RoundRunStateBlocked, Session: "s2"}
	h := hosts.Host{Label: "a", Status: hosts.Online, Structure: api.Structure{
		Rounds: []api.Round{r1},
		Sessions: []api.Session{
			{ID: "s1", Name: "old", Pane: "p1", Round: &api.SessionRound{Round: "r1", Run: "x1"}},
			{ID: "s2", Name: "trust", Pane: "p2", Round: &api.SessionRound{Round: "r1", Run: "x2"}},
		},
	}}
	shown := map[string]bool{}
	for _, r := range Rows([]hosts.Host{h}, nil) {
		shown[r.Name] = ShowRuns(r, false)
	}
	if shown["old"] || !shown["trust"] {
		t.Fatalf("hidden filter: %v", shown)
	}
}
