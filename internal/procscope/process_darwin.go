package procscope

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func listProcesses() ([]int, error) {
	ps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		out = append(out, int(p.Proc.P_pid))
	}
	return out, nil
}

func identity(pid int) (process, bool) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || p.Eproc.Ucred.Uid != uint32(os.Getuid()) || p.Eproc.Pcred.P_ruid != uint32(os.Getuid()) || p.Proc.P_stat == 5 {
		return process{}, false
	}
	return process{pid: pid, born: fmt.Sprintf("%d:%d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec)}, true
}

func inspectProcess(pid int, token string) (process, bool) {
	p, ok := identity(pid)
	if !ok {
		return process{}, false
	}
	env := procEnvironment(pid)
	if !matchesEnv(env, token) {
		return process{}, false
	}
	q, ok := identity(pid)
	return p, ok && p == q
}

// A full kern.procargs2 reply contains argc, a padded executable path, argv,
// then NUL-separated environment entries. A full buffer can be a truncated tail;
// only replies shorter than the allocation prove a complete copy.
func procEnvironment(pid int) []byte {
	limit, err := unix.SysctlUint32("kern.argmax")
	if err != nil || limit < 8 || limit > 2<<20 {
		return nil
	}
	mib := [3]int32{unix.CTL_KERN, 49, int32(pid)}
	for _, slack := range []int{64, 4096} {
		var size uintptr
		_, _, e := unix.Syscall6(unix.SYS_SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 3, 0, uintptr(unsafe.Pointer(&size)), 0, 0)
		if e != 0 {
			return nil
		}
		n := min(int(size)+slack, int(limit)-4)
		if n <= 4 {
			return nil
		}
		b := make([]byte, n)
		got := uintptr(n)
		_, _, e = unix.Syscall6(unix.SYS_SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 3, uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&got)), 0, 0)
		if e != 0 {
			return nil
		}
		if int(got) < len(b) {
			return parseEnvironment(b[:got])
		}
	}
	return nil
}

func parseEnvironment(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	if argc < 0 || argc > 4096 {
		return nil
	}
	b = b[4:]
	end := bytes.IndexByte(b, 0)
	if end < 0 {
		return nil
	}
	slot := (end + 1 + 7) / 8 * 8
	b = b[end+1:]
	for n := slot - (end + 1); n > 0 && len(b) > 0 && b[0] == 0; n-- {
		b = b[1:]
	}
	for range argc {
		end = bytes.IndexByte(b, 0)
		if end < 0 {
			return nil
		}
		b = b[end+1:]
	}
	return b
}
