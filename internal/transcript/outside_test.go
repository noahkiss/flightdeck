package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeOutside(t *testing.T, path, data string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// claudeLines is a hand-written Claude Code transcript: a command wrapper, a meta line, the
// typed prompt, a tool result. The first record's cwd is a subfolder; the transcript's folder
// is named after /work/demo.
const claudeLines = `{"type":"summary","summary":"x"}
{"type":"user","cwd":"/work/demo/sub","isMeta":true,"entrypoint":"cli","message":{"role":"user","content":"<local-command-caveat>x</local-command-caveat>"}}
{"type":"user","cwd":"/work/demo","entrypoint":"cli","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"user","cwd":"/work/demo","entrypoint":"cli","message":{"role":"user","content":[{"type":"text","text":"  Add a greeting to the demo script.\nAnd a second line."}]}}
{"type":"user","cwd":"/work/demo","entrypoint":"cli","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}
`

const codexLines = `{"timestamp":"2026-01-01T10:00:00.000Z","type":"session_meta","payload":{"id":"019a0000-0000-7000-8000-00000000000a","cwd":"/work/codex"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n</environment_context>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the failing test"}]}}
`

func TestOutsideClaude(t *testing.T) {
	cfg := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	now := time.Now()
	writeOutside(t, filepath.Join(cfg, "projects", ClaudeProjectDir("/work/demo"), id+".jsonl"), claudeLines, now)
	c, err := ClaudeConversation(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Harness != "claude" || c.ID != id || c.Cwd != "/work/demo" || c.Title != "Add a greeting to the demo script." || c.SDK {
		t.Fatalf("conversation: %+v", c)
	}
	if _, err := ClaudeConversation(cfg, "22222222-0000-4000-8000-000000000000"); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("missing id: %v", err)
	}
	if _, err := ClaudeConversation(cfg, "../x"); err == nil || errors.Is(err, ErrNoTranscript) {
		t.Fatalf("bad id: %v", err)
	}
	// A folder named after none of the cwds: the first cwd.
	writeOutside(t, filepath.Join(cfg, "projects", "-elsewhere", "33333333-0000-4000-8000-000000000000.jsonl"), claudeLines, now)
	if c, _ := ClaudeConversation(cfg, "33333333-0000-4000-8000-000000000000"); c.Cwd != "/work/demo/sub" {
		t.Fatalf("first cwd: %+v", c)
	}
}

func TestOutsideProjectDir(t *testing.T) {
	if got := ClaudeProjectDir("/srv/a.b/my_repo"); got != "-srv-a-b-my-repo" {
		t.Fatalf("got %q", got)
	}
}

func TestOutsideCodex(t *testing.T) {
	home := t.TempDir()
	id := "019a0000-0000-7000-8000-00000000000a"
	writeOutside(t, filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T10-00-00-"+id+".jsonl"), codexLines, time.Now())
	c, err := CodexConversation(home, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Harness != "codex" || c.ID != id || c.Cwd != "/work/codex" || c.Title != "Fix the failing test" {
		t.Fatalf("conversation: %+v", c)
	}
	if _, err := CodexConversation(home, "019a0000-0000-7000-8000-00000000000b"); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("missing: %v", err)
	}
}

// The list is newest first, honours its limit, and leaves out claude -p transcripts.
func TestOutsideRecent(t *testing.T) {
	cfg := t.TempDir()
	base := time.Now().Add(-time.Hour)
	dir := filepath.Join(cfg, "projects", ClaudeProjectDir("/work/demo"))
	for i, id := range []string{"a1", "a2", "a3"} {
		writeOutside(t, filepath.Join(dir, id+".jsonl"), claudeLines, base.Add(time.Duration(i)*time.Minute))
	}
	writeOutside(t, filepath.Join(dir, "sdk1.jsonl"),
		`{"type":"user","cwd":"/work/demo","entrypoint":"sdk-cli","message":{"role":"user","content":"hi"}}`+"\n", base.Add(time.Hour))
	writeOutside(t, filepath.Join(dir, "empty.jsonl"), `{"type":"summary"}`+"\n", base.Add(time.Hour))
	got, err := RecentClaude(cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a3" || got[1].ID != "a2" {
		t.Fatalf("recent: %+v", got)
	}
	home := t.TempDir()
	writeOutside(t, filepath.Join(home, "sessions", "2026", "01", "01", "rollout-x-019a0000-0000-7000-8000-00000000000a.jsonl"), codexLines, base)
	writeOutside(t, filepath.Join(home, "sessions", "2026", "01", "01", "rollout-y.jsonl"), "not json\n", base.Add(time.Minute))
	cx, err := RecentCodex(home, 10)
	if err != nil || len(cx) != 1 || cx[0].ID != "019a0000-0000-7000-8000-00000000000a" {
		t.Fatalf("codex recent: %+v %v", cx, err)
	}
}

func TestOutsideTitle(t *testing.T) {
	long := strings.Repeat("é", 100)
	if got := title(long); len([]rune(got)) != 80 || !strings.HasSuffix(got, "…") {
		t.Fatalf("cut: %q", got)
	}
	if title("  <x>  ") != "" || title("\n\n") != "" {
		t.Fatal("wrapper or blank kept")
	}
}

func TestOutsideClaudeRegistry(t *testing.T) {
	cfg := t.TempDir()
	writeOutside(t, filepath.Join(cfg, "sessions", "100.json"), `{"pid":100,"sessionId":"s1","cwd":"/w"}`, time.Now())
	writeOutside(t, filepath.Join(cfg, "sessions", "bad.json"), `{`, time.Now())
	writeOutside(t, filepath.Join(cfg, "sessions", "0.json"), `{"pid":0,"sessionId":"s2"}`, time.Now())
	got := ClaudeRegistry(cfg)
	if len(got) != 1 || got[0].PID != 100 || got[0].SessionID != "s1" {
		t.Fatalf("registry: %+v", got)
	}
}
