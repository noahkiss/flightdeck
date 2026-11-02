package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// Harness runs (rounds.md 5, 7.2 to 7.6, 8.3; stage 4): a fresh round with a harness and a
// prompt starts the harness in a new pane, sends the prompt once the agent is idle, and ends at
// the first turn end after the prompt went in. The agent's states come from the harness store
// (agent.state), so this file follows the same stream the clients see.

// roundReady is how long a harness run may take to reach idle before its prompt (rounds.md 7.2).
const roundReady = 2 * time.Minute

// roundSendRetry is the pause before a refused prompt is tried again.
const roundSendRetry = 500 * time.Millisecond

// roundSendWait bounds one send of a run's prompt.
const roundSendWait = 30 * time.Second

// roundResultWait bounds the read of a run's final agent message from its transcript: the
// harness may report the turn's end a moment before the transcript holds its last message.
const roundResultWait = 5 * time.Second

// roundExitGrace is how long an exited agent state waits for the pane's own exit, which carries
// the exit status, before it ends the run itself.
const roundExitGrace = 2 * time.Second

// roundTrustPoll is how often the screen of a Claude Code run that waits for its first idle is
// read for the workspace trust dialog.
const roundTrustPoll = 500 * time.Millisecond

// roundTrustGrace is how long after the trust dialog went a client's input still counts as its
// answer, not as a take over: the poll may see the dialog go before the input's take over check.
const roundTrustGrace = 2 * time.Second

// Reasons gff-hab writes on harness runs.
const (
	reasonHarnessExited = "the harness exited before its turn ended"
	reasonTurnError     = "the turn ended with an error"
	reasonTrust         = `Claude Code asks whether to trust the run's folder ("Yes, I trust this folder"); answer it in the run's pane`
)

// harnessPhase is where a harness run is between its spawn and its end.
type harnessPhase int

const (
	// phaseStarting: the harness is starting; the prompt goes in at its first idle.
	phaseStarting harnessPhase = iota
	// phaseDelivering: a send of the prompt is in flight.
	phaseDelivering
	// phaseSent: the prompt was delivered; the turn has not been seen to start.
	phaseSent
	// phaseTurn: the harness took the prompt (read back: working or blocked after delivery).
	phaseTurn
	// phaseEnding: the turn ended; the run ends once its final message is read.
	phaseEnding
	// phaseLock: a resume run waits for its live target to be free (rounds.md 6); the run is
	// not the pane's run yet, and the agent's states do not reach it (roundlock.go).
	phaseLock
)

// harnessWatch is the part of a run watch that only a harness run has. s.mu guards it.
type harnessWatch struct {
	kind   string // claude, codex or opencode
	native string // the harness's own session id, when gff-hab chose it (claude --session-id)
	prompt string
	phase  harnessPhase
	// resume: a resume run, whose session is the user's. Its prompt goes in only under the
	// lock (roundlock.go); a send the lock refuses puts the run back to phaseLock.
	resume bool
	// lockReason says why the target is in use, while the run waits for it; waiting says the
	// run has reported waiting; lockUntil is when the wait ends (the round's wait); lockPoll
	// is the next look at the target.
	lockReason string
	waiting    bool
	lockUntil  time.Time
	lockPoll   *time.Timer
	// marker is the begin line of a resume or fork run's envelope: at adoption, a user message
	// that holds it says the prompt went in (the conversation has the target's earlier turns).
	marker string
	// delivered: this gff-hab delivered the run's prompt. A resume run's timeout interrupts
	// only a turn it knows its prompt started.
	delivered bool
	// rev is the newest agent.state revision applied; older ones are stale. state and turn are
	// that state's status and turn id.
	rev   uint64
	state api.AgentStatus
	turn  string
	// blocked: the agent reports blocked, on kindOf. trust: the screen shows Claude Code's
	// workspace trust dialog, which Claude reports no state for; trustGone is when it went.
	blocked   bool
	kindOf    api.BlockedKind
	trust     bool
	trustGone time.Time
	working   bool // the last state was working: a timeout interrupts it
	// adoptDone: the run was adopted after a restart, and its transcript held an agent message
	// after the prompt. The first idle then ends the turn: it ended while gff-hab was down.
	adoptDone bool
	// end is the run's end once its turn ended (phaseEnding). It wins over any end that comes
	// while the final message is read: a pane exit, the timeout, a take over.
	end *rounds.RunUpdate
	// ready fails the run when the harness does not reach idle in time. It runs while the
	// prompt has not gone in, and stops while the run is blocked (a sign-in at start).
	ready *time.Timer
	exit  *time.Timer // the grace after an exited state (roundExitGrace)
	poll  *time.Timer // the next trust dialog check
	// cancel ends the send of the prompt in flight, so a run that ended never pastes it.
	cancel context.CancelFunc
}

// isBlocked says whether the run shows blocked.
func (h *harnessWatch) isBlocked() bool { return h.blocked || h.trust }

// liveState is the live state the run shows: waiting for the lock, blocked or running.
func (h *harnessWatch) liveState() rounds.RunState {
	switch {
	case h.phase == phaseLock:
		return rounds.RunWaiting
	case h.isBlocked():
		return rounds.RunBlocked
	}
	return rounds.RunRunning
}

// stop stops the watch's timers and the send in flight.
func (h *harnessWatch) stop() {
	for _, t := range []*time.Timer{h.ready, h.exit, h.poll, h.lockPoll} {
		if t != nil {
			t.Stop()
		}
	}
	if h.cancel != nil {
		h.cancel()
	}
}

// readyAct is what a step does to the readiness timer.
type readyAct int

const (
	readyKeep readyAct = iota
	readyStop
	readyRestart // start again, whole
)

// stepOut is what one agent state asks of the service; step makes no other change outside h.
type stepOut struct {
	live         rounds.RunState // RunBlocked or RunRunning to report; empty: no change
	ready        readyAct
	trustCleared bool              // the agent reported after the trust dialog: it was answered
	deliver      bool              // send the prompt
	end          *rounds.RunUpdate // the turn ended: end the run once the final message is read
	native       string            // with end: the session id the state reported
	exited       bool              // the harness reported its exit
}

// step applies one agent state to a harness run (rounds.md 13, Built (stage 4)).
//
//   - A revision at or below the last one applied is stale and does nothing. retry applies
//     the agent's current state again after a refused send: only while the run waits for its
//     prompt, and only a revision not older than the last one applied.
//   - idle (or done or error) before the prompt: the prompt goes in.
//   - working or blocked after the prompt went in: the harness took it (read back).
//   - done or error after the prompt went in: the turn ended. A turn end while the send is
//     still in flight counts: the agent was idle when the send began, so the turn is the
//     prompt's. But before the read back, only a change of state or of turn ends it: a
//     republish of a done the agent showed when the send began changes other fields only.
//   - idle in the turn ends nothing: the owner's interrupt key shows idle before the input
//     that carried it takes the run over, and that run must end taken over. Only an adopted
//     run whose transcript showed the turn's end takes idle as its end.
//   - blocked: the run shows blocked until the agent goes on; working or idle shows running.
//   - exited: the harness ended before its turn did.
//   - Once the turn ended (phaseEnding), nothing changes the run's end.
func (h *harnessWatch) step(st api.AgentState, retry bool) stepOut {
	var out stepOut
	if retry {
		if h.phase != phaseStarting || st.Revision < h.rev {
			return out
		}
	} else if st.Revision <= h.rev {
		return out
	}
	h.rev = st.Revision
	prev, prevTurn := h.state, h.turn
	h.state, h.turn = st.State, st.Turn
	if h.phase == phaseEnding {
		return out
	}
	h.working = st.State == api.AgentStatusWorking
	switch st.State {
	case api.AgentStatusBlocked:
		if h.phase == phaseSent || h.phase == phaseDelivering {
			h.phase = phaseTurn
		}
		h.kindOf = st.Kind
		if !h.blocked {
			was := h.isBlocked()
			h.blocked = true
			if !was {
				out.live, out.ready = rounds.RunBlocked, readyStop
			}
		}
		return out
	case api.AgentStatusExited:
		out.exited = true
		return out
	case api.AgentStatusNone, "":
		return out
	}
	if h.blocked || h.trust {
		out.trustCleared = h.trust
		h.blocked, h.kindOf, h.trust = false, "", false
		out.live = rounds.RunRunning
		if h.phase == phaseStarting {
			out.ready = readyRestart
		}
	}
	switch st.State {
	case api.AgentStatusWorking:
		if h.phase == phaseSent || h.phase == phaseDelivering {
			h.phase = phaseTurn
		}
		h.adoptDone = false
	case api.AgentStatusIdle, api.AgentStatusDone, api.AgentStatusError:
		switch {
		case h.phase == phaseStarting:
			h.phase = phaseDelivering
			out.deliver, out.ready = true, readyStop
		case st.State == api.AgentStatusIdle && !(h.phase == phaseTurn && h.adoptDone):
			// No end: see above.
		case (h.phase == phaseDelivering || h.phase == phaseSent) && st.State == prev && st.Turn == prevTurn:
			// A republish of the state the agent had: no new turn end.
		default:
			h.phase = phaseEnding
			u := rounds.RunUpdate{State: rounds.RunSucceeded}
			if st.State == api.AgentStatusError {
				u = rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonTurnError, Network: harness.NetworkFailure(st.Harness, st.Error)}
				if st.Error != "" {
					u.Reason += ": " + st.Error
				}
			}
			h.end = &u
			end := u
			out.end, out.native, out.ready = &end, st.Session, readyStop
		}
	}
	return out
}

// harnessNotFound is the start error of a run whose harness this host does not have.
func harnessNotFound(kind string, err error) error {
	return fmt.Errorf("harness not found: %s is not on gff-hab's PATH (%v)", kind, err)
}

// checkHarness refuses a fresh round whose harness has no adapter, or whose program this host
// does not have (rounds.md 11).
func (s *roundsService) checkHarness(r rounds.Round) error {
	if r.Mode != rounds.ModeFresh {
		return s.checkTarget(r)
	}
	if r.Harness == "" {
		return nil
	}
	if !slices.Contains(harness.Harnesses, r.Harness) {
		return control.Errorf(api.ErrorKindInvalid, "harness: %q has no adapter on this hab; it has %s", r.Harness, strings.Join(harness.Harnesses, ", "))
	}
	if _, err := s.cfg.lookHarness(r.Harness); err != nil {
		return control.Errorf(api.ErrorKindUnavailable, "harness: %s is not installed on this host: %v", r.Harness, err)
	}
	return nil
}

// claudeSessionFlags are the Claude Code flags that pick the session; with none, gff-hab picks
// the new session's id itself.
var claudeSessionFlags = []string{"--session-id", "--resume", "-r", "--continue", "-c", "--fork-session"}

// startHarness starts a harness run's session: the harness, its model and arguments, in the
// round's project. It returns at the spawn; the prompt goes in once the agent is idle.
func (s *roundsService) startHarness(r rounds.Round, run rounds.Run, env []string) (string, error) {
	argv := append([]string{r.Harness}, r.HarnessArgs...)
	native := ""
	if r.Harness == detect.KindClaude && !slices.ContainsFunc(r.HarnessArgs, func(a string) bool {
		name, _, _ := strings.Cut(a, "=")
		return slices.Contains(claudeSessionFlags, name)
	}) {
		// The session id is known from the start, so the final message and a later
		// `claude --resume` find the transcript even where no hook reaches gff-hab.
		native = newUUID().String()
		argv = append(argv, "--session-id", native)
	}
	p := api.SessionCreateParams{Harness: r.Harness, Model: r.Model, Argv: argv, Project: r.Project, Cwd: r.Cwd, Env: env}
	return s.spawnHarness(r, run, p, &harnessWatch{kind: r.Harness, native: native, prompt: r.Prompt})
}

// spawnHarness makes a harness run's new session with p, as the round's run, and watches it
// with h: a fresh run, or a fork.
func (s *roundsService) spawnHarness(r rounds.Round, run rounds.Run, p api.SessionCreateParams, h *harnessWatch) (string, error) {
	if _, err := s.cfg.lookHarness(p.Harness); err != nil {
		return "", harnessNotFound(p.Harness, err)
	}
	p.IdempotencyKey = "round-run-" + run.ID
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = withRoundStart(ctx, roundStart{ref: api.SessionRound{Round: r.ID, Run: run.ID}, name: r.Name})
	made, err := s.cfg.create(ctx, p)
	if err != nil {
		return "", err
	}
	run.SessionID = made.Session
	s.watchHarness(run, r, made.Session, made.Pane, h)
	return made.Session, nil
}

// watchHarness watches a harness run: its pane as watch does, its agent's states, the
// readiness limit and, for Claude Code, the trust dialog. The pane's current state is applied
// at once: the harness may have reported before the spawn returned.
func (s *roundsService) watchHarness(run rounds.Run, r rounds.Round, session, pane string, h *harnessWatch) {
	id := run.ID
	if h.phase == phaseStarting {
		s.mu.Lock()
		h.ready = time.AfterFunc(s.cfg.ready, func() { s.goBg(func() { s.notReady(id) }) })
		s.mu.Unlock()
	}
	if !s.watchWith(run, r, session, pane, h) {
		return
	}
	s.mu.Lock()
	if w := s.watches[id]; w != nil && w.h == h {
		s.armTrustLocked(w)
		if run.State == rounds.RunBlocked && !h.isBlocked() {
			s.liveLocked(w, rounds.RunRunning) // adopted blocked before its prompt
		}
	}
	s.mu.Unlock()
	if st, ok := s.cfg.agent(pane); ok {
		s.agentState(st)
	}
}

// goLocked runs f in the background work Close waits for, unless the service is closed. The
// caller holds s.mu, so Close cannot begin its wait between the check and the start: nothing
// runs, and no prompt is pasted, after Close.
func (s *roundsService) goLocked(f func()) bool {
	if s.closed {
		return false
	}
	s.bg.Go(f)
	return true
}

// goBg is goLocked for a caller that does not hold s.mu: a timer, a link watcher.
func (s *roundsService) goBg(f func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goLocked(f)
}

// agentState takes one agent.state of any pane. The harness store calls it with its lock held,
// so it only takes s.mu and hands any work off its goroutine.
func (s *roundsService) agentState(st api.AgentState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.watches[s.byPane[st.Pane]]
	if s.closed || w == nil || w.h == nil {
		return
	}
	s.applyLocked(w, st, false)
}

// applyLocked steps a harness run with one state and starts what the step asks for. The
// revision check and the step are one critical section with the change they make.
func (s *roundsService) applyLocked(w *runWatch, st api.AgentState, retry bool) {
	h := w.h
	out := h.step(st, retry)
	if out.trustCleared {
		h.trustGone = time.Now()
	}
	switch out.ready {
	case readyStop:
		if h.ready != nil {
			h.ready.Stop()
		}
	case readyRestart:
		if h.ready != nil {
			h.ready.Reset(s.cfg.ready)
		}
	}
	if out.live != "" {
		s.liveLocked(w, out.live)
	}
	id, session := w.run.ID, w.session
	if out.deliver {
		ctx, cancel := context.WithTimeout(context.Background(), roundSendWait)
		h.cancel = cancel
		if !s.goLocked(func() {
			defer cancel()
			s.deliver(ctx, id, session)
		}) {
			cancel()
		}
	}
	if out.end != nil {
		kind, native, u := h.kind, h.native, *out.end
		if native == "" {
			native = out.native
		}
		s.goLocked(func() { s.endTurn(id, session, kind, native, u) })
	}
	if out.exited && h.exit == nil {
		h.exit = time.AfterFunc(roundExitGrace, func() {
			s.goBg(func() { s.finish(id, rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonHarnessExited}) })
		})
	}
}

// liveLocked reports a live state of a harness run to the scheduler, off the caller's
// goroutine. The reports go out one at a time, and each only while the run still shows that
// state, so a late report never overwrites a newer one. A blocked run waiting on the trust
// dialog says so in its reason.
func (s *roundsService) liveLocked(w *runWatch, state rounds.RunState) {
	id := w.run.ID
	s.goLocked(func() {
		s.liveMu.Lock()
		defer s.liveMu.Unlock()
		s.mu.Lock()
		cur := s.watches[id]
		now := cur != nil && cur.h != nil && cur.h.liveState() == state
		reason := ""
		switch {
		case now && state == rounds.RunBlocked && cur.h.trust:
			reason = reasonTrust
		case now && state == rounds.RunWaiting:
			reason = cur.h.lockReason
		}
		s.mu.Unlock()
		if !now {
			return
		}
		// A run that ended meanwhile is not live: nothing to say.
		if err := s.updateRun(id, rounds.RunUpdate{State: state, Reason: reason}); err != nil && !errors.Is(err, rounds.ErrNoRun) {
			s.cfg.logf("rounds: run %s: %v", id, err)
		}
	})
}

// deliver sends a harness run's prompt through the session.send path: a paste, then Enter,
// under the input rules. It is not a take over. ctx ends when the run does, so a run that
// ended never pastes its prompt. A send the agent refuses now (busy, or another send starting
// its turn) waits for the next idle, within the readiness limit; a prompt that may have
// reached the pane in part, or any other refusal, fails the run.
func (s *roundsService) deliver(ctx context.Context, runID, session string) {
	s.mu.Lock()
	w := s.watches[runID]
	if w == nil || w.h == nil || w.h.phase != phaseDelivering || ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	prompt, pane := w.h.prompt, w.pane
	var guard func(string) error
	if w.h.resume {
		guard = func(string) error { return s.lockGuard(session, pane) }
	}
	s.mu.Unlock()
	err := s.cfg.send(ctx, session, prompt, guard)
	var ce *control.Error
	retry := err != nil && errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindConflict && !ce.IsPartial()
	s.mu.Lock()
	w = s.watches[runID]
	if w == nil || w.h == nil {
		s.mu.Unlock()
		return
	}
	h := w.h
	switch {
	case err == nil:
		h.delivered = true
		if h.phase == phaseDelivering {
			h.phase = phaseSent
		}
		s.mu.Unlock()
		return
	case retry && h.phase == phaseDelivering && h.resume:
		// The session is the user's: the prompt waits for the lock again.
		s.relockLocked(w, err)
		s.mu.Unlock()
		return
	case retry && h.phase == phaseDelivering:
		h.phase = phaseStarting
		if h.ready != nil && !h.isBlocked() {
			h.ready.Reset(s.cfg.ready)
		}
		s.mu.Unlock()
		s.cfg.logf("rounds: run %s: the prompt waits: %v", runID, err)
		// The agent may be idle again already: its state is applied again, after a pause, so
		// a refusal that lasts (an owner's stop) does not spin until the readiness limit.
		select {
		case <-time.After(roundSendRetry):
		case <-ctx.Done():
			return
		}
		st, ok := s.cfg.agent(pane)
		if !ok {
			return
		}
		s.mu.Lock()
		if cur := s.watches[runID]; cur == w && !s.closed {
			s.applyLocked(cur, st, true)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if err != nil {
		s.finish(runID, rounds.RunUpdate{State: rounds.RunFailed, Reason: "the prompt was not delivered: " + err.Error()})
	}
}

// notReady fails a harness run that did not reach idle within the readiness limit.
func (s *roundsService) notReady(runID string) {
	s.mu.Lock()
	w := s.watches[runID]
	late := w != nil && w.h != nil && !w.h.isBlocked() && w.h.phase == phaseStarting
	s.mu.Unlock()
	if late {
		s.finish(runID, rounds.RunUpdate{State: rounds.RunFailed,
			Reason: "the harness did not reach idle within " + s.cfg.ready.String()})
	}
}

// endTurn ends a harness run whose turn ended, with the first 4 KiB of its final agent message,
// read from the harness's transcript before the pane may close (opencode's conversation is
// read through its server, which ends with the pane).
func (s *roundsService) endTurn(runID, session, kind, native string, u rounds.RunUpdate) {
	ctx, cancel := context.WithTimeout(context.Background(), roundResultWait)
	defer cancel()
	msg, err := s.cfg.result(ctx, session, kind, native)
	if err != nil {
		s.cfg.logf("rounds: run %s: final message: %v", runID, err)
	}
	u.Result = msg
	s.finishTurn(runID, u)
}

// harnessTimedOut ends a harness run past its timeout (rounds.md 7.3, 7.4): a working turn is
// interrupted through its adapter; a blocked run keeps its request open for the user, and the
// trust dialog stays for the user to answer. The pane stays either way. A turn that already
// ended keeps its end.
func (s *roundsService) harnessTimedOut(w *runWatch) {
	s.mu.Lock()
	blocked, trust, kind, working := w.h.blocked, w.h.trust, w.h.kindOf, w.h.working
	locked, why := w.h.phase == phaseLock, w.h.lockReason
	s.mu.Unlock()
	if locked {
		// The prompt never went in: the run never ran (rounds.md 6.3).
		s.finish(w.run.ID, rounds.RunUpdate{State: rounds.RunSkipped, Reason: why})
		return
	}
	reason := "ran past its timeout of " + time.Duration(w.round.Timeout).String()
	switch {
	case trust:
		reason += "; " + reasonTrust
	case blocked && kind != "":
		reason += "; the agent still waits for its " + string(kind) + " request, which stays open"
	case blocked:
		reason += " while blocked"
	}
	s.mu.Lock()
	ours := !w.h.resume || w.h.delivered // never interrupt a turn of the user's own
	s.mu.Unlock()
	if !s.finish(w.run.ID, rounds.RunUpdate{State: rounds.RunTimedOut, Reason: reason}) || blocked || trust || !working || !ours {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.cfg.interrupt(ctx, w.pane); err != nil {
		s.cfg.logf("rounds: run %s: interrupt pane %s: %v", w.run.ID, w.pane, err)
	}
}

// --- Claude Code's workspace trust dialog ---

// roundScreenRules are Flightdeck's own screen rules for harness runs, in the detection
// manifest format. Claude Code asks whether to trust a folder it has not seen before it reports
// any state; its vendored manifest has no rule for that dialog, and a refresh replaces the
// vendored files, so the rule lives here.
const roundScreenRules = `id = "flightdeck-rounds"
version = "2026.11.2.1"
min_engine_version = 2
updated_at = "2026-11-02T00:00:00Z"

[[rules]]
id = "claude_trust_folder"
state = "blocked"
priority = 1000
region = "whole_recent"
visible_blocker = true
contains = ["yes, i trust this folder"]
any = [
  { contains = ["quick safety check"] },
  { contains = ["do you trust the files in this folder"] },
]
`

var roundRules = sync.OnceValue(func() *detect.Manifest {
	m, err := detect.Parse([]byte(roundScreenRules))
	if err != nil {
		panic("rounds: screen rules: " + err.Error())
	}
	return m
})

// trustDialog says whether screen shows Claude Code's workspace trust dialog.
func trustDialog(screen string) bool {
	r := roundRules().Screen(detect.Input{Screen: screen})
	return r.Matched && r.Rule == "claude_trust_folder"
}

// armTrustLocked schedules the next trust dialog check of a Claude Code run that waits for its
// first idle.
func (s *roundsService) armTrustLocked(w *runWatch) {
	h := w.h
	if s.cfg.screen == nil || h.kind != detect.KindClaude || h.phase != phaseStarting || s.closed {
		return
	}
	id := w.run.ID
	h.poll = time.AfterFunc(roundTrustPoll, func() { s.goBg(func() { s.checkTrust(id) }) })
}

// checkTrust reads a starting run's screen for the trust dialog. While it shows, the run is
// blocked with a reason that names it, and the readiness limit waits; the user answers it in
// the pane, which is not a take over. Rounds never answer it: trusting a folder is the user's
// call.
func (s *roundsService) checkTrust(runID string) {
	s.mu.Lock()
	w := s.watches[runID]
	if w == nil || w.h == nil || w.h.phase != phaseStarting {
		s.mu.Unlock()
		return
	}
	pane := w.pane
	s.mu.Unlock()
	text, ok := s.cfg.screen(pane)
	shown := ok && trustDialog(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watches[runID] != w || w.h.phase != phaseStarting {
		return
	}
	h := w.h
	if shown != h.trust {
		was := h.isBlocked()
		h.trust = shown
		if !shown {
			h.trustGone = time.Now()
		}
		switch {
		case was == h.isBlocked():
		case shown:
			if h.ready != nil {
				h.ready.Stop()
			}
			s.liveLocked(w, rounds.RunBlocked)
		default:
			if h.ready != nil {
				h.ready.Reset(s.cfg.ready)
			}
			s.liveLocked(w, rounds.RunRunning)
		}
	}
	s.armTrustLocked(w)
}

// trustAnswer says whether a client's input to a run's pane answers the trust dialog: it shows,
// or went a moment ago. The caller holds s.mu.
func (w *runWatch) trustAnswerLocked() bool {
	return w.h != nil && (w.h.trust || (!w.h.trustGone.IsZero() && time.Since(w.h.trustGone) < roundTrustGrace))
}

// --- Final message, and adoption after a restart ---

// finalMessage picks a run's result from its conversation: the last agent message after the
// last user message, which is the run's prompt; with no user message, the last agent message.
func finalMessage(msgs []api.ChatMessage) (string, bool) {
	from := 0
	for i, m := range msgs {
		if m.Kind == api.ChatKindUser {
			from = i + 1
		}
	}
	for i := len(msgs) - 1; i >= from; i-- {
		if msgs[i].Kind == api.ChatKindAgent && strings.TrimSpace(msgs[i].Text) != "" {
			return msgs[i].Text, true
		}
	}
	return "", false
}

// readResult reads a run's final message with read, again every 200 ms until one is there or
// ctx ends: the transcript may trail the turn's end.
func readResult(ctx context.Context, read func(context.Context) ([]api.ChatMessage, error)) (string, error) {
	var lastErr error
	for {
		msgs, err := read(ctx)
		if err == nil {
			if msg, ok := finalMessage(msgs); ok {
				return msg, nil
			}
			lastErr = errors.New("no agent message after the prompt")
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", lastErr
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// roundMessages reads a run session's conversation: a Claude Code or Codex transcript by the
// native session id when gff-hab knows it, else through the chat view's reader, which finds the
// session from its agent state (and reads opencode through its server).
func roundMessages(ctx context.Context, chats *chatService, find func(harness, native string) (string, error), session, kind, native string) ([]api.ChatMessage, error) {
	if native != "" && find != nil && (kind == detect.KindClaude || kind == detect.KindCodex) {
		path, err := find(kind, native)
		if err != nil {
			return nil, err
		}
		cfg := transcript.Config{Path: path, Session: session}
		if kind == detect.KindCodex {
			return transcript.NewCodex(cfg).All(ctx)
		}
		return transcript.NewClaude(cfg).All(ctx)
	}
	if chats == nil {
		return nil, errors.New("this hab reads no transcripts")
	}
	page, err := chats.ChatPage(ctx, api.ChatPageParams{Session: session, Limit: 50})
	return page.Messages, err
}

// adoptedTurn reads where an adopted run was from its conversation: delivered, the prompt
// went in (a user message); ended, an agent message follows it. With a marker (a resume or fork
// run, whose conversation holds the target's earlier turns), only a user message that holds
// the marker is the prompt.
func adoptedTurn(msgs []api.ChatMessage, marker string) (delivered, ended bool) {
	at := slices.IndexFunc(msgs, func(m api.ChatMessage) bool {
		return m.Kind == api.ChatKindUser && (marker == "" || strings.Contains(m.Text, marker))
	})
	if at < 0 {
		return false, false
	}
	_, ended = finalMessage(msgs[at:])
	return true, ended
}

// sessionIDArg returns the value of --session-id in a Claude Code argv.
func sessionIDArg(argv []string) string {
	for i, a := range argv {
		if a == "--session-id" && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--session-id="); ok {
			return v
		}
	}
	return ""
}

// adoptHarness sets where a harness run adopted after a gff-hab restart is (rounds.md 8.4).
// Whether the prompt went in is not stored; the run's conversation says. Claude Code's session
// id comes back from the program's argv (--session-id). With no user message the prompt never
// went in: the run waits for its first idle again, under the readiness limit. With an agent
// message after it, the turn may have ended while gff-hab was down: the first idle (or done)
// ends the run. Otherwise, or when the conversation cannot be read, the run is in its turn.
func (s *roundsService) adoptHarness(session string, pid int, h *harnessWatch) {
	if h.kind == detect.KindClaude && h.native == "" && s.cfg.argv != nil && pid > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		argv, err := s.cfg.argv(ctx, pid)
		cancel()
		if err != nil {
			s.cfg.logf("rounds: session %s: harness arguments: %v", session, err)
		}
		h.native = sessionIDArg(argv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), roundResultWait)
	defer cancel()
	msgs, err := s.cfg.messages(ctx, session, h.kind, h.native)
	if errors.Is(err, transcript.ErrNoTranscript) {
		msgs, err = nil, nil // the harness writes its transcript at the first message
	}
	if err != nil {
		s.cfg.logf("rounds: session %s: conversation at adoption: %v; the run is taken to be in its turn", session, err)
		return
	}
	switch delivered, ended := adoptedTurn(msgs, h.marker); {
	case !delivered && h.resume:
		h.phase, h.blocked = phaseLock, false // the user's session: the lock again
	case !delivered:
		// A run blocked before its prompt waited on a sign-in or the trust dialog; the
		// agent's next state, or the screen, says again.
		h.phase, h.blocked = phaseStarting, false
	case ended:
		h.adoptDone = true
	}
}
