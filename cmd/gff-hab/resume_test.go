package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// TestResumeStart: a harness seen with its id resumes it (Claude Code with no transcript starts
// again under the same id); a model the session was created with stays with its harness; with
// no id, the session starts as it was created; with nothing known there is no start.
func TestResumeStart(t *testing.T) {
	transcript := func(ok bool) func(string) bool { return func(string) bool { return ok } }
	cases := []struct {
		name string
		e    resumeEntry
		tr   func(string) bool
		want api.SessionCreateParams
		ok   bool
	}{
		{"claude with a transcript", resumeEntry{Harness: "claude", Model: "opus", Seen: "claude", Native: "s1"}, transcript(true),
			api.SessionCreateParams{Harness: "claude", Model: "opus", Argv: []string{"claude", "--resume", "s1"}}, true},
		{"claude with none yet", resumeEntry{Seen: "claude", Native: "s1"}, transcript(false),
			api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--session-id", "s1"}}, true},
		{"codex", resumeEntry{Seen: "codex", Native: "r9"}, nil,
			api.SessionCreateParams{Harness: "codex", Argv: []string{"codex", "resume", "r9"}}, true},
		{"opencode", resumeEntry{Seen: "opencode", Native: "ses_1"}, nil,
			api.SessionCreateParams{Harness: "opencode", Argv: []string{"opencode", "--session", "ses_1"}}, true},
		{"claude run in a shell", resumeEntry{Argv: []string{"/bin/zsh"}, Seen: "claude", Native: "s2"}, transcript(true),
			api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--resume", "s2"}}, true},
		{"model of another harness dropped", resumeEntry{Harness: "codex", Model: "gpt", Seen: "claude", Native: "s3"}, transcript(true),
			api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--resume", "s3"}}, true},
		{"no id: as created", resumeEntry{Harness: "claude", Model: "opus", Seen: "claude"}, nil,
			api.SessionCreateParams{Harness: "claude", Model: "opus"}, true},
		{"a shell", resumeEntry{Argv: []string{"/bin/sh", "-c", "make watch"}}, nil,
			api.SessionCreateParams{Argv: []string{"/bin/sh", "-c", "make watch"}}, true},
		{"nothing known", resumeEntry{}, nil, api.SessionCreateParams{}, false},
	}
	for _, c := range cases {
		got, ok, err := resumeStart(c.e, c.tr)
		if err != nil || ok != c.ok || got.Harness != c.want.Harness || got.Model != c.want.Model || !slices.Equal(got.Argv, c.want.Argv) {
			t.Errorf("%s: got %+v %v %v, want %+v %v", c.name, got, ok, err, c.want, c.ok)
		}
	}
}

// TestResumeBook: the start and the states seen are kept per session and read back after a
// reopen; a state with no id keeps the id known; another harness replaces it; a detection id
// with no adapter is ignored; a removed session is forgotten; a file that does not read is set
// aside.
func TestResumeBook(t *testing.T) {
	d := t.TempDir()
	var mu sync.Mutex
	panes := map[string]string{"p1": "s1", "p2": "s2"}
	sessionOf := func(p string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		s, ok := panes[p]
		return s, ok
	}
	b := openResumeBook(d, sessionOf, t.Logf)
	b.started("s1", api.SessionCreateParams{Argv: []string{"/bin/zsh"}})
	b.started("s2", api.SessionCreateParams{Harness: "claude", Model: "opus"})
	b.agent(api.AgentState{Pane: "p1", Harness: "claude", Session: "n1"})
	b.agent(api.AgentState{Pane: "p2", Harness: "aider", Session: "x"}) // no adapter
	b.agent(api.AgentState{Pane: "p9", Harness: "claude", Session: "lost"})
	e, _ := b.get("s1")
	if e.Seen != "claude" || e.Native != "n1" || !slices.Equal(e.Argv, []string{"/bin/zsh"}) {
		t.Fatalf("s1: %+v", e)
	}
	b.agent(api.AgentState{Pane: "p1", Harness: "claude"}) // a later state without the id
	if e, _ := b.get("s1"); e.Native != "n1" {
		t.Fatalf("s1 lost its id: %+v", e)
	}
	b.agent(api.AgentState{Pane: "p1", Harness: "codex"})
	if e, _ := b.get("s1"); e.Seen != "codex" || e.Native != "" {
		t.Fatalf("s1 after codex: %+v", e)
	}
	b.agent(api.AgentState{Pane: "p1", Harness: "codex", Session: "c1"})
	if e, _ := b.get("s2"); e.Seen != "" || e.Harness != "claude" || e.Model != "opus" {
		t.Fatalf("s2: %+v", e)
	}
	b.forget("s2")
	b.Close()

	b = openResumeBook(d, sessionOf, t.Logf)
	if e, ok := b.get("s1"); !ok || e.Seen != "codex" || e.Native != "c1" || !slices.Equal(e.Argv, []string{"/bin/zsh"}) {
		t.Fatalf("s1 after reopen: %+v %v", e, ok)
	}
	if _, ok := b.get("s2"); ok {
		t.Fatal("s2 kept after forget")
	}
	b.Close()

	if err := os.WriteFile(filepath.Join(d, resumeFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b = openResumeBook(d, sessionOf, t.Logf)
	defer b.Close()
	if _, ok := b.get("s1"); ok {
		t.Fatal("a bad file read")
	}
	if _, err := os.Stat(filepath.Join(d, resumeFile+".bad")); err != nil {
		t.Fatalf("bad file not set aside: %v", err)
	}
}

// fakeResumable stands in for claude in an ordinary session: it logs its arguments, reports
// idle over OSC 7501, sends a SessionStart hook with its native id, and waits.
const fakeResumable = `#!/bin/sh
echo "claude $*" >> "$RESUME_LOG"
printf '\033]7501;state=idle:app=claude\033\\'
printf '{"session_id":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"startup"}' "$RESUME_ID" "$PWD" | "$RESUME_HAB" hook claude SessionStart
exec sleep 600
`

// TestSessionResumeAfterHolderLoss: an ordinary Claude Code session and a shell session run;
// the holder dies for good and a new one runs; gff-hab restarts. Both sessions show exited and
// nothing started again on its own. session.resume starts Claude Code with --session-id while it
// has no transcript, then with --resume once it has one, in the session's folder, and the shell
// with the command it was created with. A running session refuses.
func TestSessionResumeAfterHolderLoss(t *testing.T) {
	e := newHolderEnv(t, 0)
	fake := shortDir(t)
	writeFile(t, filepath.Join(fake, "claude"), fakeResumable)
	if err := os.Chmod(filepath.Join(fake, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgDir := shortDir(t)
	log := filepath.Join(e.work, "argv.log")
	const native = "0b7e3c1a-resume-test"
	e.env = []string{"PATH=" + fake + string(os.PathListSeparator) + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + cfgDir,
		"RESUME_LOG=" + log, "RESUME_HAB=" + bin, "RESUME_ID=" + native}
	e.serve()
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var agent, shell api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "agent", Harness: "claude", Cwd: e.work}, &agent); err != nil {
		t.Fatal(err)
	}
	shellLog := filepath.Join(e.work, "shell.log")
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "watch", Cwd: e.work,
		Argv: []string{"/bin/sh", "-c", "pwd >> " + shellLog + "; exec sleep 600"}}, &shell); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "claude started", func() bool { b, _ := os.ReadFile(log); return strings.Count(string(b), "\n") == 1 })
	waitUntil(t, "the shell started", func() bool { b, _ := os.ReadFile(shellLog); return strings.Count(string(b), "\n") == 1 })
	waitUntil(t, "the native id in the book", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.state, resumeFile))
		return strings.Contains(string(b), native)
	})
	if _, err := resume(t, c, agent.Session); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("resume of a running session: %v", err)
	}

	e.holder.Process.Kill()
	<-e.holderDone
	// The running gff-hab may already have removed the dead holder's registry file as stale
	// (hab-holder.md 4.2); either way it must be gone before the new holder starts.
	if err := os.Remove(holderreg.Path(e.hdir, "one")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	startHolder(t, e.hdir, "two")
	e.restart(false)
	c = e.control()
	for _, s := range []string{agent.Session, shell.Session} {
		waitSession(t, c, s, "exited after the holder loss", func(s api.Session) bool { return s.State == api.SessionStateExited })
	}
	time.Sleep(300 * time.Millisecond)
	if got := readLines(t, log); len(got) != 1 {
		t.Fatalf("claude starts %q, want one: nothing resumes on its own", got)
	}

	res, err := resume(t, c, agent.Session)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Session != agent.Session || res.Pane == agent.Pane {
		t.Fatalf("resume: %+v", res)
	}
	waitSession(t, c, agent.Session, "running again", func(s api.Session) bool { return s.State == api.SessionStateRunning && s.Pane == res.Pane })
	waitUntil(t, "claude started again", func() bool { return len(readLines(t, log)) == 2 })
	if got := readLines(t, log)[1]; got != "claude --session-id "+native {
		t.Fatalf("second start %q", got)
	}

	if _, err := resume(t, c, shell.Session); err != nil {
		t.Fatalf("resume the shell: %v", err)
	}
	waitUntil(t, "the shell ran again", func() bool { b, _ := os.ReadFile(shellLog); return strings.Count(string(b), "\n") == 2 })
	if got := readLines(t, shellLog); got[1] != got[0] {
		t.Fatalf("the shell ran in %q, then %q", got[0], got[1])
	}

	// With a transcript, the next resume is --resume.
	proj := filepath.Join(cfgDir, "projects", "w")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(proj, native+".jsonl"), "{}\n")
	var empty api.Empty
	if err := c.Call(ctx, "pane.close", api.PaneRef{Pane: res.Pane}, &empty); err != nil {
		t.Fatal(err)
	}
	waitSession(t, c, agent.Session, "exited", func(s api.Session) bool { return s.State == api.SessionStateExited })
	if _, err := resume(t, c, agent.Session); err != nil {
		t.Fatalf("resume again: %v", err)
	}
	waitUntil(t, "claude started a third time", func() bool { return len(readLines(t, log)) == 3 })
	if got := readLines(t, log)[2]; got != "claude --resume "+native {
		t.Fatalf("third start %q", got)
	}
}

// An agent can report its native id before session.create records the start.
func TestResumeBookAgentBeforeStart(t *testing.T) {
	d := t.TempDir()
	b := openResumeBook(d, func(string) (string, bool) { return "s1", true }, t.Logf)
	b.agent(api.AgentState{Pane: "p1", Harness: "claude", Session: "native"})
	if e, _ := b.get("s1"); e.Native != "native" {
		t.Fatalf("agent state: %+v", e)
	}
	b.started("s1", api.SessionCreateParams{Harness: "claude", Model: "opus"})
	b.Close()
	b = openResumeBook(d, func(string) (string, bool) { return "s1", true }, t.Logf)
	defer b.Close()
	if e, _ := b.get("s1"); e.Native != "native" || e.Seen != "claude" || e.Model != "opus" {
		t.Fatalf("reopened: %+v", e)
	}
}

// Concurrent starts and agent states must all survive the final write.
func TestResumeBookConcurrentSaves(t *testing.T) {
	d := t.TempDir()
	b := openResumeBook(d, func(p string) (string, bool) { return p, true }, t.Logf)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("s%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.started(id, api.SessionCreateParams{Harness: "claude"})
			b.agent(api.AgentState{Pane: id, Harness: "claude", Session: id})
		}()
	}
	wg.Wait()
	b.Close()
	b = openResumeBook(d, func(p string) (string, bool) { return p, true }, t.Logf)
	defer b.Close()
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("s%d", i)
		if e, ok := b.get(id); !ok || e.Harness != "claude" || e.Native != id {
			t.Fatalf("%s: %+v %v", id, e, ok)
		}
	}
}
