//go:build linux || darwin

package peerproc

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The system table reads this test process as the kernel sees it.
func TestSystemSelf(t *testing.T) {
	p, err := System().Proc(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != os.Getpid() || p.PPID != os.Getppid() || p.SID != sid {
		t.Fatalf("got %+v, want ppid %d sid %d", p, os.Getppid(), sid)
	}
	if now := time.Now(); p.Start.After(now.Add(Slack)) || p.Start.Before(now.Add(-24*time.Hour)) {
		t.Fatalf("start %v, now %v", p.Start, now)
	}
	if _, err := System().Proc(1 << 30); err == nil {
		t.Fatal("a pid out of range read as a process")
	}
}

// A real orphan: a session leader starts a child and exits. The child's parent changes, but it
// keeps the session, so the check still counts it as the pane's; without the pane's pid it is
// outside every pane. A live child of a holder counts as the holder's by its ancestry.
func TestSystemOrphanAndAncestry(t *testing.T) {
	leader := exec.Command("/bin/sh", "-c", "sleep 60 </dev/null >/dev/null 2>&1 & echo $!")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(orphan, syscall.SIGKILL) })
	if err := leader.Wait(); err != nil {
		t.Fatal(err)
	}
	sys := System()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, err := sys.Proc(orphan)
		if err != nil {
			t.Fatal(err)
		}
		if p.PPID != leader.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the orphan kept its exited parent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	now := time.Now()
	if c := Classify(sys, orphan, now, Roots{Panes: []int{leader.Process.Pid}}); !c.Pane || !strings.Contains(c.Why, "session") {
		t.Fatalf("the orphan with its pane known: %+v", c)
	}
	if c := Classify(sys, orphan, now, Roots{}); c.Pane {
		t.Fatalf("the orphan with no pane known: %+v", c)
	}
	if c := Classify(sys, orphan, now, Roots{Holders: []int{os.Getpid()}}); c.Pane {
		t.Fatalf("the orphan left this process's tree, yet: %+v", c)
	}

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	if c := Classify(sys, child.Process.Pid, time.Now(), Roots{Holders: []int{os.Getpid()}}); !c.Pane || !strings.Contains(c.Why, "holder") {
		t.Fatalf("a child of a holder: %+v", c)
	}
	if c := Classify(sys, child.Process.Pid, time.Now(), Roots{}); c.Pane {
		t.Fatalf("a child with no holder known: %+v", c)
	}
	if c := Classify(sys, child.Process.Pid, time.Now().Add(-time.Hour), Roots{}); !c.Pane {
		t.Fatalf("a caller that started after its connection: %+v", c)
	}
}
