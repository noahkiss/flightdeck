package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/hook"
)

const statusInput = `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1767268800}}}`

// fakeHab serves the hook endpoint on a Unix socket in dir with handle and returns the GFF_HAB
// value that names it.
func fakeHab(t *testing.T, dir string, handle http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(dir, "hook.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handle}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return hook.Addr(sock, strings.Repeat("ab", 16))
}

// runStatus runs `gff-hab statusline` with the status-line input on stdin and returns its
// stdout, stderr, exit code and how long it took.
func runStatus(t *testing.T, env []string, args ...string) (stdout, stderr string, code int, d time.Duration) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"statusline"}, args...)...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(statusInput)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	d = time.Since(start)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code, d
}

// TestStatuslineHungHab: a gff-hab that takes the connection and never answers holds the
// status line no longer than the post budget, not the hook budget.
func TestStatuslineHungHab(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "hook.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	defer func() { l.Close(); <-accepted }()
	go func() {
		defer close(accepted)
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c) // held open, never answered
		}
	}()
	env := []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep, hook.Env + "=" + hook.Addr(sock, strings.Repeat("ab", 16))}
	runStatus(t, env, "claude") // the first exec of a fresh binary can be slow; do not time it
	limit := min(scaleTimeout(500*time.Millisecond), hook.Budget-100*time.Millisecond)
	for range 3 {
		out, _, code, d := runStatus(t, env, "claude", "--", "echo hi")
		if out != "hi\n" || code != 0 || d > limit {
			t.Errorf("hung gff-hab: %q, exit %d after %v, want at most %v", out, code, d, limit)
		}
	}
}

// TestStatuslinePostBeside: the post runs while the command runs, and gff-hab waits for the
// post before it exits.
func TestStatuslinePostBeside(t *testing.T) {
	dir := shortDir(t)
	started := filepath.Join(dir, "started")
	var got atomic.Int32
	addr := fakeHab(t, dir, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// The answer waits for the command to start: a post that runs before the command times
		// out.
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond) // the command has long ended
		got.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	env := []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep, "GFF_HOOK_DEBUG=1", "DIR=" + dir, hook.Env + "=" + addr}
	runStatus(t, append(env[:len(env):len(env)], hook.Env+"="), "claude") // warm up, posting nothing
	out, errOut, code, _ := runStatus(t, env, "claude", "--", `touch "$DIR/started"; echo ok`)
	if out != "ok\n" || code != 0 || errOut != "" {
		t.Errorf("post beside the command: %q, exit %d, stderr %q", out, code, errOut)
	}
	if n := got.Load(); n != 1 {
		t.Errorf("gff-hab exited with %d posts answered, want 1", n)
	}
}

// TestStatuslineArgv: one word after -- is a shell string; several are the argv, quoting kept.
func TestStatuslineArgv(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep}
	if out, _, code, _ := runStatus(t, env, "claude", "--", "printf", "%s|", "a b", "c"); out != "a b|c|" || code != 0 {
		t.Errorf("argv: %q, exit %d", out, code)
	}
	if out, _, code, _ := runStatus(t, env, "claude", "--", `printf '%s|' "a b" c`); out != "a b|c|" || code != 0 {
		t.Errorf("shell string: %q, exit %d", out, code)
	}
	if _, _, code, _ := runStatus(t, env, "claude", "--", "/nonexistent/status", "x"); code != 127 {
		t.Errorf("a command that does not exist: exit %d, want 127", code)
	}
}

// TestStatuslineNoOrphan: SIGTERM to gff-hab ends the command and its children, and so does a
// command that runs past -timeout.
func TestStatuslineNoOrphan(t *testing.T) {
	dir := shortDir(t)
	env := []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep, "DIR=" + dir}
	// Each command starts a child that would outlive it and writes the child's pid.
	script := func(name string) string {
		return `sleep 30 & echo $! > "$DIR/` + name + `.tmp"; mv "$DIR/` + name + `.tmp" "$DIR/` + name + `"; wait`
	}
	readPid := func(name string) int {
		t.Helper()
		var pid int
		waitUntil(t, name+" pid file", func() bool {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			return err == nil
		})
		// The test started this child (through gff-hab): kill it if the test fails.
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		return pid
	}
	gone := func(what string, pid int) {
		t.Helper()
		end := time.Now().Add(scaleTimeout(3 * time.Second))
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(end) {
				t.Errorf("%s: child %d still runs", what, pid)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	cmd := exec.Command(bin, "statusline", "claude", "--", script("term"))
	cmd.Env, cmd.Stdin = env, strings.NewReader(statusInput)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	pid := readPid("term")
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-waited:
	case <-time.After(scaleTimeout(5 * time.Second)):
		cmd.Process.Kill()
		t.Fatal("gff-hab did not exit after SIGTERM")
	}
	gone("SIGTERM", pid)

	start := time.Now()
	cmd = exec.Command(bin, "statusline", "claude", "-timeout", "300ms", "--", script("timeout"))
	cmd.Env, cmd.Stdin = env, strings.NewReader(statusInput)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = readPid("timeout")
	err := cmd.Wait()
	if d := time.Since(start); err == nil || d > scaleTimeout(3*time.Second) {
		t.Errorf("-timeout 300ms: %v after %v", err, d)
	}
	gone("-timeout", pid)
}
