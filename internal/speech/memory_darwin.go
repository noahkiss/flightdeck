package speech

import "golang.org/x/sys/unix"

// HostMemory returns the host's physical memory in bytes, or 0 when unknown.
func HostMemory() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
}
