//go:build darwin

package detect

import (
	"context"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// kernProcArgs2 is KERN_PROCARGS2 in sys/sysctl.h; x/sys has no name for it.
const kernProcArgs2 = 49

// argMax is kern.argmax, read once; the kernel refuses a kern.procargs2 buffer larger than it.
var argMax = sync.OnceValue(func() int {
	if n, err := unix.SysctlUint32("kern.argmax"); err == nil && n > 0 {
		return int(n)
	}
	return 1 << 18 // below ARG_MAX on every macOS: a cap too low only loses Args
})

// procArgs2 reads pid's argv, only from a reply that proves a whole copy (readProcArgs).
// x/sys SysctlRaw sizes its buffer exactly, so its reply cannot tell a whole area from a tail;
// this calls sysctl directly, still with no cgo.
func procArgs2(pid int) []string {
	return readProcArgs(
		func() (int, error) { return sysctlProcArgs2(pid, nil) },
		func(buf []byte) (int, error) { return sysctlProcArgs2(pid, buf) },
		argMax())
}

// sysctlProcArgs2 reads kern.procargs2 of pid into buf and returns the reply's length; with
// an empty buf it returns the size of the args area.
func sysctlProcArgs2(pid int, buf []byte) (int, error) {
	mib := [3]int32{unix.CTL_KERN, kernProcArgs2, int32(pid)}
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	n := uintptr(len(buf))
	_, _, e := unix.Syscall6(unix.SYS_SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(p), uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 {
		return 0, e
	}
	return int(n), nil
}

// listGroup asks the kernel for group pgid with sysctl, in process and with no cgo:
// kern.proc.pgrp for the members, their pids and names (p_comm, cut at 16 bytes as ps's ucomm
// is), then kern.procargs2 for each member's argv (procArgs2). It runs no program, so a loaded machine
// cannot stall it on a fork or exec, and the reads go on their own goroutine, one listing per
// group at a time (bounded). kern.procargs2 copies from the target's memory and takes its
// memory lock, so a process stuck in a page fault (a hung network mount) can hold that read
// for good. A member whose argv cannot be read (a zombie, one that exited, or one that belongs
// to another user) has no Args.
func listGroup(ctx context.Context, pgid int) ([]Process, error) {
	return bounded(ctx, flightKey{"sysctl", pgid}, func(ctx context.Context) ([]Process, error) {
		kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
		if err != nil {
			return nil, err
		}
		var out []Process
		for i := range kps {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			k := &kps[i]
			if int(k.Eproc.Pgid) != pgid {
				continue
			}
			p := Process{PID: int(k.Proc.P_pid), Name: unix.ByteSliceToString(k.Proc.P_comm[:])}
			if !readsArgs(k.Proc.P_stat) {
				// a zombie: its argv is gone, and the read would take its task's memory lock
			} else {
				p.Args = procArgs2(p.PID)
			}
			if out = append(out, p); len(out) == maxGroup {
				break
			}
		}
		return out, nil
	})
}
