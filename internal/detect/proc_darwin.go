//go:build darwin

package detect

import (
	"context"

	"golang.org/x/sys/unix"
)

// listGroup asks the kernel for group pgid with sysctl, in process and with no cgo:
// kern.proc.pgrp for the members, their pids and names (p_comm, cut at 16 bytes as ps's ucomm
// is), then kern.procargs2 for each member's argv. It runs no program, so a loaded machine
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
			} else if b, err := unix.SysctlRaw("kern.procargs2", p.PID); err == nil {
				p.Args = parseProcArgs(b)
			}
			if out = append(out, p); len(out) == maxGroup {
				break
			}
		}
		return out, nil
	})
}
