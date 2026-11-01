//go:build darwin

package detect

import (
	"context"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGroupProcessesEmptyArgv0: a program run with an empty argv[0] lists that empty argument
// and no environment string. The kernel's buffer holds the path's padding, then the empty
// argument's own NUL, then "30", then the environment.
func TestGroupProcessesEmptyArgv0(t *testing.T) {
	pid, err := syscall.ForkExec("/bin/sleep", []string{"", "30"}, &syscall.ProcAttr{
		Env: []string{"GFF_TEST_SECRET=x"},
		Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		t.Skip(err)
	}
	defer func() {
		syscall.Kill(pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		syscall.Wait4(pid, &ws, 0, nil)
	}()
	want := []string{"", "30"}
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
		time.Sleep(20 * time.Millisecond)
	}
}
