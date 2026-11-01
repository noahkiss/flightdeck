package instructions

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const hostWant = "Host only.\nFrom home.\n"

func TestImportPath(t *testing.T) {
	cases := []struct {
		line string
		path string
		ok   bool
	}{
		{"@a.md\n", "a.md", true},
		{"   @sub/a.md  \n", "sub/a.md", true},
		{"@~/x.md", "~/x.md", true},
		{"@/abs/x.md", "/abs/x.md", true},
		{`@with\ space.md`, "with space.md", true},
		{"@two words", "", false},
		{`@"quoted.md"`, "", false},
		{"@'quoted.md'", "", false},
		{"see @a.md", "", false},
		{"user@host", "", false},
		{"@", "", false},
		{"@`a.md`", "", false},
		{"`@a.md`", "", false},
	}
	for _, c := range cases {
		p, ok := importPath(c.line)
		if p != c.path || ok != c.ok {
			t.Errorf("importPath(%q) = %q, %v; want %q, %v", c.line, p, ok, c.path, c.ok)
		}
	}
}

func TestFences(t *testing.T) {
	var f fenceState
	in := []string{"text\n", "````md\n", "```\n", "@a.md\n", "````\n", "after\n", "~~~\n", "@b.md\n", "~~~~\n", "out\n", "``` `x` ```\n", "@c.md\n"}
	want := []bool{false, true, true, true, true, false, true, true, true, false, false, false}
	for i, l := range in {
		if got := f.step(l); got != want[i] {
			t.Errorf("line %d %q: in fence %v, want %v", i, l, got, want[i])
		}
	}
}

func TestExpandFixture(t *testing.T) {
	dir := fixture(t)
	ex := newExpander(filepath.Join(dir, "home"), importHops[Claude])
	got := ex.expand(filepath.Join(dir, "src", "root.md"), false)
	if got != rootWant {
		t.Fatalf("flattened root:\n%s\nwant:\n%s", got, rootWant)
	}
	markers := map[string]Marker{}
	depths := map[string]int{}
	for _, v := range ex.visits {
		rel, _ := filepath.Rel(filepath.Join(dir, "src"), v.Path)
		if _, ok := markers[rel]; !ok {
			markers[rel], depths[rel] = v.Marker, v.Depth
		}
	}
	want := map[string]Marker{
		"root.md": Read, "d1.md": Read, "d4.md": Read, "d5.md": TooDeep,
		"missing.md": Missing, "with space.md": Read, "shared.md": Read,
		"sub/side.md": Read, "sub/cyc.md": Read,
	}
	for k, m := range want {
		if markers[k] != m {
			t.Errorf("%s: marker %q, want %q", k, markers[k], m)
		}
	}
	if depths["d5.md"] != 5 {
		t.Errorf("d5.md at depth %d, want 5", depths["d5.md"])
	}
	if _, ok := markers["d6.md"]; ok {
		t.Error("d6.md (depth 6) was visited; the depth-5 drop must stop the branch")
	}
	if _, ok := markers["quoted.md"]; ok {
		t.Error("a quoted path was treated as an import")
	}
	// The cycle (side -> cyc -> side) and the diamond (root and side both import shared) are
	// both visible as markers.
	var cycle, dup bool
	for _, v := range ex.visits {
		if v.Marker == Cycle && strings.HasSuffix(v.Path, filepath.Join("sub", "side.md")) && strings.HasSuffix(v.From, "cyc.md") {
			cycle = true
		}
		if v.Marker == Duplicate && strings.HasSuffix(v.Path, "shared.md") && strings.HasSuffix(v.From, "side.md") {
			dup = true
		}
	}
	if !cycle || !dup {
		t.Errorf("cycle seen %v, duplicate seen %v; want both", cycle, dup)
	}
}

func TestExpandImportsOnly(t *testing.T) {
	dir := fixture(t)
	ex := newExpander(filepath.Join(dir, "home"), importHops[Codex])
	got := ex.expand(filepath.Join(dir, "src", "host.md"), true)
	if want := "Shared once.\nFrom home.\n"; got != want {
		t.Fatalf("imports only: %q, want %q", got, want)
	}
}

func TestExpandSymlinkCycleAndLarge(t *testing.T) {
	dir := fixture(t)
	src := filepath.Join(dir, "src")
	if err := os.Symlink(filepath.Join(src, "loop.md"), filepath.Join(src, "alias.md")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "loop.md"), []byte("Loop.\n@alias.md\n@big.md\n"), 0o644)
	os.WriteFile(filepath.Join(src, "big.md"), bytes.Repeat([]byte("x"), 64), 0o644)
	ex := newExpander(dir, 4)
	ex.maxBytes = 32
	if got := ex.expand(filepath.Join(src, "loop.md"), false); got != "Loop.\n" {
		t.Fatalf("got %q", got)
	}
	var cyc, large bool
	for _, v := range ex.visits {
		cyc = cyc || v.Marker == Cycle
		large = large || v.Marker == TooLarge
	}
	if !cyc || !large {
		t.Fatalf("cycle through a link %v, too large %v; want both", cyc, large)
	}
}

func TestPlanRendersEverySource(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	p := r.Plan(Codex)
	if p.Status != Ready {
		t.Fatalf("status %s (%s), want ready", p.Status, p.Reason)
	}
	head, body, ok := strings.Cut(p.Text, "-->\n\n")
	if !ok {
		t.Fatalf("no header end in:\n%s", p.Text)
	}
	if body != rootWant+"\n"+hostWant {
		t.Fatalf("body:\n%s\nwant:\n%s", body, rootWant+"\n"+hostWant)
	}
	wantHead := headerFirst + "\n     digest: sha256:" + p.Digest + "  renderer: 1  at: 2026-11-01T12:00:00Z\n     sources: 11 read, 1 missing "
	if head != wantHead {
		t.Fatalf("header:\n%q\nwant:\n%q", head, wantHead)
	}
	if strings.Contains(head, dir) {
		t.Fatal("the header names a path")
	}
}

func TestDryRunPrintsDiffWritesNothing(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	// An older render already sits at the Codex target.
	old := header(strings.Repeat("0", 64), time.Unix(0, 0), nil) + "\nOld text.\n"
	codex := r.Env.TargetPath(Codex)
	os.MkdirAll(filepath.Dir(codex), 0o755)
	os.WriteFile(codex, []byte(old), 0o644)

	before := snapshot(t, dir)
	var out bytes.Buffer
	for _, p := range r.PlanAll() {
		p.Report(&out)
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("the dry run changed the tree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	s := out.String()
	for _, want := range []string{
		"--- " + codex + "\n+++ " + codex + " (rendered)\n",
		"-Old text.\n",
		"+Shared once.\n",
		"--- /dev/null\n",
		"skipped missing: " + filepath.Join(dir, "src", "missing.md") + " (imported from " + filepath.Join(dir, "src", "root.md") + ")",
		"skipped depth: " + filepath.Join(dir, "src", "d5.md"),
		"skipped duplicate: " + filepath.Join(dir, "src", "shared.md"),
		"write: not enabled",
		"opencode loads this file only when",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run lacks %q:\n%s", want, s)
		}
	}
}

func TestDigest(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	base := r.Plan(Codex).Digest

	r.Now = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	if d := r.Plan(Codex).Digest; d != base {
		t.Fatal("the render time changed the digest")
	}
	if d := r.Plan(Pi).Digest; d == base {
		t.Fatal("the harness name is not in the digest")
	}
	edit := func(name, body string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "src", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return r.Plan(Codex).Digest
	}
	seen := map[string]string{"base": base}
	check := func(what, d string) {
		t.Helper()
		for k, v := range seen {
			if v == d {
				t.Fatalf("%s gave the same digest as %s", what, k)
			}
		}
		seen[what] = d
	}
	check("deep import edit", edit("d4.md", "Depth 4, edited.\n@d5.md\n"))
	check("second source edit", edit("host.md", "Host only, edited.\n@shared.md\n@~/home.md\n"))
	check("missing file created", edit("missing.md", ""))
	// A file past the depth cap is not read, so editing it changes nothing.
	d := edit("d6.md", "Still never read.\n")
	if d != seen["missing file created"] {
		t.Fatal("editing a file past the depth cap changed the digest")
	}
}

func TestGitWorkTreeWarning(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	// The home directory is a git work tree, as a dotfiles repo would be. No git binary needed.
	if err := os.Mkdir(filepath.Join(r.Env.Home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := r.Plan(Claude)
	want := "the target sits inside the git work tree at " + r.Env.Home
	if len(p.Warnings) == 0 || !strings.HasPrefix(p.Warnings[0], want) {
		t.Fatalf("warnings %q, want one starting %q", p.Warnings, want)
	}
	var out bytes.Buffer
	p.Report(&out)
	if !strings.Contains(out.String(), "warning: "+want) {
		t.Fatalf("report lacks the warning:\n%s", out.String())
	}
	os.Remove(filepath.Join(r.Env.Home, ".git"))
	if w := r.Plan(Claude).Warnings; len(w) != 0 {
		t.Fatalf("warnings outside a work tree: %q", w)
	}
}

func TestWrite(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	before := snapshot(t, dir)
	beforePaths := paths(t, dir)
	if _, err := r.Write(Codex); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("write without opt-in: %v, want ErrNotEnabled", err)
	}
	if snapshot(t, dir) != before {
		t.Fatal("a refused write changed the tree")
	}

	r.Config.Write = []string{"codex"}
	p, err := r.Write(Codex)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p.Path)
	if err != nil || string(got) != p.Text {
		t.Fatalf("written file differs: %v", err)
	}
	if info, _ := os.Stat(p.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	rec, err := os.ReadFile(r.Env.RecordPath(Codex))
	if err != nil || !strings.Contains(string(rec), p.Digest) || !strings.Contains(string(rec), filepath.Join(dir, "src", "missing.md")) {
		t.Fatalf("state record: %v\n%s", err, rec)
	}
	// Only the target and the state record are new.
	var added []string
	for p := range paths(t, dir) {
		if !beforePaths[p] {
			added = append(added, p)
		}
	}
	sort.Strings(added)
	wantAdded := []string{"home/.codex/", "home/.codex/AGENTS.md", "state/", "state/instructions/", "state/instructions/codex.json"}
	if strings.Join(added, ",") != strings.Join(wantAdded, ",") {
		t.Fatalf("write added %q, want %q", added, wantAdded)
	}

	if p := r.Plan(Codex); p.Status != UpToDate {
		t.Fatalf("after the write: %s, want up-to-date", p.Status)
	}
	os.WriteFile(filepath.Join(dir, "src", "d1.md"), []byte("Depth 1, edited.\n@d2.md\n"), 0o644)
	if p := r.Plan(Codex); p.Status != Ready {
		t.Fatalf("after a source edit: %s, want ready", p.Status)
	}
	if _, err := r.Write(Codex); err != nil {
		t.Fatalf("rewrite of our own file: %v", err)
	}
	if got, _ := os.ReadFile(r.Env.TargetPath(Codex)); !strings.Contains(string(got), "Depth 1, edited.") {
		t.Fatal("the rewrite did not land")
	}
}

func TestRefusals(t *testing.T) {
	put := func(t *testing.T, p, body string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		h      Harness
		setup  func(t *testing.T, r *Renderer)
		status Status
		reason string
	}{
		{"codex user file", Codex, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.CodexHome, "AGENTS.md"), "My own rules.\n")
		}, UserLoaded, "user's own file"},
		{"codex override", Codex, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.CodexHome, "AGENTS.override.md"), "Override.\n")
		}, UserLoaded, "AGENTS.override.md"},
		{"codex empty file without header", Codex, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.CodexHome, "AGENTS.md"), "")
		}, Refused, "no Flightdeck header"},
		{"pi claude file", Pi, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.PiDir, "CLAUDE.md"), "Pi reads this.\n")
		}, UserLoaded, "CLAUDE.md"},
		{"opencode user file", Opencode, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.OpencodeDir, "AGENTS.md"), "Mine.\n")
		}, UserLoaded, "user's own file"},
		{"opencode plugin names a source", Opencode, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.OpencodeDir, "plugin", "load.js"), "read('~/../src/root.md')\n"+r.Config.Sources[1].Path+"\n")
		}, UserLoaded, "names a source"},
		{"pi extension names a source in ~/ form", Pi, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.PiDir, "extensions", "rules.ts"), "load('~/notes.md')\n")
			r.Config.Sources = append(r.Config.Sources, Source{Path: filepath.Join(r.Env.Home, "notes.md"), Scope: ScopeGlobal})
		}, UserLoaded, "~/notes.md"},
		{"claude hook names a source", Claude, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.ClaudeDir, "settings.json"), `{"hooks":{"SessionStart":[{"command":"cat `+r.Config.Sources[0].Path+`"}]}}`)
		}, UserLoaded, "names a source"},
		{"claude rules file without header", Claude, func(t *testing.T, r *Renderer) {
			put(t, filepath.Join(r.Env.ClaudeDir, "rules", "flightdeck.md"), "Someone else's.\n")
		}, Refused, "no Flightdeck header"},
		{"symlink target", Codex, func(t *testing.T, r *Renderer) {
			p := filepath.Join(r.Env.CodexHome, "AGENTS.md")
			elsewhere := filepath.Join(r.Env.Home, "dotfiles-copy.md")
			put(t, elsewhere, header(strings.Repeat("0", 64), time.Unix(0, 0), nil))
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(elsewhere, p); err != nil {
				t.Fatal(err)
			}
		}, Refused, "symbolic link"},
		{"claude only native source", Claude, func(t *testing.T, r *Renderer) {
			native := filepath.Join(r.Env.ClaudeDir, "CLAUDE.md")
			put(t, native, "Native.\n")
			r.Config.Sources = []Source{{Path: native, Scope: ScopeGlobal}}
		}, Nothing, "loads these sources itself"},
		{"no source for this host", Codex, func(t *testing.T, r *Renderer) {
			r.Config.Sources = r.Config.Sources[2:]
		}, Nothing, "no global source"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := fixture(t)
			r := testRenderer(dir, string(c.h))
			c.setup(t, r)
			before := snapshot(t, dir)
			p := r.Plan(c.h)
			if p.Status != c.status || !strings.Contains(p.Reason, c.reason) {
				t.Fatalf("status %q reason %q; want %q containing %q", p.Status, p.Reason, c.status, c.reason)
			}
			if _, err := r.Write(c.h); err == nil {
				t.Fatal("the write went ahead")
			}
			if snapshot(t, dir) != before {
				t.Fatal("a refused write changed the tree")
			}
		})
	}
}

func TestClaudeSkipsNativeSourceKeepsOthers(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	native := filepath.Join(r.Env.ClaudeDir, "CLAUDE.md")
	os.MkdirAll(filepath.Dir(native), 0o755)
	os.WriteFile(native, []byte("Native.\n@"+filepath.Join(dir, "src", "shared.md")+"\n"), 0o644)
	r.Config.Sources = append(r.Config.Sources, Source{Path: native, Scope: ScopeGlobal, Order: 0})
	cl := r.Plan(Claude)
	if cl.Status != Ready || strings.Contains(cl.Text, "Native.") {
		t.Fatalf("claude: %s, holds the native file: %v", cl.Status, strings.Contains(cl.Text, "Native."))
	}
	cx := r.Plan(Codex)
	if !strings.Contains(cx.Text, "-->\n\nNative.\nShared once.\n\n# Root rules") {
		t.Fatalf("codex render lacks the flattened Claude file first:\n%s", cx.Text)
	}
}

func TestCodexSizeWarning(t *testing.T) {
	dir := fixture(t)
	r := testRenderer(dir)
	os.WriteFile(filepath.Join(dir, "src", "shared.md"), bytes.Repeat([]byte("Long line of rules.\n"), 1500), 0o644)
	if w := strings.Join(r.Plan(Codex).Warnings, "\n"); !strings.Contains(w, "32 KiB") {
		t.Fatalf("no size warning: %q", w)
	}
	if w := strings.Join(r.Plan(Pi).Warnings, "\n"); strings.Contains(w, "32 KiB") {
		t.Fatal("the size warning is Codex only")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "conf", "flightdeck.toml")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte(`
[other]
ignored = true

[instructions]
write = ["codex"]

[[instructions.source]]
path = "~/rules.md"
scope = "global"
order = 2

[[instructions.source]]
path = "local/base.md"
scope = "global"
hosts = ["box-a"]
harnesses = ["codex", "pi"]
order = 1

[[instructions.source]]
path = "/abs/proj.md"
scope = "project"
project = "example/repo"
`), 0o644)
	c, err := LoadConfig(cfg, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if c.Sources[0].Path != "/home/u/rules.md" || c.Sources[1].Path != filepath.Join(dir, "conf", "local", "base.md") || c.Sources[2].Path != "/abs/proj.md" {
		t.Fatalf("paths: %+v", c.Sources)
	}
	if got := c.globalSources(Codex, "box-a"); len(got) != 2 || got[0].Order != 1 {
		t.Fatalf("codex on box-a: %+v", got)
	}
	if got := c.globalSources(Claude, "box-a"); len(got) != 1 {
		t.Fatalf("claude on box-a: %+v", got)
	}
	if got := c.globalSources(Codex, "box-b"); len(got) != 1 {
		t.Fatalf("codex on box-b: %+v", got)
	}
	if !c.writeEnabled(Codex) || c.writeEnabled(Pi) {
		t.Fatal("write list")
	}

	for name, body := range map[string]string{
		"typo":          "[instructions]\nwrites = [\"codex\"]\n",
		"scope":         "[[instructions.source]]\npath = \"a.md\"\nscope = \"machine\"\n",
		"no project":    "[[instructions.source]]\npath = \"a.md\"\nscope = \"project\"\n",
		"harness":       "[[instructions.source]]\npath = \"a.md\"\nscope = \"global\"\nharnesses = [\"vim\"]\n",
		"write harness": "[instructions]\nwrite = [\"all\"]\n",
		"empty path":    "[[instructions.source]]\nscope = \"global\"\n",
	} {
		os.WriteFile(cfg, []byte(body), 0o644)
		if _, err := LoadConfig(cfg, "/home/u"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestEnvFrom(t *testing.T) {
	env := map[string]string{"CODEX_HOME": "/c", "XDG_CONFIG_HOME": "/x"}
	e := EnvFrom(func(k string) string { return env[k] }, "/h", "/s")
	if e.ClaudeDir != "/h/.claude" || e.CodexHome != "/c" || e.OpencodeDir != "/x/opencode" || e.PiDir != "/h/.pi/agent" {
		t.Fatalf("%+v", e)
	}
	if e.TargetPath(Claude) != "/h/.claude/rules/flightdeck.md" || e.TargetPath(Opencode) != "/s/instructions/opencode.md" {
		t.Fatal("target paths")
	}
}
