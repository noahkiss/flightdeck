package main

// Unit tests of harness runs (roundharness.go): the step rules one agent state at a time, and
// the service around them with fake sends, results and scheduler updates.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestHarnessStep drives harnessWatch.step one state at a time from a given watch.
func TestHarnessStep(t *testing.T) {
	st := func(rev uint64, s api.AgentStatus) api.AgentState {
		return api.AgentState{Revision: rev, State: s, Harness: "claude", Session: "native-1"}
	}
	withTurn := func(a api.AgentState, turn string) api.AgentState { a.Turn = turn; return a }
	withErr := func(a api.AgentState, kind string) api.AgentState { a.Error = kind; return a }
	type want struct {
		phase   harnessPhase
		deliver bool
		end     rounds.RunState // "" none
		network bool
		live    rounds.RunState
		ready   readyAct
		exited  bool
	}
	tests := []struct {
		name  string
		h     harnessWatch
		st    api.AgentState
		retry bool
		want  want
	}{
		{"idle before the prompt delivers it and stops the readiness limit", harnessWatch{phase: phaseStarting},
			st(1, api.AgentStatusIdle), false, want{phase: phaseDelivering, deliver: true, ready: readyStop}},
		{"done before the prompt delivers it", harnessWatch{phase: phaseStarting},
			st(1, api.AgentStatusDone), false, want{phase: phaseDelivering, deliver: true, ready: readyStop}},
		{"working before the prompt waits", harnessWatch{phase: phaseStarting},
			st(1, api.AgentStatusWorking), false, want{phase: phaseStarting}},
		// Finding 2: stale revisions, and the retry after a refused send.
		{"a stale revision does nothing", harnessWatch{phase: phaseStarting, rev: 5},
			st(5, api.AgentStatusIdle), false, want{phase: phaseStarting}},
		{"an older revision does nothing", harnessWatch{phase: phaseStarting, rev: 5},
			st(4, api.AgentStatusIdle), false, want{phase: phaseStarting}},
		{"a retry applies the current revision again", harnessWatch{phase: phaseStarting, rev: 5, state: api.AgentStatusIdle},
			st(5, api.AgentStatusIdle), true, want{phase: phaseDelivering, deliver: true, ready: readyStop}},
		{"a retry with a revision older than the applied one does nothing", harnessWatch{phase: phaseStarting, rev: 6},
			st(5, api.AgentStatusIdle), true, want{phase: phaseStarting}},
		{"a retry after a newer state started the send does nothing", harnessWatch{phase: phaseDelivering, rev: 6},
			st(6, api.AgentStatusIdle), true, want{phase: phaseDelivering}},
		// Read back and turn end.
		{"working after the send is the read back", harnessWatch{phase: phaseSent, rev: 1, state: api.AgentStatusIdle},
			st(2, api.AgentStatusWorking), false, want{phase: phaseTurn}},
		{"done in the turn ends it succeeded", harnessWatch{phase: phaseTurn, rev: 2, state: api.AgentStatusWorking},
			st(3, api.AgentStatusDone), false, want{phase: phaseEnding, end: rounds.RunSucceeded, ready: readyStop}},
		{"error in the turn fails it with the network flag", harnessWatch{phase: phaseTurn, rev: 2, state: api.AgentStatusWorking},
			withErr(st(3, api.AgentStatusError), "authentication_failed"), false, want{phase: phaseEnding, end: rounds.RunFailed, network: true, ready: readyStop}},
		{"idle in the turn ends nothing", harnessWatch{phase: phaseTurn, rev: 2, state: api.AgentStatusWorking},
			st(3, api.AgentStatusIdle), false, want{phase: phaseTurn}},
		{"done during the send, after idle, ends the turn", harnessWatch{phase: phaseDelivering, rev: 1, state: api.AgentStatusIdle},
			st(2, api.AgentStatusDone), false, want{phase: phaseEnding, end: rounds.RunSucceeded, ready: readyStop}},
		// Finding 7: a republish that changes neither state nor turn.
		{"a republished done during the send ends nothing", harnessWatch{phase: phaseDelivering, rev: 1, state: api.AgentStatusDone, turn: "t1"},
			withTurn(st(2, api.AgentStatusDone), "t1"), false, want{phase: phaseDelivering}},
		{"a republished error after the send ends nothing", harnessWatch{phase: phaseSent, rev: 1, state: api.AgentStatusError},
			st(2, api.AgentStatusError), false, want{phase: phaseSent}},
		{"done of a new turn after the send ends it", harnessWatch{phase: phaseSent, rev: 1, state: api.AgentStatusDone, turn: "t1"},
			withTurn(st(2, api.AgentStatusDone), "t2"), false, want{phase: phaseEnding, end: rounds.RunSucceeded, ready: readyStop}},
		// Finding 1: once the turn ended nothing changes the run.
		{"exited while the result is read does nothing", harnessWatch{phase: phaseEnding, rev: 3, state: api.AgentStatusDone},
			st(4, api.AgentStatusExited), false, want{phase: phaseEnding}},
		{"blocked while the result is read does nothing", harnessWatch{phase: phaseEnding, rev: 3, state: api.AgentStatusDone},
			st(4, api.AgentStatusBlocked), false, want{phase: phaseEnding}},
		// Blocked, and the readiness limit.
		{"blocked before the prompt stops the limit", harnessWatch{phase: phaseStarting},
			st(1, api.AgentStatusBlocked), false, want{phase: phaseStarting, live: rounds.RunBlocked, ready: readyStop}},
		{"going on before the prompt starts the limit again", harnessWatch{phase: phaseStarting, rev: 1, blocked: true},
			st(2, api.AgentStatusWorking), false, want{phase: phaseStarting, live: rounds.RunRunning, ready: readyRestart}},
		{"idle after a sign-in delivers", harnessWatch{phase: phaseStarting, rev: 1, blocked: true},
			st(2, api.AgentStatusIdle), false, want{phase: phaseDelivering, deliver: true, live: rounds.RunRunning, ready: readyStop}},
		{"blocked in the turn shows blocked", harnessWatch{phase: phaseSent, rev: 1},
			st(2, api.AgentStatusBlocked), false, want{phase: phaseTurn, live: rounds.RunBlocked, ready: readyStop}},
		{"an agent state after the trust dialog clears it", harnessWatch{phase: phaseStarting, trust: true},
			st(1, api.AgentStatusIdle), false, want{phase: phaseDelivering, deliver: true, live: rounds.RunRunning, ready: readyStop}},
		{"exited asks for the exit grace", harnessWatch{phase: phaseTurn, rev: 1},
			st(2, api.AgentStatusExited), false, want{phase: phaseTurn, exited: true}},
		// Adoption after a restart.
		{"an adopted run whose transcript ended takes idle as its end", harnessWatch{phase: phaseTurn, adoptDone: true},
			st(1, api.AgentStatusIdle), false, want{phase: phaseEnding, end: rounds.RunSucceeded, ready: readyStop}},
		{"an adopted run that works again does not", harnessWatch{phase: phaseTurn, adoptDone: true, rev: 1},
			st(2, api.AgentStatusWorking), false, want{phase: phaseTurn}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h
			out := h.step(tc.st, tc.retry)
			got := want{phase: h.phase, deliver: out.deliver, live: out.live, ready: out.ready, exited: out.exited}
			if out.end != nil {
				got.end, got.network = out.end.State, out.end.Network
				if h.end == nil || *h.end != *out.end {
					t.Errorf("pending end %+v, step's %+v", h.end, out.end)
				}
				if out.native != "native-1" {
					t.Errorf("native %q", out.native)
				}
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if tc.h.trust && !out.trustCleared {
				t.Error("trust not cleared")
			}
		})
	}
	// The reason of a failed turn names the harness's kind.
	h := harnessWatch{phase: phaseTurn}
	if out := h.step(withErr(st(1, api.AgentStatusError), "server_error"), false); out.end.Reason != reasonTurnError+": server_error" {
		t.Errorf("reason %q", out.end.Reason)
	}
}

// harnessRig is a rounds service with no holder link, one watched harness run on pane-1, and
// the scheduler updates it would send.
type harnessRig struct {
	svc *roundsService
	ups chan rounds.RunUpdate
}

func newHarnessRig(t *testing.T, edit func(*roundsConfig)) *harnessRig {
	t.Helper()
	svc, _ := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.ready = time.Hour
		if edit != nil {
			edit(c)
		}
	})
	r := &harnessRig{svc: svc, ups: make(chan rounds.RunUpdate, 16)}
	svc.updateRun = func(_ string, u rounds.RunUpdate) error { r.ups <- u; return nil }
	return r
}

// watch starts watching run-1 in pane-1, in phase.
func (r *harnessRig) watch(phase harnessPhase) *harnessWatch {
	h := &harnessWatch{kind: "claude", native: "native-1", prompt: "Do it.", phase: phase}
	r.svc.watchHarness(rounds.Run{ID: "run-1", Created: time.Now()}, rounds.Round{ID: "round-1", Timeout: rounds.Duration(time.Hour)},
		"session-1", "pane-1", h)
	return h
}

func (r *harnessRig) state(rev uint64, s api.AgentStatus) {
	r.svc.agentState(api.AgentState{Pane: "pane-1", Revision: rev, State: s, Harness: "claude"})
}

func (r *harnessRig) next(t *testing.T) rounds.RunUpdate {
	t.Helper()
	select {
	case u := <-r.ups:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("no run update")
	}
	return rounds.RunUpdate{}
}

func (r *harnessRig) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case u := <-r.ups:
		t.Fatalf("unexpected run update %+v", u)
	case <-time.After(d):
	}
}

func (r *harnessRig) phase() harnessPhase {
	r.svc.mu.Lock()
	defer r.svc.mu.Unlock()
	if w := r.svc.watches["run-1"]; w != nil {
		return w.h.phase
	}
	return -1
}

// TestHarnessTurnEndWins (finding 1): once the turn's end is seen, a take over, the pane's exit
// and the timeout that come while the final message is read leave the end as it is.
func TestHarnessTurnEndWins(t *testing.T) {
	release := make(chan struct{})
	rig := newHarnessRig(t, func(c *roundsConfig) {
		c.result = func(context.Context, string, string, string) (string, error) {
			<-release
			return "All done.", nil
		}
	})
	rig.watch(phaseTurn)
	rig.state(1, api.AgentStatusDone)
	rig.svc.takeOver("pane-1", reasonTyped)
	rig.svc.exited("pane-1", hp.ExitStatus{Code: 1})
	rig.svc.timedOut("run-1")
	rig.none(t, 100*time.Millisecond)
	close(release)
	if u := rig.next(t); u.State != rounds.RunSucceeded || u.Result != "All done." || u.SessionID != "session-1" {
		t.Fatalf("end %+v", u)
	}
	rig.none(t, 100*time.Millisecond)
}

// TestHarnessBusyRetry: a send refused for a busy agent goes back to waiting, and the agent's
// current state, applied again at the same revision, sends the prompt once more.
func TestHarnessBusyRetry(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	rig := newHarnessRig(t, func(c *roundsConfig) {
		c.send = func(context.Context, string, string) error {
			mu.Lock()
			defer mu.Unlock()
			sends++
			if sends == 1 {
				return control.Errorf(api.ErrorKindConflict, "the agent is working")
			}
			return nil
		}
		c.agent = func(string) (api.AgentState, bool) {
			return api.AgentState{Pane: "pane-1", Revision: 2, State: api.AgentStatusIdle, Harness: "claude"}, true
		}
	})
	rig.watch(phaseStarting)
	rig.state(2, api.AgentStatusIdle)
	waitUntil(t, "the second send", func() bool { mu.Lock(); defer mu.Unlock(); return sends == 2 })
	waitUntil(t, "the prompt sent", func() bool { return rig.phase() == phaseSent })
	rig.none(t, 100*time.Millisecond)
}

// TestHarnessSendEndsWithRun (finding 3): a run that ends while its prompt is being sent
// cancels the send, and the refused send fails nothing.
func TestHarnessSendEndsWithRun(t *testing.T) {
	sendErr := make(chan error, 1)
	started := make(chan struct{})
	rig := newHarnessRig(t, func(c *roundsConfig) {
		c.send = func(ctx context.Context, _, _ string) error {
			close(started)
			<-ctx.Done()
			sendErr <- ctx.Err()
			return control.Errorf(api.ErrorKindUnavailable, "cancelled")
		}
	})
	h := rig.watch(phaseStarting)
	rig.state(1, api.AgentStatusIdle)
	<-started
	rig.svc.mu.Lock()
	ready := h.ready.Stop()
	rig.svc.mu.Unlock()
	if ready {
		t.Error("the readiness timer still ran while the prompt was being sent")
	}
	rig.svc.takeOver("pane-1", reasonSent)
	if u := rig.next(t); u.State != rounds.RunTakenOver {
		t.Fatalf("end %+v", u)
	}
	select {
	case err := <-sendErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("send ended by %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the send was not cancelled")
	}
	rig.none(t, 100*time.Millisecond)
}

// TestHarnessNothingAfterClose (finding 5): after Close no state starts a send, no end is sent,
// and no background work starts.
func TestHarnessNothingAfterClose(t *testing.T) {
	sent := make(chan struct{}, 1)
	rig := newHarnessRig(t, func(c *roundsConfig) {
		c.send = func(context.Context, string, string) error { sent <- struct{}{}; return nil }
	})
	rig.watch(phaseStarting)
	rig.svc.Close()
	rig.state(1, api.AgentStatusIdle)
	if rig.svc.goBg(func() { t.Error("ran after Close") }) {
		t.Error("goBg started work after Close")
	}
	if rig.svc.finish("run-1", rounds.RunUpdate{State: rounds.RunFailed}) {
		t.Error("a run ended after Close")
	}
	select {
	case <-sent:
		t.Error("a prompt went in after Close")
	case <-time.After(200 * time.Millisecond):
	}
	rig.none(t, 50*time.Millisecond)
}

// TestHarnessNetworkFlag: a StopFailure's sign-in kind reaches UpdateRun with the network flag.
func TestHarnessNetworkFlag(t *testing.T) {
	rig := newHarnessRig(t, nil)
	rig.watch(phaseTurn)
	rig.svc.agentState(api.AgentState{Pane: "pane-1", Revision: 1, State: api.AgentStatusError, Harness: "claude", Error: "authentication_failed"})
	u := rig.next(t)
	if u.State != rounds.RunFailed || !u.Network || u.Reason != reasonTurnError+": authentication_failed" {
		t.Fatalf("end %+v", u)
	}
}

// TestHarnessExitGrace: an exited state ends the run only when the pane's exit, which carries
// the status, does not come first.
func TestHarnessExitGrace(t *testing.T) {
	rig := newHarnessRig(t, nil)
	rig.watch(phaseTurn)
	rig.state(1, api.AgentStatusExited)
	rig.svc.exited("pane-1", hp.ExitStatus{Code: 3})
	if u := rig.next(t); u.State != rounds.RunFailed || u.Reason != reasonHarnessExited+" (exit status 3)" {
		t.Fatalf("end %+v", u)
	}
	rig2 := newHarnessRig(t, nil)
	rig2.watch(phaseTurn)
	rig2.state(1, api.AgentStatusExited)
	if u := rig2.next(t); u.State != rounds.RunFailed || u.Reason != reasonHarnessExited {
		t.Fatalf("end with no pane exit %+v", u)
	}
}

func TestTrustDialog(t *testing.T) {
	dialog := strings.Join([]string{
		" Accessing workspace:",
		"",
		" /work/app",
		"",
		" Quick safety check: Is this a project you created or one you trust? (Like your own code,",
		" a well-known open source project, or work from your team). If not, take a moment to",
		" review what's in this folder first.",
		"",
		" ❯ 1. Yes, I trust this folder",
		"   2. No, exit",
		"",
		" Enter to confirm · Esc to cancel",
	}, "\n")
	if !trustDialog(dialog) {
		t.Error("the trust dialog was not seen")
	}
	older := "Do you trust the files in this folder?\n\n❯ 1. Yes, I trust this folder\n  2. No, exit"
	if !trustDialog(older) {
		t.Error("the older wording was not seen")
	}
	for _, other := range []string{
		"",
		"❯ Try \"fix the tests\"",
		"Do you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel",
		"Quick safety check: a prompt that talks about trust, with no options",
	} {
		if trustDialog(other) {
			t.Errorf("seen in %q", other)
		}
	}
}

// TestHarnessTrustBlocks: a starting Claude Code run whose screen shows the trust dialog shows
// blocked with a reason that names it, input to its pane is its answer, not a take over, and
// it shows running again once the dialog goes; rounds never type into it.
func TestHarnessTrustBlocks(t *testing.T) {
	var mu sync.Mutex
	screen := "Quick safety check: Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit"
	rig := newHarnessRig(t, func(c *roundsConfig) {
		c.screen = func(string) (string, bool) { mu.Lock(); defer mu.Unlock(); return screen, true }
		c.send = func(context.Context, string, string) error { t.Error("a send during the dialog"); return nil }
	})
	h := rig.watch(phaseStarting)
	if u := rig.next(t); u.State != rounds.RunBlocked || u.Reason != reasonTrust {
		t.Fatalf("live %+v", u)
	}
	rig.svc.mu.Lock()
	if h.ready.Stop() {
		t.Error("the readiness limit ran while the dialog showed")
	}
	rig.svc.mu.Unlock()
	rig.svc.takeOver("pane-1", reasonTyped)
	mu.Lock()
	screen = "starting"
	mu.Unlock()
	if u := rig.next(t); u.State != rounds.RunRunning || u.Reason != "" {
		t.Fatalf("live %+v", u)
	}
	rig.svc.takeOver("pane-1", reasonTyped) // inside the grace: still the dialog's answer
	rig.none(t, 100*time.Millisecond)
	if rig.phase() != phaseStarting {
		t.Fatalf("phase %d", rig.phase())
	}
}

func TestAdoptedTurn(t *testing.T) {
	user := api.ChatMessage{Kind: api.ChatKindUser, Text: "Do it."}
	agent := api.ChatMessage{Kind: api.ChatKindAgent, Text: "Done."}
	for _, tc := range []struct {
		msgs             []api.ChatMessage
		delivered, ended bool
	}{
		{nil, false, false},
		{[]api.ChatMessage{agent}, false, false},
		{[]api.ChatMessage{user}, true, false},
		{[]api.ChatMessage{user, agent}, true, true},
		{[]api.ChatMessage{agent, user}, true, false},
	} {
		if d, e := adoptedTurn(tc.msgs); d != tc.delivered || e != tc.ended {
			t.Errorf("%+v: delivered %v ended %v", tc.msgs, d, e)
		}
	}
	for argv, want := range map[string]string{
		"/bin/sh /x/claude --fake a --session-id abc --model m": "abc",
		"claude --session-id=abc":                               "abc",
		"claude --session-id":                                   "",
		"claude --resume abc":                                   "",
	} {
		if got := sessionIDArg(strings.Fields(argv)); got != want {
			t.Errorf("%q: %q", argv, got)
		}
	}
}
