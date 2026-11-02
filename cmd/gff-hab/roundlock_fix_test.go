package main

// Unit tests of the session lock's edges (rounds.md 6, 7.5; stage 5): the guard under the send's
// claim, the quiet window after a start, the target memo, a conversation live in another pane,
// two runs on one exited target, a run that ends while its target starts again, and the hand-off
// of a pane between two runs.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// lockRig is a rounds service with no holder link, whose agents, held sends, sends and resumes
// the test plays. Its panes: "pane-1" and "pane-2" live, "pane-x" exited.
type lockRig struct {
	svc *roundsService
	st  *structure.Store
	ups chan runUp

	mu      sync.Mutex
	agents  map[string]api.AgentState
	held    map[string]bool
	sends   []string // session ids sent to
	send    func(session string, guard func(string) error) error
	resume  func(session string) (string, error)
	resumes int
}

type runUp struct {
	id string
	u  rounds.RunUpdate
}

func newLockRig(t *testing.T, edit func(*roundsConfig)) *lockRig {
	t.Helper()
	code := 0
	holder := structure.NewFakeHolder(structure.HolderPane{ID: "pane-1"}, structure.HolderPane{ID: "pane-2"},
		structure.HolderPane{ID: "pane-x", Exit: &api.ExitStatus{Code: &code}})
	k := &lockRig{agents: map[string]api.AgentState{}, held: map[string]bool{}, ups: make(chan runUp, 64)}
	k.svc, k.st = fakeRoundsServiceWith(t, t.TempDir(), holder, func(c *roundsConfig) {
		c.ready = time.Hour
		c.agent = func(p string) (api.AgentState, bool) {
			k.mu.Lock()
			defer k.mu.Unlock()
			a, ok := k.agents[p]
			return a, ok
		}
		c.agents = func() []api.AgentState {
			k.mu.Lock()
			defer k.mu.Unlock()
			var out []api.AgentState
			for _, a := range k.agents {
				out = append(out, a)
			}
			return out
		}
		c.sendHeld = func(p string) bool {
			k.mu.Lock()
			defer k.mu.Unlock()
			return k.held[p]
		}
		c.send = func(_ context.Context, session, _ string, guard func(string) error) error {
			k.mu.Lock()
			f := k.send
			k.mu.Unlock()
			if f != nil {
				if err := f(session, guard); err != nil {
					return err
				}
			}
			k.mu.Lock()
			k.sends = append(k.sends, session)
			k.mu.Unlock()
			return nil
		}
		c.folder = func(context.Context, string) string { return "/work/app" }
		c.resume = func(_ context.Context, session string, _ api.SessionCreateParams, _ string) (string, error) {
			k.mu.Lock()
			k.resumes++
			f := k.resume
			k.mu.Unlock()
			if f == nil {
				return "", errors.New("no resume")
			}
			return f(session)
		}
		if edit != nil {
			edit(c)
		}
	})
	k.svc.updateRun = func(id string, u rounds.RunUpdate) error { k.ups <- runUp{id, u}; return nil }
	return k
}

// agent sets the agent state of pane.
func (k *lockRig) agent(pane string, s api.AgentStatus, native string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.agents[pane] = api.AgentState{Pane: pane, Revision: uint64(len(k.agents) + 1), Harness: "claude", State: s, Session: native}
}

func (k *lockRig) session(t *testing.T, name, pane string) api.Session {
	t.Helper()
	ss, err := k.st.CreateSession(structure.SessionSpec{Name: name, Pane: pane, Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

// round creates a manual resume round on target.
func (k *lockRig) round(t *testing.T, name, target string) rounds.Round {
	t.Helper()
	made, err := k.svc.Create(context.Background(), api.RoundCreateParams{Round: api.RoundSpec{Name: name, Mode: api.RoundModeResume,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}, Target: target, Prompt: "check", WaitMS: 60_000}}, "")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := k.svc.store.Get(made.Round.ID)
	return e.Round
}

// start starts a resume run of r.
func (k *lockRig) start(t *testing.T, r rounds.Round, id string) {
	t.Helper()
	run := rounds.Run{ID: id, RoundID: r.ID, Number: 1, Created: time.Now(), State: rounds.RunRunning}
	if _, err := k.svc.startResume(r, run, nil); err != nil {
		t.Fatal(err)
	}
}

// next waits for the next update of run id, skipping others.
func (k *lockRig) next(t *testing.T, id string) rounds.RunUpdate {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-k.ups:
			if u.id == id {
				return u.u
			}
		case <-deadline:
			t.Fatalf("no update of %s", id)
		}
	}
}

func (k *lockRig) sent() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.sends...)
}

func (k *lockRig) phase(id string) harnessPhase {
	k.svc.mu.Lock()
	defer k.svc.mu.Unlock()
	if w := k.svc.watches[id]; w != nil && w.h != nil {
		return w.h.phase
	}
	return -1
}

// TestLockGuardUnderClaim (finding 6, M01): client input that comes after the lock's check and
// before the paste is refused under the send's claim; the run waits for the lock again.
func TestLockGuardUnderClaim(t *testing.T) {
	k := newLockRig(t, nil)
	ss := k.session(t, "target", "pane-1")
	k.agent("pane-1", api.AgentStatusIdle, "n1")
	k.send = func(_ string, guard func(string) error) error {
		if guard == nil {
			t.Error("a resume run's send has no guard")
			return nil
		}
		k.svc.noteInput("pane-1") // the user types between the check and the paste
		return guard("pane-1")
	}
	k.start(t, k.round(t, "r", ss.ID), "run-1")
	if u := k.next(t, "run-1"); u.State != rounds.RunWaiting || u.Reason != lockTyped {
		t.Fatalf("after a refused send: %+v", u)
	}
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
	if p := k.phase("run-1"); p != phaseLock {
		t.Errorf("phase %d, want the lock", p)
	}
}

// TestSendGuardedRefused (M02): a guard's error refuses the send before any write, and frees
// the pane's claim.
func TestSendGuardedRefused(t *testing.T) {
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder(structure.HolderPane{ID: "p"})})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := st.CreateSession(structure.SessionSpec{Name: "s", Pane: "p", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	in := &failFrom{from: 99}
	a := &agentService{store: st, agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: in, logf: t.Logf}
	a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	calls := 0
	refuse := control.Errorf(api.ErrorKindConflict, "in use")
	err = a.sendGuarded(context.Background(), "round", api.SessionSendParams{Session: ss.ID, Text: "hello"}, func(pane string) error {
		calls++
		if pane != "p" || !a.sendHeld("p") {
			t.Errorf("guard on %q, held %v", pane, a.sendHeld("p"))
		}
		return refuse
	})
	if !errors.Is(err, refuse) || calls != 1 || in.writes != 0 {
		t.Errorf("err %v, guard calls %d, writes %d", err, calls, in.writes)
	}
	if a.sendHeld("p") {
		t.Error("the claim stays after a refused send")
	}
}

// TestLockSendHeld (M06): a target with another message still starting its turn makes the run
// wait.
func TestLockSendHeld(t *testing.T) {
	k := newLockRig(t, nil)
	ss := k.session(t, "target", "pane-1")
	k.agent("pane-1", api.AgentStatusIdle, "n1")
	k.held["pane-1"] = true
	k.start(t, k.round(t, "r", ss.ID), "run-1")
	if u := k.next(t, "run-1"); u.State != rounds.RunWaiting || u.Reason != lockSending {
		t.Fatalf("update %+v", u)
	}
	k.mu.Lock()
	k.held["pane-1"] = false
	k.mu.Unlock()
	if u := k.next(t, "run-1"); u.State != rounds.RunRunning {
		t.Fatalf("after the send started its turn: %+v", u)
	}
	waitUntil(t, "the prompt sent", func() bool { return len(k.sent()) == 1 })
}

// TestLockRoundHandOff (M07): a run that holds the target's pane makes a second run wait, even
// with the agent idle; once the first ends, the second takes the pane.
func TestLockRoundHandOff(t *testing.T) {
	k := newLockRig(t, nil)
	ss := k.session(t, "target", "pane-1")
	k.agent("pane-1", api.AgentStatusIdle, "n1")
	r := k.round(t, "r", ss.ID)
	first := &harnessWatch{kind: "claude", native: "n1", prompt: "x", resume: true, phase: phaseTurn}
	k.svc.watchHarness(rounds.Run{ID: "run-1", RoundID: r.ID, Created: time.Now()}, r, ss.ID, "pane-1", first)
	k.start(t, r, "run-2")
	if u := k.next(t, "run-2"); u.State != rounds.RunWaiting || u.Reason != lockRound {
		t.Fatalf("update %+v", u)
	}
	if len(k.sent()) != 0 {
		t.Fatal("sent while another run held the pane")
	}
	k.svc.finish("run-1", rounds.RunUpdate{State: rounds.RunSucceeded})
	if u := k.next(t, "run-2"); u.State != rounds.RunRunning {
		t.Fatalf("after the hand-off: %+v", u)
	}
	waitUntil(t, "the second prompt sent", func() bool { return len(k.sent()) == 1 })
}

// stubAgents answers session.send with err.
type stubAgents struct {
	control.Agents
	err error
}

func (a stubAgents) Send(context.Context, string, api.SessionSendParams) error { return a.err }

// TestClientSendInUse (M08): a client's session.send, delivered or refused, makes the session
// in use.
func TestClientSendInUse(t *testing.T) {
	for _, refused := range []bool{false, true} {
		k := newLockRig(t, nil)
		ss := k.session(t, "target", "pane-1")
		var err error
		if refused {
			err = control.Errorf(api.ErrorKindConflict, "busy")
		}
		_ = roundAgents{Agents: stubAgents{err: err}, rounds: k.svc}.Send(context.Background(), "c", api.SessionSendParams{Session: ss.ID, Text: "hi"})
		k.svc.mu.Lock()
		why := k.svc.clientUseLocked(ss.ID, "pane-1")
		k.svc.mu.Unlock()
		if why != lockTyped {
			t.Errorf("refused %v: %q", refused, why)
		}
	}
}

// TestLockQuietAfterStart (finding 2): for the quiet window after the service starts every
// pane is in use, under the send's guard too: a client may have typed or watched while
// gff-hab restarted.
func TestLockQuietAfterStart(t *testing.T) {
	k := newLockRig(t, func(c *roundsConfig) { c.quiet = time.Hour })
	ss := k.session(t, "target", "pane-1")
	k.agent("pane-1", api.AgentStatusIdle, "n1")
	if err := k.svc.lockGuard(ss.ID, "pane-1"); err == nil || !strings.Contains(err.Error(), lockStarted) {
		t.Errorf("guard: %v", err)
	}
	k.start(t, k.round(t, "r", ss.ID), "run-1")
	if u := k.next(t, "run-1"); u.State != rounds.RunWaiting || u.Reason != lockStarted {
		t.Fatalf("update %+v", u)
	}
	k.svc.mu.Lock()
	k.svc.started = time.Now().Add(-2 * time.Hour)
	k.svc.mu.Unlock()
	if u := k.next(t, "run-1"); u.State != rounds.RunRunning {
		t.Fatalf("after the window: %+v", u)
	}
}

// TestTargetMemo (finding 3): the memo names the session it was seen in; a refused edit writes
// nothing, and a run still waiting on the old target after a re-target neither overwrites nor
// reads the new target's memo.
func TestTargetMemo(t *testing.T) {
	k := newLockRig(t, nil)
	a := k.session(t, "a", "pane-1")
	k.agent("pane-1", api.AgentStatusWorking, "na")
	r := k.round(t, "r", a.ID)
	memo := func() rounds.TargetSeen {
		e, _ := k.svc.store.Get(r.ID)
		if e.State.Target == nil {
			return rounds.TargetSeen{}
		}
		return *e.State.Target
	}
	if got := memo(); got != (rounds.TargetSeen{Session: a.ID, Harness: "claude", Native: "na"}) {
		t.Fatalf("memo at create: %+v", got)
	}
	k.start(t, r, "run-1")
	if u := k.next(t, "run-1"); u.Reason != lockWorking {
		t.Fatalf("update %+v", u)
	}
	b := k.session(t, "b", "pane-2")
	k.agent("pane-2", api.AgentStatusIdle, "nb")
	got, _ := k.svc.Get(api.RoundRef{ID: r.ID})
	spec := got.RoundSpec
	spec.Target = b.ID
	// A refused edit (a stale revision) leaves the memo as it was.
	if _, err := k.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Revision: got.Revision + 7, Round: spec}); err == nil {
		t.Fatal("a stale edit went through")
	}
	if m := memo(); m.Session != a.ID {
		t.Fatalf("memo after a refused edit: %+v", m)
	}
	if _, err := k.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Round: spec}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * roundLockPoll) // run-1 still polls the old target
	if m := memo(); m != (rounds.TargetSeen{Session: b.ID, Harness: "claude", Native: "nb"}) {
		t.Fatalf("memo after the re-target: %+v", m)
	}
	// The old target exited: its run does not take b's conversation for a.
	k.mu.Lock()
	delete(k.agents, "pane-1")
	k.mu.Unlock()
	if seen := k.svc.targetSeen(r.ID, a); seen.Native != "" {
		t.Errorf("the old target reads %+v", seen)
	}
}

// TestForkMemoRefusedEdit (finding 3a): a fork round's edit runs the target check before the
// revision check; a refused edit leaves the memo naming the old target.
func TestForkMemoRefusedEdit(t *testing.T) {
	k := newLockRig(t, nil)
	a := k.session(t, "a", "pane-1")
	b := k.session(t, "b", "pane-2")
	k.agent("pane-1", api.AgentStatusIdle, "na")
	k.agent("pane-2", api.AgentStatusIdle, "nb")
	made, err := k.svc.Create(context.Background(), api.RoundCreateParams{Round: api.RoundSpec{Name: "f", Mode: api.RoundModeFork,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}, Target: a.ID, Prompt: "next"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	spec := made.Round.RoundSpec
	spec.Target = b.ID
	if _, err := k.svc.Update(context.Background(), api.RoundUpdateParams{ID: made.Round.ID, Revision: made.Round.Revision + 7, Round: spec}); err == nil {
		t.Fatal("a stale edit went through")
	}
	e, _ := k.svc.store.Get(made.Round.ID)
	if e.State.Target == nil || *e.State.Target != (rounds.TargetSeen{Session: a.ID, Harness: "claude", Native: "na"}) {
		t.Errorf("memo after a refused edit: %+v", e.State.Target)
	}
}

// exitedTarget is the session on the exited pane, with a round on it that remembered its
// conversation "nx".
func (k *lockRig) exitedTarget(t *testing.T) (api.Session, rounds.Round) {
	t.Helper()
	ss := k.session(t, "gone", "pane-x")
	k.agent("pane-x", api.AgentStatusIdle, "nx")
	r := k.round(t, "r", ss.ID)
	k.mu.Lock()
	delete(k.agents, "pane-x")
	k.mu.Unlock()
	if e, _ := k.svc.store.Get(r.ID); e.State.Target == nil || e.State.Target.Native != "nx" {
		t.Fatalf("memo %+v", e.State.Target)
	}
	return ss, r
}

// TestLockConversationElsewhere (finding 4): an exited target whose conversation runs on in
// another pane is not started again: the run waits.
func TestLockConversationElsewhere(t *testing.T) {
	k := newLockRig(t, nil)
	_, r := k.exitedTarget(t)
	k.agent("pane-2", api.AgentStatusIdle, "nx") // the user resumed it there
	k.start(t, r, "run-1")
	if u := k.next(t, "run-1"); u.State != rounds.RunWaiting || u.Reason != lockElse {
		t.Fatalf("update %+v", u)
	}
	time.Sleep(3 * roundLockPoll)
	k.mu.Lock()
	n := k.resumes
	k.agents["pane-2"] = api.AgentState{Pane: "pane-2", Revision: 9, Harness: "claude", State: api.AgentStatusExited, Session: "nx"}
	k.resume = func(string) (string, error) { k.agent("pane-1", api.AgentStatusIdle, "nx"); return "pane-1", nil }
	k.mu.Unlock()
	if n != 0 {
		t.Fatalf("resumed %d times while the conversation ran elsewhere", n)
	}
	if u := k.next(t, "run-1"); u.State != rounds.RunRunning {
		t.Fatalf("once it exited there: %+v", u)
	}
	waitUntil(t, "the prompt sent", func() bool { return len(k.sent()) == 1 })
}

// TestLockResumeConflict (finding 5): a resume refused because another run started the target
// first sends the run back to the lock; it does not fail.
func TestLockResumeConflict(t *testing.T) {
	k := newLockRig(t, nil)
	_, r := k.exitedTarget(t)
	k.resume = func(session string) (string, error) {
		k.mu.Lock()
		n := k.resumes
		k.mu.Unlock()
		if n == 1 {
			return "", control.Errorf(api.ErrorKindConflict, "session %s is running", session)
		}
		k.agent("pane-1", api.AgentStatusIdle, "nx")
		return "pane-1", nil
	}
	k.start(t, r, "run-1")
	waitUntil(t, "the prompt sent", func() bool { return len(k.sent()) == 1 })
	k.mu.Lock()
	n := k.resumes
	k.mu.Unlock()
	if n != 2 {
		t.Errorf("resumes %d, want 2", n)
	}
	for {
		select {
		case u := <-k.ups:
			if u.u.State == rounds.RunFailed {
				t.Fatalf("the run failed: %+v", u.u)
			}
			continue
		default:
		}
		break
	}
}

// TestLockEndedDuringResume (finding 8): a run that ends while its exited target starts again
// leaves the target alone: nothing watches it, nothing is sent.
func TestLockEndedDuringResume(t *testing.T) {
	k := newLockRig(t, nil)
	_, r := k.exitedTarget(t)
	in, out := make(chan struct{}), make(chan struct{})
	k.resume = func(string) (string, error) {
		close(in)
		<-out
		k.agent("pane-1", api.AgentStatusIdle, "nx")
		return "pane-1", nil
	}
	k.start(t, r, "run-1")
	<-in
	k.svc.finish("run-1", rounds.RunUpdate{State: rounds.RunSkipped, Reason: rounds.ReasonPaused})
	close(out)
	time.Sleep(4 * roundLockPoll)
	if p := k.phase("run-1"); p != -1 {
		t.Errorf("an ended run is watched again, phase %d", p)
	}
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

// TestTakeOverBeforePrompt (finding 9): input to a resumed target before the prompt went in
// sends the run back to the lock; closing the pane still takes it over.
func TestTakeOverBeforePrompt(t *testing.T) {
	k := newLockRig(t, nil)
	ss := k.session(t, "target", "pane-1")
	r := k.round(t, "r", ss.ID)
	for _, id := range []string{"run-1", "run-2"} {
		h := &harnessWatch{kind: "claude", native: "n1", prompt: "x", resume: true, marker: envelopeMarker(id), phase: phaseStarting}
		k.svc.watchHarness(rounds.Run{ID: id, RoundID: r.ID, Created: time.Now()}, r, ss.ID, "pane-1", h)
		if id == "run-1" {
			k.svc.takeOver("pane-1", reasonTyped)
			if u := k.next(t, id); u.State != rounds.RunWaiting {
				t.Fatalf("typed before the prompt: %+v", u)
			}
			if p := k.phase(id); p != phaseLock {
				t.Errorf("phase %d", p)
			}
			k.svc.finish(id, rounds.RunUpdate{State: rounds.RunSkipped})
			k.next(t, id)
			continue
		}
		k.svc.takeOver("pane-1", reasonClosed)
		if u := k.next(t, id); u.State != rounds.RunTakenOver {
			t.Fatalf("closed before the prompt: %+v", u)
		}
	}
}

// TestAdoptedTurnMarker: with a marker, only the user message that holds it is the run's
// prompt; earlier turns of the target's own do not count.
func TestAdoptedTurnMarker(t *testing.T) {
	m := envelopeMarker("run-7")
	user := func(s string) api.ChatMessage { return api.ChatMessage{Kind: api.ChatKindUser, Text: s} }
	agent := func(s string) api.ChatMessage { return api.ChatMessage{Kind: api.ChatKindAgent, Text: s} }
	earlier := []api.ChatMessage{user("fix the bug"), agent("Fixed.")}
	cases := []struct {
		name             string
		msgs             []api.ChatMessage
		delivered, ended bool
	}{
		{"the target's own turns only", earlier, false, false},
		{"another run's prompt", append(earlier, user("[begin round message run-6]\ncheck"), agent("Done.")), false, false},
		{"the prompt, no reply", append(earlier, user("intro\n"+m+"\ncheck")), true, false},
		{"the prompt and a reply", append(earlier, user("intro\n"+m+"\ncheck"), agent("All good.")), true, true},
	}
	for _, c := range cases {
		if d, e := adoptedTurn(c.msgs, m); d != c.delivered || e != c.ended {
			t.Errorf("%s: delivered %v ended %v", c.name, d, e)
		}
	}
}

// TestLockTimeoutKeepsUsersTurn (M13): a resume run past its timeout before its prompt went in
// never interrupts the target's turn: that turn is the user's.
func TestLockTimeoutKeepsUsersTurn(t *testing.T) {
	interrupts := 0
	k := newLockRig(t, func(c *roundsConfig) {
		c.interrupt = func(context.Context, string) error { interrupts++; return nil }
	})
	ss := k.session(t, "target", "pane-1")
	r := k.round(t, "r", ss.ID)
	r.Timeout = rounds.Duration(500 * time.Millisecond)
	k.agent("pane-1", api.AgentStatusWorking, "n1")
	h := &harnessWatch{kind: "claude", native: "n1", prompt: "x", resume: true, marker: envelopeMarker("run-1"), phase: phaseStarting}
	k.svc.watchHarness(rounds.Run{ID: "run-1", RoundID: r.ID, Created: time.Now()}, r, ss.ID, "pane-1", h)
	if u := k.next(t, "run-1"); u.State != rounds.RunTimedOut {
		t.Fatalf("update %+v", u)
	}
	time.Sleep(200 * time.Millisecond)
	k.mu.Lock()
	defer k.mu.Unlock()
	if interrupts != 0 || len(k.sends) != 0 {
		t.Errorf("interrupts %d, sends %v", interrupts, k.sends)
	}
}

// TestLockPauseFirst (M15): a waiting run whose move to running the scheduler refuses (a pause
// ended it first) sends nothing.
func TestLockPauseFirst(t *testing.T) {
	k := newLockRig(t, nil)
	ss := k.session(t, "target", "pane-1")
	k.agent("pane-1", api.AgentStatusIdle, "n1")
	k.held["pane-1"] = true
	k.svc.updateRun = func(id string, u rounds.RunUpdate) error {
		k.ups <- runUp{id, u}
		if u.State == rounds.RunRunning {
			return rounds.ErrNoRun
		}
		return nil
	}
	k.start(t, k.round(t, "r", ss.ID), "run-1")
	if u := k.next(t, "run-1"); u.State != rounds.RunWaiting {
		t.Fatalf("update %+v", u)
	}
	k.mu.Lock()
	k.held["pane-1"] = false
	k.mu.Unlock()
	if u := k.next(t, "run-1"); u.State != rounds.RunRunning {
		t.Fatalf("update %+v", u)
	}
	time.Sleep(4 * roundLockPoll)
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v after the run ended", got)
	}
}

// TestAdoptResumeNotDelivered (M17): a resume run adopted after a restart whose envelope is not
// in its target's conversation waits for the lock again; it does not wait for the next idle as
// a fresh run would.
func TestAdoptResumeNotDelivered(t *testing.T) {
	k := newLockRig(t, func(c *roundsConfig) {
		c.messages = func(context.Context, string, string, string) ([]api.ChatMessage, error) {
			return []api.ChatMessage{{Kind: api.ChatKindUser, Text: "fix the bug"}, {Kind: api.ChatKindAgent, Text: "Fixed."}}, nil
		}
	})
	h := &harnessWatch{kind: "claude", native: "n1", prompt: "x", resume: true, marker: envelopeMarker("run-1"), phase: phaseTurn, blocked: true}
	k.svc.adoptHarness("s-1", 0, h)
	if h.phase != phaseLock || h.blocked {
		t.Errorf("phase %d blocked %v, want the lock", h.phase, h.blocked)
	}
}
