package main

import (
	"fmt"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/peerproc"
)

// peerProofsFile, in the state folder, lists the device ids that proved their hab key on a peer
// link (transport.md 6.3).
const peerProofsFile = "peer-proofs"

// callerRoots is what localCaller reads at each connection (hab-logic-api.md 4.1).
type callerRoots struct {
	// serve is the pid of gff-hab serve. Everything it starts (git and its hooks, lane checks,
	// teardown scripts) runs code the agent may have written, so it counts as a pane's.
	serve int
	// holderDir is the holder registry folder. Every holder in it is a root, also one the link
	// is not connected to (an older protocol, a dropped connection). "" reads none.
	holderDir string
	// lk gives the connected holders and the panes' first processes. Nil with the holder link
	// off.
	lk         *link.Link
	hookSocket string
	// alive reports whether a registry entry's holder runs; nil is holderreg.Alive.
	alive func(pid int) bool
}

// localCaller is control.Config.LocalCaller (hab-logic-api.md 4.1): it reads the hab's roots at
// each connection and asks peerproc whether the caller runs inside a pane.
func localCaller(r callerRoots, table peerproc.Table) func(pid int, at time.Time) (bool, string) {
	return func(pid int, at time.Time) (bool, string) {
		roots, err := r.read()
		if err != nil {
			// Fail closed: a registry the hab cannot read may hide a holder.
			return true, err.Error()
		}
		c := peerproc.Classify(table, pid, at, roots)
		return c.Pane, c.Why
	}
}

// read collects the roots. An error reading the holder registry is returned: the caller then
// counts as a pane's.
func (r callerRoots) read() (peerproc.Roots, error) {
	out := peerproc.Roots{HookSocket: r.hookSocket}
	if r.serve > 0 {
		out.Holders = append(out.Holders, r.serve)
	}
	if r.holderDir != "" {
		entries, errs := holderreg.List(r.holderDir)
		if len(errs) > 0 {
			return out, fmt.Errorf("the holder registry %s: %v", r.holderDir, errs[0])
		}
		alive := r.alive
		if alive == nil {
			alive = holderreg.Alive
		}
		for _, e := range entries {
			if e.Pid > 0 && alive(e.Pid) {
				out.Holders = append(out.Holders, e.Pid)
			}
		}
	}
	if r.lk != nil {
		out.Holders = append(out.Holders, r.lk.HolderPIDs()...)
		for _, p := range r.lk.Panes() {
			// An exited pane counts too: what its first process left running keeps its
			// session.
			if p.Pid > 0 {
				out.Panes = append(out.Panes, int(p.Pid))
			}
		}
	}
	return out, nil
}
