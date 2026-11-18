package flightrules

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/instructions"
	"github.com/noahkiss/flightdeck/internal/opencode"
)

// ocServer serves the opencode fixture as an opencode 2.3 server would. mcp may be changed
// between calls.
type ocServer struct {
	mu       sync.Mutex
	fixture  map[string]json.RawMessage
	messages []json.RawMessage
}

func (s *ocServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := map[string]string{"/api/location": "location", "/api/config": "config", "/api/skill": "skill", "/api/mcp": "mcp"}[r.URL.Path]
	if key != "" {
		w.Write(s.fixture[key])
		return
	}
	if r.URL.Path != "/api/session/ses_01/message" {
		http.NotFound(w, r)
		return
	}
	// Newest first, a page at a time; the cursor is an offset from the newest.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	off, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	var page opencode.MessagePage
	for i := len(s.messages) - 1 - off; i >= 0 && len(page.Data) < limit; i-- {
		page.Data = append(page.Data, s.messages[i])
	}
	if off+len(page.Data) < len(s.messages) {
		page.Cursor.Next = strconv.Itoa(off + len(page.Data))
	}
	json.NewEncoder(w).Encode(page)
}

// ocTree writes the files the fixture's discovery finds and starts a server for the fixture.
func ocTree(t *testing.T) (root, state string, srv *ocServer, src *Opencode) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(root, "state")
	files := map[string]string{
		filepath.Join(root, "config", "opencode", "AGENTS.md"): "Global opencode rules.\n",
		filepath.Join(root, "home", ".claude", "CLAUDE.md"):    "Not read: the global AGENTS.md exists.\n",
		filepath.Join(root, "proj", "AGENTS.md"):               "Project rule.\n",
		filepath.Join(root, "proj", "CLAUDE.md"):               "Not read: AGENTS.md is beside it.\n",
		filepath.Join(root, "proj", "sub", "CLAUDE.md"):        "Sub rule from CLAUDE.md.\n",
		filepath.Join(root, "proj", "docs", "a.md"):            "Doc a.\n",
		filepath.Join(state, "instructions", "opencode.md"):    renderedText(codexRenderNow, "2026-01-01T09:00:00Z", "Rendered rules.\n"),
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("testdata/opencode-2.3.2.json")
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.NewReplacer("{{ROOT}}", root, "{{STATE}}", state).Replace(string(data)))
	srv = &ocServer{}
	if err := json.Unmarshal(data, &srv.fixture); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(srv.fixture["messages"], &srv.messages); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	c, err := opencode.New(hs.URL, "pw")
	if err != nil {
		t.Fatal(err)
	}
	running := true
	src = NewOpencode("ses_01", func() (OpencodeAPI, bool) { return c, running }, filepath.Join(root, "home"), filepath.Join(root, "config", "opencode"))
	src.now = func() time.Time { return time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { running = false })
	return root, state, srv, src
}

// TestOpencodeFixture reads the hand-written 2.3.2 fixture: the messages that carry nested
// instruction files and invoked skills (recorded), a change notice and a nameless skill
// (unknown), then the files opencode's discovery finds and the server's skill and MCP lists
// (computed, or a placeholder where the server gives no text). A later read marks what left
// gone and adds what is new (spec 11.2, 11.4).
func TestOpencodeFixture(t *testing.T) {
	root, state, srv, src := ocTree(t)
	s := NewSession(src, nil)
	entries, warnings, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	const now = "2026-01-01T11:00:00Z"
	ms := func(sec string) string { return "2026-01-01T10:00:" + sec + "Z" }
	listing := "- demo: Does a demo thing. (" + p("proj", ".opencode", "skill", "demo", "SKILL.md") + ")\n" +
		"- opencode: Answers questions about opencode. (skill://opencode/SKILL.md)"
	wants := []want{
		{kind: "instruction", scope: "project", source: p("proj", "sub", "deep", "AGENTS.md"), attribution: "recorded", first: ms("02"), last: ms("02"), text: "Deep rules.\n", note: "folder"},
		{kind: "skill", scope: "skill", source: "release-notes", attribution: "recorded", first: ms("04"), last: ms("04"), text: "Skill body."},
		{kind: "instruction", scope: "session", source: "instructions changed", attribution: "unknown", first: ms("05"), last: ms("05"), text: "The instructions changed:\nInstructions from: " + p("proj", "AGENTS.md") + "\nProject rule, edited.\n", note: "core/instructions"},
		{kind: "skill", scope: "skill", source: "skill", attribution: "unknown", first: ms("06"), last: ms("06"), text: "A skill body with no name.", note: "without its name"},
		{kind: "instruction", scope: "project", source: p("proj", "sub", "x", "AGENTS.md"), attribution: "recorded", first: ms("07"), last: ms("07"), text: "X rules."},
		{kind: "instruction", scope: "project", source: p("proj", "sub", "y", "AGENTS.md"), attribution: "recorded", first: ms("07"), last: ms("07"), text: "Y rules.\n"},
		{kind: "instruction", scope: "user", source: p("config", "opencode", "AGENTS.md"), attribution: "computed", first: now, last: now, text: "Global opencode rules.\n"},
		{kind: "instruction", scope: "project", source: p("proj", "sub", "CLAUDE.md"), attribution: "computed", first: now, last: now, text: "Sub rule from CLAUDE.md.\n"},
		{kind: "instruction", scope: "project", source: p("proj", "AGENTS.md"), attribution: "computed", first: now, last: now, text: "Project rule.\n"},
		{kind: "instruction", scope: "user", source: filepath.Join(state, "instructions", "opencode.md"), attribution: "computed", first: now, last: now, text: renderedText(codexRenderNow, "2026-01-01T09:00:00Z", "Rendered rules.\n")},
		{kind: "instruction", scope: "project", source: p("proj", "docs", "a.md"), attribution: "computed", first: now, last: now, text: "Doc a.\n"},
		{kind: "instruction", source: "https://example.com/rules.md", attribution: "unknown", first: now, last: now, note: "fetches this address"},
		{kind: "skills", scope: "skill", source: "skill listing", attribution: "computed", first: now, last: now, text: listing, note: "2 skills"},
		{kind: "mcp", scope: "mcp", source: "docs", attribution: "unknown", first: now, last: now, note: "does not return"},
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

	// A poll that finds the same files and lists adds nothing and moves nothing.
	again, _, err := s.List(context.Background())
	if err != nil || !slices.EqualFunc(again, entries, func(a, b api.FlightRule) bool {
		return a.ID == b.ID && a.Count == b.Count && a.LastSeen == b.LastSeen && a.Gone == b.Gone
	}) {
		t.Errorf("second poll changed the ledger: %v", err)
	}

	// The project file changes and the MCP server goes: the old text and the server are gone,
	// the new text is a new entry; a new message is read.
	if err := os.WriteFile(p("proj", "AGENTS.md"), []byte("Project rule, edited.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.fixture["mcp"] = json.RawMessage(`{"data":[]}`)
	srv.messages = append(srv.messages, json.RawMessage(`{"id":"msg_09","type":"skill","time":{"created":1767261609000},"name":"later","text":"Later skill."}`))
	srv.mu.Unlock()
	entries, _, err = s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gone := map[string]string{}
	for _, e := range entries {
		if e.Gone != "" {
			gone[e.Source+" "+e.Digest[:min(8, len(e.Digest))]] = e.Gone
		}
	}
	if len(entries) != len(wants)+2 || len(gone) != 2 || gone[p("proj", "AGENTS.md")+" "+Digest("Project rule.\n")[:8]] != now || gone["docs "] != now {
		t.Errorf("after changes: %d entries, gone %v", len(entries), gone)
	}
	if e := entries[len(entries)-2]; e.Source != "later" || e.Attribution != api.FlightRecorded {
		t.Errorf("new message: %+v", e)
	}
	if e := entries[len(entries)-1]; e.Source != p("proj", "AGENTS.md") || e.Digest != Digest("Project rule, edited.\n") {
		t.Errorf("new file text: %+v", e)
	}
	// The old text of the file is no longer on disk: its text is not served.
	if _, _, _, err := s.Text(context.Background(), entries[8].ID); err != ErrNoEntry {
		t.Errorf("changed file's old text: %v", err)
	}
}

// TestOpencodeStaleAndOffline: the rendered file opencode loads names its configured sources
// and is stale only when the render record holds another digest. With the server gone, the
// ledger stays as it was, with a warning.
func TestOpencodeStaleAndOffline(t *testing.T) {
	_, state, _, src := ocTree(t)
	target := filepath.Join(state, "instructions", "opencode.md")
	writeRenderRecord(t, state, instructions.Opencode, target, codexRenderOld, "/src/agents.md")
	s := NewSession(src, Rendered(state, instructions.Opencode)).CheckStale(Current(state, instructions.Opencode))
	es, _, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var stale []string
	for _, e := range es {
		if e.Stale {
			stale = append(stale, e.Source)
		}
		if e.Source == target && len(e.ConfiguredSources) != 0 {
			t.Errorf("another render's record named sources: %v", e.ConfiguredSources)
		}
	}
	if !slices.Equal(stale, []string{target}) {
		t.Errorf("stale %v", stale)
	}
	writeRenderRecord(t, state, instructions.Opencode, target, codexRenderNow, "/src/agents.md")
	es, _, _ = s.List(context.Background())
	for _, e := range es {
		if e.Stale {
			t.Errorf("%s stale with the current render", e.Source)
		}
	}

	src.client = func() (OpencodeAPI, bool) { return nil, false }
	after, warnings, err := s.List(context.Background())
	if err != nil || len(after) != len(es) || len(warnings) != 1 || !strings.Contains(warnings[0], "not running") {
		t.Errorf("offline: %d entries, %q, %v", len(after), warnings, err)
	}
}

// TestSplitInstructionBlocks: a text that does not start with the first path's block is not
// split.
func TestSplitInstructionBlocks(t *testing.T) {
	if _, ok := splitInstructionBlocks("Preamble\nInstructions from: /a\nA", []string{"/a"}); ok {
		t.Error("split a text with a preamble")
	}
	if got, ok := splitInstructionBlocks("Instructions from: /a\nA\n\nInstructions from: /b\nB", []string{"/a", "/b"}); !ok || !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("%q %v", got, ok)
	}
}
