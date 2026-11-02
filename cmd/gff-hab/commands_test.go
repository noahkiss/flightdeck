package main

// End-to-end tests of the commands found by OSC 133 marks (hab-logic-api.md 2.5): a real holder,
// gff-hab and a real interactive shell that emits the marks (testdata/marks).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/cmdmarks"
	"github.com/noahkiss/flightdeck/internal/control"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// markedShell starts an interactive shell with the test mark snippet and returns the pane id. It
// skips the test when the shell is not installed, or when no bash is new enough (markedBash).
func (e *habEnv) markedShell(c *control.Client, shell string) string {
	e.t.Helper()
	var path string
	if shell == "bash" {
		path = markedBash(e.t)
	} else {
		var err error
		if path, err = exec.LookPath(shell); err != nil {
			e.t.Skipf("%s is not installed", shell)
		}
	}
	fix, err := filepath.Abs(filepath.Join("testdata", "marks"))
	if err != nil {
		e.t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "TERM=xterm-256color", "HOME=" + e.work}
	var argv []string
	switch shell {
	case "bash":
		argv = []string{path, "--noprofile", "--rcfile", filepath.Join(fix, "marks.bash"), "-i"}
	case "zsh":
		argv = []string{path, "-d", "-i"}
		env = append(env, "ZDOTDIR="+fix)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var res api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Argv: argv, Cwd: e.work, Cols: 80, Rows: 24, Env: env}, &res); err != nil {
		e.t.Fatal(err)
	}
	return res.Pane
}

// minBash is the oldest bash the mark snippets support: bash sends C from PS0, new in 4.4
// (hab-logic-api.md 2.5, Shells).
var minBash = [2]int{4, 4}

// markedBash picks the bash for the marked shell tests and logs its path and version. With
// $GFF_TEST_BASH set, that bash, and the test fails when it is older than minBash. Otherwise the
// first of: bash on PATH, /opt/homebrew/bin/bash, /usr/local/bin/bash, /bin/bash that is minBash
// or newer. macOS ships bash 3.2 as /bin/bash: with no newer bash the test skips and says so.
func markedBash(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("GFF_TEST_BASH"); p != "" {
		v, ok := bashVersion(p)
		if !ok {
			t.Fatalf("GFF_TEST_BASH=%s: not a bash that runs", p)
		}
		if !bashAtLeast(v, minBash) {
			t.Fatalf("GFF_TEST_BASH=%s is bash %d.%d; the marks need bash %d.%d or newer (PS0)", p, v[0], v[1], minBash[0], minBash[1])
		}
		t.Logf("bash: %s (%d.%d, from GFF_TEST_BASH)", p, v[0], v[1])
		return p
	}
	var cands []string
	if p, err := exec.LookPath("bash"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/bash", "/usr/local/bin/bash", "/bin/bash")
	seen := map[string]bool{}
	var found []string
	for _, p := range cands {
		if seen[p] {
			continue
		}
		seen[p] = true
		v, ok := bashVersion(p)
		if !ok {
			continue
		}
		if bashAtLeast(v, minBash) {
			t.Logf("bash: %s (%d.%d)", p, v[0], v[1])
			return p
		}
		found = append(found, fmt.Sprintf("%s is %d.%d", p, v[0], v[1]))
	}
	if len(found) == 0 {
		t.Skip("bash is not installed")
	}
	t.Skipf("the marks need bash %d.%d or newer (PS0 sends the C mark); %s. Set GFF_TEST_BASH to a newer bash", minBash[0], minBash[1], strings.Join(found, ", "))
	return ""
}

// bashVersion runs the bash at path and returns its major and minor version.
func bashVersion(path string) ([2]int, bool) {
	out, err := exec.Command(path, "--norc", "--noprofile", "-c", `echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"`).Output()
	if err != nil {
		return [2]int{}, false
	}
	var v [2]int
	if n, _ := fmt.Sscan(string(out), &v[0], &v[1]); n != 2 {
		return [2]int{}, false
	}
	return v, true
}

func bashAtLeast(v, want [2]int) bool {
	return v[0] > want[0] || v[0] == want[0] && v[1] >= want[1]
}

func listCommands(t *testing.T, c *control.Client, pane string) []api.Command {
	t.Helper()
	var res api.CommandList
	call(t, c, "command.list", api.CommandListParams{Pane: pane}, &res)
	return res.Commands
}

// commandEventsFor subscribes c to the host's events and returns the command.updated events for
// pane, in order.
func commandEventsFor(t *testing.T, c *control.Client, pane string) <-chan api.Command {
	t.Helper()
	call(t, c, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, nil)
	out := make(chan api.Command, 256)
	go func() {
		defer close(out)
		for n := range c.Notifications() {
			if n.Method != "event" {
				continue
			}
			ev, err := n.Event()
			if err != nil || ev.Type != "command.updated" || ev.ID != pane || ev.Revision != nil {
				continue
			}
			var cmd api.Command
			if json.Unmarshal(ev.Data, &cmd) == nil {
				out <- cmd
			}
		}
	}()
	return out
}

func sameCommand(a, b api.Command) bool {
	return a.ID == b.ID && a.State == b.State && a.Output == b.Output && fmt.Sprint(deref(a.End)) == fmt.Sprint(deref(b.End)) &&
		fmt.Sprint(deref(a.Exit)) == fmt.Sprint(deref(b.Exit)) && fmt.Sprint(deref(a.Prompt)) == fmt.Sprint(deref(b.Prompt)) &&
		fmt.Sprint(deref(a.Input)) == fmt.Sprint(deref(b.Input)) && a.Text == b.Text
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestCommandMarks is the stage's end-to-end acceptance test: a marked shell runs three
// commands, typed one at a time once the one before has ended. command.list shows three commands
// with their exit statuses, and command.output gives each command's output and nothing else. The
// list survives a planned restart, a SIGKILL of gff-hab and the loss of the checkpoint with the
// same ids, and each command's start and end reach a subscriber as command.updated events.
func TestCommandMarks(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) { testCommandMarks(t, shell) })
	}
}

func testCommandMarks(t *testing.T, shell string) {
	e := newHabEnv(t, 0)
	cc := e.control()
	pane := e.markedShell(cc, shell)
	evs := commandEventsFor(t, e.control(), pane)
	work, err := filepath.EvalSymlinks(e.work)
	if err != nil {
		t.Fatal(err)
	}
	// Builtins only: a forked job can get the shell's own text in its output
	// (TestCommandMarksRing says when).
	steps := []struct {
		line, output string
		exit         int
	}{
		{"echo one", "one\r\n", 0},
		{"false", "", 1},
		{"printf 'two\\nthree\\n'; f() { return 3; }; f", "two\r\nthree\r\n", 3},
	}
	for i, s := range steps {
		// Type the next line only once the command before has ended: a line typed while a
		// command runs is echoed by the terminal into that command's output.
		call(t, cc, "pane.input", api.InputParams{Pane: pane, Text: s.line + "\r"}, nil)
		waitUntil(t, fmt.Sprintf("command %d to end", i+1), func() bool {
			cs := listCommands(t, cc, pane)
			return len(cs) == i+1 && cs[i].End != nil
		})
	}
	cs := listCommands(t, cc, pane)
	if len(cs) != 3 {
		t.Fatalf("got %d commands, want 3", len(cs))
	}
	for i, s := range steps {
		c := cs[i]
		if c.State != api.CommandStateDone || c.Exit == nil || *c.Exit != s.exit {
			t.Errorf("%q: state %s exit %v, want done %d", s.line, c.State, deref(c.Exit), s.exit)
		}
		if c.Prompt == nil || c.Input == nil || c.StartedAt == "" || c.EndedAt == "" {
			t.Errorf("%q: prompt, input or times missing: %+v", s.line, c)
		}
		if shell == "zsh" && c.Text != s.line {
			t.Errorf("%q: text %q", s.line, c.Text)
		}
		if got, _ := filepath.EvalSymlinks(c.Cwd); got != work {
			t.Errorf("%q: cwd %q, want %q", s.line, c.Cwd, work)
		}
		var out api.CommandOutput
		call(t, cc, "command.output", api.CommandOutputParams{Pane: pane, Command: c.ID}, &out)
		if string(out.Data) != s.output || out.Gap || out.More || out.Start != c.Output || out.End != *c.End {
			t.Errorf("%q: output %q [%d, %d) gap %v more %v; want %q [%d, %d)", s.line, out.Data, out.Start, out.End,
				out.Gap, out.More, s.output, c.Output, *c.End)
		}
	}
	// A bounded read goes on from where the last one ended.
	var part, rest api.CommandOutput
	call(t, cc, "command.output", api.CommandOutputParams{Pane: pane, Command: cs[2].ID, Limit: 5}, &part)
	from := part.End
	call(t, cc, "command.output", api.CommandOutputParams{Pane: pane, Command: cs[2].ID, From: &from}, &rest)
	if string(part.Data) != "two\r\n" || !part.More || string(rest.Data) != "three\r\n" || rest.More {
		t.Errorf("paged output %q (more %v), then %q (more %v)", part.Data, part.More, rest.Data, rest.More)
	}
	if msg := callErr(t, e.sock, "command.output", api.CommandOutputParams{Pane: pane, Command: cs[0].ID + 1}); msg == "" {
		t.Error("command.output of an unknown command did not fail")
	}

	// Each command's start and end arrive as events; the last event of each is its final state.
	last := map[uint64]api.Command{}
	waitUntil(t, "command.updated events for every end", func() bool {
	drain:
		for {
			select {
			case c := <-evs:
				last[c.ID] = c
			default:
				break drain
			}
		}
		for _, c := range cs {
			if !sameCommand(last[c.ID], c) {
				return false
			}
		}
		return true
	})

	// The list outlives gff-hab with the same ids and offsets. A planned restart restores it from
	// the checkpoint written at the stop, times included. The new gff-hab writes no checkpoint
	// for an hour, so the fourth command is found only in bytes replayed after a SIGKILL, and it
	// has no times there. With the checkpoint deleted, the whole list is rebuilt from the ring,
	// with no times at all.
	e.env = []string{"GFF_HAB_CHECKPOINT_EVERY=1h", "GFF_HAB_CHECKPOINT_IDLE=1h"}
	e.restart(false)
	c2 := e.control()
	waitForList(t, c2, pane, "the list after a planned restart", cs)
	call(t, c2, "pane.input", api.InputParams{Pane: pane, Text: "echo four\r"}, nil)
	waitUntil(t, "command 4 to end", func() bool {
		got := listCommands(t, c2, pane)
		return len(got) == 4 && got[3].End != nil
	})
	cs = listCommands(t, c2, pane)
	if ck := e.checkpointOffset(); ck >= *cs[3].End {
		t.Fatalf("a checkpoint at %d already holds command 4 (ends at %d)", ck, *cs[3].End)
	}
	four := cs[3]
	if four.State != api.CommandStateDone || deref(four.Exit) != 0 || four.StartedAt == "" {
		t.Fatalf("command 4 as live output: %+v", four)
	}
	four.StartedAt, four.EndedAt = "", ""
	e.restart(true)
	c3 := e.control()
	waitForList(t, c3, pane, "the list after a SIGKILL", append(cs[:3:3], four))

	e.stopHab(true)
	ckpts, _ := filepath.Glob(filepath.Join(e.state, "checkpoints", "*.ckpt"))
	if len(ckpts) != 1 {
		t.Fatalf("checkpoints: %v", ckpts)
	}
	if err := os.Remove(ckpts[0]); err != nil {
		t.Fatal(err)
	}
	e.serve()
	c4 := e.control()
	bare := make([]api.Command, len(cs))
	for i, c := range cs {
		c.StartedAt, c.EndedAt = "", ""
		bare[i] = c
	}
	waitForList(t, c4, pane, "the list rebuilt from the ring", bare)
	var out api.CommandOutput
	call(t, c4, "command.output", api.CommandOutputParams{Pane: pane, Command: cs[0].ID}, &out)
	if string(out.Data) != steps[0].output {
		t.Errorf("after the rebuild from the ring: output %q", out.Data)
	}
}

// waitForList waits until pane's command list is want: the same commands with the same
// started_at and ended_at.
func waitForList(t *testing.T, c *control.Client, pane, what string, want []api.Command) {
	t.Helper()
	same := func(got []api.Command) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if !sameCommand(got[i], want[i]) || got[i].StartedAt != want[i].StartedAt || got[i].EndedAt != want[i].EndedAt {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(scaleTimeout(15 * time.Second))
	for got := listCommands(t, c, pane); !same(got); got = listCommands(t, c, pane) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s:\n got %+v\nwant %+v", what, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkpointOffset returns the offset in the header of the one pane checkpoint in the state
// directory.
func (e *habEnv) checkpointOffset() uint64 {
	e.t.Helper()
	ckpts, _ := filepath.Glob(filepath.Join(e.state, "checkpoints", "*.ckpt"))
	if len(ckpts) != 1 {
		e.t.Fatalf("checkpoints: %v", ckpts)
	}
	b, err := os.ReadFile(ckpts[0])
	if err != nil {
		e.t.Fatal(err)
	}
	hdr, _, _ := bytes.Cut(b, []byte{'\n'})
	var h struct {
		Offset uint64 `json:"offset"`
	}
	if err := json.Unmarshal(hdr, &h); err != nil {
		e.t.Fatal(err)
	}
	return h.Offset
}

// TestCommandEventsDropped: a flush sends a pane's newest maxCommandEvents changes, and the first
// event it sends says how many older ones it left out. A flush that left none out says nothing.
func TestCommandEventsDropped(t *testing.T) {
	u := hp.UUID{1}
	var pending []cmdmarks.Command
	var sent []api.Command
	e := &commandEvents{dirty: map[string]bool{}, wake: make(chan struct{}, 1),
		changes: func(got hp.UUID) []cmdmarks.Command {
			if got != u {
				t.Errorf("changes for pane %s", got)
			}
			cs := pending
			pending = nil
			return cs
		},
		publish: func(ev structure.Event) { sent = append(sent, ev.Data.(api.Command)) }}
	for _, n := range []int{maxCommandEvents + 8, 3} {
		pending, sent = nil, nil
		for i := range n {
			pending = append(pending, cmdmarks.Command{ID: uint64(i), State: cmdmarks.Done})
		}
		e.notice(link.Notice{Kind: link.PaneCommands, Pane: link.PaneStatus{UUID: u}})
		e.flush()
		want := min(n, maxCommandEvents)
		if len(sent) != want || sent[0].ID != uint64(n-want) || sent[0].Dropped != n-want {
			t.Fatalf("%d changes: sent %d, the first is %d with dropped %d", n, len(sent), sent[0].ID, sent[0].Dropped)
		}
		for _, c := range sent[1:] {
			if c.Dropped != 0 {
				t.Fatalf("%d changes: event %d says dropped %d", n, c.ID, c.Dropped)
			}
		}
	}
}

// TestCommandMarksRing:a command whose output has left the holder's ring is dropped from the
// list, and a read of a command whose first bytes left the ring says gap and starts later.
//
// Both commands are bash builtins, so bash forks nothing. A forked job can get bash's own text in
// its output: under load on macOS, bash prints "child setpgid (...): Operation not permitted"
// when a job's child fails to set its process group. Those bytes are rightly the command's
// output, but they change its length.
func TestCommandMarksRing(t *testing.T) {
	e := newHabEnv(t, 64<<10)
	cc := e.control()
	pane := e.markedShell(cc, "bash")
	var prev uint64 // the id of the command before: ids are offsets, so a newer one is larger
	for i, step := range []struct {
		line string
		size uint64 // the output's length
	}{{"echo first", uint64(len("first\r\n"))}, {"printf 'x%.0s' {1..100000}", 100000}} {
		call(t, cc, "pane.input", api.InputParams{Pane: pane, Text: step.line + "\r"}, nil)
		// Wait for the command to end, whatever its length, then check the length: a wrong
		// length fails at once and says so, rather than waiting out the deadline.
		var c api.Command
		waitUntil(t, fmt.Sprintf("command %q to end", step.line), func() bool {
			cs := listCommands(t, cc, pane)
			if len(cs) == 0 {
				return false
			}
			c = cs[len(cs)-1]
			return c.End != nil && (i == 0 || c.ID > prev)
		})
		if n := *c.End - c.Output; n != step.size {
			t.Fatalf("command %q: %d bytes of output, want %d", step.line, n, step.size)
		}
		prev = c.ID
	}
	cs := listCommands(t, cc, pane)
	if len(cs) != 1 || *cs[0].End-cs[0].Output != 100000 {
		t.Fatalf("got %d commands, want only the long one: %+v", len(cs), cs)
	}
	var out api.CommandOutput
	call(t, cc, "command.output", api.CommandOutputParams{Pane: pane, Command: cs[0].ID}, &out)
	if !out.Gap || out.Start <= cs[0].Output || out.End != *cs[0].End || uint64(len(out.Data)) != out.End-out.Start {
		t.Fatalf("output [%d, %d) gap %v, %d bytes; want a gap and the tail up to %d", out.Start, out.End, out.Gap, len(out.Data), *cs[0].End)
	}
	for _, b := range out.Data {
		if b != 'x' {
			t.Fatalf("the output holds %q", b)
		}
	}
}
