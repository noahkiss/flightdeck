package testtmp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startIn runs a shell loop from a script in dir, in its own session as a holder runs, so its
// command line names dir.
func startIn(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	script := filepath.Join(dir, "loop.sh")
	if err := os.WriteFile(script, []byte("while :; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	return cmd
}

// waitGone waits for pid to end (the starter reaps it).
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still runs", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitListed waits until ps lists a process under dir.
func waitListed(t *testing.T, dir string) []Proc {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ps := ProcsUnder([]string{dir}); len(ps) > 0 {
			return ps
		}
		if time.Now().After(deadline) {
			t.Fatalf("ps never listed a process under %s", dir)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGuardFindsAndStops: ProcsUnder finds a process by its directory and no other, leaked
// reports it while it runs, and Stop ends it.
func TestGuardFindsAndStops(t *testing.T) {
	d := Dir(t, "ttg")
	if b, err := os.ReadFile(filepath.Join(d, pidFile)); err != nil || string(b) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file %q, %v", b, err)
	}
	cmd := startIn(t, d)
	ps := waitListed(t, d)
	if len(ps) != 1 || ps[0].Pid != cmd.Process.Pid || !strings.Contains(ps[0].Args, d) {
		t.Fatalf("ProcsUnder(%s) = %+v, want pid %d", d, ps, cmd.Process.Pid)
	}
	if other := ProcsUnder([]string{d + "x"}); len(other) != 0 {
		t.Fatalf("a sibling directory matched: %+v", other)
	}
	if left := leaked(0); len(left) != 1 || left[0].Pid != cmd.Process.Pid {
		t.Fatalf("leaked = %+v, want the loop", left)
	}
	Stop(ps, time.Second)
	waitGone(t, cmd.Process.Pid)
	if left := leaked(time.Second); len(left) != 0 {
		t.Fatalf("leaked after Stop = %+v", left)
	}
}

// TestSweepStopsProcesses: sweeping a dead owner's directory ends the process started from it
// before it removes the directory.
func TestSweepStopsProcesses(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(tmp, "tts1")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, pidFile), []byte(strconv.Itoa(dead.Process.Pid)), 0o600)
	cmd := startIn(t, d)
	waitListed(t, d)
	Sweep("tts")
	waitGone(t, cmd.Process.Pid)
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Fatalf("directory still there: %v", err)
	}
}

// TestMainDir: a dead owner's directory goes, a live owner's and a foreign one stay, the new
// directory names this process, and Remove leaves nothing.
func TestMainDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	mk := func(name, pid string) string {
		d := filepath.Join(tmp, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if pid != "" {
			os.WriteFile(filepath.Join(d, pidFile), []byte(pid), 0o600)
		}
		return d
	}
	dead := mk("tt1", strconv.Itoa(cmd.Process.Pid))
	live := mk("tt2", strconv.Itoa(os.Getppid()))
	foreign := mk("tt3", "")
	other := mk("zz1", strconv.Itoa(cmd.Process.Pid))

	dir, err := MainDir("tt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(tmp, "tt")) {
		t.Fatalf("dir %s not under %s", dir, tmp)
	}
	for d, want := range map[string]bool{dead: false, live: true, foreign: true, other: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s: present %v, want %v", filepath.Base(d), err == nil, want)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, pidFile))
	if string(b) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file %q", b)
	}
	Sweep("tt") // this process's own directory stays
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "bin"), []byte("x"), 0o700)
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
}
