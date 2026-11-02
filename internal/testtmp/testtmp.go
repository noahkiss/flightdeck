// Package testtmp gives a test binary's TestMain one temporary directory that does not outlive
// it. Tests use t.TempDir for their own files; this is for what TestMain builds once and shares
// (helper binaries, fakes), which t.TempDir cannot hold.
//
// MainDir writes the owner's pid into the directory. A test binary that ends without its
// cleanup (a panic, a -timeout, a kill) leaves its directory behind; the next MainDir with the
// same prefix removes every such directory whose owner is gone.
package testtmp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidFile names the owner's pid inside a MainDir directory.
const pidFile = ".testtmp-pid"

// MainDir sweeps the directories an earlier, dead test binary left under prefix, then makes a
// fresh one in the temporary folder, as os.MkdirTemp("", prefix) does. The caller removes it
// with Remove after m.Run.
func MainDir(prefix string) (string, error) {
	Sweep(prefix)
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, pidFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// Sweep removes each directory in the temporary folder whose name starts with prefix and whose
// pid file names a process that no longer runs. A directory with no pid file is not one of ours
// and stays.
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

// Exit removes dir and exits with code; a directory left behind turns a pass into exit 1. It
// is the last call of a TestMain, after m.Run: os.Exit skips deferred calls.
func Exit(code int, dir string) {
	if err := Remove(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
