//go:build unix

package worktree

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own and makes cancelling it kill the whole
// group, so a script's background children end with it.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
