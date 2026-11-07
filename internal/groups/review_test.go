package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// onHosts is an installed check from a table of host to harnesses.
func onHosts(m map[string][]string) func(host, harness string) bool {
	return func(host, harness string) bool {
		for _, h := range m[host] {
			if h == harness {
				return true
			}
		}
		return false
	}
}

// TestAddReviewers (groups.md 6.2, 16.2 B7): a Claude Code lead gets a Codex reviewer; Codex
// on another plan host is found there; a Codex lead gets Claude Code; with only the lead's
// harness the reviewer runs in it and the record says same harness; a plan's own reviewer is
// kept; review off drops it.
func TestAddReviewers(t *testing.T) {
	p := api.GroupPlan{Title: "T", Project: "app",
		Items: []api.GroupPlanItem{{ID: "a", Title: "A", Review: "on"}, {ID: "b", Title: "B", Review: "on"}, {ID: "c", Title: "C", Review: "on"},
			{ID: "d", Title: "D", Review: "off"}},
		Members: []api.GroupPlanMember{{Role: RoleLead, Item: "a", Harness: "claude"}, {Role: RoleLead, Item: "b", Harness: "codex", Host: "bee"},
			{Role: RoleLead, Item: "c", Harness: "opencode"}, {Role: RoleReviewer, Item: "c", Harness: "opencode", Name: "mine"},
			{Role: RoleLead, Item: "d", Harness: "claude"}, {Role: RoleReviewer, Item: "d", Harness: "codex"}}}
	got := AddReviewers(p, onHosts(map[string][]string{"": {"claude", "opencode"}, "bee": {"codex", "claude"}}))
	rev := map[string]api.GroupPlanMember{}
	for _, m := range got.Members {
		if m.Role == RoleReviewer {
			rev[m.Item] = m
		}
	}
	if r := rev["a"]; r.Harness != "codex" || r.Host != "bee" {
		t.Fatalf("item a's reviewer %+v, want codex on bee", r)
	}
	if r := rev["b"]; r.Harness != "claude" || r.Host != "bee" {
		t.Fatalf("item b's reviewer %+v, want claude on the lead's host bee", r)
	}
	if r := rev["c"]; r.Name != "mine" {
		t.Fatalf("the plan's own reviewer was replaced: %+v", r)
	}
	if _, ok := rev["d"]; ok {
		t.Fatal("an item with review off kept its reviewer")
	}
	if len(p.Members) != 6 {
		t.Fatal("AddReviewers changed its input")
	}

	// One harness only: the same harness, and the card and the record say so.
	one := api.GroupPlan{Title: "T", Project: "app", Items: []api.GroupPlanItem{{ID: "a", Title: "A", Review: "on"}},
		Members: []api.GroupPlanMember{{Role: RoleLead, Item: "a", Harness: "claude"}}}
	one = Normalize(AddReviewers(one, onHosts(map[string][]string{"": {"claude"}})), "3fa9c2deadbeef00")
	if len(one.Members) != 2 || one.Members[1].Harness != "claude" || one.Members[1].Name != "claude-3fa9c2-reviewer-1" {
		t.Fatalf("members %+v", one.Members)
	}
	if l := SameHarnessLines(one); len(l) != 1 || !strings.Contains(l[0], "same harness") {
		t.Fatalf("card lines %v", l)
	}
	g := New("3fa9c2deadbeef00", one, "app", "dev", api.GroupCoordinator{}, time.Now())
	if it := g.Items[0]; !it.SameHarness || it.Reviewer != "claude-3fa9c2-reviewer-1" || it.ReviewBranch != "group/3fa9c2deadbeef00/a-review" {
		t.Fatalf("item %+v", it)
	}
	if err := Check(one, 2); err != nil {
		t.Fatal(err)
	}
	two := one
	two.Members = append(append([]api.GroupPlanMember{}, one.Members...), api.GroupPlanMember{Role: RoleReviewer, Item: "a", Harness: "codex", Name: "second"})
	if err := Check(two, 2); err == nil || !strings.Contains(err.Error(), "reviewers") {
		t.Fatalf("two reviewers: %v", err)
	}
}

func finding(n int) []api.GroupFinding {
	var out []api.GroupFinding
	for i := 0; i < n; i++ {
		out = append(out, api.GroupFinding{Severity: "high", Title: "t", Place: "a.go:1", Text: "p"})
	}
	return out
}

// TestCheckVerdict (groups.md 6.3, 16.2 B7): more than max_findings findings are refused, with
// 2 the third; a low finding and a fix_needed with none are refused.
func TestCheckVerdict(t *testing.T) {
	if err := CheckVerdict(api.ReviewVerdictParams{Verdict: "fix_needed", Findings: finding(5)}, 5); err != nil {
		t.Fatal(err)
	}
	if err := CheckVerdict(api.ReviewVerdictParams{Verdict: "fix_needed", Findings: finding(6)}, 5); err == nil || !strings.Contains(err.Error(), "max_findings") {
		t.Fatalf("six findings: %v", err)
	}
	if err := CheckVerdict(api.ReviewVerdictParams{Verdict: "pass", Findings: finding(2)}, 2); err != nil {
		t.Fatal(err)
	}
	if err := CheckVerdict(api.ReviewVerdictParams{Verdict: "pass", Findings: finding(3)}, 2); err == nil {
		t.Fatal("the third finding with max_findings 2 was taken")
	}
	low := finding(1)
	low[0].Severity = "low"
	for _, p := range []api.ReviewVerdictParams{{Verdict: "pass", Findings: low}, {Verdict: "fix_needed"}, {Verdict: "maybe"},
		{Verdict: "pass", Findings: []api.GroupFinding{{Severity: "high", Title: "two\nlines", Text: "p"}}}} {
		if err := CheckVerdict(p, 5); err == nil {
			t.Fatalf("taken: %+v", p)
		}
	}
}

func reviewGroup() api.Group {
	p := api.GroupPlan{Title: "T", Project: "app", Items: []api.GroupPlanItem{{ID: "a", Title: "A", Review: "on"}, {ID: "b", Title: "B", Review: "off"}},
		Members: []api.GroupPlanMember{{Role: RoleLead, Item: "a", Harness: "claude", Name: "la"}, {Role: RoleReviewer, Item: "a", Harness: "codex", Name: "ra"},
			{Role: RoleLead, Item: "b", Harness: "claude", Name: "lb"}}}
	g := New("g1", p, "app", "dev", api.GroupCoordinator{}, time.Now())
	g.State = StateRunning
	for i := range g.Items {
		g.Items[i].State = ItemWorking
	}
	return g
}

// TestSubmitAndVerdict (groups.md 6.2 to 6.4): submit to review, the same head twice, a new head
// restarting the review; fix_needed to fix; a pass with fixes that descend lands the review
// head; fixes that do not, or are not checked against the submitted head, are ignored and the
// submitted head lands; review off goes straight to ready.
func TestSubmitAndVerdict(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	g := reviewGroup()
	if r, err := Submit(&g, "a", "c1", now); err != nil || r.State != ItemReview || r.Restart || r.Reviewer != "ra" {
		t.Fatalf("submit %+v %v", r, err)
	}
	if r, _ := Submit(&g, "a", "c1", now); !r.Same {
		t.Fatal("the same head again changed something")
	}
	if r, _ := Submit(&g, "a", "c2", now); !r.Restart || g.Items[0].Submitted != "c2" || g.Items[0].Submits != 2 {
		t.Fatalf("a new head: %+v %+v", r, g.Items[0])
	}
	out, err := TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "fix_needed", Findings: finding(2)}, now)
	if err != nil || out.State != ItemFix || g.Items[0].Verdict.Head != "c2" {
		t.Fatalf("fix_needed %+v %v", out, err)
	}
	if line := VerdictLine("a", *g.Items[0].Verdict); line != "item a: fix needed, 2 findings" {
		t.Fatalf("line %q", line)
	}
	if _, err := TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "pass"}, now); err == nil {
		t.Fatal("a verdict while the item is in fix")
	}
	Submit(&g, "a", "c3", now)
	fixed := &api.ReviewFixed{Commits: []string{"f1"}, Head: "f1"}
	out, _ = TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "pass", Fixed: fixed, Checked: "c3", Descends: true}, now)
	if out.State != ItemReady || out.Land != "f1" || g.Items[0].Land != "f1" || len(g.Lane.Queue) != 1 {
		t.Fatalf("pass with fixes %+v %+v %v", out, g.Items[0], g.Lane.Queue)
	}
	if line := VerdictLine("a", *g.Items[0].Verdict); !strings.Contains(line, "with the reviewer's fixes") {
		t.Fatalf("line %q", line)
	}
	// From ready a new head goes back to review, out of the lane queue.
	Submit(&g, "a", "c4", now)
	if g.Items[0].State != ItemReview || len(g.Lane.Queue) != 0 {
		t.Fatalf("resubmit from ready %+v %v", g.Items[0], g.Lane.Queue)
	}
	out, _ = TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "pass", Fixed: fixed, Checked: "c4", Descends: false}, now)
	if out.Land != "c4" || !strings.Contains(out.FixesIgnored, "does not descend") {
		t.Fatalf("fixes that do not descend %+v", out)
	}
	Submit(&g, "a", "c5", now)
	out, _ = TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "pass", Fixed: fixed, Checked: "c4", Descends: true}, now)
	if out.Land != "c5" || !strings.Contains(out.FixesIgnored, "not checked") {
		t.Fatalf("fixes checked against an older head %+v", out)
	}
	off := false
	g.Plan.Lane.FixInPlace = &off
	Submit(&g, "a", "c6", now)
	out, _ = TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "pass", Fixed: fixed, Checked: "c6", Descends: true}, now)
	if out.Land != "c6" || !strings.Contains(out.FixesIgnored, "does not allow") {
		t.Fatalf("fixes the plan does not allow %+v", out)
	}
	// Review off: ready at once.
	if r, err := Submit(&g, "b", "d1", now); err != nil || r.State != ItemReady || g.Items[1].Land != "d1" {
		t.Fatalf("review off %+v %v", r, err)
	}
	if _, err := Submit(&g, "zz", "d1", now); err == nil {
		t.Fatal("an unknown item")
	}
	g.Items[1].State = ItemQueued
	if _, err := Submit(&g, "b", "d2", now); err == nil {
		t.Fatal("a submit of a queued item")
	}
	// The lead hears every finding; the coordinator's line holds none of their text.
	g.Items[0].State = ItemReview
	TakeVerdict(&g, api.ReviewVerdictParams{Item: "a", Verdict: "fix_needed", Findings: []api.GroupFinding{{Severity: "medium", Title: "Leak", Place: "x.go:9", Text: "The file stays open."}}}, now)
	text := FindingsText(g, g.Items[0], *g.Items[0].Verdict)
	if !strings.Contains(text, "[medium] Leak (x.go:9): The file stays open.") || !strings.Contains(text, "lane_submit") {
		t.Fatalf("findings text %q", text)
	}
	if strings.Contains(VerdictLine("a", *g.Items[0].Verdict), "open") {
		t.Fatal("the coordinator's line carries a finding")
	}
	if n := len(CardFindings(&api.GroupVerdict{Findings: finding(5)}, 3)); n != 3 {
		t.Fatalf("card findings %d", n)
	}
}

// TestStuckReviews (groups.md 6.3, 16.2 B7): with reviewer_idle at 5 minutes an idle reviewer is
// reported at 5 minutes on a fake clock, once; a working reviewer resets the time; an exited one
// is reported at once; the next submit clears the mark.
func TestStuckReviews(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	g := reviewGroup()
	Submit(&g, "a", "c1", t0)
	ri := MemberIndex(g, "ra")
	g.Members[ri].State, g.Members[ri].Session, g.Members[ri].Agent = MemberRunning, "s1", api.AgentStatusIdle
	idle := 5 * time.Minute
	if s := StuckReviews(&g, idle, t0); len(s) != 0 {
		t.Fatalf("at once %+v", s)
	}
	if s := StuckReviews(&g, idle, t0.Add(4*time.Minute+59*time.Second)); len(s) != 0 {
		t.Fatalf("before 5 minutes %+v", s)
	}
	s := StuckReviews(&g, idle, t0.Add(5*time.Minute))
	if len(s) != 1 || s[0].Item != "a" || !strings.Contains(s[0].Why, "idle 5m0s") || g.Items[0].State != ItemReview {
		t.Fatalf("at 5 minutes %+v %+v", s, g.Items[0])
	}
	if s := StuckReviews(&g, idle, t0.Add(20*time.Minute)); len(s) != 0 {
		t.Fatal("reported twice")
	}
	// A working reviewer resets the idle time; the next submit clears the mark.
	Submit(&g, "a", "c2", t0.Add(21*time.Minute))
	g.Members[ri].Agent = api.AgentStatusWorking
	StuckReviews(&g, idle, t0.Add(22*time.Minute))
	g.Members[ri].Agent = api.AgentStatusIdle
	StuckReviews(&g, idle, t0.Add(23*time.Minute))
	if s := StuckReviews(&g, idle, t0.Add(27*time.Minute)); len(s) != 0 {
		t.Fatal("the idle time counted from before the reviewer worked")
	}
	if s := StuckReviews(&g, idle, t0.Add(28*time.Minute)); len(s) != 1 {
		t.Fatalf("the second idle spell %+v", s)
	}
	Submit(&g, "a", "c3", t0.Add(29*time.Minute))
	g.Members[ri].State = MemberExited
	if s := StuckReviews(&g, idle, t0.Add(29*time.Minute)); len(s) != 1 || !strings.Contains(s[0].Why, "exited") {
		t.Fatalf("an exited reviewer %+v", s)
	}
}

func TestReviewKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(p, []byte("[groups]\nmax_findings = 2\nbrief_findings = 3\nreviewer_idle = \"5m\"\n"), 0o600)
	var logs []string
	k := LoadKeys(p, func(f string, a ...any) { logs = append(logs, f) })
	if k.MaxFindings != 2 || k.BriefFindings != 2 || k.ReviewerIdle != 5*time.Minute || len(logs) != 1 {
		t.Fatalf("keys %+v, logs %v", k, logs)
	}
	os.WriteFile(p, []byte("[groups]\nmax_findings = 21\nreviewer_idle = \"4m\"\n"), 0o600)
	logs = nil
	if k := LoadKeys(p, func(f string, a ...any) { logs = append(logs, f) }); k.MaxFindings != 5 || k.ReviewerIdle != 30*time.Minute || len(logs) != 2 {
		t.Fatalf("out of range %+v %v", k, logs)
	}
}
