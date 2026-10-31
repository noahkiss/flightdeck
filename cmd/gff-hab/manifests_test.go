package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestManifestsRefreshCLI: -dry-run prints the diff and writes nothing; without it the new
// manifest lands in the state directory and `manifests list` shows it in force. The catalogue is
// a local directory, so the test needs no network.
func TestManifestsRefreshCLI(t *testing.T) {
	cat := t.TempDir()
	state := t.TempDir()
	files := map[string]string{
		"index.toml":  "schema_version = 1\n[[agents]]\nid = \"claude\"\npath = \"claude.toml\"\n",
		"claude.toml": "id = \"claude\"\nversion = \"2099.1.1.1\"\nmin_engine_version = 1\n\n[[rules]]\nid = \"r\"\nstate = \"working\"\ncontains = [\"busy\"]\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cat, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := run(t, "manifests", "refresh", "-dry-run", "-state", state, "-from", cat)
	for _, want := range []string{"--- claude.toml ", "+++ claude.toml 2099.1.1.1", "+version = \"2099.1.1.1\"", "would update claude", "dry run, nothing written"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "manifests")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the data directory: %v", err)
	}

	run(t, "manifests", "refresh", "-state", state, "-from", cat)
	list := run(t, "manifests", "list", "-state", state)
	if !strings.Contains(list, "claude\t2099.1.1.1\tdata\t") {
		t.Fatalf("list does not show the refreshed claude manifest:\n%s", list)
	}

	// A broken file in the data directory is drift: list says so and exits 1.
	os.WriteFile(filepath.Join(state, "manifests", "codex.toml"), []byte("id = \"codex\"\n"), 0o600)
	out2, err := exec.Command(bin, "manifests", "list", "-state", state).Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 || !strings.Contains(string(out2), "problem\t") {
		t.Fatalf("list with a broken manifest: %v\n%s", err, out2)
	}
}
