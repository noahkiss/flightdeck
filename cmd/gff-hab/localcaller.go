package main

import (
	"time"

	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/peerproc"
)

// peerProofsFile, in the state folder, lists the device ids that proved their hab key on a peer
// link (transport.md 6.3).
const peerProofsFile = "peer-proofs"

// localCaller is control.Config.LocalCaller (hab-logic-api.md 4.1): it reads the hab's holders
// and panes at each connection and asks peerproc whether the caller runs inside a pane. lk is
// nil with the holder link off; then only the environment check can match, since no pane runs.
func localCaller(lk *link.Link, hookSocket string, table peerproc.Table) func(pid int, at time.Time) (bool, string) {
	return func(pid int, at time.Time) (bool, string) {
		r := peerproc.Roots{HookSocket: hookSocket}
		if lk != nil {
			r.Holders = lk.HolderPIDs()
			for _, p := range lk.Panes() {
				// An exited pane counts too: what its first process left running keeps its
				// session.
				if p.Pid > 0 {
					r.Panes = append(r.Panes, int(p.Pid))
				}
			}
		}
		c := peerproc.Classify(table, pid, at, r)
		return c.Pane, c.Why
	}
}
