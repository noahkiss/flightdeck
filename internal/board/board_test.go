package board

import (
	"fmt"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

func TestRankOf(t *testing.T) {
	a := func(s api.AgentStatus) *api.AgentState { return &api.AgentState{State: s} }
	for _, c := range []struct {
		state api.SessionState
		agent *api.AgentState
		seen  bool
		want  Rank
	}{
		{api.SessionStateRunning, a(api.AgentStatusBlocked), false, RankBlocked},
		{api.SessionStateRunning, a(api.AgentStatusError), false, RankError},
		{api.SessionStateRunning, a(api.AgentStatusDone), false, RankDone},
		{api.SessionStateRunning, a(api.AgentStatusDone), true, RankIdle},
		{api.SessionStateRunning, a(api.AgentStatusWorking), false, RankWorking},
		{api.SessionStateRunning, a(api.AgentStatusIdle), false, RankIdle},
		{api.SessionStateRunning, a(api.AgentStatusNone), false, RankOther},
		{api.SessionStateRunning, a("thinking"), false, RankOther}, // an unknown word
		{api.SessionStateRunning, nil, false, RankOther},
		{api.SessionStateExited, a(api.AgentStatusBlocked), false, RankOther},
	} {
		if got := RankOf(c.state, c.agent, c.seen); got != c.want {
			t.Errorf("%v %+v seen %v: %v, want %v", c.state, c.agent, c.seen, got, c.want)
		}
	}
}

// TestRowsAndGroups: a project on two hosts is one group by its remote; an unfiled pane sits
// at the bottom; a failed host's rows are hidden; seen turns done into idle.
func TestRowsAndGroups(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	hs := []hosts.Host{
		{Label: "a", Status: hosts.Online, Structure: api.Structure{
			Projects: []api.Project{{ID: "pa", Name: "app", Remote: "example.com/team/app"}},
			Sessions: []api.Session{{ID: "s1", Name: "fix", Project: "pa", Pane: "p1", State: api.SessionStateRunning},
				{ID: "s2", Name: "docs", Project: "pa", Pane: "p2", State: api.SessionStateRunning}},
			Panes: []api.Pane{{ID: "p1", Session: "s1"}, {ID: "p2", Session: "s2"}, {ID: "p3"}},
			Agents: []api.AgentState{{Pane: "p1", State: api.AgentStatusWorking, At: at(1)},
				{Pane: "p2", State: api.AgentStatusDone, At: at(2)}},
		}},
		{Label: "b", Status: hosts.Offline, Structure: api.Structure{
			Projects: []api.Project{{ID: "pb", Name: "app", Remote: "example.com/team/app"}},
			Sessions: []api.Session{{ID: "s3", Name: "review", Project: "pb", Pane: "p9", State: api.SessionStateRunning}},
			Panes:    []api.Pane{{ID: "p9", Session: "s3"}},
			Agents: []api.AgentState{{Pane: "p9", State: api.AgentStatusBlocked, Kind: api.BlockedKindQuestion, At: at(3),
				Request: &api.PermissionRequest{Title: "Which branch?"}}},
		}},
		{Label: "c", Status: hosts.Failed, Structure: api.Structure{Sessions: []api.Session{{ID: "x", Name: "hidden"}}}},
	}
	var o Order
	o.Now = func() time.Time { return now }
	rows := Rows(hs, nil)
	o.Sort(rows)
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if got := fmt.Sprint(names); got != "[review docs fix unfiled p3]" {
		t.Fatalf("order %s", got)
	}
	if rows[0].StateWord() != "blocked:question" || rows[0].Context() != "Which branch?" || !rows[0].Offline() {
		t.Errorf("blocked row %+v", rows[0])
	}
	if n := NeedsYou(rows); n != 1 {
		t.Errorf("needs you %d, want 1: the done row; the blocked one is offline", n)
	}
	gs := Groups(rows, GroupProject)
	if len(gs) != 2 || gs[0].Title != "app" || len(gs[0].Rows) != 3 || gs[1].Title != "no project" {
		t.Fatalf("groups %+v", gs)
	}
	if gs := Groups(rows, GroupHost); len(gs) != 2 || gs[0].Title != "b" || gs[1].Title != "a" {
		t.Fatalf("host groups %+v", gs)
	}

	seen := func(host, pane string, a *api.AgentState) bool { return pane == "p2" }
	rows = Rows(hs, seen)
	o.Sort(rows)
	if rows[1].Name != "fix" || rows[2].Name != "docs" || rows[2].Rank != RankIdle {
		t.Fatalf("after seen: %v %v %v", rows[1].Name, rows[2].Name, rows[2].Rank)
	}
	if !Match(rows[0], "REV") || !Match(rows[0], "app") || !Match(rows[0], "b") || Match(rows[0], "zzz") {
		t.Error("filter")
	}
}
