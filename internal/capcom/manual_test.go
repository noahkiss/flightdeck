package capcom

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The service manual replaces the pane manual's filing rule with Capcom's memory in Logbook
// (capcom-service.md 6.4), and the service folder writes it as CLAUDE.md.
func TestServiceManual(t *testing.T) {
	if strings.Contains(ServiceManual, fileJob) || !strings.Contains(ServiceManual, "the tag capcom") || !strings.Contains(ServiceManual, "## Memory") {
		t.Fatalf("service manual:\n%s", ServiceManual)
	}
	if !strings.Contains(Manual, fileJob) {
		t.Fatal("the pane manual lost its filing rule")
	}
	dir := t.TempDir()
	if err := WriteServiceFolder(dir, "/usr/local/bin/gff-hab", ReadScope{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != ServiceManual {
		t.Fatalf("CLAUDE.md %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
}
