package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstructionsRenderCLI: the default run is a dry run that prints a diff and writes
// nothing; -write writes only a target in the config's write list. Every harness directory,
// the home directory and the state directory are temporary.
func TestInstructionsRenderCLI(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	state := filepath.Join(dir, "state")
	files := map[string]string{
		"src/rules.md":    "# My rules\n@extra.md\n@gone.md\n",
		"src/extra.md":    "Keep it short.\n",
		"flightdeck.toml": "[instructions]\nwrite = [\"codex\"]\n\n[[instructions.source]]\npath = \"src/rules.md\"\nscope = \"global\"\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(home, 0o755)
	env := append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"CODEX_HOME="+filepath.Join(home, ".codex"),
		"PI_CODING_AGENT_DIR="+filepath.Join(home, ".pi", "agent"),
		"GFF_CONFIG="+filepath.Join(dir, "flightdeck.toml"),
		"GFF_HAB_STATE="+state,
		"GFF_HAB_HOST_ID=test-host",
	)
	gff := func(args ...string) (string, error) {
		cmd := exec.Command(bin, append([]string{"instructions", "render"}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := gff()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	codex := filepath.Join(home, ".codex", "AGENTS.md")
	for _, want := range []string{
		"== codex: " + codex,
		"--- /dev/null\n+++ " + codex + " (rendered)\n",
		"+# My rules\n+Keep it short.\n",
		"skipped missing: " + filepath.Join(dir, "src", "gone.md"),
		"dry run, nothing written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("the dry run wrote into home: %v", entries)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("the dry run made the state directory: %v", err)
	}

	if out, err := gff("-write", "pi"); err == nil || !strings.Contains(out, "not listed in the instructions write list") {
		t.Fatalf("write of a target not opted in: %v\n%s", err, out)
	}
	if out, err := gff("-write", "codex"); err != nil || !strings.Contains(out, "wrote "+codex) {
		t.Fatalf("write codex: %v\n%s", err, out)
	}
	got, err := os.ReadFile(codex)
	if err != nil || !strings.Contains(string(got), "Keep it short.") || !strings.HasPrefix(string(got), "<!-- Rendered by Flightdeck.") {
		t.Fatalf("written file: %v\n%s", err, got)
	}
	if out, err := gff("-write", "codex"); err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("second write: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".pi")); !os.IsNotExist(err) {
		t.Fatal("a target that was not written exists")
	}
}
