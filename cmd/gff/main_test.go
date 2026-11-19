package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/noahkiss/flightdeck/internal/gffclient"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/instructions"
	"github.com/noahkiss/flightdeck/internal/mcpreg"
)

// The host reader's warnings about the [keys] and [bar] tables are dropped, since
// gffclient.LoadSettings checks those tables itself; its other warnings stay.
func TestHostProblems(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	body := "[[hosts]]\nlabel = \"a\"\nendpoints = [\"/tmp/a.sock\"]\ncolour = \"red\"\n\n" +
		"[keys.session]\nz = \"zoom\"\n\n[bar]\nclock = \"\"\n\n[barn]\nx = 1\n"
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

// TestExampleConfig: docs/examples/gff.toml, which holds gff's tables and the [instructions]
// and [mcp] tables gff-hab reads from the same file, loads in gff with no problem, and each of
// gff-hab's readers takes its own table (instructions-mcp-flight-rules.md 6.6, 8.4).
func TestExampleConfig(t *testing.T) {
	p := filepath.Join("..", "..", "docs", "examples", "gff.toml")
	specs, problems, err := hosts.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostProblems(p, problems); len(got) > 0 {
		t.Errorf("gff reports %q", got)
	}
	if len(specs) != 2 {
		t.Errorf("%d hosts, want 2", len(specs))
	}
	if _, problems, err := gffclient.LoadSettings(p); err != nil || len(problems) > 0 {
		t.Errorf("settings: %v %q", err, problems)
	}
	ic, err := instructions.LoadConfig(p, t.TempDir())
	if err != nil || len(ic.Sources) != 1 {
		t.Errorf("instructions: %v, %d sources", err, len(ic.Sources))
	}
	mc, err := mcpreg.LoadConfig(p)
	if err != nil || len(mc.Servers) != 1 {
		t.Errorf("mcp: %v, %d servers", err, len(mc.Servers))
	}
	// A table neither reader owns is still reported.
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	q := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(q, append(body, "\n[mcpx]\ny = 1\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, problems, err = hosts.LoadConfig(q)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hostProblems(q, problems), []string{q + ": unknown key mcpx", q + ": unknown key mcpx.y"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
