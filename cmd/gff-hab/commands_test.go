package main

// End-to-end tests of the commands found by OSC 133 marks (hab-logic-api.md 2.5): a real holder,
// gff-hab and a real interactive shell that emits the marks (testdata/marks).

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// markedShell starts an interactive shell with the test mark snippet and returns the pane id. It
// skips the test when the shell is not installed.
func (e *habEnv) markedShell(c *control.Client, shell string) string {
	e.t.Helper()
	path, err := exec.LookPath(shell)
	if err != nil {
		e.t.Skipf("%s is not installed", shell)
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
// list survives a planned restart and a SIGKILL of gff-hab with the same ids, and each command's
// start and end reach a subscriber as command.updated events.
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
	steps := []struct {
		line, output string
		exit         int
	}{
		{"echo one", "one\r\n", 0},
		{"false", "", 1},
		{"printf 'two\\nthree\\n'; (exit 3)", "two\r\nthree\r\n", 3},
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

	// The list outlives gff-hab: a planned restart restores it from the checkpoint, a SIGKILL
	// from the checkpoint and the ring. The ids and offsets stay the same.
	for _, kill := range []bool{false, true} {
		e.restart(kill)
		c2 := e.control()
		waitUntil(t, fmt.Sprintf("the list after a restart (kill %v)", kill), func() bool {
			got := listCommands(t, c2, pane)
			if len(got) != len(cs) {
				return false
			}
			for i := range got {
				if !sameCommand(got[i], cs[i]) {
					return false
				}
			}
			return true
		})
		var out api.CommandOutput
		call(t, c2, "command.output", api.CommandOutputParams{Pane: pane, Command: cs[0].ID}, &out)
		if string(out.Data) != steps[0].output {
			t.Errorf("after a restart (kill %v): output %q", kill, out.Data)
		}
	}
}

// TestCommandMarksRing: a command whose output has left the holder's ring is dropped from the
// list, and a read of a command whose first bytes left the ring says gap and starts later.
func TestCommandMarksRing(t *testing.T) {
	e := newHabEnv(t, 64<<10)
	cc := e.control()
	pane := e.markedShell(cc, "bash")
	for _, step := range []struct {
		line string
		size uint64 // the output's length
	}{{"echo first", uint64(len("first\r\n"))}, {"head -c 100000 /dev/zero | tr '\\0' x", 100000}} {
		call(t, cc, "pane.input", api.InputParams{Pane: pane, Text: step.line + "\r"}, nil)
		waitUntil(t, fmt.Sprintf("command %q to end", step.line), func() bool {
			cs := listCommands(t, cc, pane)
			last := cs[max(len(cs)-1, 0):]
			return len(last) == 1 && last[0].End != nil && *last[0].End-last[0].Output == step.size
		})
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
