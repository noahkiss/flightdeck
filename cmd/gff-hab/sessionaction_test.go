package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Session actions (hab-logic-api.md 3.13; capcom-service.md 18 A10): compact and clear on a
// Claude Code pane with their end seen, unavailable for a harness without a translation,
// restart, close and recreate after the turn ends, one per session, and who may run one.

// actionRig is a sessionActionService over the compaction rig's fakes, with recording fakes
// for restart, close and recreate.
type actionRig struct {
	*compactRig
	acts  *sessionActionService
	evCh  chan api.SessionAction
	mu    sync.Mutex
	calls []string
}

func newActionRig(t *testing.T) *actionRig {
	cr := newCompactRig(t)
	r := &actionRig{compactRig: cr, evCh: make(chan api.SessionAction, 64)}
	r.acts = newSessionActions(nil, cr.agents, func(e structure.Event) {
		a := e.Data.(api.SessionAction)
		if e.Type != api.EventSessionAction || e.ID != a.ID {
			t.Errorf("event %s %s", e.Type, e.ID)
		}
		r.evCh <- a
	}, t.Logf)
	r.acts.compacts = cr.svc
	rec := func(s string) {
		r.mu.Lock()
		r.calls = append(r.calls, s)
		r.mu.Unlock()
	}
	r.acts.restart = func(_ context.Context, session string) (string, error) { rec("restart " + session); return "", nil }
	r.acts.closePane = func(_ context.Context, pane string) error { rec("close pane " + pane); return nil }
	r.acts.closeSession = func(session string) error { rec("close session " + session); return nil }
	r.acts.recreate = func(_ context.Context, ss api.Session, st api.AgentState, seed, _ string) (string, error) {
		rec("recreate " + ss.Name + " " + st.Harness + " " + seed)
		return "new-id", nil
	}
	t.Cleanup(r.acts.Close)
	return r
}

func (r *actionRig) next(t *testing.T, state string) api.SessionAction {
	t.Helper()
	select {
	case a := <-r.evCh:
		if a.State != state {
			t.Fatalf("state %s (%s), want %s", a.State, a.Reason, state)
		}
		return a
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s event", state)
	}
	return api.SessionAction{}
}

func (r *actionRig) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func liveSession(pane string) api.Session {
	return api.Session{ID: "sess-" + pane, Name: "lead-" + pane, Pane: pane, State: api.SessionStateRunning}
}

// TestSessionActionClear: clear sends /clear once the turn ends, ends at SessionStart with
// source clear (not compact), then sends the follow-up; no agent.compact event goes.
func TestSessionActionClear(t *testing.T) {
	t.Parallel()
	r := newActionRig(t)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	a, err := r.acts.Request(liveSession("p1"), "clear", "read handoff.md and continue", actionOpts{})
	if err != nil || a.State != actionQueued || a.Harness != "claude" {
		t.Fatalf("request %+v %v", a, err)
	}
	r.next(t, actionQueued)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	r.next(t, actionSent)
	if s := r.sends(); len(s) != 1 || s[0] != "sess-p1: /clear" {
		t.Fatalf("sends %v", s)
	}
	r.compactRig.svc.Observe("p1", hookEnv("SessionStart", `{"session_id":"n1","source":"compact"}`))
	time.Sleep(50 * time.Millisecond)
	if len(r.evCh) != 0 {
		t.Fatal("a compaction's end ended a clear")
	}
	r.compactRig.svc.Observe("p1", hookEnv("SessionStart", `{"session_id":"n2","source":"clear"}`))
	r.next(t, actionRunning)
	done := r.next(t, actionDone)
	if done.ID != a.ID {
		t.Fatalf("done %+v", done)
	}
	if s := r.sends(); len(s) != 2 || s[1] != "sess-p1: read handoff.md and continue" {
		t.Fatalf("sends %v", s)
	}
	r.compactRig.mu.Lock()
	n := len(r.compactRig.events)
	r.compactRig.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d agent.compact events for a session action", n)
	}
}

// TestSessionActionCompact: compact through the same path, ended by its SessionStart.
func TestSessionActionCompact(t *testing.T) {
	t.Parallel()
	r := newActionRig(t)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	if _, err := r.acts.Request(liveSession("p1"), "compact", "", actionOpts{}); err != nil {
		t.Fatal(err)
	}
	r.next(t, actionQueued)
	r.next(t, actionSent)
	r.compactRig.svc.Observe("p1", hookEnv("PreCompact", `{"session_id":"n1","trigger":"manual"}`))
	r.next(t, actionRunning)
	r.compactRig.svc.Observe("p1", hookEnv("SessionStart", `{"session_id":"n1","source":"compact"}`))
	r.next(t, actionDone)
	if s := r.sends(); len(s) != 1 || s[0] != "sess-p1: /compact" {
		t.Fatalf("sends %v", s)
	}
}

// TestSessionActionRefusals: compact and clear answer unavailable for a harness without a
// translation and for a pane with no agent; one action per session; a bad action or a
// follow-up with the wrong action is invalid.
func TestSessionActionRefusals(t *testing.T) {
	t.Parallel()
	r := newActionRig(t)
	r.agents.set("cx", api.AgentState{Harness: "codex", State: api.AgentStatusIdle})
	r.agents.set("oc", api.AgentState{Harness: "opencode", State: api.AgentStatusIdle})
	for _, c := range []struct {
		pane   string
		action api.SessionActionKind
	}{{"cx", "compact"}, {"cx", "clear"}, {"oc", "compact"}, {"oc", "clear"}, {"none", "clear"}} {
		_, err := r.acts.Request(liveSession(c.pane), c.action, "", actionOpts{})
		if paneErrKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "recreate") {
			t.Errorf("%s on %s: %v, want unavailable naming recreate", c.action, c.pane, err)
		}
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	if _, err := r.acts.Request(liveSession("p1"), "restart", "", actionOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.acts.Request(liveSession("p1"), "close", "", actionOpts{}); paneErrKind(err) != api.ErrorKindConflict {
		t.Fatalf("a second action: %v", err)
	}
	if _, err := r.acts.Request(api.Session{ID: "s", Name: "gone", State: api.SessionStateExited}, "close", "", actionOpts{}); paneErrKind(err) != api.ErrorKindConflict {
		t.Fatalf("an exited session: %v", err)
	}
	for _, p := range []api.SessionActionParams{{Session: "x", Action: "reboot"}, {Session: "x", Action: "close", FollowUp: "y"},
		{Session: "x", Action: "compact", FollowUp: strings.Repeat("x", maxFollowUp+1)}, {Action: "close"}} {
		if err := checkSessionAction(p); paneErrKind(err) != api.ErrorKindInvalid {
			t.Errorf("%+v: %v, want invalid", p, err)
		}
	}
}

// TestSessionActionAfterTurn: restart, close and recreate wait in queued while the agent works,
// then run; recreate passes the harness and the follow-up as its seed.
func TestSessionActionAfterTurn(t *testing.T) {
	t.Parallel()
	r := newActionRig(t)
	for _, c := range []struct {
		pane   string
		action api.SessionActionKind
		want   []string
	}{
		{"p1", "restart", []string{"restart sess-p1"}},
		{"p2", "close", []string{"close pane p2", "close session sess-p2"}},
		{"p3", "recreate", []string{"recreate lead-p3 claude carry on"}},
	} {
		r.agents.set(c.pane, api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
		follow := ""
		if c.action == "recreate" {
			follow = "carry on"
		}
		n := len(r.took())
		if _, err := r.acts.Request(liveSession(c.pane), c.action, follow, actionOpts{}); err != nil {
			t.Fatal(err)
		}
		r.next(t, actionQueued)
		time.Sleep(50 * time.Millisecond)
		if len(r.took()) != n {
			t.Fatalf("%s ran while the agent worked", c.action)
		}
		r.agents.set(c.pane, api.AgentState{Harness: "claude", State: api.AgentStatusDone})
		r.next(t, actionRunning)
		done := r.next(t, actionDone)
		if got := r.took()[n:]; strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Fatalf("%s: %v", c.action, got)
		}
		if c.action == "recreate" && done.NewSession != "new-id" {
			t.Fatalf("recreate: %+v", done)
		}
	}
	// A failure ends the action as failed, with the reason.
	r.acts.restart = func(context.Context, string) (string, error) {
		return "", control.Errorf(api.ErrorKindUnavailable, "session p4: how it starts again is not known")
	}
	r.agents.set("p4", api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	r.acts.Request(liveSession("p4"), "restart", "", actionOpts{})
	r.next(t, actionQueued)
	r.next(t, actionRunning)
	if f := r.next(t, actionFailed); !strings.Contains(f.Reason, "not known") {
		t.Fatalf("failed %+v", f)
	}
}

// TestSessionActionDoors: the owner runs an action at every value; the Capcom route where
// agent.drive_panes lets it type into the session's pane, and its marked forward; any other
// agent door is denied, its own session included.
func TestSessionActionDoors(t *testing.T) {
	t.Parallel()
	dr := newDriveRig(t)
	ar := newActionRig(t)
	dr.s.acts = ar.acts
	for _, p := range []string{paneMine, paneMate, paneOther, paneShell, paneLoose} {
		ar.agents.set(p, api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	}
	ar.agents.set(paneShell, api.AgentState{})
	ctx := context.Background()
	act := func(c paneCaller, session string) error {
		_, err := dr.s.Action(ctx, c, api.SessionActionParams{Session: session, Action: "recreate"})
		if err == nil {
			// Let the next call on the same session in: end the queued action.
			ar.acts.mu.Lock()
			for k := range ar.acts.pending {
				delete(ar.acts.pending, k)
			}
			ar.acts.mu.Unlock()
		}
		return err
	}
	type want struct {
		rule    string
		caller  paneCaller
		session string
		kind    api.ErrorKind
	}
	for _, w := range []want{
		{readPanesOff, fromOwner, "other", ""},
		{readPanesAll, fromCap, "other", ""}, {readPanesAll, fromCap, "shell", ""},
		{readPanesProject, fromCap, "shell", ""},
		{drivePanesAgents, fromCap, "other", ""}, {drivePanesAgents, fromCap, "shell", api.ErrorKindDenied},
		{readPanesOff, fromCap, "other", api.ErrorKindDenied},
		{readPanesAll, fromCapPeer, "other", ""}, {drivePanesAgents, fromCapPeer, "shell", api.ErrorKindDenied},
		{readPanesAll, fromMine, "mate", api.ErrorKindDenied}, {readPanesAll, fromMine, "me", api.ErrorKindDenied},
		{readPanesAll, fromPeer, "mate", api.ErrorKindDenied},
	} {
		dr.rule = w.rule
		if err := act(w.caller, w.session); paneErrKind(err) != w.kind {
			t.Errorf("%s: door %d capcom %v on %s: %v, want %q", w.rule, w.caller.door, w.caller.isCapcom(), w.session, err, w.kind)
		}
	}
	// The Capcom forward carries the mark; the peer must have session-action.
	rec := &recordPeer{}
	dr.peer.Caller = rec
	dr.rule = readPanesAll
	if err := act(fromCap, "bee/far"); paneErrKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("a peer without session-action: %v", err)
	}
	dr.peer.Caps = append(dr.peer.Caps, sessionActionCapability)
	if err := act(fromCap, "bee/far"); err != nil {
		t.Fatal(err)
	}
	if c := rec.take(); len(c) != 1 || c[0] != "session.action as=capcom" {
		t.Fatalf("forward %q", c)
	}
	// The route's own handler: an agent's pane is denied.
	params := []byte(`{"session":"mate","action":"close"}`)
	if _, _, err := paneHook(dr.s)(ctx, paneMine, "session.action", params); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("an agent's route: %v", err)
	}
}
