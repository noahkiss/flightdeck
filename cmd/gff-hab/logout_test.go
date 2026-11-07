package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeClearing stands in for claude: like fakeResumable, then each time $RESUME_NEXT appears it
// reports the id written there, as Claude Code does after /clear, and removes the file.
const fakeClearing = `#!/bin/sh
echo "claude $*" >> "$RESUME_LOG"
printf '\033]7501;state=idle:app=claude\033\\'
printf '{"session_id":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"startup"}' "$RESUME_ID" "$PWD" | "$RESUME_HAB" hook claude SessionStart
while :; do
  if [ -f "$RESUME_NEXT" ]; then
    id=$(cat "$RESUME_NEXT"); rm -f "$RESUME_NEXT"
    printf '{"session_id":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"clear"}' "$id" "$PWD" | "$RESUME_HAB" hook claude SessionStart
    echo "cleared $id"
  fi
  sleep 0.05
done
`

// TestLogoutCheckpoint is the logout, restart and shutdown case (hab-holder.md 2.11): the init
// system sends SIGTERM to the holder and to gff-hab at the same moment, mid-work. A session was
// renamed and its agent took a new native id just before. After the reboot (a new holder, a new
// gff-hab, the old registry gone), the structure has the new name, both sessions show exited,
// nothing started on its own, and session.resume starts the agent with the newest id. The init
// system orders nothing, so each order is tried: both at once, the holder 1 s first (gff-hab sees
// every program end), gff-hab 1 s first.
func TestLogoutCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name                string
		holderLead, habLead time.Duration
	}{{"together", 0, 0}, {"holder-first", time.Second, 0}, {"hab-first", 0, time.Second}} {
		t.Run(tc.name, func(t *testing.T) { logoutCheckpoint(t, tc.holderLead, tc.habLead) })
	}
}

func logoutCheckpoint(t *testing.T, holderLead, habLead time.Duration) {
	e := newHolderEnv(t, 0)
	fake := shortDir(t)
	writeFile(t, filepath.Join(fake, "claude"), fakeClearing)
	if err := os.Chmod(filepath.Join(fake, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(e.work, "argv.log")
	next := filepath.Join(e.work, "next")
	e.env = []string{"PATH=" + fake + string(os.PathListSeparator) + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + shortDir(t),
		"RESUME_LOG=" + log, "RESUME_HAB=" + bin, "RESUME_ID=first-id", "RESUME_NEXT=" + next}
	e.serve()
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	var agent, shell api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "agent", Harness: "claude", Cwd: e.work}, &agent); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "watch", Cwd: e.work,
		Argv: []string{"/bin/sh", "-c", "exec sleep 600"}}, &shell); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the first id in the book", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.state, resumeFile))
		return strings.Contains(string(b), "first-id")
	})

	// Mid-work: a new native id and a rename, each seen by gff-hab, then the signal at once.
	placeFile(t, next, "second-id")
	waitUntil(t, "the agent's new id", func() bool {
		return agentStates(t, e.sock)[agent.Pane].Session == "second-id"
	})
	var empty api.Empty
	if err := c.Call(ctx, "session.rename", api.SessionRenameParams{Session: shell.Session, Name: "watch-2"}, &empty); err != nil {
		t.Fatal(err)
	}
	if habLead > 0 {
		e.srv.cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(habLead)
	}
	e.holder.Process.Signal(syscall.SIGTERM)
	time.Sleep(holderLead)
	e.srv.cmd.Process.Signal(syscall.SIGTERM)
	for what, done := range map[string]<-chan struct{}{"the holder": e.holderDone, "gff-hab": waitErr(e.srv.done)} {
		select {
		case <-done:
		case <-time.After(scaleTimeout(15 * time.Second)):
			t.Fatalf("%s did not exit after SIGTERM", what)
		}
	}
	e.srv.stopped = true
	if m, _ := filepath.Glob(filepath.Join(e.hdir, "*.json")); len(m) != 0 {
		t.Fatalf("registry left after the holder's shutdown: %v", m)
	}

	// The reboot.
	startHolder(t, e.hdir, "two")
	e.serve()
	c = e.control()
	waitSession(t, c, agent.Session, "the agent exited", func(s api.Session) bool { return s.State == api.SessionStateExited })
	if got := waitSession(t, c, shell.Session, "the shell exited", func(s api.Session) bool { return s.State == api.SessionStateExited }); got.Name != "watch-2" {
		t.Fatalf("shell name %q after the reboot", got.Name)
	}
	time.Sleep(300 * time.Millisecond)
	if got := readLines(t, log); len(got) != 1 {
		t.Fatalf("claude starts %q, want one: nothing resumes on its own", got)
	}
	if _, err := resume(t, c, agent.Session); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitUntil(t, "claude started again", func() bool { return len(readLines(t, log)) == 2 })
	if got := readLines(t, log)[1]; got != "claude --session-id second-id" {
		t.Fatalf("resumed with %q, want the id from before the signal", got)
	}
}

// waitErr turns a process's result channel into a done channel. It does not consume the result
// for later readers: the value is put back.
func waitErr(ch chan error) <-chan struct{} {
	d := make(chan struct{})
	go func() { err := <-ch; ch <- err; close(d) }()
	return d
}

// The watchdog logs and exits once its deadline passes, and never when stopped first.
func TestShutdownWatchdog(t *testing.T) {
	codes := make(chan int, 1)
	lines := make(chan string, 1)
	shutdownWatchdog(20*time.Millisecond, func(f string, a ...any) { lines <- fmt.Sprintf(f, a...) }, func(c int) { codes <- c })
	select {
	case c := <-codes:
		if line := <-lines; c != 1 || !strings.Contains(line, "did not finish") {
			t.Fatalf("exit %d, log %q", c, line)
		}
	case <-time.After(scaleTimeout(5 * time.Second)):
		t.Fatal("no exit at the deadline")
	}
	w := shutdownWatchdog(20*time.Millisecond, t.Logf, func(int) { t.Error("exit after stop") })
	w.Stop()
	time.Sleep(60 * time.Millisecond)
}

// flush writes a pending agent state at once, with the book still open.
func TestResumeBookFlush(t *testing.T) {
	d := t.TempDir()
	b := openResumeBook(d, func(string) (string, bool) { return "s1", true }, t.Logf)
	defer b.Close()
	b.mu.Lock()
	b.pend["p1"] = api.AgentState{Pane: "p1", Harness: "claude", Session: "native-2"}
	b.mu.Unlock()
	b.flush()
	data, err := os.ReadFile(filepath.Join(d, resumeFile))
	if err != nil || !strings.Contains(string(data), "native-2") {
		t.Fatalf("after flush: %s, %v", data, err)
	}
}
