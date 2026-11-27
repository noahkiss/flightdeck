package peerproc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/hook"
)

// fakeTable is a process table written by hand.
type fakeTable struct {
	procs map[int]Proc
	envs  map[int]string
}

func (f fakeTable) Proc(pid int) (Proc, error) {
	p, ok := f.procs[pid]
	if !ok {
		return Proc{}, errors.New("no such process")
	}
	return p, nil
}

func (f fakeTable) Env(pid int) []byte { return []byte(f.envs[pid]) }

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// tree: launchd 1; holder 100 (sid 100); pane 200 (sid 200, child of the holder); its shell's
// child 210; an orphan 220 of the pane (parent 1, sid 200); a detached orphan 230 (parent 1,
// sid 230); a terminal app 300 and its shell 310 (sid 310) and a client 320.
func tree() fakeTable {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	return fakeTable{
		procs: map[int]Proc{
			100: {PID: 100, PPID: 1, SID: 100, Start: at(0)},
			200: {PID: 200, PPID: 100, SID: 200, Start: at(1)},
			210: {PID: 210, PPID: 200, SID: 200, Start: at(2)},
			220: {PID: 220, PPID: 1, SID: 200, Start: at(3)},
			230: {PID: 230, PPID: 1, SID: 230, Start: at(4)},
			240: {PID: 240, PPID: 200, SID: 240, Start: at(4)},
			300: {PID: 300, PPID: 1, SID: 300, Start: at(0)},
			310: {PID: 310, PPID: 300, SID: 310, Start: at(1)},
			320: {PID: 320, PPID: 310, SID: 310, Start: at(5)},
		},
		envs: map[int]string{},
	}
}

var roots = Roots{Holders: []int{100}, Panes: []int{200}, HookSocket: "/run/gff/hooks.sock"}

func TestClassify(t *testing.T) {
	at := t0.Add(time.Hour)
	for _, c := range []struct {
		name string
		pid  int
		edit func(*fakeTable)
		pane bool
		why  string
		r    *Roots
	}{
		{name: "a client in a terminal", pid: 320, pane: false},
		{name: "a process in a pane", pid: 210, pane: true, why: "session of pane process 200"},
		{name: "a process in a pane the link has no pid for yet", pid: 240, pane: true, why: "holder 100", r: &Roots{Holders: []int{100}}},
		{name: "the pane's own process", pid: 200, pane: true, why: "session of pane process 200"},
		{name: "an orphan that kept the pane's session", pid: 220, pane: true, why: "session of pane process 200"},
		{name: "a detached orphan with the pane's environment", pid: 230, pane: true, why: "environment",
			edit: func(f *fakeTable) { f.envs[230] = "A=1\x00" + hook.Env + "=" + hook.Addr("/run/gff/hooks.sock", "0123456789abcdef") + "\x00" }},
		{name: "a detached orphan with another hab's environment", pid: 230, pane: false,
			edit: func(f *fakeTable) { f.envs[230] = hook.Env + "=" + hook.Addr("/elsewhere/hooks.sock", "0123456789abcdef") + "\x00" }},
		{name: "a detached orphan with a cleared environment", pid: 230, pane: false},
		{name: "no pid", pid: 0, pane: true, why: "no caller pid"},
		{name: "a caller that ended", pid: 999, pane: true, why: "caller 999"},
		{name: "a pid a newer process took", pid: 320, pane: true, why: "started after",
			edit: func(f *fakeTable) { p := f.procs[320]; p.Start = at.Add(time.Minute); f.procs[320] = p }},
		{name: "an ancestor that ended during the walk", pid: 320, pane: true, why: "ended during",
			edit: func(f *fakeTable) { delete(f.procs, 310) }},
		{name: "an ancestor pid that a newer process took", pid: 320, pane: true, why: "was reused",
			edit: func(f *fakeTable) { p := f.procs[310]; p.Start = t0.Add(30 * time.Minute); f.procs[310] = p }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := tree()
			if c.edit != nil {
				c.edit(&f)
			}
			r := roots
			if c.r != nil {
				r = *c.r
			}
			got := Classify(f, c.pid, at, r)
			if got.Pane != c.pane || !strings.Contains(got.Why, c.why) {
				t.Fatalf("got %+v, want pane %v with %q", got, c.pane, c.why)
			}
		})
	}
}

// A cycle in the table ends at the depth bound, as a pane's.
func TestClassifyLoop(t *testing.T) {
	f := fakeTable{procs: map[int]Proc{
		10: {PID: 10, PPID: 11, SID: 10, Start: t0},
		11: {PID: 11, PPID: 10, SID: 11, Start: t0},
	}}
	if got := Classify(f, 10, t0, Roots{}); !got.Pane || !strings.Contains(got.Why, "deeper") {
		t.Fatalf("got %+v", got)
	}
}

// The /proc reader parses a stat line whose command name holds spaces and a parenthesis, and
// places the start time by the uptime (Linux; the reader runs on any system against a fixture).
func TestProcfs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Fields after ')': state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt
	// utime stime cutime cstime priority nice threads itreal starttime ...
	stat := "42 (a b) c) S 7 42 40 34816 42 4194304 1 2 3 4 5 6 7 8 20 0 1 0 12345 1000 10\n"
	files := map[string]string{
		"42/stat":    stat,
		"42/environ": "X=1\x00Y=2\x00",
		"uptime":     "200.00 100.00\n",
	}
	for name, s := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(10_000, 0)
	f := procfs{root: root, now: func() time.Time { return now }}
	p, err := f.Proc(42)
	if err != nil {
		t.Fatal(err)
	}
	// Boot at now-200 s; start 12345 ticks (123.45 s) after it.
	want := now.Add(-200 * time.Second).Add(12345 * 10 * time.Millisecond)
	if p.PID != 42 || p.PPID != 7 || p.SID != 40 || !p.Start.Equal(want) {
		t.Fatalf("got %+v, want ppid 7 sid 40 start %v", p, want)
	}
	if got := string(f.Env(42)); got != files["42/environ"] {
		t.Fatalf("env %q", got)
	}
	if _, err := f.Proc(43); err == nil {
		t.Fatal("a missing process read")
	}
	if err := os.WriteFile(filepath.Join(root, "42/stat"), []byte(strings.Replace(stat, ") S ", ") Z ", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Proc(42); err == nil {
		t.Fatal("a zombie read as a live process")
	}
}
