package peerproc

import (
	"fmt"
	"time"

	"github.com/noahkiss/flightdeck/internal/procscope"
	"golang.org/x/sys/unix"
)

// System reads the processes of this machine with sysctl and getsid.
func System() Table { return darwinTable{} }

type darwinTable struct{}

// szomb is a zombie's p_stat (sys/proc.h).
const szomb = 5

func (darwinTable) Proc(pid int) (Proc, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Proc{}, fmt.Errorf("process %d: %v", pid, err)
	}
	if int(k.Proc.P_pid) != pid {
		return Proc{}, fmt.Errorf("no process %d", pid)
	}
	if k.Proc.P_stat == szomb {
		return Proc{}, fmt.Errorf("process %d ended", pid)
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		return Proc{}, fmt.Errorf("process %d: session: %v", pid, err)
	}
	st := k.Proc.P_starttime
	return Proc{PID: pid, PPID: int(k.Eproc.Ppid), SID: sid, Start: time.Unix(st.Sec, int64(st.Usec)*1000)}, nil
}

func (darwinTable) Env(pid int) []byte { return procscope.Environ(pid) }

// Sessions returns the session id of every live process on this machine.
func Sessions() (map[int]bool, error) {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, k := range ks {
		if k.Proc.P_stat == szomb || k.Proc.P_pid <= 0 {
			continue
		}
		if sid, err := unix.Getsid(int(k.Proc.P_pid)); err == nil && sid > 0 {
			out[sid] = true
		}
	}
	return out, nil
}
