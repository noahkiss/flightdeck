package instructions

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Once a plan stops offering a write, our earlier render still loads. The plan warns, and for
// pi names the user's CLAUDE.md that it hides.
func TestStaleRenderWarning(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir, "pi", "codex")
	for _, h := range []Harness{Pi, Codex} {
		if _, err := r.Write(h); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(r.Env.PiDir, "CLAUDE.md")
	os.WriteFile(claude, []byte("Added later.\n"), 0o644)
	p := r.Plan(Pi)
	want := r.Env.TargetPath(Pi) + " was rendered by Flightdeck earlier and the harness still loads it; remove it: it hides " + claude
	if p.Status != UserLoaded || !strings.Contains(strings.Join(p.Warnings, "\n"), want) {
		t.Fatalf("status %s warnings %q; want user-loaded with %q", p.Status, p.Warnings, want)
	}
	var out bytes.Buffer
	p.Report(&out)
	if !strings.Contains(out.String(), "warning: "+want) {
		t.Fatalf("report lacks the warning:\n%s", out.String())
	}

	// Sources gone: Nothing, and the old Codex file still warns.
	r.Config.Sources = nil
	p = r.Plan(Codex)
	if p.Status != Nothing || !strings.Contains(strings.Join(p.Warnings, "\n"), "was rendered by Flightdeck earlier") {
		t.Fatalf("status %s warnings %q", p.Status, p.Warnings)
	}
	out.Reset()
	p.Report(&out)
	if !strings.Contains(out.String(), "warning: "+r.Env.TargetPath(Codex)+" was rendered") {
		t.Fatalf("report for a Nothing plan lacks the warning:\n%s", out.String())
	}
}

// A source that Claude's own CLAUDE.md or a rules file imports already reaches Claude, so the
// Claude render leaves it out.
func TestClaudeNativeImportsAreDuplicates(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	put := func(p, body string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(r.Env.ClaudeDir, "CLAUDE.md"), "Mine.\n@"+filepath.Join(dir, "src", "root.md")+"\n")
	p := r.Plan(Claude)
	if p.Status != Ready || strings.Contains(p.Text, "# Root rules") || strings.Contains(p.Text, "Mine.") || !strings.Contains(p.Text, "Host only.") {
		t.Fatalf("status %s, text:\n%s", p.Status, p.Text)
	}
	// A nested rules file importing the other source leaves nothing to render.
	put(filepath.Join(r.Env.ClaudeDir, "rules", "team", "x.md"), "@~/../src/host.md\n")
	if p := r.Plan(Claude); p.Status != Nothing || !strings.Contains(p.Reason, "own files' imports") {
		t.Fatalf("status %s reason %q, want nothing", p.Status, p.Reason)
	}
	// Other harnesses still get everything.
	if p := r.Plan(Codex); !strings.Contains(p.Text, "# Root rules") || !strings.Contains(p.Text, "Host only.") {
		t.Fatal("the Claude rule leaked into Codex")
	}
}

// A relative or ~ directory never becomes a target under the working directory.
func TestRelativeTargetRefused(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": "rel/config"}
	e := EnvFrom(func(k string) string { return env[k] }, "/h", "/s")
	if e.OpencodeDir != "/h/.config/opencode" {
		t.Fatalf("relative XDG_CONFIG_HOME used: %s", e.OpencodeDir)
	}
	dir := fixture(t)
	for _, bad := range []string{"rel/codex", "~/codex"} {
		r := testRenderer(dir, "codex")
		r.Env.CodexHome = bad
		p := r.Plan(Codex)
		if p.Status != Refused || !strings.Contains(p.Reason, "not absolute") {
			t.Fatalf("%s: status %s reason %q", bad, p.Status, p.Reason)
		}
		before := snapshot(t, dir)
		if _, err := r.Write(Codex); err == nil {
			t.Fatalf("%s: the write went ahead", bad)
		}
		if snapshot(t, dir) != before {
			t.Fatal("a refused write changed the tree")
		}
	}
}

// opencode's config may list the rendered file in ~/ form.
func TestOpencodeListedTildeForm(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	r.Env.StateDir = filepath.Join(r.Env.Home, ".local", "state", "fd")
	cfg := filepath.Join(r.Env.OpencodeDir, "opencode.json")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte(`{"instructions":["~/.local/state/fd/instructions/opencode.md"]}`), 0o644)
	if w := strings.Join(r.Plan(Opencode).Warnings, "\n"); strings.Contains(w, "opencode loads this file only when") {
		t.Fatalf("a ~/ entry was not matched: %q", w)
	}
}

// The dry run says when a write would create the target's directory.
func TestReportNewDir(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	var out bytes.Buffer
	r.Plan(Codex).Report(&out)
	if !strings.Contains(out.String(), "note: the directory "+r.Env.CodexHome+" will be created\n") {
		t.Fatalf("no new-directory note:\n%s", out.String())
	}
	os.MkdirAll(r.Env.CodexHome, 0o755)
	out.Reset()
	r.Plan(Codex).Report(&out)
	if strings.Contains(out.String(), "will be created") {
		t.Fatal("note shown for an existing directory")
	}
}
