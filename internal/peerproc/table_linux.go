package peerproc

import (
	"time"

	"golang.org/x/sys/unix"
)

// System reads the processes of this machine from /proc, with the boot placed by
// CLOCK_BOOTTIME.
func System() Table { return procfs{root: "/proc", now: time.Now, sinceBoot: bootTime} }

func bootTime() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, err
	}
	return time.Duration(ts.Nano()), nil
}
