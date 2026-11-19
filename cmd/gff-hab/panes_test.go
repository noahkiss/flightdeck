package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// Reading panes (hab-logic-api.md 3.11): gff-hab screen and gff-hab wait -pane against a real
// hab and holder, the agent.read_panes rule on the pane route and over a peer link, and a read
// of a peer's pane by label/name.

// runCode runs gff-hab with env added and returns its stdout and exit code.
func runCode(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatalf("gff-hab %v: %v", args, err)
	return "", -1
}

// setReadPanes sets agent.read_panes on the hab at sock.
func setReadPanes(t *testing.T, sock, value string) {
	t.Helper()
	var got api.SettingsResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "settings.get", `{"ids":["`+settings.AgentReadPanes+`"]}`)), &got); err != nil || len(got.Settings) != 1 {
		t.Fatalf("settings.get: %v %+v", err, got)
	}
	p, _ := json.Marshal(api.SettingsSetParams{Values: []api.SettingWrite{{ID: settings.AgentReadPanes,
		Value: json.RawMessage(strconv.Quote(value)), Revision: got.Settings[0].Revision}}})
	run(t, "api", "call", "-socket", sock, "settings.set", string(p))
}

// shellSession files an interactive shell named name on e, with a plain prompt.
func shellSession(t *testing.T, e *habEnv, name string) api.SessionCreated {
	t.Helper()
	return createSession(t, e.sock, api.SessionCreateParams{Name: name, Argv: []string{"/bin/sh"}, Cwd: "/",
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "PS1=$ ", "ENV=", noRaceExitSleep}})
}

// TestPaneReadWait drives a shell from outside: wait for its prompt, type a command, wait for
// its output by pattern, read the screen; then wait by quiet, by timeout and by exit, each with
// its exit code (hab-logic-api.md 3.11).
func TestPaneReadWait(t *testing.T) {
	t.Parallel()
	e := newHabEnv(t, 0)
	if w := welcome(t, e.sock); !slices.Contains(w.Capabilities, paneReadCapability) {
		t.Fatalf("capabilities %v lack %s", w.Capabilities, paneReadCapability)
	}
	ss := shellSession(t, e, "work")
	sock := []string{"-socket", e.sock}
	// The prompt, from the pane's first byte.
	out, code := runCode(t, nil, append([]string{"wait"}, append(sock, "-pane", "work", "-match", "$ ", "-from", "0", "-timeout", "20s")...)...)
	if code != 0 || !strings.HasPrefix(out, "match ") || !strings.HasSuffix(out, "\n$ \n") {
		t.Fatalf("wait for the prompt: %d %q", code, out)
	}
	var before api.PaneText
	out, _ = runCode(t, nil, append([]string{"screen", "-json"}, append(sock, "work")...)...)
	if err := json.Unmarshal([]byte(out), &before); err != nil || before.Pane != ss.Pane || before.Session != "work" {
		t.Fatalf("screen -json: %v %q", err, out)
	}
	// Type, then wait from the read's offset: output that came meanwhile still counts. The
	// echo of the command line does not match the anchored pattern.
	run(t, "api", "call", "-socket", e.sock, "pane.input", `{"pane":"`+ss.Pane+`","text":"echo \"do\"\"ne\"; printf '\\346\\227\\245\\346\\234\\254 wide\\n'\r"}`)
	out, code = runCode(t, nil, append([]string{"wait"}, append(sock, "-pane", ss.Pane, "-pattern", "^done$", "-from", strconv.FormatUint(before.Offset, 10), "-timeout", "20s")...)...)
	if code != 0 || !strings.HasPrefix(out, "match ") || !strings.HasSuffix(out, "\ndone\n") {
		t.Fatalf("wait for done: %d %q", code, out)
	}
	waitUntil(t, "the wide row on the screen", func() bool {
		out, _ := runCode(t, nil, append([]string{"screen"}, append(sock, "work")...)...)
		return strings.Contains(out, "日本 wide\n")
	})
	out, _ = runCode(t, nil, append([]string{"screen"}, append(sock, "work")...)...)
	if !strings.Contains(out, "$ echo \"do\"\"ne\";") || !strings.Contains(out, "\ndone\n") || strings.ContainsAny(out, "\x1b\r") {
		t.Fatalf("screen:\n%q", out)
	}
	// Quiet: nothing more prints.
	if out, code := runCode(t, nil, append([]string{"wait"}, append(sock, "-pane", "work", "-quiet", "300ms", "-timeout", "20s")...)...); code != 0 || !strings.HasPrefix(out, "quiet ") {
		t.Fatalf("wait -quiet: %d %q", code, out)
	}
	// Timeout: exit code 4, and the reason on stdout.
	if out, code := runCode(t, nil, append([]string{"wait"}, append(sock, "-pane", "work", "-match", "never printed", "-timeout", "500ms")...)...); code != 4 || !strings.HasPrefix(out, "timed_out ") {
		t.Fatalf("wait timeout: %d %q", code, out)
	}
	// Bad input is refused by the hab: exit code 3.
	if _, code := runCode(t, nil, append([]string{"wait"}, append(sock, "-pane", "work", "-pattern", "(", "-timeout", "1s")...)...); code != 3 {
		t.Fatalf("bad pattern: exit %d, want 3", code)
	}
	if _, code := runCode(t, nil, append([]string{"screen"}, append(sock, "-scrollback", "10001", "work")...)...); code != 3 {
		t.Fatalf("scrollback 10001: exit %d, want 3", code)
	}
	if _, code := runCode(t, nil, append([]string{"screen"}, append(sock, "nobody")...)...); code != 3 {
		t.Fatalf("unknown session: exit %d, want 3", code)
	}
	// Exit: code 5. The wait starts from an offset read before the exit, so it cannot miss it.
	out, _ = runCode(t, nil, append([]string{"screen", "-json"}, append(sock, "work")...)...)
	if err := json.Unmarshal([]byte(out), &before); err != nil {
		t.Fatal(err)
	}
	run(t, "api", "call", "-socket", e.sock, "pane.input", `{"pane":"`+ss.Pane+`","text":"exit\r"}`)
	out, code = runCode(t, nil, append([]string{"wait"}, append(sock, "-pane", ss.Pane, "-match", "never printed", "-from", strconv.FormatUint(before.Offset, 10), "-timeout", "20s")...)...)
	if code != 5 || !strings.HasPrefix(out, "exited ") {
		t.Fatalf("wait on an exit: %d %q", code, out)
	}
}

// TestPaneReadRule: from a pane's route, agent.read_panes decides what the agent may read; the
// owner's socket reads at every level (hab-logic-api.md 3.11).
func TestPaneReadRule(t *testing.T) {
	t.Parallel()
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, e.work+"/end", "") })
	addr := startSender(t, e) // alice: an unfiled session that keeps its GFF_HAB
	shellSession(t, e, "work")
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	route := []string{"GFF_HAB=" + addr}
	readFromAlice := func(target string) (string, int) {
		return runCode(t, route, "screen", target)
	}
	// all (Balanced, the default): alice reads work.
	if _, code := readFromAlice("work"); code != 0 {
		t.Fatalf("all: exit %d", code)
	}
	for _, rule := range []string{readPanesProject, readPanesOff} {
		setReadPanes(t, e.sock, rule)
		var r api.PaneText
		err := hook.Call(ctx, addr, "pane.read", api.PaneReadParams{Session: "work"}, &r)
		var ce *control.Error
		if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied || !strings.Contains(ce.Message, "agent.read_panes is "+rule) || strings.Contains(ce.Message, "work") {
			t.Fatalf("%s: read of work: %v", rule, err)
		}
		var w api.PaneWaitResult
		if err := hook.Call(ctx, addr, "pane.wait", api.PaneWaitParams{Session: "work", QuietMS: 10}, &w); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied {
			t.Fatalf("%s: wait on work: %v", rule, err)
		}
		if _, code := readFromAlice("work"); code != 3 {
			t.Fatalf("%s: gff-hab screen from the pane: exit %d, want 3", rule, code)
		}
		// Its own pane, always.
		if err := hook.Call(ctx, addr, "pane.read", api.PaneReadParams{Session: "alice"}, &r); err != nil || r.Session != "alice" {
			t.Fatalf("%s: read of its own pane: %v", rule, err)
		}
		// The owner's socket, always.
		if out, code := runCode(t, nil, "screen", "-socket", e.sock, "work"); code != 0 || !strings.Contains(out, "$") {
			t.Fatalf("%s: owner read: %d %q", rule, code, out)
		}
	}
	setReadPanes(t, e.sock, readPanesAll)
	if _, code := readFromAlice("work"); code != 0 {
		t.Fatalf("all again: exit %d", code)
	}
}

// TestPaneReadPeer: an agent on hab a reads and waits on a session on hab b by label/name, over
// the peer link; b's own rule decides (hab-logic-api.md 3.11).
func TestPaneReadPeer(t *testing.T) {
	t.Parallel()
	p := newMeshPair(t)
	createSession(t, p.b.sock, api.SessionCreateParams{Name: "far", Argv: []string{"/bin/sh", "-c", "printf 'peer text\\n'; exec sleep 1000"},
		Cwd: "/", Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	var r api.PaneText
	waitUntil(t, "the peer's text", func() bool {
		out, code := runCode(t, nil, "screen", "-json", "-socket", p.a.sock, "bee/far")
		return code == 0 && json.Unmarshal([]byte(out), &r) == nil && slices.Contains(r.Screen, "peer text")
	})
	if r.Session != "bee/far" {
		t.Fatalf("session %q, want bee/far", r.Session)
	}
	if out, code := runCode(t, nil, "wait", "-socket", p.a.sock, "-pane", "bee/far", "-quiet", "200ms", "-timeout", "20s"); code != 0 || !strings.HasPrefix(out, "quiet ") {
		t.Fatalf("peer wait: %d %q", code, out)
	}
	// A pane id is never forwarded; a label that is not a peer is not found.
	if _, code := runCode(t, nil, "screen", "-socket", p.a.sock, "nope/far"); code != 3 {
		t.Fatalf("unknown label: exit %d, want 3", code)
	}
	// Under project, b refuses every read from a peer link.
	setReadPanes(t, p.b.sock, readPanesProject)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	c := p.a.control()
	err := c.Call(ctx, "pane.read", api.PaneReadParams{Session: "bee/far"}, &r)
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied {
		t.Fatalf("peer read under project: %v", err)
	}
}

// TestMayRead is the rule's table (hab-logic-api.md 3.11).
func TestMayRead(t *testing.T) {
	t.Parallel()
	own := "00000000-0000-0000-0000-000000000001"
	other := "00000000-0000-0000-0000-000000000002"
	inP := paneTarget{pane: other, session: api.Session{Project: "p1"}, filed: true}
	outP := paneTarget{pane: other, session: api.Session{Project: "p2"}, filed: true}
	unfiled := paneTarget{pane: other}
	self := paneTarget{pane: own}
	route := paneCaller{door: doorRoute, pane: own}
	for _, c := range []struct {
		rule    string
		caller  paneCaller
		project string
		target  paneTarget
		want    bool
	}{
		{readPanesAll, route, "", outP, true},
		{readPanesProject, route, "p1", inP, true},
		{readPanesProject, route, "p1", outP, false},
		{readPanesProject, route, "p1", unfiled, false},
		{readPanesProject, route, "", inP, false},
		{readPanesProject, route, "", self, true},
		{readPanesOff, route, "p1", inP, false},
		{readPanesOff, route, "p1", self, true},
		{readPanesAll, paneCaller{door: doorPeer}, "", outP, true},
		{readPanesProject, paneCaller{door: doorPeer}, "", inP, false},
		{readPanesOff, paneCaller{door: doorPeer}, "", inP, false},
		{readPanesOff, paneCaller{door: doorOwner}, "", outP, true},
		{readPanesProject, paneCaller{door: doorRoute, pane: capcomRoute}, "", inP, false},
	} {
		if got := mayRead(c.rule, c.caller, c.project, c.target); got != c.want {
			t.Errorf("mayRead(%s, door %d, project %q, %+v) = %v, want %v", c.rule, c.caller.door, c.project, c.target, got, c.want)
		}
	}
}

// TestCapRows keeps the whole screen and the newest scrollback rows within the cap.
func TestCapRows(t *testing.T) {
	t.Parallel()
	screen := []string{"aaaa", "bb"}
	sb := []string{"1111", "2222", "3333"}
	if got, cut := capRows(screen, sb, 14); !slices.Equal(got, []string{"2222", "3333"}) || !cut {
		t.Fatalf("cap 14: %q %v", got, cut)
	}
	if got, cut := capRows(screen, sb, 100); len(got) != 3 || cut {
		t.Fatalf("cap 100: %q %v", got, cut)
	}
	if got, cut := capRows(screen, sb, 3); len(got) != 0 || !cut {
		t.Fatalf("cap 3: %q %v", got, cut)
	}
}
