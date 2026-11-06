package hook

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintedCommands(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "hab's binary")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, harness := range []string{"claude", "codex"} {
		config, err := Config(harness, binary)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Hooks map[string][]hookGroup `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(config), &parsed); err != nil {
			t.Fatal(err)
		}
		command := parsed.Hooks["PermissionRequest"][0].Hooks[0].Command
		run := func() (string, error) {
			c := exec.Command("/bin/sh", "-c", command)
			c.Stdin = strings.NewReader("input")
			b, err := c.CombinedOutput()
			return string(b), err
		}
		if got, err := run(); err != nil || got != "hook -wait "+harness+" PermissionRequest\ninput" {
			t.Fatalf("present: %q %v", got, err)
		}
		if err := os.Rename(binary, binary+".away"); err != nil {
			t.Fatal(err)
		}
		if got, err := run(); err != nil || got != "" {
			t.Fatalf("missing: %q %v", got, err)
		}
		if err := os.Rename(binary+".away", binary); err != nil {
			t.Fatal(err)
		}
	}
	for _, wrap := range []string{"", "printf 'wrapped: '; cat"} {
		config, err := StatusLineConfig(binary, wrap)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			StatusLine struct{ Command string } `json:"statusLine"`
		}
		if err := json.Unmarshal([]byte(config), &parsed); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(binary, binary+".away"); err != nil {
			t.Fatal(err)
		}
		c := exec.Command("/bin/sh", "-c", parsed.StatusLine.Command)
		c.Stdin = strings.NewReader("input")
		got, err := c.CombinedOutput()
		want := ""
		if wrap != "" {
			want = "wrapped: input"
		}
		if err != nil || string(got) != want {
			t.Fatalf("fallback: %q %v", got, err)
		}
		if err := os.Rename(binary+".away", binary); err != nil {
			t.Fatal(err)
		}
	}
}
