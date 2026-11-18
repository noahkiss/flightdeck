package flightrules

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// TestConfiguredSources: the fixture's rendered rules file names the configured sources when the
// render record's target and digest match its header, and none when the digest differs.
func TestConfiguredSources(t *testing.T) {
	const target = "/home/user/.claude/rules/flightdeck.md"
	state := t.TempDir()
	writeRecord := func(digest string) {
		rec := map[string]any{
			"harness": "claude", "target": target, "digest": digest, "renderer": 1,
			"written": "2026-01-01T09:00:00Z", "roots": []string{"/home/user/notes/agents.md"},
			"sources": []map[string]any{{"path": "/home/user/notes/agents.md", "marker": "read", "depth": 0}},
		}
		data, _ := json.Marshal(rec)
		if err := os.MkdirAll(filepath.Join(state, "instructions"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "instructions", "claude.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list := func() map[string][]string {
		s := NewSession(NewClaude(fixture), Rendered(state, instructions.Claude))
		es, _, err := s.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]string{}
		for _, e := range es {
			if len(e.ConfiguredSources) > 0 {
				out[e.Source] = e.ConfiguredSources
			}
		}
		return out
	}
	if got := list(); len(got) != 0 {
		t.Errorf("no record: %v", got)
	}
	writeRecord(strings.Repeat("ab", 32))
	got := list()
	if len(got) != 1 || !slices.Equal(got[target], []string{"/home/user/notes/agents.md"}) {
		t.Errorf("matching record: %v", got)
	}
	writeRecord(strings.Repeat("cd", 32))
	if got := list(); len(got) != 0 {
		t.Errorf("other digest: %v", got)
	}
}

// TestReadRenderedOldRecord: a record written before roots were kept names its root files that
// read.
func TestReadRenderedOldRecord(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "instructions"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"harness":"codex","target":"/x/AGENTS.md","digest":"aa","sources":[{"path":"/a.md","marker":"read","depth":0},{"path":"/b.md","marker":"read","depth":1},{"path":"/c.md","marker":"missing","depth":0}]}`
	if err := os.WriteFile(filepath.Join(state, "instructions", "codex.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	r, ok, err := instructions.ReadRendered(state, instructions.Codex)
	if err != nil || !ok || r.Target != "/x/AGENTS.md" || !slices.Equal(r.Sources, []string{"/a.md"}) {
		t.Errorf("%+v %v %v", r, ok, err)
	}
	// A Claude record from then names nothing: its roots mix in Claude's own files.
	claude := strings.Replace(data, `"codex"`, `"claude"`, 1)
	if err := os.WriteFile(filepath.Join(state, "instructions", "claude.json"), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, ok, err := instructions.ReadRendered(state, instructions.Claude); err != nil || !ok || len(r.Sources) != 0 {
		t.Errorf("old claude record: %+v %v %v", r, ok, err)
	}
	if _, ok, err := instructions.ReadRendered(state, instructions.Pi); ok || err != nil {
		t.Errorf("no record: %v %v", ok, err)
	}
}
