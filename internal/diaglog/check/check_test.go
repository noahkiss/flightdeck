package check

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = DiaglogPath + "/check/testdata/fixture"

// moduleRoot is the folder of go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// TestFixture is diagnostics test 10, first half (10.2): the fixture breaks each ban once, and
// the check reports each, at the line that breaks it.
func TestFixture(t *testing.T) {
	cfg := Default
	cfg.Dir = moduleRoot(t)
	cfg.Patterns = []string{fixture}
	cfg.HotPaths = []string{fixture + ".hot"}
	cfg.DiagPackages = []string{fixture}
	found, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{RuleMessage: 1, RuleSlog: 1, RuleBuilt: 1, RuleContent: 2, RuleHotPath: 1, RuleNetwork: 1}
	got := map[string]int{}
	for _, f := range found {
		got[f.Rule]++
		if !strings.HasSuffix(f.Pos.Filename, "fixture.go") || f.Pos.Line == 0 {
			t.Errorf("finding with no place: %v", f)
		}
	}
	for _, r := range Rules {
		if got[r] != want[r] {
			t.Errorf("rule %s: %d findings, want %d", r, got[r], want[r])
		}
	}
	if t.Failed() {
		for _, f := range found {
			t.Log(f)
		}
	}
}

// TestTree is diagnostics test 10, second half: the repository passes the check.
func TestTree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the export data of the packages that log")
	}
	cfg := Default
	cfg.Dir = moduleRoot(t)
	found, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Error(f)
	}
}
