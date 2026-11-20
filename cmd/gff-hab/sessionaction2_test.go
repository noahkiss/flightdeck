package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// doorRig is the drive rig with a session action service over the action rig's fakes.
func doorRig(t *testing.T) (*driveRig, *actionRig) {
	dr := newDriveRig(t)
	ar := newActionRig(t)
	ar.acts.store = dr.s.store
	dr.s.acts = ar.acts
	return dr, ar
}

// TestSessionActionReauthorize: an action Capcom queued is checked again just before it runs. A
// rule turned off during the wait, or an agent that left the pane, ends it as failed, denied,
// and nothing runs (hab-logic-api.md 3.13).
func TestSessionActionReauthorize(t *testing.T) {
	t.Parallel()
	dr, ar := doorRig(t)
	ctx := context.Background()
	ar.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	dr.setRule(drivePanesAgents)
	if _, err := dr.s.Action(ctx, fromCap, api.SessionActionParams{Session: "other", Action: "close"}); err != nil {
		t.Fatal(err)
	}
	ar.next(t, actionQueued)
	dr.setRule(readPanesOff)
	ar.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	if f := ar.next(t, actionFailed); !strings.Contains(f.Reason, "denied") {
		t.Fatalf("rule off during the wait: %+v", f)
	}
	// The agent left during the wait: the user's shell is in the pane now.
	dr.setRule(readPanesAll)
	ar.agents.set(paneMate, api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	if _, err := dr.s.Action(ctx, fromOwner, api.SessionActionParams{Session: "mate", Action: "close"}); err != nil {
		t.Fatal(err)
	}
	ar.next(t, actionQueued)
	ar.agents.set(paneMate, api.AgentState{Harness: "claude", State: api.AgentStatusExited})
	if f := ar.next(t, actionFailed); !strings.Contains(f.Reason, "agent left") {
		t.Fatalf("agent left: %+v", f)
	}
	if got := ar.took(); len(got) != 0 {
		t.Fatalf("ran %v", got)
	}
	// compact: the rule is checked again under the send's claim.
	ar.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	dr.setRule(drivePanesAgents)
	if _, err := dr.s.Action(ctx, fromCap, api.SessionActionParams{Session: "other", Action: "compact"}); err != nil {
		t.Fatal(err)
	}
	ar.next(t, actionQueued)
	dr.setRule(readPanesOff)
	ar.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	if f := ar.next(t, actionFailed); !strings.Contains(f.Reason, "agent.drive_panes is off") {
		t.Fatalf("compact after the rule went off: %+v", f)
	}
	if s := ar.sends(); len(s) != 0 {
		t.Fatalf("sent %v", s)
	}
}

// liarPeer answers session.action with control characters in every field.
type liarPeer struct{}

func (liarPeer) Call(_ context.Context, _ string, _, result any) error {
	b, _ := json.Marshal(api.SessionAction{ID: "act-1\x1b[2J", Action: "\x1b]0;evil\x07", Harness: "cla\x1bude", State: "queued\x07"})
	return json.Unmarshal(b, result)
}

// TestSessionActionPeerAnswer: a peer's answer is untrusted: its fields lose control characters,
// and the action is the one asked for.
func TestSessionActionPeerAnswer(t *testing.T) {
	t.Parallel()
	dr, _ := doorRig(t)
	dr.peer.Caller = liarPeer{}
	dr.peer.Caps = append(dr.peer.Caps, sessionActionCapability)
	r, err := dr.s.Action(context.Background(), fromCap, api.SessionActionParams{Session: "bee/far", Action: "compact"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Action != "compact" || strings.ContainsAny(r.ID+r.Harness+r.State, "\x1b\x07") || r.Session != "bee/far" {
		t.Fatalf("answer %+v", r)
	}
}

// TestSessionActionDoorWiring: the control door takes the capcom mark only from the Capcom home;
// the route's handler serves Capcom's own route; session_action is listed only by mcp -capcom.
func TestSessionActionDoorWiring(t *testing.T) {
	t.Parallel()
	dr, ar := doorRig(t)
	dr.s.capcomHome = func() string { return "home" }
	ctx := context.Background()
	ar.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	dr.setRule(drivePanesAgents)
	ctl := controlPanes{dr.s}
	p := api.SessionActionParams{Session: "other", Action: "close", As: asCapcom}
	if _, err := ctl.Action(ctx, control.PaneCaller{Peer: true, Conn: "c1", Device: "intruder"}, p); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("a peer that is not the Capcom home: %v", err)
	}
	if _, err := ctl.Action(ctx, control.PaneCaller{Peer: true, Conn: "c1", Device: "home"}, p); err != nil {
		t.Fatalf("the Capcom home's forward: %v", err)
	}
	ar.next(t, actionQueued)
	ar.acts.Close() // end the pending close before the route's call
	ar2 := newActionRig(t)
	ar2.acts.store = dr.s.store
	dr.s.acts = ar2.acts
	ar2.agents.set(paneOther, api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	params, _ := json.Marshal(api.SessionActionParams{Session: "other", Action: "restart"})
	if _, _, err := paneHook(dr.s)(ctx, capcomRoute, "session.action", params); err != nil {
		t.Fatalf("Capcom's route: %v", err)
	}
	has := func(m *mcpServer) bool {
		for _, tl := range m.tools() {
			if tl.Name == "session_action" {
				return true
			}
		}
		return false
	}
	if has(&mcpServer{}) || !has(&mcpServer{capcom: true}) {
		t.Fatal("session_action is listed outside mcp -capcom, or not inside it")
	}
}
