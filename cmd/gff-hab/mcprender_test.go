package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPRenderCLI: `gff-hab mcp render` is a dry run by default; -write writes only a harness
// in the config's write list. Every harness folder, the home folder and the state folder are
// temporary, and a stand-in claude is named with -claude-bin.
func TestMCPRenderCLI(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	state := filepath.Join(dir, "state")
	os.MkdirAll(home, 0o755)
	cfg := filepath.Join(dir, "flightdeck.toml")
	body := "[mcp]\nwrite = [\"codex\", \"claude\"]\n\n[[mcp.server]]\nname = \"docs\"\ntransport = \"stdio\"\ncommand = \"docs-mcp\"\nenv = { DOCS_TOKEN = { env = \"DOCS_TOKEN\" } }\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeClaude := filepath.Join(dir, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\necho \"stand-in refused $2\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"CODEX_HOME="+filepath.Join(home, ".codex"),
		"GFF_CONFIG="+cfg,
		"GFF_HAB_STATE="+state,
		"GFF_HAB_HOST_ID=test-host",
	)
	gff := func(args ...string) (string, error) {
		cmd := exec.Command(bin, append([]string{"mcp", "render", "-claude-bin", fakeClaude}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := gff()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	codex := filepath.Join(home, ".codex", "config.toml")
	for _, want := range []string{
		"== claude: " + filepath.Join(home, ".claude", ".claude.json"),
		"== codex: " + codex,
		"== opencode: " + filepath.Join(home, ".config", "opencode", "opencode.jsonc"),
		"server docs: add",
		"--- /dev/null\n+++ " + codex + " (rendered)\n",
		"+[mcp_servers.docs]\n+command = \"docs-mcp\"\n+env_vars = [\"DOCS_TOKEN\"]\n",
		"\"DOCS_TOKEN\": \"{env:DOCS_TOKEN}\"",
		"write: not enabled (add it to the mcp write list)",
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

	if out, err := gff("-dry-run", "-write", "codex"); err == nil || !strings.Contains(out, "-dry-run and -write together") {
		t.Fatalf("-dry-run with -write: %v\n%s", err, out)
	}
	if out, err := gff("-write", "opencode"); err == nil || !strings.Contains(out, "not listed in the mcp write list") {
		t.Fatalf("write not opted in: %v\n%s", err, out)
	}
	if out, err := gff("-write", "codex"); err != nil || !strings.Contains(out, "wrote "+codex) {
		t.Fatalf("write codex: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(codex); err != nil || !strings.Contains(string(got), "[mcp_servers.docs]") {
		t.Fatalf("written file: %v\n%s", err, got)
	}
	if _, err := os.Stat(filepath.Join(state, "mcp", "codex.json")); err != nil {
		t.Fatalf("no state record: %v", err)
	}
	if out, err := gff("-write", "codex"); err != nil || !strings.Contains(out, "nothing to write") {
		t.Fatalf("second write: %v\n%s", err, out)
	}
	if out, err := gff("-write", "claude"); err == nil || !strings.Contains(out, "stand-in refused add-json") {
		t.Fatalf("write claude through the stand-in: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Fatal("a harness that was not written has a config")
	}
}
