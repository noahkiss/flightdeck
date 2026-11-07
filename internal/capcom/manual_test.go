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
	if strings.Contains(ServiceManual, watchLimit) || !strings.Contains(ServiceManual, "group_create") || !strings.Contains(ServiceManual, "group_close") {
		t.Fatalf("service manual without the group tools (groups.md 12.1):\n%s", ServiceManual)
	}
	if !strings.Contains(Manual, fileJob) || !strings.Contains(Manual, watchLimit) {
		t.Fatal("the pane manual lost its filing rule")
	}
	dir := t.TempDir()
	if err := WriteServiceFolder(dir, "/usr/local/bin/gff-hab", ReadScope{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"CLAUDE.md", "AGENTS.md"} { // the Claude engine's, the Codex engine's
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != ServiceManual {
			t.Fatalf("%s %q", f, b)
		}
	}
	// Codex reads at most 32 KiB of AGENTS.md (its project_doc_max_bytes).
	if len(ServiceManual) > 32<<10 {
		t.Fatalf("the service manual is %d bytes", len(ServiceManual))
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
}
