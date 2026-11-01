package detect

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// fakeProc writes a procfs entry under root: stat with the given state and group, and cmdline
// unless it is "".
func fakeProc(t *testing.T, root, pid, comm, state string, pgrp int, cmdline string) string {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := pid + " (" + comm + ") " + state + " 1 " + strconv.Itoa(pgrp) + " 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if cmdline != "" {
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestProcGroup reads a hand-made procfs: the group's members with their argv, a comm with
// spaces and parentheses, other groups and non-pid entries skipped.
func TestProcGroup(t *testing.T) {
	root := t.TempDir()
	fakeProc(t, root, "100", "claude", "S", 100, "claude\x00--resume\x00x\x00")
	fakeProc(t, root, "101", "a (b) c", "R", 100, "")
	fakeProc(t, root, "200", "zsh", "S", 200, "-zsh\x00")
	os.MkdirAll(filepath.Join(root, "self"), 0o755)
	ps, err := procGroup(context.Background(), root, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{{PID: 100, Name: "claude", Args: []string{"claude", "--resume", "x"}}, {PID: 101, Name: "a (b) c"}}
	if !reflect.DeepEqual(ps, want) {
		t.Fatalf("got %+v, want %+v", ps, want)
	}
}

// TestProcGroupSkipsCmdlineInDState: a process in uninterruptible sleep keeps its name but its
// cmdline is never read. The cmdline here is a FIFO with no writer, so a read would hang.
func TestProcGroupSkipsCmdlineInDState(t *testing.T) {
	root := t.TempDir()
	dir := fakeProc(t, root, "100", "claude", "D", 100, "")
	fifo := filepath.Join(dir, "cmdline")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ps, err := procGroup(ctx, root, 100)
	if err != nil || len(ps) != 1 || ps[0].Name != "claude" || ps[0].Args != nil {
		t.Fatalf("got %+v, %v; want claude with no args, at once", ps, err)
	}
}

// TestProcGroupHonoursContext: a read that hangs (a cmdline FIFO with no writer, on a process
// not in D) cannot hold the caller past its context.
func TestProcGroupHonoursContext(t *testing.T) {
	root := t.TempDir()
	dir := fakeProc(t, root, "100", "claude", "S", 100, "")
	fifo := filepath.Join(dir, "cmdline")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	defer func() { // free the walk's blocked open
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := procGroup(ctx, root, 100)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("got %v after %v; want the deadline at once", err, time.Since(start))
	}
}

// TestProcGroupBounded: a group larger than maxGroup lists maxGroup processes.
func TestProcGroupBounded(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= maxGroup+10; i++ {
		fakeProc(t, root, strconv.Itoa(1000+i), "sleep", "S", 5, "")
	}
	ps, err := procGroup(context.Background(), root, 5)
	if err != nil || len(ps) != maxGroup {
		t.Fatalf("got %d processes, %v; want %d", len(ps), err, maxGroup)
	}
}

func TestParseStat(t *testing.T) {
	name, state, g, ok := parseStat([]byte("42 (we ird) (x)) D 1 77 77 0"))
	if !ok || name != "we ird) (x)" || state != "D" || g != 77 {
		t.Errorf("got %q %q %d %v", name, state, g, ok)
	}
	if _, _, _, ok := parseStat([]byte("42 no parens")); ok {
		t.Error("parsed a line with no comm")
	}
}

// TestParsePS: rows whose group is not pgid (a pid reused between the ps runs) are dropped from
// both the name and the args run.
func TestParsePS(t *testing.T) {
	names := []string{"  100   100 claude", "  101   100 node", "  102   999 sshd", "junk"}
	args := []string{"100 100 /opt/bin/claude --resume x", "101 555 /usr/sbin/other --secret", "102 999 sshd: user"}
	got := parsePS(names, args, 100)
	want := []Process{
		{PID: 100, Name: "claude", Args: []string{"/opt/bin/claude", "--resume", "x"}},
		{PID: 101, Name: "node"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if pids := psGroupPIDs([]string{"1 1", "2 100", " 3  100", "4 1000"}, 100); !reflect.DeepEqual(pids, []string{"2", "3"}) {
		t.Errorf("pids %v", pids)
	}
	if got := parsePS([]string{"7 100 Google Chrome Helper"}, nil, 100); len(got) != 1 || got[0].Name != "Google Chrome Helper" {
		t.Errorf("a name with spaces: %+v", got)
	}
}

// TestParseProcArgs reads hand-made kern.procargs2 buffers: argc, the executable path, its NUL
// padding, argv (an argument with a space stays one), then the environment, never read.
func TestParseProcArgs(t *testing.T) {
	buf := func(argc int32, rest string) []byte {
		b := binary.LittleEndian.AppendUint32(nil, uint32(argc))
		return append(b, rest...)
	}
	for _, tc := range []struct {
		name string
		in   []byte
		want []string
	}{
		{"argv", buf(3, "/bin/sh\x00\x00\x00\x00sh\x00-c\x00sleep 30; :\x00HOME=/x\x00"), []string{"sh", "-c", "sleep 30; :"}},
		{"no padding", buf(1, "/x/claude\x00claude\x00PATH=/bin\x00"), []string{"claude"}},
		{"an empty argument", buf(3, "/bin/echo\x00echo\x00\x00b\x00"), []string{"echo", "", "b"}},
		{"cut short", buf(3, "/bin/sh\x00sh\x00-c"), []string{"sh", "-c"}},
		{"argc beyond the buffer", buf(9, "/bin/sh\x00sh\x00"), []string{"sh"}},
		{"no argc", []byte{1, 0}, nil},
		{"negative argc", buf(-1, "/bin/sh\x00sh\x00"), nil},
		{"no path end", buf(1, "/bin/sh"), nil},
	} {
		if got := parseProcArgs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBoundedReturnsOnContext: a listing that hangs and never looks at ctx holds only its own
// goroutine; bounded returns ctx's error when ctx ends.
func TestBoundedReturnsOnContext(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := bounded(ctx, func() ([]Process, error) { <-hang; return nil, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned after %v", d)
	}
}

// TestGroupProcessesExactArgs: on Linux and macOS argv is read as the kernel holds it, so a
// shell's script with spaces in it stays one argument.
func TestGroupProcessesExactArgs(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("ps prints argv as one line")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30; :")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()
	want := []string{"/bin/sh", "-c", "sleep 30; :"}
	var fg Foreground
	for deadline := time.Now().Add(10 * time.Second); ; {
		var err error
		if fg, err = GroupProcesses(context.Background(), cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(fg.Leader.Args, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leader %+v; want args %q", fg.Leader, want)
		}
		time.Sleep(20 * time.Millisecond) // until the child has run exec
	}
}
