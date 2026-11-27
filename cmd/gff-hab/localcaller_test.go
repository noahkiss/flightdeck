package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/peerproc"
)

// TestCapcomSendFromPane: a process inside a pane cannot send the user's message to the Capcom
// service session: it would open a user turn (hab-logic-api.md 4.1).
func TestCapcomSendFromPane(t *testing.T) {
	t.Parallel()
	a := engineAgents{eng: &capcomEngine{session: "capcom-1"}}
	ctx := control.WithPaneCall(context.Background())
	err := a.Send(ctx, "c1", api.SessionSendParams{Session: "capcom-1", Text: "act now"})
	if errKind(err) != api.ErrorKindDenied {
		t.Fatalf("session.send to Capcom from a pane: %v", err)
	}
}

// callerTable is a hand-made process table: pid -> parent; every process is its own session
// and started at the epoch.
type callerTable map[int]int

func (t callerTable) Proc(pid int) (peerproc.Proc, error) {
	ppid, ok := t[pid]
	if !ok {
		return peerproc.Proc{}, fmt.Errorf("no process %d", pid)
	}
	return peerproc.Proc{PID: pid, PPID: ppid, SID: pid, Start: time.Unix(0, 0)}, nil
}

func (callerTable) Env(int) []byte { return nil }

// TestLocalCallerRoots: gff-hab serve and every holder in the registry are roots, also a holder
// the link is not connected to (an older protocol, a dropped connection). A registry the hab
// cannot read counts every caller as a pane's (hab-logic-api.md 4.1).
func TestLocalCallerRoots(t *testing.T) {
	t.Parallel()
	const serve, holder, gone = 100, 200, 300
	// 1 init; serve -> git (110) -> hook (111); an unreachable holder -> shell (210) -> agent
	// (211); a terminal (400) -> the person's gff (401).
	// A program serve started that detached (500), with the session serve recorded.
	table := callerTable{1: 0, serve: 1, 110: serve, 111: 110, holder: 1, 210: holder, 211: 210, 400: 1, 401: 400, 500: 1}
	dir := t.TempDir()
	if err := holderreg.Write(dir, holderreg.Entry{Instance: "old", Build: "old", Pid: holder, Socket: filepath.Join(dir, "old.sock")}); err != nil {
		t.Fatal(err)
	}
	if err := holderreg.Write(dir, holderreg.Entry{Instance: "gone", Build: "old", Pid: gone, Socket: filepath.Join(dir, "gone.sock")}); err != nil {
		t.Fatal(err)
	}
	alive := func(pid int) bool { _, err := table.Proc(pid); return err == nil }
	var warned []string
	now := time.Unix(1000, 0)
	junk := &junkWarn{logf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }, now: func() time.Time { return now }}
	check := localCaller(callerRoots{serve: serve, holderDir: dir, alive: alive, sessions: func() []int { return []int{500} }, junk: junk}, table)
	for pid, want := range map[int]bool{111: true, 110: true, 211: true, 401: false, 500: true} {
		if pane, why := check(pid, time.Now()); pane != want {
			t.Errorf("caller %d: pane %v (%s), want %v", pid, pane, why, want)
		}
	}
	// A registry file that does not parse: fail closed.
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pane, why := check(401, time.Now()); !pane || !strings.Contains(why, "registry") {
		t.Errorf("an unreadable registry: pane %v (%s)", pane, why)
	}
	// One loud line, not one per caller, until junkEvery has passed.
	check(401, time.Now())
	if len(warned) != 1 || !strings.Contains(warned[0], "WARNING") || !strings.Contains(warned[0], "junk.json") {
		t.Errorf("warnings %q, want one that names the file", warned)
	}
	now = now.Add(junkEvery)
	check(401, time.Now())
	if len(warned) != 2 {
		t.Errorf("%d warnings after %v, want 2", len(warned), junkEvery)
	}
}
