package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// procStart reads a pid's start as Claude Code records it in its registry on macOS: ps's
// lstart in UTC ("Mon Jan _2 15:04:05 2006").
func procStart(pid int) (string, bool) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return "", false
	}
	return time.Unix(p.Proc.P_starttime.Sec, 0).UTC().Format(time.ANSIC), true
}
