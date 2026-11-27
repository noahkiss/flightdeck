package peerproc

import "time"

// System reads the processes of this machine from /proc.
func System() Table { return procfs{root: "/proc", now: time.Now} }
