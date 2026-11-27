// Package peerproc tells whether the process at the far end of the control socket runs inside
// a hab pane (docs/specs/hab-logic-api.md 4.1). The control server reads the caller's pid from
// the socket when it accepts the connection; Classify walks that process's ancestors and checks
// its session and its environment against the hab's holders and panes.
//
// The check fails closed: a caller it cannot read, a pid that a newer process took, or an
// ancestor chain it cannot follow counts as a pane. It is not a boundary against a process that
// leaves the pane's process tree on purpose (the service manager, ssh to this host): those
// limits are in the spec.
package peerproc

import (
	"bytes"
	"fmt"
	"path/filepath"
	"time"

	"github.com/noahkiss/flightdeck/internal/hook"
)

// Proc is what the check reads about one process.
type Proc struct {
	PID   int
	PPID  int
	SID   int       // the session id; a pane's first process leads its pane's session
	Start time.Time // when the process started
}

// Table reads processes. System is the real one; tests use a fake.
type Table interface {
	// Proc returns the process with this pid, or an error when there is none (or it ended).
	Proc(pid int) (Proc, error)
	// Env returns the environment the process started with, NUL-separated, or nil.
	Env(pid int) []byte
}

// Roots is what makes a process a pane's, read when the check runs.
type Roots struct {
	// Holders are the pids of every holder process the hab knows, draining ones too. A holder
	// starts nothing but panes, so each of its descendants is a pane's.
	Holders []int
	// Panes are the pids of the panes' first processes. Each leads its pane's session, so a
	// process in that session is the pane's, also after its parent exited.
	Panes []int
	// HookSocket is the hab's hook socket. A process whose environment holds GFF_HAB naming it
	// started inside a pane of this hab.
	HookSocket string
}

// Class is the verdict on one caller.
type Class struct {
	Pane bool
	Why  string // one line for the log: which signal decided, and the pids involved
}

// Slack absorbs clock granularity when start times are compared.
const Slack = time.Second

// maxDepth bounds the ancestor walk.
const maxDepth = 256

// Classify decides whether pid, the caller of a connection accepted at time at, runs inside a
// pane. pid 0 or less means the socket gave no pid.
func Classify(t Table, pid int, at time.Time, r Roots) Class {
	if pid <= 0 {
		return Class{Pane: true, Why: "the socket gave no caller pid"}
	}
	holders := set(r.Holders)
	panes := set(r.Panes)
	p, err := t.Proc(pid)
	if err != nil {
		return Class{Pane: true, Why: fmt.Sprintf("caller %d: %v", pid, err)}
	}
	if p.Start.After(at.Add(Slack)) {
		// The connecting process ended and a newer process took its pid.
		return Class{Pane: true, Why: fmt.Sprintf("caller %d started after the connection", pid)}
	}
	if r.HookSocket != "" && NamesSocket(t.Env(pid), r.HookSocket) {
		return Class{Pane: true, Why: fmt.Sprintf("caller %d has a pane's environment", pid)}
	}
	cur := p
	for depth := 0; ; depth++ {
		if holders[cur.PID] {
			return Class{Pane: true, Why: fmt.Sprintf("caller %d descends from holder %d", pid, cur.PID)}
		}
		if cur.SID > 0 && panes[cur.SID] {
			return Class{Pane: true, Why: fmt.Sprintf("caller %d is in the session of pane process %d", pid, cur.SID)}
		}
		if cur.PPID <= 1 || cur.PPID == cur.PID {
			return Class{Why: fmt.Sprintf("caller %d is outside every pane", pid)}
		}
		if depth >= maxDepth {
			return Class{Pane: true, Why: fmt.Sprintf("caller %d: ancestors deeper than %d", pid, maxDepth)}
		}
		if holders[cur.PPID] {
			return Class{Pane: true, Why: fmt.Sprintf("caller %d descends from holder %d", pid, cur.PPID)}
		}
		parent, err := t.Proc(cur.PPID)
		if err != nil {
			// The parent ended during the walk: its children moved to another parent, and the
			// chain read so far may have skipped a pane.
			return Class{Pane: true, Why: fmt.Sprintf("caller %d: ancestor %d ended during the check", pid, cur.PPID)}
		}
		if parent.Start.After(cur.Start.Add(Slack)) {
			// A parent is never younger than its child: a newer process took the pid.
			return Class{Pane: true, Why: fmt.Sprintf("caller %d: ancestor pid %d was reused", pid, cur.PPID)}
		}
		cur = parent
	}
}

// NamesSocket reports whether env, NUL-separated, holds a GFF_HAB entry whose socket is socket.
func NamesSocket(env []byte, socket string) bool {
	want := filepath.Clean(socket)
	for _, entry := range bytes.Split(env, []byte{0}) {
		v, ok := bytes.CutPrefix(entry, []byte(hook.Env+"="))
		if !ok {
			continue
		}
		if sock, _, err := hook.ParseAddr(string(v)); err == nil && filepath.Clean(sock) == want {
			return true
		}
	}
	return false
}

func set(pids []int) map[int]bool {
	m := make(map[int]bool, len(pids))
	for _, p := range pids {
		if p > 0 {
			m[p] = true
		}
	}
	return m
}
