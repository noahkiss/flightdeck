package check

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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

// TestFixture is diagnostics test 10, first half (10.2): the fixture breaks each ban, and the
// check reports each, at the line whose comment names its rule. relay reaches diaglog only
// through fixture.
func TestFixture(t *testing.T) {
	cfg := Default
	cfg.Dir = moduleRoot(t)
	cfg.Patterns = []string{fixture, fixture + "/relay"}
	cfg.HotPaths = []string{fixture + ".hot"}
	cfg.DiagPackages = []string{fixture}
	found, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{RuleMessage: 5, RuleSlog: 1, RuleBuilt: 2, RuleContent: 3, RuleHotPath: 1, RuleNetwork: 1}
	got := map[string]int{}
	files := map[string][]string{}
	for _, f := range found {
		got[f.Rule]++
		lines, ok := files[f.Pos.Filename]
		if !ok {
			b, err := os.ReadFile(f.Pos.Filename)
			if err != nil {
				t.Fatal(err)
			}
			lines = strings.Split(string(b), "\n")
			files[f.Pos.Filename] = lines
		}
		if f.Pos.Line < 1 || f.Pos.Line > len(lines) || !strings.Contains(lines[f.Pos.Line-1], "// ban: "+f.Rule) {
			t.Errorf("finding at a line that names no such ban: %v", f)
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

// TestTree is diagnostics test 10, second half: the repository passes the check, and each hot
// path and content type the check lists names a declaration in it.
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
	missing, err := Unresolved(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range missing {
		t.Errorf("%s names no declaration: a rename turned its ban off", m)
	}
}

// TestUnresolved: a hot path or content type that names no declaration is reported.
func TestUnresolved(t *testing.T) {
	cfg := Default
	cfg.Dir = moduleRoot(t)
	cfg.HotPaths = []string{mod + "/internal/holder.Holder.pump", mod + "/internal/holder.Holder.gone"}
	cfg.ContentTypes = []string{api + ".ChatMessage", api + ".Gone"}
	missing, err := Unresolved(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(missing, " ") != mod+"/internal/holder.Holder.gone "+api+".Gone" {
		t.Errorf("Unresolved = %v", missing)
	}
}

// TestFreeTextSites: each line of diagnostics.md 1.5 that wrote free text is a typed line, and
// writes its values through exactly these constructors: the free text under text, path or a
// name key, never in its message (stage 5).
func TestFreeTextSites(t *testing.T) {
	sites := []struct {
		file, msg string
		ctors     []string
	}{
		{"cmd/gff-hab/groupmove.go", "queued move runs", []string{"Group", "Text"}},
		{"cmd/gff-hab/groupmove.go", "queued move ran", []string{"Group", "Text"}},
		{"cmd/gff-hab/rounds.go", "round notice", []string{"Text"}},
		{"cmd/gff-hab/capcompace.go", "usage reading drift", []string{"Text"}},
		{"internal/capcomengine/supervisor.go", "engine start check failed", []string{"Text"}},
		{"internal/capcomengine/supervisor.go", "engine reported", []string{"Text"}},
		{"internal/speech/helper.go", "helper stderr", []string{"Text"}},
		{"internal/modhost/host.go", "module host stderr", []string{"Count", "Text"}},
		{"cmd/gff-hab/trust.go", "trust prompt shown", []string{"Harness", "Pane", "Path"}},
		{"cmd/gff-hab/trust.go", "trust prompt answered", []string{"ClientID", "Pane", "Path", "Word"}},
	}
	root := moduleRoot(t)
	for _, s := range sites {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, s.file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var calls [][]string
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			lit, isLit := call.Args[0].(*ast.BasicLit)
			if !ok || !isLit || !slices.Contains([]string{"Debug", "Info", "Warn", "Error"}, sel.Sel.Name) ||
				lit.Value != strconv.Quote(s.msg) {
				return true
			}
			var ctors []string
			for _, a := range call.Args[1:] {
				if c, ok := a.(*ast.CallExpr); ok {
					if cs, ok := c.Fun.(*ast.SelectorExpr); ok {
						if x, ok := cs.X.(*ast.Ident); ok && x.Name == "diaglog" {
							ctors = append(ctors, cs.Sel.Name)
							continue
						}
					}
				}
				ctors = append(ctors, "?")
			}
			sort.Strings(ctors)
			calls = append(calls, ctors)
			return true
		})
		if len(calls) != 1 || !slices.Equal(calls[0], s.ctors) {
			t.Errorf("%s %q: constructors %v, want one line with %v", s.file, s.msg, calls, s.ctors)
		}
	}
}
