package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// adoptTranscript is a hand-written Claude Code transcript that ran in dir.
func adoptTranscript(dir string) string {
	return `{"type":"user","cwd":"` + dir + `","entrypoint":"cli","message":{"role":"user","content":"Tidy the demo"}}` + "\n"
}

// adoptWrite writes a file, making its folders.
func adoptWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, s)
}

// testAdopter is an adopter over temporary harness folders, with fake process readers.
func testAdopter(t *testing.T, env map[string]string, procs []outsideProc, alive map[int]bool) *adopter {
	t.Helper()
	book := openResumeBook(t.TempDir(), func(p string) (string, bool) { return p, true }, t.Logf)
	t.Cleanup(book.Close)
	return &adopter{book: book, getenv: func(k string) string { return env[k] },
		procs: func(context.Context) ([]outsideProc, error) { return procs, nil },
		alive: func(pid int) bool { return alive[pid] }, self: 1, logf: t.Logf}
}

// TestAdoptPlan: the harness found by id, an id under both harnesses, a bad id, opencode, a
// folder gone and the cwd override.
func TestAdoptPlan(t *testing.T) {
	cfg, home, work := t.TempDir(), t.TempDir(), t.TempDir()
	env := map[string]string{"CLAUDE_CONFIG_DIR": cfg, "CODEX_HOME": home}
	adoptWrite(t, filepath.Join(cfg, "projects", transcript.ClaudeProjectDir(work), "c1.jsonl"), adoptTranscript(work))
	rollout := func(id, dir string) {
		adoptWrite(t, filepath.Join(home, "sessions", "2026", "01", "02", "rollout-x-"+id+".jsonl"),
			`{"type":"session_meta","payload":{"id":"`+id+`","cwd":"`+dir+`"}}`+"\n")
	}
	rollout("x1", work)
	rollout("c1", work) // the same id under both
	d := testAdopter(t, env, nil, nil)

	if c, err := d.find("", "x1"); err != nil || c.Harness != "codex" || c.Cwd != work {
		t.Fatalf("x1: %+v %v", c, err)
	}
	if c, err := d.find("claude", "c1"); err != nil || c.Harness != "claude" || c.Cwd != work {
		t.Fatalf("c1 as claude: %+v %v", c, err)
	}
	for _, tc := range []struct {
		harness, id string
		want        api.ErrorKind
	}{
		{"", "c1", api.ErrorKindInvalid},
		{"", "../x", api.ErrorKindInvalid},
		{"", "nope", api.ErrorKindNotFound},
		{"opencode", "c1", api.ErrorKindUnavailable},
		{"pi", "c1", api.ErrorKindInvalid},
	} {
		if _, err := d.find(tc.harness, tc.id); errKind(err) != tc.want {
			t.Errorf("%s %s: %v, want %s", tc.harness, tc.id, err, tc.want)
		}
	}

	gone := filepath.Join(work, "gone")
	rollout("g1", gone)
	_, err := d.Adopt(context.Background(), api.SessionAdoptParams{ID: "g1"})
	if errKind(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), gone) {
		t.Fatalf("folder gone: %v", err)
	}
	_, err = d.Adopt(context.Background(), api.SessionAdoptParams{ID: "g1", Cwd: "relative"})
	if errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("relative cwd: %v", err)
	}
}

// TestAdoptLive: a session in the book, Claude Code's registry with a live pid, and a process
// whose arguments hold the id each refuse; a dead pid, gff-hab itself and a grep do not.
func TestAdoptLive(t *testing.T) {
	cfg, work := t.TempDir(), t.TempDir()
	env := map[string]string{"CLAUDE_CONFIG_DIR": cfg, "CODEX_HOME": t.TempDir()}
	adoptWrite(t, filepath.Join(cfg, "projects", transcript.ClaudeProjectDir(work), "c1.jsonl"), adoptTranscript(work))
	adoptWrite(t, filepath.Join(cfg, "sessions", "200.json"), `{"pid":200,"sessionId":"c1"}`)
	d := testAdopter(t, env, []outsideProc{
		{1, "/usr/local/bin/claude --resume c1"}, // the hab itself
		{7, "/opt/bin/gff-hab adopt -harness claude c1"},
		{8, "grep c1 notes.txt"},
	}, map[int]bool{200: false})
	if pid := d.runner(d.view(context.Background()), "claude", "c1"); pid != 0 {
		t.Fatalf("nothing runs it, got pid %d", pid)
	}
	d.alive = func(pid int) bool { return pid == 200 }
	_, err := d.Adopt(context.Background(), api.SessionAdoptParams{ID: "c1"})
	if errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "pid 200") {
		t.Fatalf("registry: %v", err)
	}
	d.alive = func(int) bool { return false }
	d.procs = func(context.Context) ([]outsideProc, error) {
		return []outsideProc{{42, "node /opt/lib/claude-code/cli.js --resume c1"}}, nil
	}
	if _, err := d.Adopt(context.Background(), api.SessionAdoptParams{ID: "c1"}); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "pid 42") {
		t.Fatalf("argv: %v", err)
	}
	d.procs = func(context.Context) ([]outsideProc, error) { return nil, nil }
	d.book.started("s1", api.SessionCreateParams{Harness: "claude"})
	d.book.adopted("s1", "claude", "c1")
	if s, ok := d.book.holder("claude", "c1"); !ok || s != "s1" {
		t.Fatalf("holder: %q %v", s, ok)
	}
	if _, ok := d.book.holder("codex", "c1"); ok {
		t.Fatal("another harness's id held")
	}
}

func TestAdoptParseProcs(t *testing.T) {
	got := parseProcs([]byte("  1 /sbin/launchd\n 42 codex resume r1\nbad line\n 0 x\n"))
	if len(got) != 2 || got[1].pid != 42 || got[1].args != "codex resume r1" {
		t.Fatalf("procs: %+v", got)
	}
	if !runsConversation("codex resume r1", "codex", "r1") || runsConversation("codex resume r10", "codex", "r1") {
		t.Fatal("word match")
	}
}

// TestSessionAdopt: a Claude Code conversation started outside, with a hand-written transcript;
// the real holder and gff-hab with a stand-in claude. The list shows it; adopt starts claude
// --resume <id> in the recorded folder; a second adopt is refused naming the session; the list
// leaves it out; after the pane closes, session.resume starts it again; the transcript is
// unchanged. An id Claude Code's registry shows live is refused; an unknown id is not found.
func TestSessionAdopt(t *testing.T) {
	e := newHolderEnv(t, 0)
	fake := shortDir(t)
	writeFile(t, filepath.Join(fake, "claude"), fakeResumable)
	if err := os.Chmod(filepath.Join(fake, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgDir := shortDir(t)
	log := filepath.Join(e.work, "argv.log")
	const native = "5f0c2d9e-adopt-test"
	path := filepath.Join(cfgDir, "projects", transcript.ClaudeProjectDir(e.work), native+".jsonl")
	body := adoptTranscript(e.work)
	adoptWrite(t, path, body)
	// A conversation a process outside runs: this test's own pid is alive.
	const busy = "7a1d0000-adopt-busy"
	adoptWrite(t, filepath.Join(cfgDir, "projects", transcript.ClaudeProjectDir(e.work), busy+".jsonl"), body)
	adoptWrite(t, filepath.Join(cfgDir, "sessions", strconv.Itoa(os.Getpid())+".json"),
		`{"pid":`+strconv.Itoa(os.Getpid())+`,"sessionId":"`+busy+`"}`)
	e.env = []string{"PATH=" + fake + string(os.PathListSeparator) + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + cfgDir,
		"CODEX_HOME=" + shortDir(t), "RESUME_LOG=" + log, "RESUME_HAB=" + bin, "RESUME_ID=" + native}
	e.serve()
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()

	outside := func() map[string]api.OutsideConversation {
		var res api.OutsideConversations
		if err := c.Call(ctx, "session.outside", api.SessionOutsideParams{}, &res); err != nil {
			t.Fatal(err)
		}
		m := map[string]api.OutsideConversation{}
		for _, o := range res.Conversations {
			m[o.ID] = o
		}
		return m
	}
	list := outside()
	if o, ok := list[native]; !ok || o.Harness != "claude" || o.Cwd != e.work || o.Title != "Tidy the demo" || o.Live {
		t.Fatalf("outside: %+v", list)
	}
	if !list[busy].Live {
		t.Fatalf("busy not live: %+v", list[busy])
	}

	var res api.SessionAdopted
	if err := c.Call(ctx, "session.adopt", api.SessionAdoptParams{ID: busy}, &res); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("adopt of a live conversation: %v", err)
	}
	if err := c.Call(ctx, "session.adopt", api.SessionAdoptParams{ID: "00000000-none"}, &res); errKind(err) != api.ErrorKindNotFound {
		t.Fatalf("adopt of an unknown id: %v", err)
	}
	if err := c.Call(ctx, "session.adopt", api.SessionAdoptParams{ID: native, Name: "moved"}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Harness != "claude" || res.Cwd != e.work || res.Pane == "" {
		t.Fatalf("adopted: %+v", res)
	}
	waitSession(t, c, res.Session, "running", func(s api.Session) bool { return s.State == api.SessionStateRunning && s.Name == "moved" })
	waitUntil(t, "claude started", func() bool { b, _ := os.ReadFile(log); return strings.Count(string(b), "\n") == 1 })
	if got := readLines(t, log)[0]; got != "claude --resume "+native {
		t.Fatalf("start %q", got)
	}
	var again api.SessionAdopted
	if err := c.Call(ctx, "session.adopt", api.SessionAdoptParams{ID: native}, &again); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("second adopt: %v", err)
	}
	if _, ok := outside()[native]; ok {
		t.Fatal("an adopted conversation still listed")
	}

	var empty api.Empty
	if err := c.Call(ctx, "pane.close", api.PaneRef{Pane: res.Pane}, &empty); err != nil {
		t.Fatal(err)
	}
	waitSession(t, c, res.Session, "exited", func(s api.Session) bool { return s.State == api.SessionStateExited })
	if _, err := resume(t, c, res.Session); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitUntil(t, "claude started again", func() bool { b, _ := os.ReadFile(log); return strings.Count(string(b), "\n") == 2 })
	if got := readLines(t, log)[1]; got != "claude --resume "+native {
		t.Fatalf("resume start %q", got)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != body {
		t.Fatalf("transcript changed: %q %v", b, err)
	}
}
