package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// Resume and fork runs (rounds.md 5, 6; stage 5). A resume run sends its prompt into the
// target session, the user's, only when nobody uses it: it waits for the lock, then delivers
// through the session.send path with the lock's check under the send's claim. A fork run starts
// a new session from the target's conversation and is then a fresh harness run. Both reuse the
// harness run machinery of roundharness.go for the delivery, the turn and the run's end.

// The in-use windows (rounds.md 6.1).
const (
	roundInputWindow = 2 * time.Minute  // a client typed into the pane, or sent it a message
	roundFocusWindow = 60 * time.Second // a client reported the session in focus
)

// roundLockPoll is how often a waiting resume run looks at its target.
const roundLockPoll = 250 * time.Millisecond

// Why a resume run waits, and the reason it ends skipped with when the wait runs out
// (rounds.md 6.3).
const (
	lockWorking = "working"
	lockBlocked = "blocked"
	lockTyped   = "in use: a client typed into it or sent it a message in the last 2 minutes"
	lockFocused = "in use: a client has it in focus"
	lockSending = "in use: another message is starting its turn"
	lockRound   = "in use: another round's run holds it"
	lockNoAgent = "in use: no agent this hab knows runs in it"
	lockLost    = "in use: its holder is not connected; its program may still run"
	lockStarted = "in use: gff-hab just started"
	lockElse    = "in use: its conversation runs in another pane"
)

// forks says whether a harness's adapter forks a conversation natively (rounds.md 5): Claude
// Code (--resume <id> --fork-session) and Codex (codex fork <id>). opencode is not verified yet.
func forks(kind string) bool { return kind == detect.KindClaude || kind == detect.KindCodex }

// runEnv is the GFF_ROUND_* values of a run (rounds.md 5.1), as NAME=value.
func (s *roundsService) runEnv(r rounds.Round, run rounds.Run) []string {
	env := []string{"GFF_ROUND=" + r.ID, "GFF_ROUND_RUN=" + run.ID}
	if run.Slot != nil {
		env = append(env, "GFF_ROUND_SLOT="+run.Slot.UTC().Format(time.RFC3339))
	}
	if e, ok := s.store.Get(r.ID); ok && e.State.LastOK != nil {
		env = append(env, "GFF_ROUND_LAST_OK="+e.State.LastOK.UTC().Format(time.RFC3339))
	}
	if run.Events > 0 {
		env = append(env, "GFF_ROUND_EVENTS="+s.store.EventsPath(r.ID, run.ID))
	}
	return env
}

// envelope is a resume or fork run's prompt in its envelope, with the run's values in it: a
// live session's environment cannot carry them. An event run's block follows it in the same
// paste (rounds.md 4.5).
func (s *roundsService) envelope(r rounds.Round, run rounds.Run, env []string) string {
	slot := ""
	if run.Slot != nil {
		slot = run.Slot.UTC().Format(time.RFC3339)
	}
	text := channels.RoundEnvelope(run.ID, r.Name, run.Number, slot, env, r.Prompt)
	if run.Events > 0 {
		text += "\n\n" + s.eventBlockOf(run)
	}
	return text
}

// envelopeMarker is the begin line of a run's envelope.
func envelopeMarker(runID string) string { return "[begin round message " + runID + "]" }

// --- What a target is ---

// targetSeen returns the target's harness and native session id: from its live agent, else
// from what the round remembered. The memo counts only when it was seen in ss and ss is the
// round's target now: a run still on the target an edit replaced never reads another's.
func (s *roundsService) targetSeen(roundID string, ss api.Session) rounds.TargetSeen {
	seen, _ := s.seenOf(roundID, ss)
	return seen
}

// seenOf is targetSeen, and whether the round should remember what it returns: it is new, it
// was seen live, and ss is the round's target.
func (s *roundsService) seenOf(roundID string, ss api.Session) (rounds.TargetSeen, bool) {
	var seen rounds.TargetSeen
	e, ok := s.store.Get(roundID)
	current := ok && ss.ID != "" && e.Round.Target == ss.ID
	if current && e.State.Target != nil && e.State.Target.Session == ss.ID {
		seen = *e.State.Target
	}
	if ss.Pane == "" {
		return seen, false
	}
	st, ok := s.cfg.agent(ss.Pane)
	if !ok || !slices.Contains(harness.Harnesses, st.Harness) {
		return seen, false
	}
	now := rounds.TargetSeen{Session: ss.ID, Harness: st.Harness, Native: st.Session}
	if now.Native == "" && now.Harness == seen.Harness {
		now.Native = seen.Native
	}
	return now, current && now != seen
}

// noteTargetLocked remembers on the round what its target runs, when it was seen live and ss
// is the round's target. The caller holds s.editMu, so no edit changes the target between the
// check and the write.
func (s *roundsService) noteTargetLocked(roundID string, ss api.Session) {
	now, changed := s.seenOf(roundID, ss)
	if !changed {
		return
	}
	if _, err := s.store.SetState(roundID, func(rs *rounds.RoundState) { rs.Target = &now }); err != nil {
		s.cfg.logf("rounds: round %s: remember target: %v", roundID, err)
	}
}

// checkTarget refuses a fork round whose target's adapter cannot fork (rounds.md 5). A target
// whose harness is not known yet passes; its runs say why they cannot start.
func (s *roundsService) checkTarget(r rounds.Round) error {
	if r.Mode != rounds.ModeFork {
		return nil
	}
	ss, ok := s.cfg.store.Session(r.Target)
	if !ok {
		return nil // definition refused it already
	}
	seen := s.targetSeen(r.ID, ss)
	if seen.Harness != "" && !forks(seen.Harness) {
		return control.Errorf(api.ErrorKindInvalid, "mode: the target runs %s, which cannot fork a conversation yet; use resume", seen.Harness)
	}
	return nil
}

// rememberTarget saves what a new or edited round's target runs, so a later run can resume or
// fork it after its program exited. It runs after the edit committed; the caller holds
// s.editMu.
func (s *roundsService) rememberTarget(r rounds.Round) {
	if r.Mode == rounds.ModeFresh {
		return
	}
	if ss, ok := s.cfg.store.Session(r.Target); ok {
		s.noteTargetLocked(r.ID, ss)
	}
}

// --- Fork ---

// startFork starts a fork run: a new session from the target's conversation, named after the
// round, in the target's folder, then the prompt as in a fresh run. The target is untouched.
func (s *roundsService) startFork(r rounds.Round, run rounds.Run, env []string) (string, error) {
	ss, ok := s.cfg.store.Session(r.Target)
	if !ok {
		return "", rounds.ErrTargetGone
	}
	seen := s.targetSeen(r.ID, ss)
	switch {
	case seen.Harness == "":
		return "", errors.New("the target's harness is not known: no agent this hab knows was seen in it")
	case !forks(seen.Harness):
		return "", fmt.Errorf("the target runs %s, which cannot fork a conversation yet", seen.Harness)
	case seen.Native == "":
		return "", fmt.Errorf("the target's %s session id is not known yet", seen.Harness)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cwd := s.cfg.folder(ctx, ss.ID)
	cancel()
	if cwd == "" {
		return "", errors.New("the target's folder is not known")
	}
	argv := forkArgv(seen.Harness, seen.Native, r.HarnessArgs)
	p := api.SessionCreateParams{Harness: seen.Harness, Model: r.Model, Argv: argv, Project: ss.Project, Cwd: cwd, Env: env}
	return s.spawnHarness(r, run, p, &harnessWatch{kind: seen.Harness, prompt: s.envelope(r, run, env), marker: envelopeMarker(run.ID)})
}

// forkArgv is the start of a fork of native: Claude Code `claude <args> --resume <id>
// --fork-session`, Codex `codex fork <id> <args>`.
func forkArgv(kind, native string, args []string) []string {
	if kind == detect.KindCodex {
		return append([]string{"codex", "fork", native}, args...)
	}
	return append(append([]string{"claude"}, args...), "--resume", native, "--fork-session")
}

// resumeArgv is the start of an exited target again: `claude <args> --resume <id>`, `codex
// resume <id> <args>`.
func resumeArgv(kind, native string, args []string) []string {
	if kind == detect.KindCodex {
		return append([]string{"codex", "resume", native}, args...)
	}
	return append(append([]string{"claude"}, args...), "--resume", native)
}

// --- Resume ---

// startResume starts a resume run. A live target: the run waits for the lock, then its prompt
// goes in. An exited target is started again first (resumeTarget). It returns the target's id
// at once; waiting comes back as a run update.
func (s *roundsService) startResume(r rounds.Round, run rounds.Run, env []string) (string, error) {
	ss, ok := s.cfg.store.Session(r.Target)
	if !ok {
		return "", rounds.ErrTargetGone
	}
	seen := s.targetSeen(r.ID, ss)
	h := &harnessWatch{kind: seen.Harness, native: seen.Native, prompt: s.envelope(r, run, env), resume: true,
		marker: envelopeMarker(run.ID), phase: phaseLock}
	s.lockWatch(run, r, ss, h)
	return ss.ID, nil
}

// lockWatch watches a resume run that waits for its target, and looks at it at once.
func (s *roundsService) lockWatch(run rounds.Run, r rounds.Round, ss api.Session, h *harnessWatch) bool {
	h.lockUntil = lockUntil(run, r)
	h.waiting = run.State == rounds.RunWaiting
	if !s.watchWith(run, r, ss.ID, ss.Pane, h) {
		return false
	}
	id := run.ID
	s.goBg(func() { s.checkLock(id) })
	return true
}

// lockUntil is when a resume run's wait for the lock runs out: wait, counted from its start.
func lockUntil(run rounds.Run, r rounds.Round) time.Time {
	started := run.Created
	if run.StartedAt != nil {
		started = *run.StartedAt
	}
	return started.Add(time.Duration(r.Wait))
}

// noteInput records a client's input to a pane.
func (s *roundsService) noteInput(pane string) {
	s.mu.Lock()
	s.inputAt[pane] = time.Now()
	s.mu.Unlock()
}

// typedWithin reports whether a client typed into pane, or sent its session a message, within d
// (self-compact's send guard, harness-adapters.md).
func (s *roundsService) typedWithin(pane string, d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.inputAt[canonicalPane(pane)]
	return ok && time.Since(at) < d
}

// noteSessionInput records a client's session.send to a session.
func (s *roundsService) noteSessionInput(session string) {
	if ss, ok := s.cfg.store.Session(session); ok && ss.Pane != "" {
		s.noteInput(ss.Pane)
	}
}

// noteFocus records a client's session.focus report: active true while the session is on
// screen in an active window; false when it left.
func (s *roundsService) noteFocus(client, session string, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !active {
		delete(s.focus[session], client)
		if len(s.focus[session]) == 0 {
			delete(s.focus, session)
		}
		return
	}
	if s.focus[session] == nil {
		s.focus[session] = map[string]time.Time{}
	}
	s.focus[session][client] = time.Now()
}

// clientUseLocked is the in-use reason from clients: input to the pane in the last 2 minutes,
// or the session in focus on a client within 60 s. Those signals live in memory only, so for
// the quiet window after the service starts every pane counts as in use: a client may have
// typed into it, or have it on screen, while gff-hab restarted. The caller holds s.mu.
func (s *roundsService) clientUseLocked(session, pane string) string {
	now := time.Now()
	if at, ok := s.inputAt[pane]; ok && now.Sub(at) < roundInputWindow {
		return lockTyped
	}
	for _, at := range s.focus[session] {
		if now.Sub(at) < roundFocusWindow {
			return lockFocused
		}
	}
	if now.Sub(s.started) < s.cfg.quiet {
		return lockStarted
	}
	return ""
}

// lockGuard runs under a resume run's send claim, before any byte is written: the agent's
// state has passed the send's own check, and no other send can start now. A client's input or
// focus refuses the send with conflict, so the run waits again.
func (s *roundsService) lockGuard(session, pane string) error {
	s.mu.Lock()
	why := s.clientUseLocked(session, pane)
	s.mu.Unlock()
	if why != "" {
		return control.Errorf(api.ErrorKindConflict, "session %s is %s", session, why)
	}
	return nil
}

// agentUse is the in-use reason from the target's agent (rounds.md 6.1).
func agentUse(st api.AgentState, ok bool) string {
	switch {
	case !ok || !slices.Contains(harness.Harnesses, st.Harness):
		return lockNoAgent
	case st.State == api.AgentStatusWorking:
		return lockWorking
	case st.State == api.AgentStatusBlocked || st.Request != nil:
		return lockBlocked
	case st.State == api.AgentStatusIdle, st.State == api.AgentStatusDone, st.State == api.AgentStatusError:
		return ""
	}
	return lockNoAgent
}

// checkLock looks at a waiting resume run's target. Gone: the round completes. Exited: it is
// started again. In use: the run waits, and past its wait ends skipped with the reason. Free:
// the run takes the pane and its prompt goes in under the lock.
func (s *roundsService) checkLock(runID string) {
	s.mu.Lock()
	w := s.watches[runID]
	if s.closed || w == nil || w.h == nil || w.h.phase != phaseLock {
		s.mu.Unlock()
		return
	}
	session := w.session
	s.mu.Unlock()

	ss, ok := s.cfg.store.Session(session)
	if !ok {
		s.finish(runID, rounds.RunUpdate{State: rounds.RunSkipped, Reason: rounds.ErrTargetGone.Error(), TargetGone: true})
		return
	}
	lost := ss.Pane != "" && ss.Exit == nil && !s.cfg.complete()
	why := ""
	if (ss.State == api.SessionStateExited || ss.Pane == "") && !lost {
		// Its conversation may run on in another pane: the user resumed it there, or Capcom
		// did. A second program on one conversation is never started.
		if !s.liveElsewhere(s.targetSeen(w.run.RoundID, ss), ss.Pane) {
			s.resumeTarget(w, ss)
			return
		}
		why = lockElse
	}
	var st api.AgentState
	if why == "" {
		var agentOK bool
		st, agentOK = s.cfg.agent(ss.Pane)
		why = agentUse(st, agentOK)
		if ss.State == api.SessionStateExited {
			why = lockLost
		}
		if why == "" && s.cfg.sendHeld(ss.Pane) {
			why = lockSending
		}
		if agentOK && slices.Contains(harness.Harnesses, st.Harness) {
			s.editMu.Lock()
			s.noteTargetLocked(w.run.RoundID, ss)
			s.editMu.Unlock()
		}
	}

	s.liveMu.Lock() // the waiting and running reports go out in order
	defer s.liveMu.Unlock()
	s.mu.Lock()
	if s.watches[runID] != w || w.h.phase != phaseLock || s.closed {
		s.mu.Unlock()
		return
	}
	h := w.h
	if why == "" {
		why = s.clientUseLocked(ss.ID, ss.Pane)
	}
	if other, held := s.byPane[ss.Pane]; why == "" && held && other != runID {
		why = lockRound
	}
	if why != "" {
		if time.Now().After(h.lockUntil) {
			s.mu.Unlock()
			s.finish(runID, rounds.RunUpdate{State: rounds.RunSkipped, Reason: why})
			return
		}
		changed := h.lockReason != why
		h.lockReason = why
		report := !h.waiting || changed
		h.waiting = true
		h.lockPoll = time.AfterFunc(roundLockPoll, func() { s.goBg(func() { s.checkLock(runID) }) })
		s.mu.Unlock()
		if report {
			if err := s.updateRun(runID, rounds.RunUpdate{State: rounds.RunWaiting, Reason: why}); err != nil && !errors.Is(err, rounds.ErrNoRun) {
				s.cfg.logf("rounds: run %s: %v", runID, err)
			}
		}
		return
	}
	// Free: the run takes the pane. From here on the agent's states reach it, and the
	// pane's input, exit and close are the run's.
	w.pane = ss.Pane
	s.byPane[ss.Pane] = runID
	h.kind, h.lockReason = st.Harness, ""
	if h.native == "" {
		h.native = st.Session
	}
	h.rev, h.state, h.turn = st.Revision, st.State, st.Turn
	h.phase = phaseDelivering
	waited := h.waiting
	h.waiting = false
	s.mu.Unlock()
	if waited {
		// The scheduler's running is the commit: a pause that ended the waiting run first
		// leaves nothing to send.
		if err := s.updateRun(runID, rounds.RunUpdate{State: rounds.RunRunning}); err != nil {
			s.mu.Lock()
			if s.watches[runID] == w {
				s.dropLocked(w)
			}
			s.mu.Unlock()
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watches[runID] != w || h.phase != phaseDelivering {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), roundSendWait)
	h.cancel = cancel
	if !s.goLocked(func() {
		defer cancel()
		s.deliver(ctx, runID, w.session)
	}) {
		cancel()
	}
}

// relockLocked puts a resume run whose send was refused back to waiting for the lock: the
// agent turned busy, another send came first, or a client used the session. The caller holds
// s.mu.
func (s *roundsService) relockLocked(w *runWatch, err error) {
	h := w.h
	h.phase = phaseLock
	if h.lockUntil.IsZero() { // a target this run started again
		h.lockUntil = lockUntil(w.run, w.round)
	}
	if s.byPane[w.pane] == w.run.ID {
		delete(s.byPane, w.pane)
	}
	s.cfg.logf("rounds: run %s: the prompt waits for the lock: %v", w.run.ID, err)
	id := w.run.ID
	s.goLocked(func() {
		time.Sleep(roundLockPoll)
		s.checkLock(id)
	})
}

// liveElsewhere says whether an agent this hab knows runs seen's conversation in a pane other
// than pane.
func (s *roundsService) liveElsewhere(seen rounds.TargetSeen, pane string) bool {
	if seen.Native == "" {
		return false
	}
	for _, a := range s.cfg.agents() {
		if a.Pane != pane && a.Harness == seen.Harness && a.Session == seen.Native && a.State != api.AgentStatusExited {
			return true
		}
	}
	return false
}

// resumeTarget starts an exited target's harness again in a new pane of the same session, as
// session.resume does; the run's prompt then goes in at the agent's first idle, as in a fresh
// run (rounds.md 5). Nobody used the new pane yet, but the send still runs the lock's check.
func (s *roundsService) resumeTarget(w *runWatch, ss api.Session) {
	fail := func(reason string) {
		s.finish(w.run.ID, rounds.RunUpdate{State: rounds.RunFailed, Reason: "resume the exited target: " + reason})
	}
	seen := s.targetSeen(w.run.RoundID, ss)
	switch {
	case seen.Harness == detect.KindOpencode:
		fail("resuming an exited opencode session is not built yet")
		return
	case seen.Harness == "":
		fail("its harness is not known: no agent this hab knows was seen in it")
		return
	case seen.Native == "":
		fail("its " + seen.Harness + " session id is not known")
		return
	}
	if _, err := s.cfg.lookHarness(seen.Harness); err != nil {
		fail(harnessNotFound(seen.Harness, err).Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cwd := s.cfg.folder(ctx, ss.ID)
	if cwd == "" {
		fail("its folder is not known")
		return
	}
	s.mu.Lock()
	if s.watches[w.run.ID] != w || w.h.phase != phaseLock || s.closed {
		s.mu.Unlock()
		return
	}
	run, r, waited := w.run, w.round, w.h.waiting
	h := &harnessWatch{kind: seen.Harness, native: seen.Native, prompt: w.h.prompt, resume: true, marker: w.h.marker}
	s.mu.Unlock()
	if waited {
		if err := s.updateRun(run.ID, rounds.RunUpdate{State: rounds.RunRunning}); err != nil {
			s.unwatch(w)
			return
		}
	}
	p := api.SessionCreateParams{Harness: seen.Harness, Model: r.Model, Argv: resumeArgv(seen.Harness, seen.Native, r.HarnessArgs)}
	pane, err := s.cfg.resume(ctx, ss.ID, p, cwd)
	var ce *control.Error
	if err != nil && errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindConflict {
		// Another run started it first (two rounds on one target): the run waits for the
		// lock, and takes its turn.
		s.mu.Lock()
		if s.watches[run.ID] == w && !s.closed {
			w.h.waiting = false // it reported running: waiting goes out again
			s.relockLocked(w, err)
		}
		s.mu.Unlock()
		return
	}
	if err != nil {
		fail(err.Error())
		return
	}
	// The run may have ended during the start (its timeout, a pause, a delete), or gff-hab
	// closed: then the target runs on, and gets no prompt.
	if !s.watchHarnessOver(w, run, r, ss.ID, pane, h) {
		s.cfg.logf("rounds: run %s: it ended while its target started again; the target runs on", run.ID)
	}
}

// adoptResume picks up a resume run that was live when the last gff-hab stopped. Its
// conversation says whether the prompt went in (the envelope's begin line in a user
// message): then the run is in its turn, else it waits for the lock again.
func (s *roundsService) adoptResume(run rounds.Run, r rounds.Round, ss api.Session) {
	env := s.runEnv(r, run)
	seen := s.targetSeen(r.ID, ss)
	h := &harnessWatch{kind: seen.Harness, native: seen.Native, prompt: s.envelope(r, run, env), resume: true,
		marker: envelopeMarker(run.ID), phase: phaseLock}
	if run.State == rounds.RunWaiting || ss.Pane == "" || ss.State == api.SessionStateExited {
		s.lockWatch(run, r, ss, h)
		return
	}
	h.phase, h.blocked = phaseTurn, run.State == rounds.RunBlocked
	pid := 0
	if st, ok := s.cfg.pane(ss.Pane); ok {
		pid = int(st.Pid)
	}
	s.adoptHarness(ss.ID, pid, h)
	if h.phase == phaseLock {
		s.lockWatch(run, r, ss, h)
		return
	}
	s.watchHarness(run, r, ss.ID, ss.Pane, h)
}

// roundPush passes session.focus reports to the rounds' lock (rounds.md 6.1).
type roundPush struct {
	control.Push
	rounds *roundsService
}

func (p roundPush) Focus(client string, f api.SessionFocusParams) error {
	err := p.Push.Focus(client, f)
	if err == nil || !f.Active {
		p.rounds.noteFocus(client, f.Session, f.Active)
	}
	return err
}

// forgetTarget drops what a round remembered of its target, when an edit names another.
func (s *roundsService) forgetTarget(roundID string) {
	if _, err := s.store.SetState(roundID, func(rs *rounds.RoundState) { rs.Target = nil }); err != nil {
		s.cfg.logf("rounds: round %s: forget target: %v", roundID, err)
	}
}

// resumeExited starts an exited session's harness again in a new pane of the same session, as
// Capcom's resume does: an exited pane the holder still keeps is released first, so the new
// pane is the session's only one.
func resumeExited(ctx context.Context, a *actions, session string, p api.SessionCreateParams, cwd string) (string, error) {
	ss, ok := a.store.Session(session)
	if !ok {
		return "", rounds.ErrTargetGone
	}
	if ss.State != api.SessionStateExited {
		return "", control.Errorf(api.ErrorKindConflict, "session %s is running", session)
	}
	if ss.Pane != "" {
		if err := a.ClosePane(ctx, ss.Pane); err != nil {
			var ce *control.Error
			if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
				return "", err
			}
		}
	}
	return a.resumeSession(ctx, session, p, cwd)
}
