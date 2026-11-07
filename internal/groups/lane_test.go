package groups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

func TestMayLand(t *testing.T) {
	pass := func(head, fixed string) *api.GroupVerdict {
		return &api.GroupVerdict{Verdict: "pass", Head: head, FixedHead: fixed}
	}
	cases := []struct {
		name string
		it   api.GroupItem
		want bool
	}{
		{"ready with a pass on the submitted head", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "a", Land: "a", Verdict: pass("a", "")}, true},
		{"landing after a restart", api.GroupItem{State: ItemLanding, Reviewer: "r", Submitted: "a", Land: "a", Verdict: pass("a", "")}, true},
		{"the reviewer's fixed head", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "a", Land: "b", Verdict: pass("a", "b")}, true},
		{"a land head nobody reviewed", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "a", Land: "c", Verdict: pass("a", "b")}, false},
		{"a pass on an older head", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "b", Land: "b", Verdict: pass("a", "")}, false},
		{"fix needed", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "a", Land: "a", Verdict: &api.GroupVerdict{Verdict: "fix_needed", Head: "a"}}, false},
		{"no verdict", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "a", Land: "a"}, false},
		{"no land head", api.GroupItem{State: ItemReady, Reviewer: "r", Submitted: "a", Verdict: pass("a", "")}, false},
		{"still in review", api.GroupItem{State: ItemReview, Reviewer: "r", Submitted: "a", Land: "a", Verdict: pass("a", "")}, false},
		{"review off", api.GroupItem{State: ItemReady, Review: "off", Submitted: "a", Land: "a"}, true},
		{"review off, another head", api.GroupItem{State: ItemReady, Review: "off", Submitted: "a", Land: "b"}, false},
		{"landed already", api.GroupItem{State: ItemLanded, Reviewer: "r", Submitted: "a", Land: "a", Verdict: pass("a", "")}, false},
	}
	for _, c := range cases {
		if got := MayLand(c.it); got != c.want {
			t.Errorf("%s: MayLand %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNextLand(t *testing.T) {
	ready := func(id, after string) api.GroupItem {
		return api.GroupItem{ID: id, After: after, State: ItemReady, Review: "off", Submitted: id + "-h", Land: id + "-h"}
	}
	g := api.Group{Items: []api.GroupItem{ready("a", ""), ready("b", "a"), ready("c", "")}}
	g.Items[0].State = ItemReview // a is not ready: b stacks on it and waits, c goes first
	g.Lane.Queue = []string{"b", "c"}
	if got := NextLand(g); got != "c" {
		t.Fatalf("next %q, want c: b waits for its base", got)
	}
	g.Items[0].State = ItemLanded
	if got := NextLand(g); got != "b" {
		t.Fatalf("next %q, want b in queue order once a landed", got)
	}
	g.Items[0].State = ItemDropped // a dropped base holds nothing back
	if got := NextLand(g); got != "b" {
		t.Fatalf("next %q with the base dropped", got)
	}
	g.Lane.Landing = "c" // a run cut short goes first
	if got := NextLand(g); got != "c" {
		t.Fatalf("next %q, want the landing item", got)
	}
	g.Items[2].Land = "" // the landing item may no longer land: back to the queue
	if got := NextLand(g); got != "b" {
		t.Fatalf("next %q after the landing item lost its head", got)
	}
	g.Lane.Queue = nil
	g.Lane.Landing = ""
	if got := NextLand(g); got != "" {
		t.Fatalf("next %q from an empty queue", got)
	}
}

func TestUpstreamPriorFinishedDequeue(t *testing.T) {
	g := api.Group{Items: []api.GroupItem{{ID: "a", Submitted: "s1"}, {ID: "b", After: "a"}, {ID: "c", After: "gone"}}}
	if got := Upstream(g, g.Items[1]); got != "s1" {
		t.Fatalf("upstream %q, want the submitted head", got)
	}
	g.Items[0].Land = "l1"
	if got := Upstream(g, g.Items[1]); got != "l1" {
		t.Fatalf("upstream %q, want the land head", got)
	}
	if Upstream(g, g.Items[0]) != "" || Upstream(g, g.Items[2]) != "" {
		t.Fatal("upstream of an item with no base, or a base not in the group")
	}
	it := api.GroupItem{Attempts: []api.GroupLaneAttempt{{Rebased: "x"}, {}, {Rebased: "y"}, {Rebased: "x"}}}
	if got := strings.Join(Prior(it), ","); got != "x,y" {
		t.Fatalf("prior %q", got)
	}
	if Finished(api.Group{}) {
		t.Fatal("an empty group finished")
	}
	g.Items[0].State, g.Items[1].State, g.Items[2].State = ItemLanded, ItemDropped, ItemReady
	if Finished(g) {
		t.Fatal("finished with an item ready")
	}
	g.Items[2].State = ItemLanded
	if !Finished(g) {
		t.Fatal("not finished with every item landed or dropped")
	}
	g.Lane.Queue, g.Lane.Landing = []string{"a", "b", "a"}, "a"
	Dequeue(&g, "a")
	if strings.Join(g.Lane.Queue, ",") != "b" || g.Lane.Landing != "" {
		t.Fatalf("dequeue left %v landing %q", g.Lane.Queue, g.Lane.Landing)
	}
}

func TestStackOn(t *testing.T) {
	plan := []api.GroupPlanItem{
		{ID: "api", Touches: []string{"internal/api"}},
		{ID: "ui", Touches: []string{"apple/"}},
		{ID: "docs", Touches: []string{"docs/specs/groups.md"}},
	}
	earlier := []api.GroupItem{{ID: "api"}, {ID: "ui"}, {ID: "docs"}}
	cases := []struct {
		touches []string
		want    string
	}{
		{[]string{"internal/api/server.go"}, "api"},   // a file under a folder another item touches
		{[]string{"/apple"}, "ui"},                    // slashes trimmed
		{[]string{"docs"}, "docs"},                    // a folder above a file
		{[]string{"ui"}, "ui"},                        // an item id
		{[]string{"internal/apix"}, ""},               // a sibling name is no overlap
		{[]string{"internal/api", "apple/App"}, "ui"}, // the latest earlier item wins
		{[]string{""}, ""},                            // an empty path overlaps nothing
		{nil, ""},
	}
	for _, c := range cases {
		it := api.GroupPlanItem{ID: "new", Touches: c.touches}
		if got := StackOn(earlier, plan, it); got != c.want {
			t.Errorf("touches %q: stacks on %q, want %q", c.touches, got, c.want)
		}
	}
	if got := StackOn(earlier, plan, api.GroupPlanItem{ID: "api", Touches: []string{"internal/api"}}); got != "" {
		t.Fatalf("an item stacked on itself: %q", got)
	}
}

// A landed item frees its lead slot for the next queued item (groups.md 4.3).
func TestNextLeadsAfterLand(t *testing.T) {
	g := api.Group{State: StateRunning, Items: []api.GroupItem{
		{ID: "a", Lead: "h", State: ItemLanding},
		{ID: "b", Lead: "h", State: ItemQueued},
	}}
	if got := NextLeads(g, 1); got != nil {
		t.Fatalf("a landing item holds its slot: %v", got)
	}
	g.Items[0].State = ItemLanded
	if got := strings.Join(NextLeads(g, 1), ","); got != "b" {
		t.Fatalf("after a land %q", got)
	}
}

func TestLaneKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(p, []byte("[groups]\nlane_tries = 7\nfix_output_lines = 0\n"), 0o600)
	var logs []string
	k := LoadKeys(p, func(f string, a ...any) { logs = append(logs, f) })
	if k.LaneTries != 7 || k.FixOutputLines != 0 || len(logs) != 0 {
		t.Fatalf("keys %+v, logs %v", k, logs)
	}
	os.WriteFile(p, []byte("[groups]\nlane_tries = 0\nfix_output_lines = 501\n"), 0o600)
	logs = nil
	if k := LoadKeys(p, func(f string, a ...any) { logs = append(logs, f) }); k.LaneTries != 3 || k.FixOutputLines != 40 || len(logs) != 2 {
		t.Fatalf("out of range %+v %v", k, logs)
	}
	os.WriteFile(p, []byte("[groups]\nlane_tries = \"3\"\n"), 0o600)
	logs = nil
	if k := LoadKeys(p, func(f string, a ...any) { logs = append(logs, f) }); k.LaneTries != 3 || len(logs) != 1 {
		t.Fatalf("wrong type %+v %v", k, logs)
	}
}
