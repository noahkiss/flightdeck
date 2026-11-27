package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/hosts"
)

// Inside a pane ($GFF_HAB set), gff runs no command to reach a hab: -via is refused, and a
// cmd: endpoint is dropped with a note while the host's other endpoints stay
// (hab-logic-api.md 4.1).
func TestCommandInPane(t *testing.T) {
	t.Setenv("GFF_HAB", "/tmp/hook.sock#token")
	if _, _, err := transport("", "ssh elsewhere gff-hab connect", ""); err == nil || !strings.Contains(err.Error(), "-via") {
		t.Fatalf("-via in a pane: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := `[[hosts]]
label = "only-cmd"
endpoints = ["cmd:ssh elsewhere gff-hab connect"]

[[hosts]]
label = "mixed"
endpoints = ["cmd:ssh other gff-hab connect", "other.example"]
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := workspaceHosts(path, []string{"flag=cmd:true"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Label != "mixed" || len(specs[0].Endpoints) != 1 || specs[0].Endpoints[0].Kind != hosts.EndpointSSH {
		t.Fatalf("hosts in a pane: %+v", specs)
	}

	t.Setenv("GFF_HAB", "")
	if _, _, err := transport("", "true", ""); err != nil {
		t.Fatalf("-via outside a pane: %v", err)
	}
	specs, err = workspaceHosts(path, nil, nil, "")
	if err != nil || len(specs) != 2 {
		t.Fatalf("hosts outside a pane: %+v %v", specs, err)
	}
}
