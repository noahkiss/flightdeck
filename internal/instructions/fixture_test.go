package instructions

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// tree is the hand-written fake source tree. Every test copies it into its own temporary
// directory, so nothing outside that directory is ever read or written.
var tree = map[string]string{
	"src/root.md": "# Root rules\n" +
		"\n" +
		"Use short sentences.\n" +
		"@d1.md\n" +
		"Mail user@host stays text.\n" +
		"@missing.md\n" +
		"@\"quoted.md\"\n" +
		"@with\\ space.md\n" +
		"@shared.md\n" +
		"@sub/side.md\n" +
		"\n" +
		"```sh\n" +
		"@d1.md\n" +
		"```\n" +
		"\n" +
		"~~~\n" +
		"@shared.md\n" +
		"~~~\n" +
		"Inline `@shared.md` stays.\n" +
		"End of root.\n",
	"src/d1.md":         "Depth 1.\n@d2.md\n",
	"src/d2.md":         "Depth 2.\n@d3.md\n",
	"src/d3.md":         "Depth 3.\n@d4.md\n",
	"src/d4.md":         "Depth 4.\n@d5.md\n",
	"src/d5.md":         "Depth 5 must never appear.\n@d6.md\n",
	"src/d6.md":         "Depth 6 must never appear.\n",
	"src/with space.md": "Spaced name.\n",
	"src/quoted.md":     "Quoted must never appear.\n",
	"src/shared.md":     "Shared once.\n",
	"src/sub/side.md":   "Side.\n@../shared.md\n@cyc.md\n",
	"src/sub/cyc.md":    "Cycle.\n@side.md\n",
	"src/host.md":       "Host only.\n@shared.md\n@~/home.md\n",
	"home/home.md":      "From home.\n",
}

// rootWant is root.md flattened: imports replaced, the depth-5 import and everything after it
// gone, the missing and the cycle import leaving nothing, code left alone.
const rootWant = "# Root rules\n" +
	"\n" +
	"Use short sentences.\n" +
	"Depth 1.\n" +
	"Depth 2.\n" +
	"Depth 3.\n" +
	"Depth 4.\n" +
	"Mail user@host stays text.\n" +
	"@\"quoted.md\"\n" +
	"Spaced name.\n" +
	"Shared once.\n" +
	"Side.\n" +
	"Cycle.\n" +
	"\n" +
	"```sh\n" +
	"@d1.md\n" +
	"```\n" +
	"\n" +
	"~~~\n" +
	"@shared.md\n" +
	"~~~\n" +
	"Inline `@shared.md` stays.\n" +
	"End of root.\n"

// fixture copies the tree into a temporary directory and returns its real path.
func fixture(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range tree {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// testEnv puts every harness directory and the state directory inside the fixture.
func testEnv(dir string) Env {
	home := filepath.Join(dir, "home")
	return Env{
		Home:        home,
		ClaudeDir:   filepath.Join(home, ".claude"),
		CodexHome:   filepath.Join(home, ".codex"),
		OpencodeDir: filepath.Join(home, ".config", "opencode"),
		PiDir:       filepath.Join(home, ".pi", "agent"),
		StateDir:    filepath.Join(dir, "state"),
	}
}

func testRenderer(dir string, write ...string) *Renderer {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	return &Renderer{
		Config: Config{
			Sources: []Source{
				{Path: filepath.Join(dir, "src", "host.md"), Scope: ScopeGlobal, Order: 2},
				{Path: filepath.Join(dir, "src", "root.md"), Scope: ScopeGlobal, Order: 1},
				{Path: filepath.Join(dir, "src", "other-host.md"), Scope: ScopeGlobal, Hosts: []string{"elsewhere"}},
				{Path: filepath.Join(dir, "src", "proj.md"), Scope: ScopeProject, Project: "example/repo"},
			},
			Write: write,
		},
		Env:  testEnv(dir),
		Host: "this-host",
		Now:  func() time.Time { return at },
	}
}

// snapshot lists every path under dir with its size and contents, to prove a run wrote nothing.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			out = append(out, rel+"/")
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, rel+"="+string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// paths lists every path under dir, directories with a trailing slash.
func paths(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			rel += "/"
		}
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	return out
}
