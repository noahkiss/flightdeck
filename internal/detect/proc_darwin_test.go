//go:build darwin

package detect

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startGroup runs path with argv and env in a new process group from dir, and kills it at the
// end of the test.
func startGroup(t *testing.T, path, dir string, argv, env []string) int {
	t.Helper()
	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{
		Dir: dir,
		Env: env,
		Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		syscall.Wait4(pid, &ws, 0, nil)
	})
	return pid
}

// waitArgs lists group pid until its leader's Args are want, failing on an environment
// string in them or after 10 s.
func waitArgs(t *testing.T, pid int, want []string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		fg, err := GroupProcesses(context.Background(), pid)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range fg.Leader.Args {
			if strings.Contains(a, "GFF_TEST_SECRET") {
				t.Fatalf("an environment string in argv: %q", fg.Leader.Args)
			}
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

// TestGroupProcessesEmptyArgv0: a program run with an empty argv[0] lists that empty argument
// and no environment string. The kernel's buffer holds the path's padding, then the empty
// argument's own NUL, then "30", then the environment.
func TestGroupProcessesEmptyArgv0(t *testing.T) {
	pid := startGroup(t, "/bin/sleep", "", []string{"", "30"}, []string{"GFF_TEST_SECRET=x"})
	waitArgs(t, pid, []string{"", "30"})
}

// TestGroupProcessesThroughLink: a program run through a symbolic link under another name,
// by absolute and by relative path, keeps its argv. The kernel saves the path as the caller
// gave it, while p_comm is the resolved file's name (sleep), so nothing may compare the two.
func TestGroupProcessesThroughLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "mysleeplink")
	if err := os.Symlink("/bin/sleep", link); err != nil {
		t.Fatal(err)
	}
	env := []string{"GFF_TEST_SECRET=x"}
	t.Run("absolute", func(t *testing.T) {
		pid := startGroup(t, link, "", []string{"mysleeplink", "30"}, env)
		waitArgs(t, pid, []string{"mysleeplink", "30"})
	})
	t.Run("relative", func(t *testing.T) {
		pid := startGroup(t, "./mysleeplink", dir, []string{"mysleeplink", "31"}, env)
		waitArgs(t, pid, []string{"mysleeplink", "31"})
	})
}
