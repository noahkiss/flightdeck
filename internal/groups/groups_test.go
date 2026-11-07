package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func plan3() api.GroupPlan {
	return api.GroupPlan{Title: "Add export", Brief: "Export the board.", Project: "app",
		Items: []api.GroupPlanItem{{ID: "api", Title: "API"}, {ID: "ui", Title: "UI", After: "api"}, {ID: "docs", Title: "Docs"}},
		Members: []api.GroupPlanMember{{Role: RoleLead, Item: "api", Harness: "claude"}, {Role: RoleLead, Item: "ui", Harness: "codex", Host: "bee"},
			{Role: RoleLead, Item: "docs", Harness: "claude"}, {Role: RoleReviewer, Item: "api", Harness: "codex"}}}
}

func TestNormalizeNames(t *testing.T) {
	p := Normalize(plan3(), "3fa9c2deadbeef00")
	if got := p.Members[3].Name; got != "codex-3fa9c2-reviewer-1" {
		t.Fatalf("auto name %q", got)
	}
	if p.Items[0].Review != "on" {
		t.Fatalf("review default %q", p.Items[0].Review)
	}
	if err := Check(p, 2); err != nil {
		t.Fatal(err)
	}
	// Two unnamed workers on one item, and an earlier member already named like the first,
	// get names of their own.
	q := plan3()
	first := AutoName("claude", "3fa9c2deadbeef00", RoleWorker, 1)
	q.Members = append(q.Members,
		api.GroupPlanMember{Role: RoleWorker, Item: q.Items[0].ID, Harness: "claude", Name: first},
		api.GroupPlanMember{Role: RoleWorker, Item: q.Items[0].ID, Harness: "claude"},
		api.GroupPlanMember{Role: RoleWorker, Item: q.Items[0].ID, Harness: "claude"})
	q = Normalize(q, "3fa9c2deadbeef00")
	n := len(q.Members)
	if q.Members[n-2].Name != first+"-2" || q.Members[n-1].Name != first+"-3" {
		t.Fatalf("names %q %q", q.Members[n-2].Name, q.Members[n-1].Name)
	}
}

func TestCheckRefusals(t *testing.T) {
	cases := map[string]func(p *api.GroupPlan){
		"no lead": func(p *api.GroupPlan) { p.Members = p.Members[1:] },
		"two leads": func(p *api.GroupPlan) {
			p.Members = append(p.Members, api.GroupPlanMember{Role: RoleLead, Item: "api", Harness: "claude", Name: "x"})
		},
		"twice":         func(p *api.GroupPlan) { p.Items = append(p.Items, api.GroupPlanItem{ID: "api", Title: "again"}) },
		"cycle":         func(p *api.GroupPlan) { p.Items[0].After = "ui" },
		"unknown after": func(p *api.GroupPlan) { p.Items[0].After = "nope" },
		"bad id":        func(p *api.GroupPlan) { p.Items[0].ID = "Bad ID" },
		"bad name":      func(p *api.GroupPlan) { p.Members[0].Name = "two words" },
		"same name":     func(p *api.GroupPlan) { p.Members[0].Name, p.Members[2].Name = "a", "a" },
		"bad role":      func(p *api.GroupPlan) { p.Members[3].Role = "boss" },
		"no project":    func(p *api.GroupPlan) { p.Project = "" },
		"workers": func(p *api.GroupPlan) {
			for i := 0; i < 3; i++ {
				p.Members = append(p.Members, api.GroupPlanMember{Role: RoleWorker, Item: "docs", Harness: "claude"})
			}
		},
	}
	for name, edit := range cases {
		p := plan3()
		edit(&p)
		p = Normalize(p, "abcdef0123456789")
		if name == "workers" {
			for i := range p.Members {
				if p.Members[i].Role == RoleWorker {
					p.Members[i].Name = "w" + string(rune('a'+i))
				}
			}
		}
		if err := Check(p, 2); err == nil {
			t.Errorf("%s: no refusal", name)
		}
	}
}

func TestNextLeads(t *testing.T) {
	g := New("abcdef0123456789", Normalize(plan3(), "abcdef0123456789"), "app", "dev", api.GroupCoordinator{}, time.Now())
	if got := NextLeads(g, 3); got != nil {
		t.Fatalf("confirming started %v", got)
	}
	g.State = StateRunning
	if got := strings.Join(NextLeads(g, 3), ","); got != "api,docs" {
		t.Fatalf("first leads %q: ui stacks after api", got)
	}
	if got := strings.Join(NextLeads(g, 1), ","); got != "api" {
		t.Fatalf("one slot %q", got)
	}
	g.Items[0].State = ItemWorking
	if got := strings.Join(NextLeads(g, 1), ","); got != "" {
		t.Fatalf("slot held: %q", got)
	}
	g.Items[0].State = ItemStalled // a stalled item frees its slot and is skipped
	if got := strings.Join(NextLeads(g, 1), ","); got != "docs" {
		t.Fatalf("after stall %q", got)
	}
	g.Items[0].State = ItemReview
	if got := strings.Join(NextLeads(g, 3), ","); got != "ui,docs" {
		t.Fatalf("after review %q", got)
	}
}

func TestInboxBounds(t *testing.T) {
	g := api.Group{}
	now := time.Now()
	for i := 0; i < 11; i++ {
		AddReport(&g, api.GroupReport{At: stamp(now), Text: string(rune('a' + i))}, 10, 7, now)
	}
	if len(g.Inbox) != 10 || g.Inbox[0].Text != "b" {
		t.Fatalf("inbox %v: the eleventh drops the oldest", g.Inbox)
	}
	old := api.GroupReport{At: stamp(now.Add(-8 * 24 * time.Hour)), Text: "old"}
	g.Inbox = append([]api.GroupReport{old}, g.Inbox...)
	PruneInbox(&g, 10, 7, now)
	if g.Inbox[0].Text == "old" {
		t.Fatal("a report past inbox_days stays")
	}
}

func TestPauseReasonsApart(t *testing.T) {
	g := api.Group{State: StateRunning}
	now := time.Now()
	SetPause(&g, "user", true, "", now)
	SetPause(&g, "usage", true, "later", now)
	SetPause(&g, "usage", false, "", now)
	if g.State != StatePaused || g.Pause.User == "" {
		t.Fatalf("a usage resume lifted the user pause: %+v", g)
	}
	SetPause(&g, "user", false, "", now)
	if g.State != StateRunning {
		t.Fatalf("state %s", g.State)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := New("abcdef0123456789", Normalize(plan3(), "abcdef0123456789"), "app", "dev", api.GroupCoordinator{Name: "lead"}, time.Now())
	if err := s.Save(g); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "0123456789abcdef.json"), []byte("{"), 0o600)
	got, problems := s.Load(time.Now())
	if len(got) != 1 || got[0].ID != g.ID || len(got[0].Items) != 3 {
		t.Fatalf("loaded %+v", got)
	}
	if len(problems) != 1 {
		t.Fatalf("problems %v", problems)
	}
	if _, err := os.Stat(filepath.Join(dir, "0123456789abcdef.json")); err == nil {
		t.Fatal("bad file not set aside")
	}
}

func TestLoadKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hab.toml")
	os.WriteFile(p, []byte("[groups]\ninbox_max = 5\ninbox_days = 3\nkeep_ended = 2\ntrust_hold = \"5s\"\n"), 0o600)
	var logs []string
	k := LoadKeys(p, func(f string, a ...any) { logs = append(logs, f) })
	if k.InboxMax != 200 || k.InboxDays != 3 || k.KeepEnded != 48*time.Hour {
		t.Fatalf("keys %+v", k)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d lines, want one for inbox_max", len(logs))
	}
	// The example config's keys are the defaults, and nothing in it is logged.
	logs = nil
	if k := LoadKeys(filepath.Join("..", "..", "docs", "examples", "hab.toml"), func(f string, a ...any) { logs = append(logs, f) }); k != DefaultKeys || len(logs) != 0 {
		t.Fatalf("the example: %+v %v", k, logs)
	}
}

func TestWarningsAndTrust(t *testing.T) {
	p := plan3()
	p.Items = p.Items[:1]
	if w := Warnings(p); len(w) != 1 || !strings.Contains(w[0], "single session") {
		t.Fatalf("warnings %v", w)
	}
	r := TrustRule("g1", []string{"dev", "bee"}, []string{"claude", "codex"})
	if !strings.Contains(r, "group/g1/") || !strings.Contains(r, "Codex") {
		t.Fatalf("trust rule %q", r)
	}
}
