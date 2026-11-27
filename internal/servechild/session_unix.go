//go:build unix

package servechild

import (
	"os/exec"
	"syscall"
)

// ownSession starts cmd in a session of its own. A session leader leads its process group too,
// so a Cancel that kills the group -pid still reaches every process the program started; a
// Setpgid already set is dropped, as setpgid fails on a session leader.
func ownSession(cmd *exec.Cmd) {
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	attr.Setpgid, attr.Pgid = false, 0
	attr.Setsid = true
	cmd.SysProcAttr = &attr
}
