package flightrules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

const fixture = "testdata/claude-2.1.327.jsonl"

func at(sec string) string { return "2026-01-01T10:00:" + sec + ".000Z" }

// want is the part of an entry a fixture test checks.
type want struct {
	kind        api.FlightRuleKind
	scope       api.FlightRuleScope
	source      string
	event       string
	attribution api.FlightRuleAttribution
	first, last string
	gone        string
	reason      string
	text        string // "" for a placeholder
	note        string // a substring the note holds
}

// TestClaudeFixture reads the hand-written 2.1.327 fixture: each attachment type the spec lists
// for Claude Code, a compaction that resends and replaces files, removals, a sidechain record,
// ignored types and an unknown one (spec 10, 11.2).
func TestClaudeFixture(t *testing.T) {
	s := NewSession(NewClaude(fixture), nil)
	entries, warnings, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wants := []want{
		{kind: "instruction", scope: "managed", source: "/etc/claude-code/CLAUDE.md", attribution: "recorded", first: at("02"), last: at("02"), reason: "session_start", text: "Managed policy text."},
		{kind: "instruction", scope: "user", source: "/home/user/.claude/CLAUDE.md", attribution: "recorded", first: at("02"), last: at("13"), reason: "session_start", text: "User instructions."},
		{kind: "instruction", scope: "user", source: "/home/user/.claude/shared.md", attribution: "recorded", first: at("02"), last: at("02"), reason: "session_start", text: "Imported by the user file."},
		{kind: "instruction", scope: "user", source: "/home/user/.claude/rules/flightdeck.md", attribution: "recorded", first: at("02"), last: at("02"), reason: "session_start"},
		{kind: "instruction", scope: "project", source: "/work/demo/CLAUDE.md", attribution: "recorded", first: at("02"), last: at("02"), reason: "session_start", text: "Project instructions, first version."},
		{kind: "instruction", scope: "local", source: "/work/demo/CLAUDE.local.md", attribution: "recorded", first: at("02"), last: at("02"), gone: at("13"), reason: "session_start", text: "Local notes.", note: "Only the start"},
		{kind: "memory", scope: "memory", source: "/home/user/.claude/projects/-work-demo/memory/MEMORY.md", attribution: "recorded", first: at("02"), last: at("02"), reason: "session_start", text: "- [A memory](a.md)"},
		{kind: "skills", scope: "skill", source: "skill listing", attribution: "recorded", first: at("03"), last: at("03"), text: "- demo-skill: Does a demo thing.\n- other-skill: Does another.", note: "2 skills"},
		{kind: "mcp", scope: "mcp", source: "docs", attribution: "recorded", first: at("04"), last: at("04"), text: "## docs\nUse the docs server for manuals."},
		{kind: "mcp", scope: "mcp", source: "tracker", attribution: "recorded", first: at("04"), last: at("04"), gone: at("14"), text: "## tracker\nFile issues here."},
		{kind: "hook", scope: "session", source: "SessionStart", event: "SessionStart", attribution: "recorded", first: at("05"), last: at("05"), text: "Context from a start hook."},
		{kind: "instruction", scope: "project", source: "/work/demo/sub/CLAUDE.md", attribution: "recorded", first: at("07"), last: at("07"), text: "Nested folder rules.", note: "folder"},
		{kind: "skill", scope: "skill", source: "demo-skill", attribution: "recorded", first: at("08"), last: at("08"), text: "Demo skill body."},
		{kind: "system", scope: "session", source: "attachment relevant_memories", attribution: "unknown", first: at("09"), last: at("09"), note: "does not read"},
		{kind: "instruction", scope: "project", source: "/work/demo/CLAUDE.md", attribution: "recorded", first: at("13"), last: at("13"), reason: "compaction", text: "Project instructions, second version."},
		{kind: "hook", scope: "session", source: "PostToolUse:Bash", event: "PostToolUse", attribution: "recorded", first: at("15"), last: at("15"), text: "Lint passed with 0 warnings."},
		{kind: "hook", scope: "session", source: "hook", attribution: "unknown", first: at("16"), last: at("16"), text: "Context from a hook with no name.", note: "without the hook's name"},
	}
	if len(entries) != len(wants) {
		for _, e := range entries {
			t.Logf("%s %s %s", e.Kind, e.Scope, e.Source)
		}
		t.Fatalf("got %d entries, want %d", len(entries), len(wants))
	}
	for i, w := range wants {
		e := entries[i]
		if e.Kind != w.kind || e.Scope != w.scope || e.Source != w.source || e.Event != w.event ||
			e.Attribution != w.attribution || e.FirstSeen != w.first || e.LastSeen != w.last ||
			e.Gone != w.gone || e.Reason != w.reason {
			t.Errorf("entry %d = %+v\nwant %+v", i, e, w)
		}
		if w.note != "" && !strings.Contains(e.Note, w.note) {
			t.Errorf("entry %d note %q lacks %q", i, e.Note, w.note)
		}
		if w.kind == "system" {
			if e.Digest != "" || e.Bytes != 0 {
				t.Errorf("placeholder %d has digest %q and %d bytes", i, e.Digest, e.Bytes)
			}
			if _, _, _, err := s.Text(context.Background(), e.ID); !errors.Is(err, ErrNoEntry) {
				t.Errorf("text of placeholder %d: %v, want ErrNoEntry", i, err)
			}
			continue
		}
		if w.text != "" {
			if e.Digest != Digest(w.text) || e.Bytes != int64(len(w.text)) {
				t.Errorf("entry %d digest %s bytes %d, want the text's", i, e.Digest, e.Bytes)
			}
			got, size, cut, err := s.Text(context.Background(), e.ID)
			if err != nil || got != w.text || size != int64(len(w.text)) || cut {
				t.Errorf("text of entry %d = %q, %d, %v, %v", i, got, size, cut, err)
			}
		}
	}
	if s.Harness() != "claude" {
		t.Errorf("harness %q", s.Harness())
	}
	// The skill listing's path field is empty; the invoked skill names its file.
	if entries[12].Path != "/home/user/.claude/skills/demo-skill/SKILL.md" {
		t.Errorf("invoked skill path %q", entries[12].Path)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "relevant_memories") {
		t.Errorf("warnings %q, want one about relevant_memories", warnings)
	}
	// A sidechain record names a file no entry holds.
	for _, e := range entries {
		if strings.Contains(e.Source, "sub2") {
			t.Errorf("sidechain file in the ledger: %+v", e)
		}
	}
	// A second read with nothing new changes nothing.
	again, _, err := s.List(context.Background())
	if err != nil || !slices.Equal(ids(again), ids(entries)) {
		t.Errorf("second list changed: %v", err)
	}
}

func ids(es []api.FlightRule) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID+e.LastSeen+e.Gone)
	}
	return out
}

// TestClaudeHookNamesHookAndEvent is the task's acceptance case: a hook injection yields an
// entry that names the hook and its event.
func TestClaudeHookNamesHookAndEvent(t *testing.T) {
	path := writeLines(t, `{"type":"attachment","timestamp":"2026-01-01T10:00:00Z","version":"2.1.327","attachment":{"type":"hook_additional_context","content":["Remember the style guide."],"hookName":"UserPromptSubmit","toolUseID":"x","hookEvent":"UserPromptSubmit"}}`)
	es, _, err := NewSession(NewClaude(path), nil).List(context.Background())
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %v", es, err)
	}
	if e := es[0]; e.Kind != api.FlightRuleHook || e.Source != "UserPromptSubmit" || e.Event != "UserPromptSubmit" || e.Attribution != api.FlightRecorded {
		t.Errorf("%+v", e)
	}
}

// TestClaudeOldInstructionsShape: the 2.1.310 shape carries text with no file, so the source
// is unknown, never guessed.
func TestClaudeOldInstructionsShape(t *testing.T) {
	path := writeLines(t, `{"isSidechain":false,"type":"attachment","timestamp":"2026-01-01T10:00:01.000Z","version":"2.1.310","attachment":{"type":"instructions","content":"Project notes for the demo."}}`)
	es, warnings, err := NewSession(NewClaude(path), nil).List(context.Background())
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %v", es, err)
	}
	e := es[0]
	if e.Attribution != api.FlightUnknown || e.Scope != "" || e.Source != "instructions" || e.Digest != Digest("Project notes for the demo.") || e.Note == "" {
		t.Errorf("%+v", e)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings %q: 2.1.310 is in the tested range", warnings)
	}
}

// TestClaudeIncremental: each list reads only what was appended; a partial last line waits;
// a replaced file is read again from the start.
func TestClaudeIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	src := NewClaude(path)
	s := NewSession(src, nil)
	if es, _, err := s.List(context.Background()); err != nil || len(es) != 0 {
		t.Fatalf("missing file: %v %v", es, err)
	}
	line := func(name, text string) string {
		return `{"type":"attachment","timestamp":"2026-01-01T10:00:00Z","attachment":{"type":"invoked_skills","skills":[{"name":"` + name + `","content":"` + text + `"}]}}`
	}
	writeFile(t, path, line("a", "one")+"\n"+line("b", "two")[:40])
	if es, _, _ := s.List(context.Background()); len(es) != 1 {
		t.Fatalf("partial line read: %d entries", len(es))
	}
	appendFile(t, path, line("b", "two")[40:]+"\n")
	es, _, _ := s.List(context.Background())
	if len(es) != 2 || es[1].Source != "b" {
		t.Fatalf("after completing the line: %+v", es)
	}
	writeFile(t, path, line("c", "three")+"\n")
	es, warnings, _ := s.List(context.Background())
	if len(es) != 1 || es[0].Source != "c" {
		t.Fatalf("after replacement: %+v", es)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "replaced") {
		t.Errorf("warnings %q", warnings)
	}
}

// TestClaudeOversizeLine: a line over the limit is skipped with a warning, and the lines after
// it still read.
func TestClaudeOversizeLine(t *testing.T) {
	old := maxLine
	maxLine = 300
	t.Cleanup(func() { maxLine = old })
	big := `{"type":"attachment","attachment":{"type":"invoked_skills","skills":[{"name":"big","content":"` + strings.Repeat("x", 400) + `"}]}}`
	small := `{"type":"attachment","attachment":{"type":"invoked_skills","skills":[{"name":"small","content":"y"}]}}`
	path := writeLines(t, big, small)
	es, warnings, err := NewSession(NewClaude(path), nil).List(context.Background())
	if err != nil || len(es) != 1 || es[0].Source != "small" {
		t.Fatalf("%+v %v", es, err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "longer than") {
		t.Errorf("warnings %q", warnings)
	}
}

// TestClaudeVersionDrift: a version outside the tested range warns once and still reads.
func TestClaudeVersionDrift(t *testing.T) {
	l := `{"type":"attachment","version":"2.9.0","attachment":{"type":"invoked_skills","skills":[{"name":"a","content":"x"}]}}`
	path := writeLines(t, l, l)
	es, warnings, _ := NewSession(NewClaude(path), nil).List(context.Background())
	if len(es) != 1 || len(warnings) != 1 || !strings.Contains(warnings[0], "outside the tested range") {
		t.Errorf("%+v %q", es, warnings)
	}
}

// TestClaudeUnreadableShapes: a bad line warns; an attachment whose fields have the wrong
// types becomes a placeholder of its type; an MCP block that is not text is a placeholder.
func TestClaudeUnreadableShapes(t *testing.T) {
	path := writeLines(t,
		`{"type":"attachment", nope`,
		`{"type":"attachment","attachment":{"type":"invoked_skills","skills":"not a list"}}`,
		`{"type":"attachment","attachment":{"type":"mcp_instructions_delta","addedNames":["srv"],"addedBlocks":[{"x":1}]}}`,
	)
	es, warnings, _ := NewSession(NewClaude(path), nil).List(context.Background())
	if len(es) != 2 || es[0].Source != "attachment invoked_skills" || es[0].Attribution != api.FlightUnknown ||
		es[1].Kind != api.FlightRuleMCP || es[1].Source != "srv" || es[1].Digest != "" || es[1].Attribution != api.FlightUnknown {
		t.Errorf("%+v", es)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings %q", warnings)
	}
}

// TestTextTruncated: a text over MaxText is cut on a character boundary.
func TestTextTruncated(t *testing.T) {
	body := strings.Repeat("é", MaxText/2+10)
	path := writeLines(t, `{"type":"attachment","attachment":{"type":"invoked_skills","skills":[{"name":"long","content":"`+body+`"}]}}`)
	s := NewSession(NewClaude(path), nil)
	es, _, _ := s.List(context.Background())
	text, size, cut, err := s.Text(context.Background(), es[0].ID)
	if err != nil || !cut || size != int64(len(body)) || len(text) > MaxText || !strings.HasPrefix(body, text) || len(text)%2 != 0 {
		t.Errorf("len %d size %d cut %v err %v", len(text), size, cut, err)
	}
	if _, _, _, err := s.Text(context.Background(), "nope"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("unknown id: %v", err)
	}
}

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	// A new file, so a replacement is a different file, as when a harness rewrites one.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}
