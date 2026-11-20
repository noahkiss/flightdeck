package main

import (
	"context"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// TestPaneCapcomMarkReads: pane.read, pane.wait and pane.list take the capcom mark on the
// control door as pane.type does, only from the Capcom home's device; pane.new takes no mark.
// Under project the peer door may read nothing, so a marked call reaches the target lookup (an
// unknown session is not_found) where an unmarked one is denied.
func TestPaneCapcomMarkReads(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	home := "home-device"
	r.s.capcomHome = func() string { return home }
	r.s.rule = func() string { return readPanesProject }
	ctl := controlPanes{r.s}
	ctx := context.Background()
	marked := control.PaneCaller{Peer: true, Conn: "c1", Device: home}
	other := control.PaneCaller{Peer: true, Conn: "c2", Device: "intruder"}
	for _, c := range []struct {
		name string
		from control.PaneCaller
		want api.ErrorKind
	}{{"marked", marked, api.ErrorKindNotFound}, {"another peer", other, api.ErrorKindDenied}} {
		_, err := ctl.Read(ctx, c.from, api.PaneReadParams{Session: "ghost", As: asCapcom})
		if paneErrKind(err) != c.want {
			t.Errorf("read, %s: %v, want %s", c.name, err, c.want)
		}
		_, err = ctl.Wait(ctx, c.from, api.PaneWaitParams{Session: "ghost", QuietMS: 1, As: asCapcom})
		if paneErrKind(err) != c.want {
			t.Errorf("wait, %s: %v, want %s", c.name, err, c.want)
		}
	}
	// list: marked, a host label from a peer is "a forward never forwards"; unmarked, denied.
	if _, err := ctl.List(ctx, marked, api.PaneListParams{Host: "bee", As: asCapcom}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Errorf("list, marked: %v", err)
	}
	if _, err := ctl.List(ctx, other, api.PaneListParams{Host: "bee", As: asCapcom}); paneErrKind(err) != api.ErrorKindDenied {
		t.Errorf("list, another peer: %v", err)
	}
	// pane.new carries no mark: the Capcom home's peer door starts nothing under project.
	r.rule = readPanesProject
	if _, err := ctl.New(ctx, marked, api.PaneNewParams{Cwd: "/tmp"}); paneErrKind(err) != api.ErrorKindDenied {
		t.Errorf("new from the Capcom home's peer door: %v", err)
	}
	if len(r.created) != 0 {
		t.Fatal("a refused new created a pane")
	}
}

// TestPaneCapcomHelloDevice documents a limit (hab-logic-api.md 3.11): the far hab takes the
// device id from the peer's own hello, so a peer link that names the Capcom home's device gets
// Capcom's rule. It must move to the transport's key once a hab key may hold less than the
// owner's door.
func TestPaneCapcomHelloDevice(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	r.s.capcomHome = func() string { return "home-device" }
	c := r.s.markCapcom(controlCaller(control.PaneCaller{Peer: true, Conn: "any", Device: "home-device"}), "home-device", asCapcom)
	if !c.isCapcom() {
		t.Fatal("the hello's device id no longer decides the mark: update the spec's limit")
	}
}

// TestPaneCapcomRawGate: the raw line still passes the send gate: a person's recent input and
// an open permission request each refuse it with conflict, and nothing is written.
func TestPaneCapcomRawGate(t *testing.T) {
	t.Parallel()
	keys := &keyLog{}
	a := &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf}
	ctx := context.Background()
	a.agents.Hook("p1", claudeEnv("SessionStart", `{"session_id":"s","hook_event_name":"SessionStart"}`, 1))
	recent := func(string) bool { return true }
	if _, err := a.typeInto(ctx, "p1", typeWrite{text: "/compact", enter: true, agentDoor: true, raw: true, recent: recent}); paneErrKind(err) != api.ErrorKindConflict {
		t.Fatalf("a person typing: %v", err)
	}
	a.agents.Hook("p1", claudeEnv("PermissionRequest", `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`, 2))
	never := func(string) bool { return false }
	if _, err := a.typeInto(ctx, "p1", typeWrite{text: "/compact", enter: true, agentDoor: true, raw: true, recent: never}); paneErrKind(err) != api.ErrorKindConflict {
		t.Fatalf("an open permission request: %v", err)
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if len(keys.writes) != 0 {
		t.Fatalf("a refused raw line wrote %q", keys.writes)
	}
}
