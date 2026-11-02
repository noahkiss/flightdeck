package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// roundResultWait bounds the read of a run's final agent message from its transcript: the
// harness may report the turn's end a moment before the transcript holds its last message.
const roundResultWait = 5 * time.Second

// Reasons gff-hab writes on harness runs.
const (
	reasonHarnessExited = "the harness exited before its turn ended"
	reasonTurnError     = "the turn ended with an error"
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
)

// harnessWatch is the part of a run watch that only a harness run has.
type harnessWatch struct {
	kind   string // claude, codex or opencode
	native string // the harness's own session id, when gff-hab chose it (claude --session-id)
	prompt string
	phase  harnessPhase
	// rev is the newest agent.state revision applied; older ones are stale.
	rev     uint64
	blocked bool
	kindOf  api.BlockedKind // with blocked: what the agent waits for
	working bool            // the last state was working: a timeout interrupts it
	// ready fails the run when the harness does not reach idle in time. It runs while the
	// prompt has not gone in, and stops while the run is blocked (a sign-in at start).
	ready *time.Timer
}

// harnessNotFound is the start error of a run whose harness this host does not have.
func harnessNotFound(kind string, err error) error {
	return fmt.Errorf("harness not found: %s is not on gff-hab's PATH (%v)", kind, err)
}

// checkHarness refuses a fresh round whose harness has no adapter, or whose program this host
// does not have (rounds.md 11).
func (s *roundsService) checkHarness(r rounds.Round) error {
	if r.Mode != rounds.ModeFresh || r.Harness == "" {
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
	if _, err := s.cfg.lookHarness(r.Harness); err != nil {
		return "", harnessNotFound(r.Harness, err)
	}
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
	p := api.SessionCreateParams{Harness: r.Harness, Model: r.Model, Argv: argv, Project: r.Project, Cwd: r.Cwd, Env: env,
		IdempotencyKey: "round-run-" + run.ID}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = withRoundStart(ctx, roundStart{ref: api.SessionRound{Round: r.ID, Run: run.ID}, name: r.Name})
	made, err := s.cfg.create(ctx, p)
	if err != nil {
		return "", err
	}
	run.SessionID = made.Session
	s.watchHarness(run, r, made.Session, made.Pane, &harnessWatch{kind: r.Harness, native: native, prompt: r.Prompt})
	return made.Session, nil
}

// watchHarness watches a harness run: its pane as watch does, its agent's states, and the
// readiness limit. The pane's current state is applied at once: the harness may have reported
// before the spawn returned.
func (s *roundsService) watchHarness(run rounds.Run, r rounds.Round, session, pane string, h *harnessWatch) {
	if h.phase == phaseStarting {
		s.mu.Lock()
		h.ready = time.AfterFunc(s.cfg.ready, func() { s.notReady(run.ID) })
		s.mu.Unlock()
	}
	s.watchWith(run, r, session, pane, h)
	if st, ok := s.cfg.agent(pane); ok {
		s.agentState(st)
	}
}

// agentState takes one agent.state of any pane. The harness store calls it with its lock held,
// so it only takes s.mu and hands any work off its goroutine.
func (s *roundsService) agentState(st api.AgentState) {
	s.mu.Lock()
	w := s.watches[s.byPane[st.Pane]]
	if s.closed || w == nil || w.h == nil || st.Revision <= w.h.rev {
		s.mu.Unlock()
		return
	}
	w.h.rev = st.Revision
	act := s.stepLocked(w, st)
	s.mu.Unlock()
	if act != nil {
		act()
	}
}

// stepLocked applies one state to a harness run and returns what to do after s.mu is released.
//
//   - idle (or done or error) before the prompt: the prompt goes in.
//   - working or blocked after the prompt went in: the harness took it (read back).
//   - done or error after the prompt went in: the turn ended; the run ends with its final
//     message. A turn end while the send is still in flight counts: the agent was idle when
//     the send began, so the turn is the prompt's.
//   - blocked: the run shows blocked until the agent goes on; working or idle shows running.
//   - exited: the harness ended before its turn did.
//
// idle after the prompt went in ends nothing: the owner's interrupt key shows idle before the
// input that carried it takes the run over, and a run must end taken over then, not failed.
func (s *roundsService) stepLocked(w *runWatch, st api.AgentState) func() {
	h := w.h
	if h.phase == phaseEnding {
		return nil
	}
	var acts []func()
	h.working = st.State == api.AgentStatusWorking
	switch st.State {
	case api.AgentStatusBlocked:
		if h.phase == phaseSent || h.phase == phaseDelivering {
			h.phase = phaseTurn
		}
		h.kindOf = st.Kind
		if !h.blocked {
			h.blocked = true
			if h.ready != nil {
				h.ready.Stop()
			}
			acts = append(acts, s.liveLater(w, rounds.RunBlocked))
		}
		return join(acts)
	case api.AgentStatusExited:
		id := w.run.ID
		return func() { s.finish(id, rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonHarnessExited}) }
	case api.AgentStatusNone, "":
		return nil
	}
	if h.blocked {
		h.blocked, h.kindOf = false, ""
		if h.ready != nil && h.phase <= phaseDelivering {
			h.ready.Reset(s.cfg.ready)
		}
		acts = append(acts, s.liveLater(w, rounds.RunRunning))
	}
	switch st.State {
	case api.AgentStatusWorking:
		if h.phase == phaseSent || h.phase == phaseDelivering {
			h.phase = phaseTurn
		}
	case api.AgentStatusIdle, api.AgentStatusDone, api.AgentStatusError:
		switch {
		case h.phase == phaseStarting:
			h.phase = phaseDelivering
			id, session := w.run.ID, w.session
			acts = append(acts, func() { s.bg.Go(func() { s.deliver(id, session) }) })
		case st.State == api.AgentStatusIdle:
			// No end: see above.
		default:
			h.phase = phaseEnding
			if h.ready != nil {
				h.ready.Stop()
			}
			id, session, kind, native := w.run.ID, w.session, h.kind, h.native
			if native == "" {
				native = st.Session
			}
			u := rounds.RunUpdate{State: rounds.RunSucceeded}
			if st.State == api.AgentStatusError {
				u = rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonTurnError, Network: harness.NetworkFailure(st.Harness, st.Error)}
				if st.Error != "" {
					u.Reason += ": " + st.Error
				}
			}
			acts = append(acts, func() { s.bg.Go(func() { s.endTurn(id, session, kind, native, u) }) })
		}
	}
	return join(acts)
}

func join(acts []func()) func() {
	if len(acts) == 0 {
		return nil
	}
	return func() {
		for _, a := range acts {
			a()
		}
	}
}

// liveLater reports a live state of a harness run to the scheduler, off the caller's goroutine.
// The reports go out one at a time, and each only while the run still shows that state, so a
// late report never overwrites a newer one.
func (s *roundsService) liveLater(w *runWatch, state rounds.RunState) func() {
	id := w.run.ID
	return func() {
		s.bg.Go(func() {
			s.liveMu.Lock()
			defer s.liveMu.Unlock()
			s.mu.Lock()
			cur := s.watches[id]
			now := cur != nil && cur.h != nil && cur.h.blocked == (state == rounds.RunBlocked)
			s.mu.Unlock()
			if now {
				s.update(id, rounds.RunUpdate{State: state})
			}
		})
	}
}

// deliver sends a harness run's prompt through the session.send path: a paste, then Enter,
// under the input rules. It is not a take over. A send the agent refuses now (busy, or another
// send starting its turn) waits for the next idle, within the readiness limit; a prompt that
// may have reached the pane in part, or any other refusal, fails the run.
func (s *roundsService) deliver(runID, session string) {
	s.mu.Lock()
	w := s.watches[runID]
	if w == nil || w.h == nil || w.h.phase != phaseDelivering {
		s.mu.Unlock()
		return
	}
	prompt, pane := w.h.prompt, w.pane
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := s.cfg.send(ctx, session, prompt)
	cancel()
	var ce *control.Error
	retry := err != nil && errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindConflict && !ce.IsPartial()
	s.mu.Lock()
	w = s.watches[runID]
	if w == nil || w.h == nil {
		s.mu.Unlock()
		return
	}
	switch {
	case err == nil:
		if w.h.phase == phaseDelivering {
			w.h.phase = phaseSent
		}
		if w.h.ready != nil {
			w.h.ready.Stop()
		}
		s.mu.Unlock()
		return
	case retry && w.h.phase == phaseDelivering:
		w.h.phase = phaseStarting
		s.mu.Unlock()
		s.cfg.logf("rounds: run %s: the prompt waits: %v", runID, err)
		// The agent may be idle again already: its state is applied as if new, after a pause,
		// so a refusal that lasts (an owner's stop) does not spin until the readiness limit.
		time.Sleep(roundSendRetry)
		if st, ok := s.cfg.agent(pane); ok {
			s.mu.Lock()
			if cur := s.watches[runID]; cur != nil && cur.h != nil && cur.h.rev >= st.Revision {
				cur.h.rev = st.Revision - 1
			}
			s.mu.Unlock()
			s.agentState(st)
		}
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
	late := w != nil && w.h != nil && !w.h.blocked && w.h.phase <= phaseDelivering
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
	s.finish(runID, u)
}

// harnessTimedOut ends a harness run past its timeout (rounds.md 7.3, 7.4): a working turn is
// interrupted through its adapter; a blocked run keeps its request open for the user. The pane
// stays either way.
func (s *roundsService) harnessTimedOut(w *runWatch) {
	s.mu.Lock()
	blocked, kind, working := w.h.blocked, w.h.kindOf, w.h.working
	s.mu.Unlock()
	reason := "ran past its timeout of " + time.Duration(w.round.Timeout).String()
	if blocked {
		reason += "; the agent still waits for its " + string(kind) + " request, which stays open"
		if kind == "" {
			reason = "ran past its timeout of " + time.Duration(w.round.Timeout).String() + " while blocked"
		}
	}
	if !s.finish(w.run.ID, rounds.RunUpdate{State: rounds.RunTimedOut, Reason: reason}) || blocked || !working {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.cfg.interrupt(ctx, w.pane); err != nil {
		s.cfg.logf("rounds: run %s: interrupt pane %s: %v", w.run.ID, w.pane, err)
	}
}

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
