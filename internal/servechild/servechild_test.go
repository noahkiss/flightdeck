//go:build linux || darwin

package servechild

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/peerproc"
)

// TestRun: a program Run starts carries the mark and a session of its own; the session stays a
// root after the program ends while a process it left runs in it, and Sweep drops it once that
// process ends. Until Enable, Run changes nothing. Not parallel: the registry is the process's.
func TestRun(t *testing.T) {
	t.Cleanup(reset)
	reset()
	d := t.TempDir()
	out := filepath.Join(d, "out")
	script := "echo \"$GFF_HAB\" > '" + out + "'; sleep 30 & echo $! >> '" + out + "'"

	plain := exec.Command("/bin/sh", "-c", script)
	if err := Run(plain); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	lines := strings.Fields(string(b))
	if len(lines) != 1 || len(Sessions()) != 0 {
		t.Fatalf("before Enable: %q, sessions %v", b, Sessions())
	}
	kill(t, lines[0])

	Enable("/run/gff/hooks.sock")
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), hook.Env+"="+hook.Addr("/run/gff/hooks.sock", "0123456789abcdef"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Run trades it for the session
	if err := Run(cmd); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(out)
	lines = strings.Fields(string(b))
	if len(lines) != 2 {
		t.Fatalf("output %q", b)
	}
	if lines[0] != hook.ChildAddr("/run/gff/hooks.sock") {
		t.Errorf("GFF_HAB %q, want the mark in place of the pane's", lines[0])
	}
	pid := cmd.Process.Pid
	sys := peerproc.System()
	left, _ := strconv.Atoi(lines[1])
	if p, err := sys.Proc(left); err != nil || p.SID != pid {
		t.Errorf("what the program left: %+v (%v), want session %d", p, err, pid)
	}
	t.Cleanup(func() { kill(t, lines[1]) })
	if !slices.Contains(Sessions(), pid) || Ended() != 1 {
		t.Fatalf("sessions %v after the program ended", Sessions())
	}

	running := func(p int) bool { _, err := sys.Proc(p); return err == nil }
	sweep := func() {
		t.Helper()
		m, err := peerproc.Sessions()
		if err != nil {
			t.Fatal(err)
		}
		Sweep(m, running)
	}
	sweep()
	if !slices.Contains(Sessions(), pid) {
		t.Fatal("Sweep dropped a session a process still runs in")
	}
	// A pid taken by a newer process: the session is not the program's.
	Sweep(map[int]bool{pid: true}, func(int) bool { return true })
	if slices.Contains(Sessions(), pid) {
		t.Fatal("Sweep kept a session whose leader's pid a newer process took")
	}
	std.mu.Lock()
	std.ended[pid] = true
	std.mu.Unlock()
	kill(t, lines[1])
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(Sessions(), pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		sweep()
	}
	if slices.Contains(Sessions(), pid) {
		t.Fatal("Sweep kept a session with no process left")
	}
}

func kill(t *testing.T, pid string) {
	t.Helper()
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 1 {
		t.Fatalf("pid %q", pid)
	}
	syscall.Kill(n, syscall.SIGKILL)
}
