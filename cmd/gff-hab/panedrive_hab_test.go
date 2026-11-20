package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// Driving panes against real habs and holders (hab-logic-api.md 3.12): gff-hab panes, type
// and new from a pane's route, the rules, the gate after a person's input, and a peer's panes
// by label/name.

// setWord sets a hab-side setting on the hab at sock.
func setWord(t *testing.T, sock, id, value string) {
	t.Helper()
	var got api.SettingsResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "settings.get", `{"ids":["`+id+`"]}`)), &got); err != nil || len(got.Settings) != 1 {
		t.Fatalf("settings.get: %v %+v", err, got)
	}
	p, _ := json.Marshal(api.SettingsSetParams{Values: []api.SettingWrite{{ID: id,
		Value: json.RawMessage(strconv.Quote(value)), Revision: got.Settings[0].Revision}}})
	run(t, "api", "call", "-socket", sock, "settings.set", string(p))
}

// listPanes runs gff-hab panes -json with env and args added.
func listPanes(t *testing.T, env []string, args ...string) (api.PaneList, int) {
	t.Helper()
	out, code := runCode(t, env, append([]string{"panes", "-json"}, args...)...)
	var l api.PaneList
	if code == 0 {
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			t.Fatalf("panes -json: %v %q", err, out)
		}
	}
	return l, code
}

func rowOf(l api.PaneList, session string) (api.PaneListEntry, bool) {
	i := slices.IndexFunc(l.Panes, func(p api.PaneListEntry) bool { return p.Session == session })
	if i < 0 {
		return api.PaneListEntry{}, false
	}
	return l.Panes[i], true
}

// TestPaneDrive: an agent in one pane lists the panes, types a command into a shell in another,
// waits for its output and reads it, starts a pane, and is refused a type right after a
// person's input; agent.read_panes filters the list and agent.drive_panes gates the type.
func TestPaneDrive(t *testing.T) {
	t.Parallel()
	e := newHolderEnv(t, 0)
	e.env = []string{"GFF_TEST_TYPE_QUIET=60s"}
	e.serve()
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	if w := welcome(t, e.sock); !slices.Contains(w.Capabilities, paneDriveCapability) {
		t.Fatalf("capabilities %v lack %s", w.Capabilities, paneDriveCapability)
	}
	addr := startSender(t, e) // alice, unfiled
	ss := shellSession(t, e, "work")
	route := []string{"GFF_HAB=" + addr}

	// The list from alice's pane: its own pane, marked, and work.
	l, code := listPanes(t, route)
	me, ok1 := rowOf(l, "alice")
	work, ok2 := rowOf(l, "work")
	if code != 0 || !ok1 || !ok2 || !me.Own || work.Own || work.Pane != ss.Pane || work.State == "" {
		t.Fatalf("panes: %d %+v", code, l)
	}
	if out, code := runCode(t, route, "panes"); code != 0 || !strings.Contains(out, "alice (you)") || !strings.Contains(out, ss.Pane) {
		t.Fatalf("panes: %d %q", code, out)
	}
	// Under read off: its own pane only, and no hint of the others.
	setReadPanes(t, e.sock, readPanesOff)
	if l, code := listPanes(t, route); code != 0 || len(l.Panes) != 1 || !l.Panes[0].Own {
		t.Fatalf("read off: %d %+v", code, l)
	}
	if l, code := listPanes(t, nil, "-socket", e.sock); code != 0 || len(l.Panes) < 2 {
		t.Fatalf("read off, the owner: %d %+v", code, l)
	}
	setReadPanes(t, e.sock, readPanesAll)

	// drive project (Balanced, the default): alice is in no project, so it types nothing.
	if _, code := runCode(t, route, "type", "-enter", "work", "true"); code != 3 {
		t.Fatalf("drive project, unfiled caller: exit %d, want 3", code)
	}
	if _, code := runCode(t, route, "new", "--", "/bin/sh"); code != 3 {
		t.Fatalf("drive project, unfiled caller, new: exit %d, want 3", code)
	}
	setWord(t, e.sock, settings.AgentDrivePanes, readPanesAll)

	// Run a command in work and read its output, all from alice's pane.
	if out, code := runCode(t, route, "wait", "-pane", "work", "-match", "$ ", "-from", "0", "-timeout", "20s"); code != 0 {
		t.Fatalf("wait for the prompt: %d %q", code, out)
	}
	var before api.PaneText
	out, _ := runCode(t, route, "screen", "-json", "work")
	if err := json.Unmarshal([]byte(out), &before); err != nil {
		t.Fatalf("screen -json: %v %q", err, out)
	}
	if _, code := runCode(t, route, "type", "-enter", "work", `echo "do""ne"`); code != 0 {
		t.Fatalf("type: exit %d", code)
	}
	out, code = runCode(t, route, "wait", "-pane", "work", "-pattern", "^done$", "-from", strconv.FormatUint(before.Offset, 10), "-timeout", "20s")
	if code != 0 || !strings.HasSuffix(out, "\ndone\n") {
		t.Fatalf("wait for done: %d %q", code, out)
	}
	if out, _ := runCode(t, route, "screen", "work"); !strings.Contains(out, "\ndone\n") {
		t.Fatalf("screen:\n%s", out)
	}
	// Text from stdin.
	cmd := exec.Command(bin, "type", "-enter", "work", "-")
	cmd.Env = append(os.Environ(), route...)
	cmd.Stdin = strings.NewReader(`echo "pi""ped"`)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("type from stdin: %v %s", err, b)
	}
	if out, code := runCode(t, route, "wait", "-pane", "work", "-pattern", "^piped$", "-from", strconv.FormatUint(before.Offset, 10), "-timeout", "20s"); code != 0 {
		t.Fatalf("wait for piped: %d %q", code, out)
	}

	// Start a pane from alice's pane: it lists, and a type reaches it.
	var made api.PaneStarted
	out, code = runCode(t, route, "new", "-json", "-name", "fresh", "--", "/bin/sh")
	if err := json.Unmarshal([]byte(out), &made); code != 0 || err != nil || made.Name != "fresh" || made.Pane == "" {
		t.Fatalf("new: %d %v %q", code, err, out)
	}
	if l, _ := listPanes(t, route); !slices.ContainsFunc(l.Panes, func(p api.PaneListEntry) bool { return p.Pane == made.Pane }) {
		t.Fatalf("the new pane is not listed: %+v", l)
	}

	// A person types into work: a type within the quiet time is refused with conflict, and
	// nothing is written.
	run(t, "api", "call", "-socket", e.sock, "pane.input", `{"pane":"`+ss.Pane+`","text":" "}`)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var typed api.PaneTyped
	err := hook.Call(ctx, addr, "pane.type", api.PaneTypeParams{Session: "work", Text: "echo never"}, &typed)
	if paneErrKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "person comes first") {
		t.Fatalf("type after a person's input: %v", err)
	}
	// The rule is checked before the gate: drive off refuses with denied.
	setWord(t, e.sock, settings.AgentDrivePanes, readPanesOff)
	if err := hook.Call(ctx, addr, "pane.type", api.PaneTypeParams{Session: "work", Text: "x"}, &typed); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("drive off: %v", err)
	}
}

// TestPaneDrivePeer: from a's owner socket and from an agent on a, b's panes list with b's
// label, and a type reaches b's shell by label/name. a's own rule gates the forward; b's rule
// refuses its peer door under project; under read project the list leaves b out.
func TestPaneDrivePeer(t *testing.T) {
	t.Parallel()
	p := newMeshPair(t)
	shellSession(t, p.b, "far")
	addr := startSender(t, p.a)
	route := []string{"GFF_HAB=" + addr}
	for _, sock := range []string{p.a.sock, p.b.sock} {
		setWord(t, sock, settings.AgentDrivePanes, readPanesAll)
	}
	var row api.PaneListEntry
	waitUntil(t, "b's pane in a's list", func() bool {
		l, code := listPanes(t, route)
		r, ok := rowOf(l, "far")
		row = r
		return code == 0 && ok
	})
	if row.Host != "bee" {
		t.Fatalf("far's host %q, want bee", row.Host)
	}
	if l, code := listPanes(t, nil, "-socket", p.a.sock, "-host", "bee"); code != 0 || len(l.Panes) == 0 || l.Panes[0].Host != "bee" {
		t.Fatalf("owner, -host bee: %d %+v", code, l)
	}
	// Type into far from alice on a, and see it run on b.
	if out, code := runCode(t, nil, "wait", "-socket", p.b.sock, "-pane", "far", "-match", "$ ", "-from", "0", "-timeout", "20s"); code != 0 {
		t.Fatalf("far's prompt: %d %q", code, out)
	}
	var before api.PaneText
	out, _ := runCode(t, route, "screen", "-json", "bee/far")
	if err := json.Unmarshal([]byte(out), &before); err != nil {
		t.Fatalf("screen bee/far: %v %q", err, out)
	}
	if out, code := runCode(t, route, "type", "-enter", "-json", "bee/far", `echo "fa""r"`); code != 0 || !strings.Contains(out, `"session":"bee/far"`) {
		t.Fatalf("type bee/far: %d %q", code, out)
	}
	if out, code := runCode(t, route, "wait", "-pane", "bee/far", "-pattern", "^far$", "-from", strconv.FormatUint(before.Offset, 10), "-timeout", "20s"); code != 0 {
		t.Fatalf("wait bee/far: %d %q", code, out)
	}
	// a's rule gates the forward.
	setWord(t, p.a.sock, settings.AgentDrivePanes, readPanesProject)
	if _, code := runCode(t, route, "type", "bee/far", "x"); code != 3 {
		t.Fatalf("near project: exit %d, want 3", code)
	}
	setWord(t, p.a.sock, settings.AgentDrivePanes, readPanesAll)
	// b's rule refuses its peer door under project.
	setWord(t, p.b.sock, settings.AgentDrivePanes, readPanesProject)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var typed api.PaneTyped
	if err := hook.Call(ctx, addr, "pane.type", api.PaneTypeParams{Session: "bee/far", Text: "x"}, &typed); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("far project, peer door: %v", err)
	}
	// Under read project on a, the list from the route leaves b out, and -host bee is denied.
	setReadPanes(t, p.a.sock, readPanesProject)
	if l, code := listPanes(t, route); code != 0 || slices.ContainsFunc(l.Panes, func(p api.PaneListEntry) bool { return p.Host == "bee" }) {
		t.Fatalf("read project: %d %+v", code, l)
	}
	if _, code := listPanes(t, route, "-host", "bee"); code != 3 {
		t.Fatalf("read project, -host bee: exit %d, want 3", code)
	}
}
