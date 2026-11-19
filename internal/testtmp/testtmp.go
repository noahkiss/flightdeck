// Package testtmp gives a test binary's TestMain one temporary directory that does not outlive
// it. Tests use t.TempDir for their own files; this is for what TestMain builds once and shares
// (helper binaries, fakes), which t.TempDir cannot hold.
//
// MainDir writes the owner's pid into the directory. A test binary that ends without its
// cleanup (a panic, a -timeout, a kill) leaves its directory behind; the next MainDir with the
// same prefix stops the processes started from every such directory whose owner is gone, and
// removes the directory. Dir does the same for a test's short socket directory. Run and Exit
// fail a binary whose processes outlive its tests (procs.go).
package testtmp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// pidFile names the owner's pid inside a MainDir or Dir directory.
const pidFile = ".testtmp-pid"

// MainDir sweeps the directories an earlier, dead test binary left under prefix, then makes a
// fresh one in the temporary folder, as os.MkdirTemp("", prefix) does. The caller removes it
// with Run or Exit after m.Run.
func MainDir(prefix string) (string, error) {
	Sweep(prefix)
	return owned(prefix)
}

// Sweep removes each directory in the temporary folder whose name starts with prefix and whose
// pid file names a process that no longer runs, after it stops the processes whose command line
// names that directory. A directory with no pid file is not one of ours and stays.
func Sweep(prefix string) {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, pidFile))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 || pid == os.Getpid() || alive(pid) {
			continue
		}
		if ps := ProcsUnder([]string{dir}); len(ps) > 0 {
			for _, p := range ps {
				fmt.Fprintf(os.Stderr, "testtmp: stopping process %d, left by a dead test binary: %s\n", p.Pid, p.Args)
			}
			Stop(ps, 2*time.Second)
		}
		os.RemoveAll(dir)
	}
}

// alive reports whether pid names a running process. A process this user may not signal still
// runs.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Remove removes dir and reports an error when anything is left: a test that leaves a process
// writing into its TestMain directory fails instead of leaking it.
func Remove(dir string) error {
	if dir == "" {
		return nil
	}
	err := os.RemoveAll(dir)
	if _, serr := os.Lstat(dir); serr == nil {
		// A process still running may have written into it during the removal; try once more.
		err = os.RemoveAll(dir)
		if _, serr := os.Lstat(dir); serr == nil {
			return fmt.Errorf("testtmp: %s is still there after removal: %v", dir, err)
		}
	}
	return nil
}

// Exit removes dir and exits with code. It is the last call of a TestMain, after m.Run:
// os.Exit skips deferred calls. A process started from dir or a Dir that still runs 5 s after
// the tests end gets a FAIL line and is stopped; it, or a directory left behind, turns a pass
// into exit 1.
func Exit(code int, dir string) {
	if ps := leaked(5 * time.Second); len(ps) > 0 {
		for _, p := range ps {
			fmt.Fprintf(os.Stderr, "FAIL: testtmp: process %d outlived its test: %s\n", p.Pid, p.Args)
		}
		Stop(ps, 2*time.Second)
		if code == 0 {
			code = 1
		}
	}
	for _, d := range registered() {
		if d != dir {
			os.RemoveAll(d) // a Dir whose test never ran its cleanup
		}
	}
	if err := Remove(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
