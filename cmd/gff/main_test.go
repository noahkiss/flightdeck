package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/noahkiss/flightdeck/internal/hosts"
)

// The host reader's warnings about the [keys] and [bar] tables are dropped, since
// gffclient.LoadSettings checks those tables itself; its other warnings stay.
func TestHostProblems(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	body := "[[hosts]]\nlabel = \"a\"\nendpoints = [\"/tmp/a.sock\"]\ncolour = \"red\"\n\n" +
		"[keys.prefix]\nz = \"zoom\"\n\n[bar]\nclock = \"\"\n\n[barn]\nx = 1\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, problems, err := hosts.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	got := hostProblems(p, problems)
	want := []string{p + ": unknown key hosts.colour", p + ": unknown key barn", p + ": unknown key barn.x"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q\n(from %q)", got, want, problems)
	}
}
