package gffclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// TestResumeFromSidebar: Enter on an exited session in the sidebar resumes it (session.resume)
// and opens the new pane in a view; nothing resumed before the key. A refused resume says why
// on the bottom row and opens nothing.
func TestResumeFromSidebar(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	sess, pane, _ := a.AddSession("job")
	_, other, _ := a.AddSession("other")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the running rows", func(s string) bool { return line(s, "   job") > 0 && line(s, "   other") > 0 })
	a.ExitPane(pane)
	a.ExitPane(other)
	hst.waitScreen("the exited marks", func(s string) bool { return line(s, " _ job") > 0 && line(s, " _ other") > 0 })
	if n := a.Resumes(); n != 0 {
		t.Fatalf("%d resumes before any key", n)
	}

	// The rows are both exited; job sorts first.
	hst.typ("\r")
	hst.waitScreen("job running in a view", func(s string) bool {
		return line(s, "▸  job") > 0 && strings.Contains(s, " job · one") && !strings.Contains(s, "empty tab")
	})
	ss, _ := a.Session(sess)
	if ss.State != api.SessionStateRunning || ss.Pane == pane || a.Resumes() != 1 {
		t.Fatalf("after Enter: %+v, %d resumes", ss, a.Resumes())
	}

	a.FailResumes(control.Errorf(api.ErrorKindUnavailable, "session x: how it started is not known"))
	hst.typ(cmdKey + "w")
	hst.waitScreen("the sidebar", func(s string) bool { return line(s, " _ other") > 0 })
	hst.typ("j\r")
	hst.waitScreen("the refusal", func(s string) bool {
		return strings.Contains(s, "resume other on one:") && strings.Contains(s, "how it started is not known")
	})
	if strings.Contains(hst.screen(), " other · one") {
		t.Fatal("a refused resume opened a view")
	}
}

// TestResumeInView: a view of a session whose program exits shows "exited 0 · Enter resume".
// Typing other keys resumes nothing; Enter resumes it in the same view, which follows the
// session to its new pane. A resume by another client moves the view too.
func TestResumeInView(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	sess, pane, _ := a.AddSession("job")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the running row", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })
	a.ExitPane(pane)
	hst.waitScreen("the resume note", func(s string) bool { return strings.Contains(s, " job · one · exited 0 · Enter resume") })
	hst.typ("ls")
	time.Sleep(100 * time.Millisecond)
	if n := a.Resumes(); n != 0 {
		t.Fatalf("%d resumes after typing", n)
	}
	hst.typ("\r")
	waitUntil(t, "the resume", func() bool { ss, _ := a.Session(sess); return ss.State == api.SessionStateRunning && ss.Pane != pane })
	hst.waitScreen("the view following the new pane", func(s string) bool {
		return strings.Contains(s, " job · one") && !strings.Contains(s, "Enter resume") && strings.Count(s, " job · one") == 1
	})
	ss, _ := a.Session(sess)

	// Another client resumes it: the view follows.
	a.ExitPane(ss.Pane)
	hst.waitScreen("the resume note again", func(s string) bool { return strings.Contains(s, "Enter resume") })
	c, err := control.Dial(a.Sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), waitLong())
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "other", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	var res api.SessionCreated
	if err := c.Call(ctx, "session.resume", api.SessionRef{Session: sess}, &res); err != nil {
		t.Fatal(err)
	}
	hst.waitScreen("the view following the other client's resume", func(s string) bool {
		return strings.Contains(s, " job · one") && !strings.Contains(s, "Enter resume") && strings.Count(s, " job · one") == 1
	})
}

// fakeResumeClaude stands in for Claude Code: it logs its arguments, reports idle over OSC
// 7501, hands gff-hab its native session id through the SessionStart hook, prints a line, and
// waits.
const fakeResumeClaude = `#!/bin/sh
echo "claude $*" >> "$RESUME_LOG"
printf '\033]7501;state=idle:app=claude\033\\'
printf '{"session_id":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"startup"}' "$RESUME_ID" "$PWD" | "$RESUME_HAB" hook claude SessionStart
echo "agent-up $*"
exec sleep 600
`

// TestResumeAfterHolderLoss: end to end with the real holder and gff-hab. A Claude Code session
// runs in a view; the holder dies for good, a new one starts, and gff-hab restarts. The view
// shows Resume and nothing ran again. Enter starts Claude Code with its resume id in the same
// view.
func TestResumeAfterHolderLoss(t *testing.T) {
	d := shortDir(t)
	hdir, state, sock := filepath.Join(d, "h"), filepath.Join(d, "s"), filepath.Join(d, "run", "hab.sock")
	work, fake, cfgDir := filepath.Join(d, "w"), filepath.Join(d, "bin"), filepath.Join(d, "cc")
	for _, dir := range []string{hdir, work, fake, cfgDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fake, "claude"), []byte(fakeResumeClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(work, "argv.log")
	const native = "5d1c2b7a-gff-resume"
	env := append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"), "CLAUDE_CONFIG_DIR="+cfgDir,
		"RESUME_LOG="+log, "RESUME_HAB="+habBin, "RESUME_ID="+native)
	// A transcript, so the resume is --resume.
	if err := os.MkdirAll(filepath.Join(cfgDir, "projects", "w"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "projects", "w", native+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	holder, holderDone := startHolderProc(t, hdir, "one")
	serve, serveDone := startServeProc(t, sock, state, hdir, env)
	_ = serve
	h := &hab{t: t, sock: sock}
	other := h.other()
	var created api.SessionCreated
	call(t, other, "session.create", api.SessionCreateParams{Name: "agent", Harness: "claude", Cwd: work, Cols: 80, Rows: 24}, &created)
	waitUntil(t, "claude started", func() bool { b, _ := os.ReadFile(log); return strings.Count(string(b), "\n") == 1 })
	waitUntil(t, "the native id in the book", func() bool {
		b, _ := os.ReadFile(filepath.Join(state, "resume.json"))
		return strings.Contains(string(b), native)
	})

	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, sock)
	hst.waitScreen("the session", func(s string) bool { return line(s, "agent") > 0 })
	hst.typ("\r")
	hst.waitScreen("claude in its view", func(s string) bool { return strings.Contains(s, "agent-up") })

	holder.Process.Kill()
	<-holderDone
	if err := os.Remove(holderreg.Path(hdir, "one")); err != nil {
		t.Fatal(err)
	}
	startHolderProc(t, hdir, "two")
	stopProc(serve, serveDone)
	startServeProc(t, sock, state, hdir, env)
	hst.waitScreen("the view offering Resume", func(s string) bool { return strings.Contains(s, " agent · one · exited · Enter resume") })
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(log); strings.Count(string(b), "\n") != 1 {
		t.Fatalf("claude ran again on its own: %q", b)
	}

	hst.typ("\r")
	hst.waitScreen("claude resumed in the same view", func(s string) bool {
		return strings.Contains(s, "agent-up --resume "+native) && strings.Count(s, " agent · one") == 1 && !strings.Contains(s, "Enter resume")
	})
	b, _ := os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 2 || lines[1] != "claude --resume "+native {
		t.Fatalf("claude starts %q", lines)
	}
}

// startHolderProc starts a holder with its own instance id in dir and waits for its registry
// file.
func startHolderProc(t *testing.T, dir, id string) (*exec.Cmd, chan struct{}) {
	t.Helper()
	before, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	cmd := exec.Command(holderBin, "-dir", dir, "-instance", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { stopProc(cmd, done) })
	waitUntil(t, "holder "+id+"'s registry file", func() bool {
		m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		return len(m) > len(before)
	})
	return cmd, done
}

// startServeProc starts gff-hab over the holders in hdir with env, and waits for its socket.
func startServeProc(t *testing.T, sock, state, hdir string, env []string) (*exec.Cmd, chan struct{}) {
	t.Helper()
	os.Remove(sock)
	cmd := exec.Command(habBin, "serve", "-socket", sock, "-state", state, "-host-id", "test-host", "-holders", hdir)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { stopProc(cmd, done) })
	waitUntil(t, "gff-hab's socket", func() bool {
		c, err := control.Dial(sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return cmd, done
}
