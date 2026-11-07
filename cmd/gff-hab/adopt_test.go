package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	// A process list that fails proves nothing free: adopt refuses rather than guess.
	d.procs = func(context.Context) ([]outsideProc, error) { return nil, context.DeadlineExceeded }
	if _, err := d.Adopt(context.Background(), api.SessionAdoptParams{ID: "c1"}); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("ps failed: %v", err)
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

// TestAdoptReusedPid: a registry entry whose pid now names another process (its recorded start
// differs from the pid's start) frees the conversation; the same start, a start that does not
// read and an entry with no start keep it live.
func TestAdoptReusedPid(t *testing.T) {
	cfg := t.TempDir()
	env := map[string]string{"CLAUDE_CONFIG_DIR": cfg, "CODEX_HOME": t.TempDir()}
	adoptWrite(t, filepath.Join(cfg, "sessions", "300.json"), `{"pid":300,"sessionId":"c1","procStart":"Fri Nov  6 05:36:59 2026"}`)
	adoptWrite(t, filepath.Join(cfg, "sessions", "301.json"), `{"pid":301,"sessionId":"c2"}`)
	adoptWrite(t, filepath.Join(cfg, "sessions", "302.json"), `{"pid":302,"sessionId":"c3","procStart":"154671"}`)
	d := testAdopter(t, env, nil, map[int]bool{300: true, 301: true, 302: true})
	start := map[int]string{300: "Sat Nov  7 09:00:00 2026", 301: "Sat Nov  7 09:00:00 2026", 302: "154671"}
	d.started = func(pid int) (string, bool) { s, ok := start[pid]; return s, ok }
	ctx := context.Background()
	if err := d.outsideLive(ctx, "claude", "c1"); err != nil {
		t.Fatalf("reused pid still live: %v", err)
	}
	if err := d.outsideLive(ctx, "claude", "c2"); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("entry with no start: %v", err)
	}
	if err := d.outsideLive(ctx, "claude", "c3"); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "pid 302") {
		t.Fatalf("same start tick: %v", err)
	}
	// The same start, written with other spacing, is the same process.
	start[300] = "Fri Nov 6 05:36:59 2026"
	if err := d.outsideLive(ctx, "claude", "c1"); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("same start: %v", err)
	}
	// A start that does not read never frees a conversation.
	delete(start, 300)
	if err := d.outsideLive(ctx, "claude", "c1"); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("start unread: %v", err)
	}
	// A process whose arguments hold the id stays live whatever the registry says.
	start[300] = "Sat Nov  7 09:00:00 2026"
	d.procs = func(context.Context) ([]outsideProc, error) { return []outsideProc{{43, "claude --resume c1"}}, nil }
	if err := d.outsideLive(ctx, "claude", "c1"); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "pid 43") {
		t.Fatalf("argv: %v", err)
	}
}

// TestAdoptStartsDiffer: only two starts of one kind that both parse and differ free a
// conversation; another locale, another clock or a local-time lstart never do.
func TestAdoptStartsDiffer(t *testing.T) {
	local := time.Date(2026, 11, 6, 5, 36, 59, 0, time.UTC).In(time.Local).Format(time.ANSIC)
	for _, tc := range []struct {
		recorded, now string
		want          bool
	}{
		{"Fri Nov  6 05:36:59 2026", "Fri Nov  6 05:36:59 2026", false},
		{"Fri Nov 6 05:36:59 2026", "Fri Nov  6 05:36:59 2026", false},
		{"Fri Nov  6 05:36:59 2026", "Sat Nov  7 09:00:00 2026", true},
		{local, "Fri Nov  6 05:36:59 2026", false},                     // recorded in local time
		{"Fr  6 Nov 05:36:59 2026", "Sat Nov  7 09:00:00 2026", false}, // another locale
		{"1793943420073", "Sat Nov  7 09:00:00 2026", false},           // epoch milliseconds
		{"154671", "154671", false},
		{"154671", "154672", true},
		{"154671", "Sat Nov  7 09:00:00 2026", false},
		{"Fri Nov  6 05:36:59 2026", "garbage", false},
	} {
		if got := startsDiffer(tc.recorded, tc.now); got != tc.want {
			t.Errorf("startsDiffer(%q, %q) = %v, want %v", tc.recorded, tc.now, got, tc.want)
		}
	}
}

// TestAdoptFork: a fork of the conversation is no second writer; a resume is.
func TestAdoptFork(t *testing.T) {
	for _, tc := range []struct {
		harness, args string
		want          bool
	}{
		{"claude", "claude --resume c1 --fork-session", false},
		{"claude", "claude --resume c1", true},
		{"codex", "codex --no-daemon fork c1", false},
		{"codex", "codex fork c1", false},
		{"codex", "codex resume c1", true},
		{"codex", "codex resume c1 fork", true},
	} {
		if got := runsConversation(tc.args, tc.harness, "c1"); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestAdoptProcfs: the process list from a hand-made procfs: arguments joined, kernel threads
// and other names skipped; an empty or missing root is an error, so ps is asked instead.
func TestAdoptProcfs(t *testing.T) {
	root := t.TempDir()
	adoptWrite(t, filepath.Join(root, "42", "cmdline"), "node\x00/opt/claude/cli.js\x00--resume\x00c1\x00")
	adoptWrite(t, filepath.Join(root, "2", "cmdline"), "") // a kernel thread
	adoptWrite(t, filepath.Join(root, "self", "cmdline"), "x\x00")
	adoptWrite(t, filepath.Join(root, "uptime"), "1 2")
	got, err := readProcfs(context.Background(), root)
	if err != nil || len(got) != 1 || got[0] != (outsideProc{42, "node /opt/claude/cli.js --resume c1"}) {
		t.Fatalf("procfs: %+v %v", got, err)
	}
	if _, err := readProcfs(context.Background(), t.TempDir()); err == nil {
		t.Fatal("an empty procfs read")
	}
	if _, err := readProcfs(context.Background(), filepath.Join(root, "none")); err == nil {
		t.Fatal("a missing procfs read")
	}
	if runtime.GOOS == "linux" {
		procs, err := listProcs(context.Background())
		if err != nil || !slices.ContainsFunc(procs, func(p outsideProc) bool { return p.pid == os.Getpid() }) {
			t.Fatalf("listProcs: %d procs, %v", len(procs), err)
		}
	}
}

// TestAdoptProcStart: the start read for this test's own process in the registry's form: on
// macOS ps's lstart in UTC, on Linux the start tick of /proc/<pid>/stat; and the stat parser.
func TestAdoptProcStart(t *testing.T) {
	for _, tc := range []struct {
		line, want string
		ok         bool
	}{
		{"42 (claude) S 1 42 42 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 9 0 154671 1000 10", "154671", true},
		{"42 (a) b (c) S 1 42 42 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 9 0 777 1000 10", "777", true},
		{"42 (claude) S 1 2 3", "", false},
		{"no paren", "", false},
	} {
		if got, ok := statStart([]byte(tc.line)); got != tc.want || ok != tc.ok {
			t.Errorf("%q: %q %v", tc.line, got, ok)
		}
	}
	got, ok := procStart(os.Getpid())
	var want string
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid()))
		cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("ps: %v", err)
		}
		want = strings.Join(strings.Fields(string(out)), " ")
		got = strings.Join(strings.Fields(got), " ")
	case "linux":
		b, err := os.ReadFile("/proc/self/stat")
		if err != nil {
			t.Fatal(err)
		}
		want, _ = statStart(b)
	default:
		if ok {
			t.Fatalf("start read on %s: %q", runtime.GOOS, got)
		}
		return
	}
	if !ok || got != want {
		t.Fatalf("procStart = %q %v, want %q", got, ok, want)
	}
	if _, ok := procStart(1 << 30); ok {
		t.Fatal("a pid that does not exist read a start")
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
	// The owner resumed the conversation in a plain terminal meanwhile (Claude Code's registry
	// names it with a live pid, this test's own): session.resume refuses a second writer.
	running := filepath.Join(cfgDir, "sessions", strconv.Itoa(os.Getpid())+".json")
	adoptWrite(t, running, `{"pid":`+strconv.Itoa(os.Getpid())+`,"sessionId":"`+native+`"}`)
	if _, err := resume(t, c, res.Session); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "outside the hab") {
		t.Fatalf("resume of a conversation live outside: %v", err)
	}
	if n := len(readLines(t, log)); n != 1 {
		t.Fatalf("a refused resume started claude: %d starts", n)
	}
	// A stale entry whose pid now names another process frees it. Its start is written in this
	// platform's form (a start tick on Linux, lstart on macOS): a start in another form keeps
	// the entry live.
	stale := "Thu Jan  1 00:00:00 1970"
	if now, ok := procStart(os.Getpid()); !ok {
		t.Skip("no process start on this platform")
	} else if _, err := strconv.ParseUint(now, 10, 64); err == nil {
		stale = "1"
	}
	adoptWrite(t, running, `{"pid":`+strconv.Itoa(os.Getpid())+`,"sessionId":"`+native+`","procStart":"`+stale+`"}`)
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
