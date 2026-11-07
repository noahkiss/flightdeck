package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// Context supervision (groups.md 9, stage B6). Each hab supervises its own group members: it
// fills each member's context_pct, compacts a Claude Code member at group.compact_at, and
// starts a fresh session under the member's name after group.fresh_after_compacts compacts,
// at group.fresh_transcript_mb, or, for a harness it cannot compact, at group.handoff_at. A
// member, the coordinator or the user asks for the same with member.compact and
// member.handoff. Every action waits until the member is not in use (the rounds lock,
// rounds.md 6.1); supervision's own waits for good, a sibling's up to [groups] sibling_wait.

// The kinds and states of a supervision action.
const (
	actCompact api.MemberActionKind = "compact"
	actHandoff api.MemberActionKind = "handoff"

	actQueued  api.MemberActionState = "queued"
	actRunning api.MemberActionState = "running"
	actDone    api.MemberActionState = "done"
	actSkipped api.MemberActionState = "skipped"
	actFailed  api.MemberActionState = "failed"
)

// superviseTick is how often members are read; superviseTickSeam replaces it in tests.
const superviseTick = 2 * time.Second

// superviseRetry is how long after a failed automatic action supervision tries again.
const superviseRetry = time.Minute

// keptActions bounds the ended actions kept for member.action.
const keptActions = 200

// compactStarter is what supervision needs of *compactService.
type compactStarter interface {
	Start(pane, followUp string, guard func(pane string) error) (api.Compaction, <-chan api.Compaction, error)
}

// superviseConfig is what the supervisor needs of gff-hab. Every func may be called from any
// goroutine.
type superviseConfig struct {
	host string // this hab's host id
	// members lists this hab's running group members with a pane; session reads one session.
	members func() []api.Session
	session func(id string) (api.Session, bool)
	agents  compactAgents
	// reading is a member's context use and its transcript path (memberReading).
	reading    func(pane string, st api.AgentState) (*int, string)
	setPct     func(id string, pct *int) bool
	addCompact func(id string) (int, error)
	compacts   compactStarter // nil: no compaction on this hab
	// inUse is the rounds lock's reason a session is in use (rounds.md 6.1); "" when free.
	inUse func(ss api.Session) string
	// hold holds channel deliveries to a member, its name kept with it (channels.Service.Hold);
	// nil: none.
	hold func(ctx context.Context, session, name string) (func(), error)
	// guard runs under a send's claim: a client's use refuses the send with conflict.
	guard func(session, pane string) error
	// send delivers text to a session through session.send's path, with guard.
	send func(ctx context.Context, session, text string, guard func(pane string) error) error
	// record reads a group's record, on this hab or from its home (the item's title, task and
	// brief, the member's model).
	record func(ctx context.Context, group string) (api.Group, error)
	// fresh replaces a member with a fresh session under its name (memberSwap.swap).
	fresh func(ctx context.Context, ss api.Session, st api.AgentState, rec *api.Group, seed string) (string, error)
	// note sends a local session a message from Flightdeck.
	note    func(name, text string) error
	setting func(id string) json.RawMessage
	keys    groups.Keys
	tick    time.Duration
	logf    func(string, ...any)
}

// superviseService runs the actions, one per member at a time.
type superviseService struct {
	cfg superviseConfig

	mu      sync.Mutex
	byID    map[string]*supAction
	ended   []string              // ended action ids, oldest first
	live    map[string]*supAction // the running action, by member session id
	retryAt map[string]time.Time  // no automatic action before this, by session id
	ours    map[string]time.Time  // a compaction of ours ended at, by session id: its hook is not counted again
	fails   map[string]int        // automatic actions failed in a row, by session id
	stop    chan struct{}
	wg      sync.WaitGroup
}

type supAction struct {
	a     api.MemberAction
	auto  bool
	until time.Time // a sibling's: skipped after this; zero for supervision's own
	pct   *int      // the reading that started it
	tell  string    // a local session to tell the end to; "" for none
	// noted closes on the member's first group_note after the hand-off ask.
	asked bool
	noted chan struct{}
	nOnce sync.Once
	// compacting is set while our compaction runs: its hook is not counted as another.
	compacting bool
	done       chan struct{}
}

func newSupervisor(cfg superviseConfig) *superviseService {
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	if cfg.tick <= 0 {
		cfg.tick = superviseTick
	}
	return &superviseService{cfg: cfg, byID: map[string]*supAction{}, live: map[string]*supAction{},
		retryAt: map[string]time.Time{}, ours: map[string]time.Time{}, fails: map[string]int{}, stop: make(chan struct{})}
}

// Start runs the reading loop until Stop.
func (s *superviseService) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.cfg.tick)
		defer t.Stop()
		for {
			s.pass()
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
}

// Stop ends the loop and every action.
func (s *superviseService) Stop() {
	s.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *superviseService) intSetting(id string, def int) int {
	var v int
	if s.cfg.setting == nil || json.Unmarshal(s.cfg.setting(id), &v) != nil {
		return def
	}
	return v
}

// supervised reports whether a member's harness is one supervision reads. SDK members manage
// their own context (groups.md 9.1).
func supervised(st api.AgentState) bool {
	return st.Harness == detect.KindClaude || st.Harness == detect.KindCodex || st.Harness == detect.KindOpencode
}

// pass reads each member once: its context_pct, and the automatic action it is due.
func (s *superviseService) pass() {
	for _, ss := range s.cfg.members() {
		st, ok := s.cfg.agents.Get(ss.Pane)
		if !ok || !supervised(st) || st.State == api.AgentStatusExited || st.State == api.AgentStatusNone {
			continue
		}
		pct, path := s.cfg.reading(ss.Pane, st)
		s.cfg.setPct(ss.ID, pct)
		kind, why := s.due(ss, st, pct, path)
		if kind == "" {
			continue
		}
		s.mu.Lock()
		busy := s.live[ss.ID] != nil || time.Now().Before(s.retryAt[ss.ID])
		s.mu.Unlock()
		if busy {
			continue
		}
		if _, err := s.begin(ss, kind, "Flightdeck", "", pct, true); err == nil {
			s.cfg.logf("supervise: %s (%s): %s: %s", ss.Name, ss.ID, kind, why)
		}
	}
}

// canCompact reports whether this hab compacts the member's harness: self-compact covers
// Claude Code only (groups.md 9.2).
func (s *superviseService) canCompact(st api.AgentState) bool {
	return s.cfg.compacts != nil && st.Harness == detect.KindClaude
}

// due is the automatic action a member is due, and why; "" for none (groups.md 9.1 to 9.3).
func (s *superviseService) due(ss api.Session, st api.AgentState, pct *int, path string) (api.MemberActionKind, string) {
	big := false
	mb := s.intSetting(settings.GroupFreshTranscriptMB, 4)
	if path != "" {
		if fi, err := os.Stat(path); err == nil && fi.Size() >= int64(mb)<<20 {
			big = true
		}
	}
	if pct == nil {
		// No reading: supervised by the transcript's size only.
		if big {
			return actHandoff, fmt.Sprintf("the transcript is %d MB or more", mb)
		}
		return "", ""
	}
	compactAt := s.intSetting(settings.GroupCompactAt, 40)
	if s.canCompact(st) && compactAt > 0 {
		if *pct < compactAt {
			return "", ""
		}
		fresh := s.intSetting(settings.GroupFreshAfterCompacts, 4)
		switch {
		case ss.Compacts >= fresh:
			return actHandoff, fmt.Sprintf("context %d%% after %d compacts", *pct, ss.Compacts)
		case big:
			return actHandoff, fmt.Sprintf("context %d%% with a transcript of %d MB or more", *pct, mb)
		}
		return actCompact, fmt.Sprintf("context %d%%", *pct)
	}
	if at := s.intSetting(settings.GroupHandoffAt, 60); *pct >= at {
		return actHandoff, fmt.Sprintf("context %d%%, the hand-off level", *pct)
	}
	if big {
		return actHandoff, fmt.Sprintf("the transcript is %d MB or more", mb)
	}
	return "", ""
}

func actionID() string {
	var b [8]byte
	rand.Read(b[:])
	return "sup-" + hex.EncodeToString(b[:])
}

func (s *superviseService) stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// begin starts an action on a local member. A second one while one runs is refused with
// conflict.
func (s *superviseService) begin(ss api.Session, kind api.MemberActionKind, asker, tell string, pct *int, auto bool) (api.MemberAction, error) {
	if ss.Group == nil {
		return api.MemberAction{}, control.Errorf(api.ErrorKindInvalid, "session %s is not a group member", ss.Name)
	}
	st, ok := s.cfg.agents.Get(ss.Pane)
	if !ok || !supervised(st) {
		return api.MemberAction{}, control.Errorf(api.ErrorKindUnavailable, "no supported harness runs in member %s", ss.Name)
	}
	if kind == actCompact && !s.canCompact(st) {
		return api.MemberAction{}, control.Errorf(api.ErrorKindUnavailable, "this hab cannot compact %s; ask for member_handoff", st.Harness)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.stop:
		return api.MemberAction{}, control.Errorf(api.ErrorKindUnavailable, "supervision is stopping")
	default:
	}
	if cur := s.live[ss.ID]; cur != nil {
		return api.MemberAction{}, control.Errorf(api.ErrorKindConflict, "action %s (%s) runs on member %s; one at a time", cur.a.ID, cur.a.Kind, ss.Name)
	}
	a := &supAction{a: api.MemberAction{ID: actionID(), Group: ss.Group.ID, Member: ss.Name, Session: ss.ID, Host: s.cfg.host,
		Kind: kind, State: actQueued, Asker: asker, At: s.stamp()}, auto: auto, pct: pct, tell: tell,
		noted: make(chan struct{}), done: make(chan struct{})}
	if !auto {
		a.until = time.Now().Add(s.cfg.keys.SiblingWait)
	}
	s.live[ss.ID] = a
	s.byID[a.a.ID] = a
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(a)
	}()
	return a.a, nil
}

// Request is a sibling action on a local member by its session name (groups.md 9.4).
func (s *superviseService) Request(kind api.MemberActionKind, ss api.Session, asker, tell string) (api.MemberAction, error) {
	return s.begin(ss, kind, asker, tell, nil, false)
}

// Get returns an action by its id.
func (s *superviseService) Get(id string) (api.MemberAction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byID[id]
	if a == nil {
		return api.MemberAction{}, false
	}
	return a.a, true
}

// Noted hears a member's group_note: a hand-off that asked for one goes on.
func (s *superviseService) Noted(session string) {
	s.mu.Lock()
	a := s.live[session]
	s.mu.Unlock()
	if a != nil && a.a.Kind == actHandoff {
		s.mu.Lock()
		asked := a.asked
		s.mu.Unlock()
		if asked {
			a.nOnce.Do(func() { close(a.noted) })
		}
	}
}

// Observe hears every hook: a Claude Code compaction of a member counts toward
// group.fresh_after_compacts, its own and the user's included (groups.md 9.1). Ours is counted
// when it ends, once.
func (s *superviseService) Observe(pane string, env harness.Envelope, session func(pane string) (api.Session, bool)) {
	if env.Harness != detect.KindClaude || env.Event != "SessionStart" {
		return
	}
	var p struct {
		Source  string `json:"source"`
		AgentID string `json:"agent_id"`
	}
	if json.Unmarshal(env.Payload, &p) != nil || p.Source != "compact" || p.AgentID != "" {
		return
	}
	ss, ok := session(pane)
	if !ok || ss.Group == nil {
		return
	}
	s.mu.Lock()
	a := s.live[ss.ID]
	mine := a != nil && a.compacting || time.Since(s.ours[ss.ID]) < 30*time.Second
	s.mu.Unlock()
	if mine {
		return
	}
	if n, err := s.cfg.addCompact(ss.ID); err == nil {
		s.cfg.logf("supervise: %s: a compaction, %d counted", ss.Name, n)
	}
}

// step moves an action to state, with reason.
func (s *superviseService) step(a *supAction, state api.MemberActionState, reason string) {
	s.mu.Lock()
	a.a.State, a.a.Reason, a.a.At = state, reason, s.stamp()
	s.mu.Unlock()
}

// end finishes an action: it leaves the member free, is kept for member.action, and its asker
// hears how it ended.
func (s *superviseService) end(a *supAction, state api.MemberActionState, reason string) {
	s.mu.Lock()
	a.a.State, a.a.Reason, a.a.At = state, reason, s.stamp()
	if s.live[a.a.Session] == a {
		delete(s.live, a.a.Session)
	}
	if a.auto && state != actDone {
		// A lasting failure backs off, doubling to 30 minutes, so the member is not asked
		// again every minute (review M4).
		s.fails[a.a.Session]++
		d := superviseRetry << min(s.fails[a.a.Session]-1, 5)
		s.retryAt[a.a.Session] = time.Now().Add(min(d, 30*time.Minute))
	} else if state == actDone {
		delete(s.fails, a.a.Session)
	}
	s.ended = append(s.ended, a.a.ID)
	for len(s.ended) > keptActions {
		delete(s.byID, s.ended[0])
		s.ended = s.ended[1:]
	}
	out := a.a
	s.mu.Unlock()
	close(a.done)
	s.cfg.logf("supervise: %s %s of %s: %s %s", out.ID, out.Kind, out.Member, out.State, out.Reason)
	if a.tell != "" && s.cfg.note != nil {
		if err := s.cfg.note(a.tell, actionText(out)); err != nil {
			s.cfg.logf("supervise: telling %s: %v", a.tell, err)
		}
	}
}

// actionText is how an action ended, as its asker reads it.
func actionText(a api.MemberAction) string {
	t := fmt.Sprintf("member_%s for %s (action %s): %s", a.Kind, a.Member, a.ID, a.State)
	if a.Reason != "" {
		t += ": " + a.Reason
	}
	return t + "."
}

// errStopped: the hab is stopping.
var errStopped = errors.New("supervision stopped")

// errEnded: the action ended while it waited (skipped, or the member is gone).
var errEnded = errors.New("ended")

// current reads the member's session, which must still be the action's and running.
func (s *superviseService) current(a *supAction) (api.Session, api.AgentState, error) {
	ss, ok := s.cfg.session(a.a.Session)
	if !ok || ss.State != api.SessionStateRunning || ss.Pane == "" || ss.Group == nil {
		return api.Session{}, api.AgentState{}, errors.New("the member's session ended")
	}
	st, ok := s.cfg.agents.Get(ss.Pane)
	if !ok || !supervised(st) || st.State == api.AgentStatusExited || st.State == api.AgentStatusNone {
		return api.Session{}, api.AgentState{}, errors.New("no agent runs in the member's pane")
	}
	return ss, st, nil
}

// waitFree waits until the member is not in use. A sibling's action past its wait ends
// skipped with the reason (rounds.md 6.3); supervision's own waits on, and looks again at each
// change of the agent's state.
func (s *superviseService) waitFree(a *supAction) (api.Session, api.AgentState, error) {
	poll := time.NewTicker(roundLockPoll)
	defer poll.Stop()
	last := ""
	for {
		ss, st, err := s.current(a)
		if err != nil {
			s.end(a, actFailed, err.Error())
			return ss, st, errEnded
		}
		changed := s.cfg.agents.Changed(ss.Pane)
		why := s.cfg.inUse(ss)
		if why == "" {
			return ss, st, nil
		}
		if !a.until.IsZero() && time.Now().After(a.until) {
			s.end(a, actSkipped, why)
			return ss, st, errEnded
		}
		if why != last {
			s.step(a, actQueued, why)
			last = why
		}
		select {
		case <-s.stop:
			s.end(a, actFailed, "gff-hab stopped")
			return ss, st, errStopped
		case <-changed:
		case <-poll.C:
		}
	}
}

// sendFree delivers text into the member once it is not in use, under the lock's guard. A
// conflict (a client typed, another send) waits again. It returns the agent's revision just
// before the send.
func (s *superviseService) sendFree(a *supAction, text string) (api.Session, uint64, error) {
	return s.sendFreeThen(a, text, nil)
}

// sendFreeThen is sendFree with before, run just before each try of the send; an error from it
// waits and tries again, as a conflict does.
func (s *superviseService) sendFreeThen(a *supAction, text string, before func(ss api.Session) error) (api.Session, uint64, error) {
	for {
		ss, st, err := s.waitFree(a)
		if err != nil {
			return ss, 0, err
		}
		guard := func(pane string) error { return s.cfg.guard(ss.ID, pane) }
		ready := true
		if before != nil {
			if berr := before(ss); berr != nil {
				ready = false // try again after the poll
			}
		}
		if ready {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = s.cfg.send(ctx, ss.ID, text, guard)
			cancel()
			if err == nil {
				return ss, st.Revision, nil
			}
			var ce *control.Error
			if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
				s.end(a, actFailed, "the message did not go in: "+err.Error())
				return ss, 0, errEnded
			}
		}
		select {
		case <-s.stop:
			s.end(a, actFailed, "gff-hab stopped")
			return ss, 0, errStopped
		case <-time.After(roundLockPoll):
		}
	}
}

// waitTurn waits until the turn our message started has ended, and, for a hand-off, the
// member's note came; at most limit. It reports whether the turn ended and the note came.
func (s *superviseService) waitTurn(a *supAction, pane string, rev uint64, note bool, limit time.Duration) (ended, noted bool, err error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	poll := time.NewTicker(roundLockPoll)
	defer poll.Stop()
	worked := false
	for {
		changed := s.cfg.agents.Changed(pane)
		st, ok := s.cfg.agents.Get(pane)
		if !ok || st.State == api.AgentStatusExited || st.State == api.AgentStatusNone {
			return false, false, errors.New("the member's agent left its pane")
		}
		if st.Revision > rev && (st.State == api.AgentStatusWorking || st.State == api.AgentStatusBlocked) {
			worked = true
		}
		atPrompt := st.State == api.AgentStatusIdle || st.State == api.AgentStatusDone || st.State == api.AgentStatusError
		// A turn too quick to see working moved the revision on twice.
		ended = atPrompt && st.Request == nil && (worked || st.Revision >= rev+2)
		select {
		case <-a.noted:
			noted = true
		default:
		}
		if ended && (!note || noted) {
			return ended, noted, nil
		}
		select {
		case <-s.stop:
			return ended, noted, errStopped
		case <-a.noted:
		case <-changed:
		case <-poll.C:
		case <-deadline.C:
			return ended, noted, nil
		}
	}
}

// item finds the member's item in its group's record.
func memberItem(rec *api.Group, ss api.Session) (api.GroupItem, bool) {
	if rec == nil || ss.Group == nil {
		return api.GroupItem{}, false
	}
	if i := groups.ItemIndex(*rec, ss.Group.Item); i >= 0 {
		return rec.Items[i], true
	}
	return api.GroupItem{}, false
}

// run moves one action through its steps (groups.md 9.2, 9.3).
func (s *superviseService) run(a *supAction) {
	ss, _, err := s.waitFree(a)
	if err != nil {
		return
	}
	s.step(a, actRunning, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rec, rerr := s.cfg.record(ctx, ss.Group.ID)
	cancel()
	var recp *api.Group
	if rerr == nil {
		recp = &rec
	} else {
		s.cfg.logf("supervise: %s: the group record: %v; the messages name the item only", ss.Name, rerr)
	}
	it, haveItem := memberItem(recp, ss)
	itemRef := fmt.Sprintf("item %s", ss.Group.Item)
	task := "your item's Logbook task (group_status shows it)"
	if haveItem {
		itemRef = fmt.Sprintf("item %s (%s)", it.ID, it.Title)
		if it.Task != "" {
			task = "task " + it.Task
		}
	}
	noteArgs := fmt.Sprintf("group_note (group %s, item %s)", ss.Group.ID, ss.Group.Item)
	if a.a.Kind == actCompact {
		pct := "high"
		if a.pct != nil {
			pct = fmt.Sprintf("at %d%%", *a.pct)
		} else if p := ss.ContextPct; p != nil {
			pct = fmt.Sprintf("at %d%%", *p)
		}
		text := fmt.Sprintf("Your context is %s: send your progress with %s, then end your turn.", pct, noteArgs)
		ss2, rev, err := s.sendFree(a, channels.NoteEnvelope(text))
		if err != nil {
			return
		}
		if _, _, err := s.waitTurn(a, ss2.Pane, rev, false, s.cfg.keys.HandoffNoteWait); err != nil {
			s.end(a, actFailed, err.Error())
			return
		}
		follow := fmt.Sprintf("Continue %s; your notes are on %s.", itemRef, task)
		s.mu.Lock()
		a.compacting = true
		s.mu.Unlock()
		// The compaction waits for the member's prompt itself; the guard keeps a client's use out.
		guard := func(pane string) error { return s.cfg.guard(ss2.ID, pane) }
		_, done, err := s.cfg.compacts.Start(ss2.Pane, follow, guard)
		if err != nil {
			s.mu.Lock()
			a.compacting = false
			s.mu.Unlock()
			s.end(a, actFailed, "the compaction did not start: "+err.Error())
			return
		}
		var c api.Compaction
		select {
		case c = <-done:
		case <-s.stop:
			s.end(a, actFailed, "gff-hab stopped")
			return
		}
		s.mu.Lock()
		a.compacting = false
		s.ours[ss2.ID] = time.Now()
		s.mu.Unlock()
		if c.State != compactDone {
			s.end(a, actFailed, "the compaction "+c.State+": "+c.Reason)
			return
		}
		if _, err := s.cfg.addCompact(ss2.ID); err != nil {
			s.cfg.logf("supervise: counting the compaction of %s: %v", ss2.Name, err)
		}
		s.end(a, actDone, "")
		return
	}

	// A hand-off, then a fresh session (9.3).
	text := fmt.Sprintf("Send a hand-off with %s: what is done, what is next, open questions; then end your turn. A fresh session continues %s from it.", noteArgs, itemRef)
	// Only a note written after the ask goes in counts as the hand-off (review M5). Channel
	// deliveries to the member are held from the ask on: one delivered between the turn's end
	// and the swap would start a turn in a pane the swap closes (review M2). The swap moves
	// what waits to the fresh session; a failure releases it to this one.
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	ss2, rev, err := s.sendFreeThen(a, channels.NoteEnvelope(text), func(ss api.Session) error {
		if release == nil && s.cfg.hold != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			r, err := s.cfg.hold(ctx, ss.ID, ss.Name)
			cancel()
			if err != nil {
				return err
			}
			release = r
		}
		s.mu.Lock()
		a.asked = true
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		return
	}
	ended, noted, err := s.waitTurn(a, ss2.Pane, rev, true, s.cfg.keys.HandoffNoteWait)
	if errors.Is(err, errStopped) {
		s.end(a, actFailed, "gff-hab stopped")
		return
	}
	if !ended {
		// Never close a pane mid-turn (review M1): the action fails and is tried again later.
		s.end(a, actFailed, fmt.Sprintf("the member's turn did not end within %s; no fresh session", s.cfg.keys.HandoffNoteWait))
		return
	}
	// The member may have gone meanwhile; the swap needs it still there.
	ss3, st, cerr := s.current(a)
	if cerr != nil {
		s.end(a, actFailed, cerr.Error())
		return
	}
	if st.State == api.AgentStatusWorking || st.State == api.AgentStatusBlocked {
		// Something else started a turn after the hand-off's: never close a pane mid-turn.
		s.end(a, actFailed, "the member started another turn; no fresh session")
		return
	}
	flag := ""
	if !noted {
		flag = fmt.Sprintf("no hand-off note came within %s", s.cfg.keys.HandoffNoteWait)
	}
	var seed string
	if recp != nil && haveItem {
		m := api.GroupMember{Name: ss3.Name, Role: ss3.Group.Role, Item: ss3.Group.Item}
		seed = leadSeed(*recp, m, it, "this worktree")
	} else {
		seed = fmt.Sprintf("You are the %s of item %s in agent group %s. Report with group_report; write notes with group_note.\n", ss3.Group.Role, ss3.Group.Item, ss3.Group.ID)
	}
	seed += fmt.Sprintf("\nYou are a fresh session of member %s, which ran out of context. Read the notes on %s and continue.", ss3.Name, task)
	fctx, fcancel := context.WithTimeout(context.Background(), 2*time.Minute)
	go func() {
		select {
		case <-s.stop:
			fcancel() // the swap is cut short at Stop, and undoes its rename
		case <-fctx.Done():
		}
	}()
	id, err := s.cfg.fresh(fctx, ss3, st, recp, seed)
	fcancel()
	if err != nil {
		s.end(a, actFailed, "the fresh session did not start: "+err.Error())
		return
	}
	s.mu.Lock()
	a.a.Fresh = id
	s.mu.Unlock()
	s.end(a, actDone, flag)
}

// supervisorRef hands the supervisor to the group service, which is made before it.
type supervisorRef struct {
	mu sync.Mutex
	s  *superviseService
}

func (r *supervisorRef) get() *superviseService {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

func (r *supervisorRef) set(s *superviseService) {
	r.mu.Lock()
	r.s = s
	r.mu.Unlock()
}
