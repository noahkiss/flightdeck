package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
)

func TestEscapedSweepNotices(t *testing.T) {
	s := newEscapedSweeps(hook.NewTokens([]byte("test-secret"), nil), t.Logf)
	defer s.Close()
	got := make(chan string, 8)
	s.sweep = func(ctx context.Context, token string) (int, error) { got <- token; return 0, nil }
	u := newUUID()
	for _, n := range []link.Notice{{Kind: link.PaneExited, Pane: link.PaneStatus{UUID: u, Exited: true}}, {Kind: link.PaneGone, Pane: link.PaneStatus{UUID: u, Lost: true}}, {Kind: link.PaneGone, Pane: link.PaneStatus{UUID: u, Gone: true}}} {
		s.notice(n)
	}
	s.notice(link.Notice{Kind: link.PaneGone, Pane: link.PaneStatus{UUID: u, Gone: true}, Released: true})
	select {
	case token := <-got:
		if token != s.tokens.Token(u) {
			t.Fatal("wrong pane token")
		}
	case <-time.After(time.Second):
		t.Fatal("no sweep")
	}
	select {
	case <-got:
		t.Fatal("exit, lost, replaced or replay-error pane swept")
	case <-time.After(20 * time.Millisecond):
	}
}

// A child outside the pane's session retains its token and dies on release. A
// detached process with another environment survives; the sweep never signals groups.
func TestEscapedProcessRelease(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real macOS environment reader; Linux reader has procfs fixtures")
	}
	helper := filepath.Join(t.TempDir(), "escaped")
	if out, err := exec.Command("go", "build", "-o", helper, "./testdata/escaped").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	otherFile := filepath.Join(t.TempDir(), "other")
	other := exec.Command(helper, "child", otherFile)
	other.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	other.Env = []string{"PATH=/usr/bin:/bin"}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	e := newHolderEnv(t, 0)
	e.serve()
	c := e.control()
	pidFile := filepath.Join(e.work, "escaped.pid")
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var res api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Argv: []string{helper, pidFile}, Cwd: e.work, Cols: 80, Rows: 24}, &res); err != nil {
		t.Fatal(err)
	}
	var pid int
	waitUntil(t, "the escaped child", func() bool { b, _ := os.ReadFile(pidFile); pid, _ = strconv.Atoi(string(b)); return pid > 0 })
	var graceful int
	waitUntil(t, "the graceful escaped child", func() bool {
		b, _ := os.ReadFile(pidFile + ".graceful")
		graceful, _ = strconv.Atoi(string(b))
		return graceful > 0
	})
	gracefulClean := false
	defer func() {
		if !gracefulClean {
			_ = syscall.Kill(graceful, syscall.SIGKILL)
		}
	}()
	ended := false
	defer func() {
		if !ended {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	var empty api.Empty
	started := time.Now()
	if err := c.Call(ctx, "pane.close", api.PaneRef{Pane: res.Pane}, &empty); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "TERM cleanup", func() bool { _, err := os.Stat(pidFile + ".graceful.term"); return err == nil })
	gracefulClean = true
	waitUntil(t, "the escaped child ended", func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	if time.Since(started) < 2*time.Second {
		t.Fatal("TERM-ignoring child died before grace elapsed")
	}
	ended = true
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated child ended: %v", err)
	}
}
