package flightrules

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/instructions"
)

// The rendered global file's two versions in the Codex fixture: the one on disk now (render
// 11…) and an older one a later block carries (render 22…).
const (
	codexRenderNow = "1111111111111111111111111111111111111111111111111111111111111111"
	codexRenderOld = "2222222222222222222222222222222222222222222222222222222222222222"
)

func renderedText(digest, at, body string) string {
	return "<!-- Rendered by Flightdeck. Do not edit; edit the sources and render again.\n" +
		"     digest: sha256:" + digest + "  renderer: 1  at: " + at + "\n" +
		"     sources: 1 read, 0 missing -->\n\n" + body
}

var (
	codexGlobalNow = renderedText(codexRenderNow, "2026-01-01T09:00:00Z", "Global rules, current render.")
	codexGlobalOld = renderedText(codexRenderOld, "2026-01-01T08:00:00Z", "Global rules, older render.")
)

// codexTree writes the files the fixture's session loaded, as they stand now: the rendered
// global file, a project file, and a nested file edited since the session loaded it. It
// returns Codex's home and the fixture rollout, with {{ROOT}} filled in.
func codexTree(t *testing.T) (root, home, rollout string) {
	t.Helper()
	root = t.TempDir()
	home = filepath.Join(root, "codex-home")
	files := map[string]string{
		filepath.Join(home, "AGENTS.md"):                    codexGlobalNow + "\n",
		filepath.Join(root, "proj", "AGENTS.md"):            "Project rule.\n",
		filepath.Join(root, "proj", "sub", "AGENTS.md"):     "Sub rule, edited.\n",
		filepath.Join(root, "proj", ".git", "placeholder"): "",
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("testdata/codex-0.169.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rollout = filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte(strings.ReplaceAll(string(data), "{{ROOT}}", root)), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, home, rollout
}

// TestCodexFixture reads the hand-written 0.169.0 rollout: the base instructions, the skill
// listing written twice, an AGENTS.md block split into the files Codex's discovery finds now,
// text no file holds (unknown), a later block that replaces it and starts with an older render
// of the global file, and lines the ledger leaves out (spec 11.2, 11.4).
func TestCodexFixture(t *testing.T) {
	root, home, rollout := codexTree(t)
	s := NewSession(NewCodex(rollout, home), nil)
	entries, warnings, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, "AGENTS.md")
	wants := []want{
		{kind: "system", scope: "session", source: "base instructions", attribution: "recorded", first: at("00"), last: at("00"), text: "You are a demo coding agent."},
		{kind: "skills", scope: "skill", source: "skill listing", attribution: "recorded", first: at("01"), last: at("01"), text: "\n## Skills\n- demo: Does a demo thing.\n"},
		{kind: "instruction", scope: "user", source: global, attribution: "matched", first: at("02"), last: at("02"), gone: at("09"), text: codexGlobalNow, note: "current text"},
		{kind: "instruction", scope: "project", source: filepath.Join(root, "proj", "AGENTS.md"), attribution: "matched", first: at("02"), last: at("09"), text: "Project rule.\n", count: 2},
		{kind: "instruction", source: "AGENTS.md instructions", attribution: "unknown", first: at("02"), last: at("02"), gone: at("09"), text: "Sub rule.\n", note: filepath.Join(root, "proj", "sub", "AGENTS.md")},
		{kind: "instruction", scope: "user", source: global, attribution: "matched", first: at("09"), last: at("09"), text: codexGlobalOld, note: "render header"},
	}
	if len(entries) != len(wants) {
		for _, e := range entries {
			t.Logf("%s %s %s %s", e.Kind, e.Scope, e.Source, e.Attribution)
		}
		t.Fatalf("got %d entries, want %d", len(entries), len(wants))
	}
	for i, w := range wants {
		checkEntry(t, s, entries[i], w)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %q", warnings)
	}
	// A second list reads nothing new.
	again, _, err := s.List(context.Background())
	if err != nil || len(again) != len(entries) || again[1].Count != entries[1].Count {
		t.Errorf("second list: %d entries, %v", len(again), err)
	}
}

// checkEntry compares one entry with a want, and its text read again from the record.
func checkEntry(t *testing.T, s *Session, e api.FlightRule, w want) {
	t.Helper()
	if e.Kind != w.kind || e.Scope != w.scope || e.Source != w.source || e.Event != w.event ||
		e.Attribution != w.attribution || e.FirstSeen != w.first || e.LastSeen != w.last || e.Gone != w.gone ||
		e.Reason != w.reason {
		t.Errorf("entry %s: got %+v, want %+v", w.source, e, w)
		return
	}
	if w.note != "" && !strings.Contains(e.Note, w.note) {
		t.Errorf("entry %s: note %q lacks %q", w.source, e.Note, w.note)
	}
	count := w.count
	if count == 0 {
		count = 1
	}
	if e.Count != count {
		t.Errorf("entry %s: count %d, want %d", w.source, e.Count, count)
	}
	if w.text == "" {
		if e.Digest != "" {
			t.Errorf("entry %s: a placeholder has a digest", w.source)
		}
		return
	}
	if e.Digest != Digest(w.text) || e.Bytes != int64(len(w.text)) {
		t.Errorf("entry %s: digest or size does not match its text", w.source)
	}
	text, size, _, err := s.Text(context.Background(), e.ID)
	if err != nil || text != w.text || size != int64(len(w.text)) {
		t.Errorf("entry %s: text %q %d %v", w.source, text, size, err)
	}
}

// writeRenderRecord writes the instruction renderer's state record for one harness.
func writeRenderRecord(t *testing.T, state string, h instructions.Harness, target, digest string, roots ...string) {
	t.Helper()
	rec := map[string]any{"harness": string(h), "target": target, "digest": digest, "renderer": 1,
		"written": "2026-01-01T09:00:00Z", "roots": roots, "sources": []any{}}
	data, _ := json.Marshal(rec)
	if err := os.MkdirAll(filepath.Join(state, "instructions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "instructions", string(h)+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCodexStale: the session's newest copy of the rendered global file carries render 22…. A
// render record of 11… makes it stale; a record of 22… does not, and names the configured
// sources. The older copy, gone since, is never stale.
func TestCodexStale(t *testing.T) {
	_, home, rollout := codexTree(t)
	state := t.TempDir()
	global := filepath.Join(home, "AGENTS.md")
	list := func() []api.FlightRule {
		s := NewSession(NewCodex(rollout, home), Rendered(state, instructions.Codex)).CheckStale(Current(state, instructions.Codex))
		es, _, err := s.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return es
	}
	stale := func(es []api.FlightRule) (out []string) {
		for _, e := range es {
			if e.Stale {
				out = append(out, e.Digest)
			}
		}
		return out
	}
	if got := stale(list()); len(got) != 0 {
		t.Errorf("no record: stale %v", got)
	}
	writeRenderRecord(t, state, instructions.Codex, global, codexRenderNow, "/src/agents.md")
	if got := stale(list()); !slices.Equal(got, []string{Digest(codexGlobalOld)}) {
		t.Errorf("old copy loaded: stale %v", got)
	}
	writeRenderRecord(t, state, instructions.Codex, global, codexRenderOld, "/src/agents.md")
	es := list()
	if got := stale(es); len(got) != 0 {
		t.Errorf("current copy loaded: stale %v", got)
	}
	if e := es[5]; !slices.Equal(e.ConfiguredSources, []string{"/src/agents.md"}) {
		t.Errorf("configured sources: %v", e.ConfiguredSources)
	}
	// Another target: nothing is stale.
	writeRenderRecord(t, state, instructions.Codex, "/elsewhere/AGENTS.md", codexRenderNow)
	if got := stale(list()); len(got) != 0 {
		t.Errorf("other target: stale %v", got)
	}
}

// TestCodexRemovedAndGrown: Codex's removal notice marks every AGENTS.md source gone, and a
// rollout that grows is read from where the last read ended; an override file hides the
// rendered global file, so a block with a render header is unknown.
func TestCodexRemovedAndGrown(t *testing.T) {
	root, home, rollout := codexTree(t)
	src := NewCodex(rollout, home)
	s := NewSession(src, nil)
	if _, _, err := s.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	removal := `{"timestamp":"2026-01-01T10:00:20.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n\n<INSTRUCTIONS>\nThe previously provided AGENTS.md instructions no longer apply.\n</INSTRUCTIONS>"}]}}` + "\n"
	if _, err := f.WriteString(removal); err != nil {
		t.Fatal(err)
	}
	f.Close()
	es, _, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Kind == api.FlightRuleInstruction && e.Gone == "" {
			t.Errorf("%s still in after the removal notice", e.Source)
		}
	}

	if err := os.WriteFile(filepath.Join(home, "AGENTS.override.md"), []byte("Override.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	es, _, err = NewSession(NewCodex(rollout, home), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var unknown []string
	for _, e := range es {
		if e.Kind == api.FlightRuleInstruction && e.Attribution == api.FlightUnknown {
			unknown = append(unknown, e.Digest)
		}
	}
	// Both renders of the global file, and the nested file's old text.
	if len(unknown) != 3 {
		t.Errorf("with an override file: %d unknown entries, want 3", len(unknown))
	}
	_ = root
}

// TestSplitAgents: a file's text counts only on part boundaries; Codex's separators are no
// part of any entry.
func TestSplitAgents(t *testing.T) {
	files := []codexFile{{path: "/g", text: "x"}, {path: "/p", text: "Project rule.\n"}}
	block := "box x" + codexProjectDocSep + "Project rule.\n\n\nmore"
	parts, missed := splitAgents(block, files, "")
	var got []string
	for _, p := range parts {
		name := "?"
		if p.file != nil {
			name = p.file.path
		}
		got = append(got, name+"="+block[p.start:p.end])
	}
	want := []string{"?=box x", "/p=Project rule.\n", "?=more"}
	if !slices.Equal(got, want) || !slices.Equal(missed, []string{"/g"}) {
		t.Errorf("parts %q, missed %q", got, missed)
	}
}
