package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/panetext"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Session actions (hab-logic-api.md 3.13): a named action on a session, which the hab translates
// for the harness in its pane. The Capcom service keeps agents fresh with it; the owner may use
// it too. compact and clear type the harness's own command through session.send's path and wait
// for its end (Claude Code only: the other harnesses answer unavailable until their rows are
// checked); restart, close and recreate are the same for every harness.

// sessionActionCapability is the capability of session.action.
const sessionActionCapability = "session-action"

// The states of a session action, in order.
const (
	actionQueued  = "queued"
	actionSent    = "sent"
	actionRunning = "running"
	actionDone    = "done"
	actionFailed  = "failed"
)

// sessionActions is the closed set of actions.
var sessionActions = []api.SessionActionKind{"compact", "clear", "restart", "close", "recreate"}

// sessionActionTurn is how long restart, close and recreate wait in queued for the agent's turn
// to end, as a compaction waits.
var sessionActionTurn = 60 * time.Minute

// sessionActionService runs session actions, one per session at most. Nil funcs answer
// unavailable for their action.
type sessionActionService struct {
	store  *structure.Store
	agents compactAgents
	// compacts runs compact and clear on a Claude Code pane.
	compacts *compactService
	// restart is session.restart's path; closePane and closeSession end a session; recreate
	// starts a fresh session under the old one's name (memberSwap.recreate).
	restart      func(ctx context.Context, session string) (string, error)
	closePane    func(ctx context.Context, pane string) error
	closeSession func(session string) error
	recreate     func(ctx context.Context, ss api.Session, st api.AgentState, seed, seedFrom string) (string, error)
	// recreatable refuses a recreate that would start the session wider, or as something else,
	// than it was started (memberSwap.recreatable). Nil: no check.
	recreatable func(ss api.Session, st api.AgentState) error
	publish     func(structure.Event)
	logf        func(string, ...any)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	pending map[string]*api.SessionAction // by session id
}

func newSessionActions(store *structure.Store, agents compactAgents, publish func(structure.Event), logf func(string, ...any)) *sessionActionService {
	if publish == nil {
		publish = func(structure.Event) {}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &sessionActionService{store: store, agents: agents, publish: publish, logf: logf, ctx: ctx, cancel: cancel,
		pending: map[string]*api.SessionAction{}}
}

// Close ends each pending action and waits for it. A pending action is in memory only: a
// recreate cut short undoes its rename (hab-logic-api.md 3.13).
func (s *sessionActionService) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

func sessionActionID() string {
	var b [8]byte
	rand.Read(b[:])
	return "act-" + hex.EncodeToString(b[:])
}

// checkSessionAction checks the params before any target is looked up.
func checkSessionAction(p api.SessionActionParams) error {
	if !slices.Contains(sessionActions, p.Action) {
		return control.Errorf(api.ErrorKindInvalid, "action %q: want one of %v", p.Action, sessionActions)
	}
	if p.FollowUp != "" && p.Action != "compact" && p.Action != "clear" && p.Action != "recreate" {
		return control.Errorf(api.ErrorKindInvalid, "follow_up goes only with compact, clear and recreate")
	}
	if n := utf8.RuneCountInString(p.FollowUp); n > maxFollowUp {
		return control.Errorf(api.ErrorKindInvalid, "the follow-up is %d characters; at most %d", n, maxFollowUp)
	}
	if p.Session == "" {
		return control.Errorf(api.ErrorKindInvalid, "session.action needs a session")
	}
	return nil
}

// actionOpts are what the door adds to an action: recheck runs the caller's rule again just
// before the action runs (nil for the owner), and seedFrom names who wrote the follow-up.
type actionOpts struct {
	recheck  func() error
	seedFrom string
}

// Request starts an action on a session of this hab and answers at once with state queued.
func (s *sessionActionService) Request(ss api.Session, action api.SessionActionKind, followUp string, o actionOpts) (api.SessionAction, error) {
	if ss.Pane == "" || ss.State == api.SessionStateExited {
		return api.SessionAction{}, control.Errorf(api.ErrorKindConflict, "session %s has no live pane", ss.Name)
	}
	st, _ := s.agents.Get(ss.Pane)
	agent := st.State != "" && st.State != api.AgentStatusNone && st.State != api.AgentStatusExited
	harness := ""
	if agent {
		harness = st.Harness
	}
	a := &api.SessionAction{ID: sessionActionID(), Session: ss.Name, SessionID: ss.ID, Action: action, Harness: harness,
		State: actionQueued, At: time.Now().UTC().Format(time.RFC3339Nano)}
	switch action {
	case "compact", "clear":
		if !agent {
			return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "%s needs an agent in session %s; recreate works for any session", action, ss.Name)
		}
		if harness != detect.KindClaude || s.compacts == nil {
			// Codex and opencode: not checked against their pinned versions (3.13).
			return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "%s is not available for %s on this hab; recreate works for every harness", action, harness)
		}
	case "restart":
		if s.restart == nil {
			return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "this hab cannot restart sessions")
		}
	case "close":
		if s.closePane == nil || s.closeSession == nil {
			return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "this hab cannot close sessions")
		}
	case "recreate":
		if s.recreate == nil {
			return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "this hab cannot recreate sessions")
		}
		if s.recreatable != nil {
			if err := s.recreatable(ss, st); err != nil {
				return api.SessionAction{}, err
			}
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "gff-hab is stopping")
	}
	if p := s.pending[ss.ID]; p != nil {
		s.mu.Unlock()
		return api.SessionAction{}, control.Errorf(api.ErrorKindConflict, "session action %s is already pending for session %s", p.ID, ss.Name)
	}
	s.pending[ss.ID] = a
	s.mu.Unlock()
	if action == "compact" || action == "clear" {
		// The rule is checked again under each send's claim, just before a key goes in.
		_, err := s.compacts.Action(ss.Pane, "/"+string(action), followUp, o.recheck, func(c api.Compaction) { s.fromCompaction(a, c) })
		if err != nil {
			s.mu.Lock()
			delete(s.pending, ss.ID)
			s.mu.Unlock()
			return api.SessionAction{}, err
		}
		return s.snapshot(a), nil
	}
	s.wg.Add(1)
	s.emit(a)
	go func() {
		defer s.wg.Done()
		s.run(a, ss, agent, followUp, o)
	}()
	return s.snapshot(a), nil
}

func (s *sessionActionService) snapshot(a *api.SessionAction) api.SessionAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *a
}

func (s *sessionActionService) emit(a *api.SessionAction) {
	c := s.snapshot(a)
	s.publish(structure.Event{Type: api.EventSessionAction, ID: c.ID, Data: c})
}

// step moves a to state and sends the event; a final state ends it.
func (s *sessionActionService) step(a *api.SessionAction, state, reason string) {
	s.mu.Lock()
	a.State, a.Reason, a.At = state, reason, time.Now().UTC().Format(time.RFC3339Nano)
	if state == actionDone || state == actionFailed {
		if s.pending[a.SessionID] == a {
			delete(s.pending, a.SessionID)
		}
	}
	s.mu.Unlock()
	if reason != "" {
		s.logf("session action %s (%s): %s: %s", a.ID, a.Action, state, reason)
	} else {
		s.logf("session action %s (%s): %s", a.ID, a.Action, state)
	}
	s.emit(a)
}

// fromCompaction maps a compaction's step to the action's: compacting is running.
func (s *sessionActionService) fromCompaction(a *api.SessionAction, c api.Compaction) {
	state := c.State
	if state == compactCompacting {
		state = actionRunning
	}
	s.step(a, state, c.Reason)
}

// waitIdle waits until the session's agent is idle, done or in error, or runs no agent, as
// session.restart needs. It gives up after sessionActionTurn.
func (s *sessionActionService) waitIdle(pane string) error {
	deadline := time.NewTimer(sessionActionTurn)
	defer deadline.Stop()
	for {
		changed := s.agents.Changed(pane)
		st, ok := s.agents.Get(pane)
		if !ok || st.State == "" || st.State == api.AgentStatusNone || st.State == api.AgentStatusExited ||
			((st.State == api.AgentStatusIdle || st.State == api.AgentStatusDone || st.State == api.AgentStatusError) && st.Request == nil) {
			return nil
		}
		select {
		case <-changed:
		case <-deadline.C:
			return fmt.Errorf("the turn did not end within %v", sessionActionTurn)
		case <-s.ctx.Done():
			return errors.New("gff-hab stopped")
		}
	}
}

// run moves restart, close and recreate through their steps.
func (s *sessionActionService) run(a *api.SessionAction, ss api.Session, hadAgent bool, followUp string, o actionOpts) {
	if err := s.waitIdle(ss.Pane); err != nil {
		s.step(a, actionFailed, err.Error())
		return
	}
	// The wait may take an hour: act on the session as it is now, and only on the pane the
	// caller named.
	if s.store != nil {
		now, ok := s.store.Session(ss.ID)
		if !ok || now.Pane != ss.Pane || now.State == api.SessionStateExited {
			s.step(a, actionFailed, "the session changed while the action waited: its pane closed or moved")
			return
		}
		ss = now
	}
	st, _ := s.agents.Get(ss.Pane)
	if hadAgent && (st.State == "" || st.State == api.AgentStatusNone || st.State == api.AgentStatusExited || st.Harness != a.Harness) {
		s.step(a, actionFailed, "denied: the agent left the pane while the action waited")
		return
	}
	// Authorise again: the rule, or the pane's program, may have changed during the wait.
	if o.recheck != nil {
		if err := o.recheck(); err != nil {
			s.step(a, actionFailed, "denied: "+errText(err))
			return
		}
	}
	s.step(a, actionRunning, "")
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	switch a.Action {
	case "restart":
		if _, err := s.restart(ctx, ss.ID); err != nil {
			s.step(a, actionFailed, errText(err))
			return
		}
	case "close":
		if err := s.closePane(ctx, ss.Pane); err != nil {
			s.step(a, actionFailed, "closing the pane: "+errText(err))
			return
		}
		if err := s.closeSession(ss.ID); err != nil {
			s.step(a, actionFailed, "the pane closed, but the record stays: "+errText(err))
			return
		}
	case "recreate":
		id, err := s.recreate(ctx, ss, st, followUp, o.seedFrom)
		if err != nil {
			s.step(a, actionFailed, errText(err))
			return
		}
		s.mu.Lock()
		a.NewSession = id
		s.mu.Unlock()
	}
	s.step(a, actionDone, "")
}

// recreateStart is how a recreate starts the fresh session: the argv and model the old one was
// started with, the session-picking flags taken out.
type recreateStart struct {
	argv  []string
	model string
}

// recreatePlan decides how a session starts again fresh (hab-logic-api.md 3.13), so a recreate
// never starts it wider, or as something else, than it was started. It refuses Capcom's own
// session and a round's run session, which their owners start; a harness the owner started by
// hand in a shell, whose flags the hab never saw; and Codex or opencode started with flags. A
// Claude Code start keeps its flags but those that pick the session. A plain shell starts again
// as it was. The hab keeps no start environment, so none is carried.
func (w *memberSwap) recreatePlan(ss api.Session, st api.AgentState) (recreateStart, error) {
	use := func(why string) error {
		return control.Errorf(api.ErrorKindUnavailable, "recreate is not available for session %s: %s; use restart, which keeps its start", ss.Name, why)
	}
	switch {
	case ss.Kind == api.SessionKindCapcom:
		return recreateStart{}, use("it is a Capcom session")
	case ss.Round != nil:
		return recreateStart{}, use("a round's run started it, with the round's arguments, environment and limits")
	}
	var e resumeEntry
	known := false
	if w.start != nil {
		e, known = w.start(ss.ID)
	}
	if st.Harness == "" {
		if known && e.Harness == "" {
			return recreateStart{argv: slices.Clone(e.Argv)}, nil
		}
		if known && e.Harness != "" {
			return recreateStart{}, use("its harness is not running")
		}
		return recreateStart{}, nil // a shell the hab knows nothing more of: the hab's shell
	}
	if !known || e.Harness != st.Harness {
		return recreateStart{}, use(st.Harness + " was started by hand or by another program, so how it started is not known")
	}
	argv := slices.Clone(e.Argv)
	switch {
	case len(argv) <= 1:
	case st.Harness == detect.KindClaude:
		argv = stripSessionFlags(argv)
	case st.Harness == detect.KindCodex:
		argv = slices.DeleteFunc(argv, func(a string) bool { return a == "--no-daemon" })
		if len(argv) > 1 {
			return recreateStart{}, use("Codex was started with flags the hab does not carry")
		}
	default:
		return recreateStart{}, use(st.Harness + " was started with flags the hab does not carry")
	}
	return recreateStart{argv: argv, model: e.Model}, nil
}

// stripSessionFlags drops the Claude Code flags that pick the session, and their values.
func stripSessionFlags(argv []string) []string {
	out := []string{argv[0]}
	for i := 1; i < len(argv); i++ {
		name, _, inline := strings.Cut(argv[i], "=")
		if !slices.Contains(claudeSessionFlags, name) {
			out = append(out, argv[i])
			continue
		}
		takesValue := name == "--session-id" || name == "--resume" || name == "-r"
		if takesValue && !inline && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
			i++
		}
	}
	return out
}

// recreatable is recreatePlan's refusal alone, for the request.
func (w *memberSwap) recreatable(ss api.Session, st api.AgentState) error {
	if ss.Group != nil {
		return nil
	}
	_, err := w.recreatePlan(ss, st)
	return err
}

// recreate starts a fresh session under ss's name, with the same folder and start, and
// follow-up as its seed from seedFrom (hab-logic-api.md 3.13). The old session is renamed
// <name>-<n> and its pane closed; its conversation stays resumable. Messages held for it move to
// the new one. A group member goes through the group's swap, which keeps its group. The swap
// mark makes a stop mid-way undone or finished at the next start; a failed create puts the old
// name back unless the fresh session runs all the same.
func (w *memberSwap) recreate(ctx context.Context, ss api.Session, st api.AgentState, seed, seedFrom string) (string, error) {
	if ss.Group != nil {
		return w.swapFrom(ctx, ss, st, nil, seed, seedFrom)
	}
	plan, err := w.recreatePlan(ss, st)
	if err != nil {
		return "", err
	}
	folder := ""
	if w.folder != nil {
		folder = w.folder(ctx, ss.ID)
	}
	if folder == "" {
		return "", fmt.Errorf("session %s: its folder is not known", ss.Name)
	}
	release, err := w.hold(ctx, ss.ID, ss.Name)
	if err != nil {
		return "", fmt.Errorf("holding its messages: %w", err)
	}
	defer release()
	name := ss.Name
	aside := w.asideName(name)
	// The mark goes on disk first, as a group swap's: a gff-hab that stops mid-way finishes or
	// undoes it at its next start (undoSwaps, finishSwaps). No group: Group stays empty.
	if err := w.store.SetSwap(ss.ID, &structure.SwapMark{Name: name, Aside: aside}); err != nil {
		return "", err
	}
	if err := w.rename(ss.ID, aside); err != nil {
		w.store.SetSwap(ss.ID, nil)
		return "", fmt.Errorf("renaming the old session: %s", errText(err))
	}
	key := sha256.Sum256([]byte(ss.ID + "\x00recreate\x00" + name))
	p := api.SessionCreateParams{Name: name, Harness: st.Harness, Argv: plan.argv, IdempotencyKey: "recreate-" + hex.EncodeToString(key[:16])}
	if st.Harness != "" {
		p.Model, p.Seed = plan.model, seed
		if seed != "" && validSeedFrom(seedFrom) {
			p.SeedFrom = seedFrom
		}
	}
	if ss.Project != "" {
		p.ProjectDir = folder // filed again in the folder's project
	} else {
		p.Cwd = folder // an unfiled session stays unfiled, in any folder
	}
	// The fresh session starts in the mode the old one was started in, not the current
	// setting (harness-adapters.md, Start mode).
	made, err := w.create(withRecordedMode(ctx, ss.StartMode), p)
	if err != nil {
		// A create that timed out may have made the session all the same: then go on with it.
		id, ok := swapSuccessor(w.store, ss.ID, name, "")
		if !ok {
			if rerr := w.rename(ss.ID, name); rerr != nil {
				w.logf("session action: putting back the name of %s: %v", aside, rerr)
			}
			w.store.SetSwap(ss.ID, nil)
			return "", err
		}
		w.logf("session action: %s: the create answered %v, but the fresh session %s runs; the recreate goes on", name, err, id)
		made = api.SessionCreated{Session: id}
	}
	if err := w.store.SetReplaced(made.Session, ss.ID); err != nil {
		w.logf("session action: %s: replaced: %v", name, err)
	}
	if n, err := w.move(ss.ID, made.Session); err != nil {
		w.logf("session action: %s: moving its held messages: %v", name, err)
	} else if n > 0 {
		w.logf("session action: %s: %d held messages moved to the fresh session", name, n)
	}
	if ss.Pane != "" {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := w.closePane(cctx, ss.Pane); err != nil {
			w.logf("session action: closing the old pane of %s (%s): %v", name, aside, err)
		}
		cancel()
	}
	if err := w.store.SetSwap(ss.ID, nil); err != nil {
		w.logf("session action: %s: clearing the swap mark: %v", name, err)
	}
	return made.Session, nil
}

// --- The door ---

// Action serves session.action (hab-logic-api.md 3.13). The owner's door runs it at every
// value. The Capcom service runs it wherever agent.drive_panes lets it type into the session's
// pane, here or through its forward. Any other agent door is denied: close, clear and recreate
// end another agent's work.
func (s *paneService) Action(ctx context.Context, c paneCaller, p api.SessionActionParams) (api.SessionAction, error) {
	if err := checkSessionAction(p); err != nil {
		return api.SessionAction{}, err
	}
	if c.door != doorOwner && !c.isCapcom() {
		s.logf("panes: session.action denied: %s, caller %s", p.Action, c.name())
		return api.SessionAction{}, control.Errorf(api.ErrorKindDenied, "session.action runs only from the owner's door and the Capcom service; an agent compacts itself with agent.compact")
	}
	if s.acts == nil {
		return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "session.action needs gff-hab serve with its holder link")
	}
	t, peer, bare, err := s.prepareBy(gateDrive, "session.action", c, "", p.Session)
	if err != nil {
		return api.SessionAction{}, err
	}
	if peer != nil {
		if !slices.Contains(peer.Caps, sessionActionCapability) {
			return api.SessionAction{}, control.Errorf(api.ErrorKindUnavailable, "peer %s runs without %s; update its gff-hab", peer.Label, sessionActionCapability)
		}
		fwd := p
		fwd.Session, fwd.As = bare, c.forwardAs()
		var r api.SessionAction
		fctx, cancel := context.WithTimeout(ctx, s.drv.forwardWait())
		defer cancel()
		if err := peer.Caller.Call(fctx, "session.action", fwd, &r); err != nil {
			return api.SessionAction{}, peerErr(peer.Label, err)
		}
		r.ID, r.State, r.Reason = panetext.CleanControls(r.ID), panetext.CleanControls(r.State), panetext.CleanControls(r.Reason)
		r.Action, r.Harness = p.Action, panetext.CleanControls(r.Harness)
		r.Session, r.SessionID, r.NewSession = peer.Label+"/"+bare, "", ""
		return r, nil
	}
	if !t.filed {
		return api.SessionAction{}, control.Errorf(api.ErrorKindInvalid, "pane %s has no session", t.pane)
	}
	var o actionOpts
	if c.isCapcom() {
		// Capcom wrote the follow-up; and its rule is checked again just before the action
		// runs, on the same pane: the rule, or the pane's program, may change while it waits.
		o.seedFrom = s.capcomAddress(c)
		o.recheck = s.recheck(c, t.session.ID, t.pane)
	}
	r, err := s.acts.Request(t.session, p.Action, p.FollowUp, o)
	if err != nil {
		return api.SessionAction{}, err
	}
	s.logf("panes: session.action: caller %s: %s on pane %s (%s)", c.name(), p.Action, t.pane, r.ID)
	return r, nil
}

// recheck runs the caller's rule for session id again, and refuses when the session's pane is
// no longer pane: the action was authorised for that pane's program.
func (s *paneService) recheck(c paneCaller, id, pane string) func() error {
	return func() error {
		t, _, _, err := s.prepareBy(gateDrive, "session.action", c, "", id)
		if err != nil {
			return err
		}
		if !samePane(t.pane, pane) {
			return control.Errorf(api.ErrorKindDenied, "the session's pane changed")
		}
		return nil
	}
}

// capcomAddress is Capcom's address as this hab names it, for a seed's author: this hab's host
// for its own route; for a forward, the label of the peer that holds the Capcom mark, so a reply
// reaches Capcom's home, not this hab. "" when that peer is not in the peer list.
func (s *paneService) capcomAddress(c paneCaller) string {
	if !c.isCapcom() {
		return ""
	}
	if c.door == doorRoute {
		return s.host + "/" + capcomServiceName
	}
	if s.capcomHome == nil || s.links == nil {
		return ""
	}
	home := s.capcomHome()
	for _, l := range s.links() {
		if home != "" && l.DeviceID == home && l.Label != "" {
			return l.Label + "/" + capcomServiceName
		}
	}
	return ""
}

func (c controlPanes) Action(ctx context.Context, from control.PaneCaller, p api.SessionActionParams) (api.SessionAction, error) {
	return c.s.Action(ctx, c.s.markCapcom(controlCaller(from), from.Device, p.As), p)
}
