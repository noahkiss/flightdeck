package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// EventAgentState is the event type of an agent state change.
const EventAgentState api.EventType = "agent.state"

// clockStep is how far behind the last accepted clock an event may be and still count as late.
// Further back means the wall clock was stepped, and the event is taken as current.
const clockStep = 5 * time.Second

// maxResolved bounds the remembered resolved request ids per pane.
const maxResolved = 64

// interruptGrace is how long after the last report of the turn's stronger source an OSC idle
// may still end the turn. Claude Code fires no hook when the owner interrupts a turn; its OSC
// 7501 idle is the only signal. Inside the grace an OSC idle is taken as a stale report from
// before the turn began.
const interruptGrace = time.Second

// rank orders the state sources: a stronger source wins inside one turn.
var rank = map[api.StateSource]int{
	api.StateSourceScreen: 1,
	api.StateSourceOsc:    2,
	api.StateSourceHook:   3,
	api.StateSourceServer: 4,
	api.StateSourceSpawn:  4,
}

// Store keeps the agent state of every pane. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	panes   map[string]*pane
	publish func(structure.Event)
	now     func() time.Time
	logf    func(string, ...any)
	// reqPrefix and reqN name a permission request whose payload has no id (Claude's
	// PermissionRequest has no tool_use_id). The prefix is random per store, so an id from an
	// earlier gff-hab epoch never names a new request.
	reqPrefix string
	reqN      uint64
}

type pane struct {
	st         api.AgentState
	bound      bool                      // a session lives in the pane
	turnSource api.StateSource           // the strongest source that spoke this turn
	lastT      map[api.StateSource]int64 // last accepted client clock per source
	// open is the open permission request id. Only a hook or server opens or closes it, or the
	// pane's harness going away (exit, a new session); PTY bytes never do (see gateSource),
	// except once Flightdeck answered it (answered): then an OSC idle or a shell prompt may.
	open     string
	resolved []resolution  // recently resolved requests, oldest first
	changed  chan struct{} // closed and replaced on every accepted change and OSC report
	// oscN counts the live OSC reports of the pane's harness; oscLast is the newest one's state.
	// The permission read-back uses them to see the turn go on.
	oscN    uint64
	oscLast api.AgentStatus

	answering string // the request an approval.answer is answering now
	answered  string // the request an answer was sent to: no second answer
	// took is the request whose answer reached the harness (Store.Sent) and was not withdrawn
	// (Store.Unsent): only then may an OSC idle or a shell prompt close it.
	took string
	// ownerStop is set by the owner's interrupt key (Store.Interrupt): the pane shows idle,
	// but sends stay refused until the harness itself reports, which shows it still runs and is
	// at its prompt. Any accepted report or hook clears it.
	ownerStop bool

	// hooked is the harness whose hook last reached gff-hab for this pane, in this gff-hab run.
	// A turn its OSC 7501 reports show finishing with no such hook marks the hooks missing.
	hooked string
}

// gateSource reports whether src may open or close a permission request: a hook or a server,
// never PTY output (osc, screen).
func gateSource(src api.StateSource) bool {
	return src == api.StateSourceHook || src == api.StateSourceServer
}

// resolution records how a permission request closed: by which source, into which state.
type resolution struct {
	id    string
	src   api.StateSource
	state api.AgentStatus
}

// Config is what NewStore needs. Publish receives each accepted change, under the store lock,
// after the change is stored; it must not block (control.Server.Publish does not).
type Config struct {
	Publish func(structure.Event)
	Now     func() time.Time
	Logf    func(string, ...any)
}

// NewStore returns an empty store.
func NewStore(cfg Config) *Store {
	var b [4]byte
	rand.Read(b[:])
	s := &Store{panes: map[string]*pane{}, publish: cfg.Publish, now: cfg.Now, logf: cfg.Logf, reqPrefix: "gff-" + hex.EncodeToString(b[:])}
	if s.publish == nil {
		s.publish = func(structure.Event) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s
}

// Hook normalises one hook envelope for pane and applies it with source hook. It reports
// whether the pane's state changed, and the id of the permission request this envelope opened,
// if it opened one; a dropped envelope (a nested session, a late one) opened none. Unknown
// harnesses and events are logged and dropped.
func (s *Store) Hook(paneID string, env Envelope) (changed bool, opened string) {
	u, err := Normalise(env)
	if err != nil {
		s.logf("hook %s for pane %s dropped: %v", env.Event, paneID, err)
		return false, ""
	}
	return s.apply(paneID, env.Harness, api.StateSourceHook, env.T, u)
}

// Apply applies one update from source src, stamped t by the client clock (ms). The rules, from
// the spec's State event model:
//
//   - Binding: while a session lives in the pane, events from another harness or session are
//     dropped (a nested agent). A session start of the same harness rebinds the pane, so a
//     session that ended with no end hook never holds it, except while the bound session has
//     a permission request open: then it is a nested agent and dropped too. A subagent never
//     binds.
//   - Ordering: an event older than the last accepted one from the same source is late and
//     changes nothing, except a resolution of the open request, which applies regardless of t.
//     A blocked state for a request already resolved is dropped. A session start or a new turn
//     is never late, and a clock more than clockStep behind is taken as a clock reset.
//   - Precedence: inside one turn a weaker source cannot overwrite a stronger one. A new turn
//     (a prompt submit, or a new turn id) resets it.
//   - Blocked: a second blocked report of the same kind without a request keeps the request.
//     A subagent's progress never leaves blocked; only a resolution or the main agent does.
//   - Exit keeps the last done or error in Last and drops working and blocked.
//
// A change is accepted and published only when a visible field changes.
func (s *Store) Apply(paneID, harness string, src api.StateSource, t int64, u Update) bool {
	changed, _ := s.apply(paneID, harness, src, t, u)
	return changed
}

// apply is Apply; it also returns the id of the permission request the update opened, if any.
func (s *Store) apply(paneID, harness string, src api.StateSource, t int64, u Update) (changed bool, opened string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.paneLocked(paneID, harness, src)

	// Binding.
	if p.bound && (harness != p.st.Harness || (u.Session != "" && p.st.Session != "" && u.Session != p.st.Session)) {
		rebind := u.Bind && !u.Subagent && harness == p.st.Harness
		if !rebind {
			s.logf("pane %s: dropped %s event for %s session %q; bound to %s session %q", paneID, harness, harness, u.Session, p.st.Harness, p.st.Session)
			return false, ""
		}
		if p.open != "" {
			// A session start from another session while the bound one waits on a permission
			// request is a nested agent (a job the turn started): the request stays open.
			s.logf("pane %s: dropped %s session start for session %q; bound session %q has request %s open", paneID, harness, u.Session, p.st.Session, p.open)
			return false, ""
		}
	}
	prevOSC := p.oscLast
	if src == api.StateSourceHook {
		p.hooked = harness
	}
	if src == api.StateSourceOsc && harness == p.st.Harness {
		p.oscN, p.oscLast = p.oscN+1, u.State
		s.wakeLocked(p) // the permission read-back watches for the turn to go on
	}

	// Ordering.
	resolvesOpen := u.Resolves != "" && u.Resolves == p.open
	// An OSC idle ends a request Flightdeck already answered: Claude fires no hook after a deny
	// by keys, and goes back to its prompt. A request nobody answered stays open (gateSource).
	endsAnswered := src == api.StateSourceOsc && u.State == api.AgentStatusIdle && harness == p.st.Harness &&
		p.open != "" && p.took == p.open
	resolvesOpen = resolvesOpen || endsAnswered
	if u.State == api.AgentStatusBlocked && u.Request != nil && u.Request.ID != "" && p.isResolved(u.Request.ID) {
		return false, "" // a late blocked for a request already resolved
	}
	newTurn := u.NewTurn || (u.Turn != "" && u.Turn != p.st.Turn) || (u.Bind && !u.Subagent)
	last, seen := p.lastT[src]
	late := seen && t < last && !newTurn && last-t <= clockStep.Milliseconds()
	if late && !resolvesOpen {
		return false, ""
	}

	// Precedence.
	if newTurn {
		p.turnSource = ""
	}
	if p.turnSource != "" && rank[src] < rank[p.turnSource] && !resolvesOpen && !p.interrupted(src, t, u) {
		if src == api.StateSourceOsc && u.State == api.AgentStatusIdle && p.open != "" {
			s.logf("pane %s: OSC idle ignored: permission request %s is open from a %s", paneID, p.open, p.turnSource)
		}
		return false, ""
	}
	p.ownerStop = false // the harness spoke after the owner's interrupt
	if rank[src] > rank[p.turnSource] {
		p.turnSource = src
	}
	if !late {
		p.lastT[src] = t // may move back: a new turn or a clock reset
	}

	next := p.st
	if harness != next.Harness || (u.Bind && !u.Subagent && u.Session != next.Session) {
		// A new harness or session in the pane: nothing of the old one carries over.
		next = api.AgentState{Pane: paneID, Revision: next.Revision, State: next.State}
		p.resolve(p.open, src, "")
		p.oscLast, p.answering, p.answered, p.took, p.ownerStop = "", "", "", "", false
	}
	next.Harness = harness
	if u.Session != "" && !u.Subagent && (u.Bind || next.Session == "") {
		next.Session = u.Session
	}
	if u.Bind && !u.Subagent {
		p.bound = true
	} else if next.Session != "" && !u.Unbind && !u.Subagent {
		p.bound = true
	}
	if u.Turn != "" {
		next.Turn = u.Turn
	}
	// Hook health (harness-adapters.md, Edge cases: untrusted Codex hooks). Codex skips a hook
	// it does not trust with no message, so config presence proves nothing: only delivery does.
	switch {
	case src == api.StateSourceHook:
		next.Hooks = ""
	case src == api.StateSourceOsc && p.hooked != harness && prevOSC == api.AgentStatusWorking && turnEnd(u.State) && next.Hooks == "":
		next.Hooks = api.HookHealthMissing
		hint := "install its hook config (gff-hab hook -print " + harness + ")"
		if harness == detect.KindCodex {
			hint += ", then trust it in Codex with /hooks"
		}
		s.logf("pane %s: error: %s finished a turn and none of its hooks reached gff-hab; %s", paneID, harness, hint)
	}
	if u.Version != "" {
		next.Version = u.Version
		next.Warning = nil
		if w := detect.CheckVersion(harness, u.Version); w != nil {
			next.Warning = &api.VersionWarning{Version: w.Version, Tested: w.Tested, Reason: w.Reason}
		}
	}
	if resolvesOpen {
		p.resolve(p.open, src, u.State)
		if next.State == api.AgentStatusBlocked {
			next.State, next.Kind, next.Request = api.AgentStatusWorking, "", nil
		}
	}
	keepBlocked := u.Subagent && next.State == api.AgentStatusBlocked && u.State != api.AgentStatusBlocked
	if u.State != "" && !(late && resolvesOpen) && !keepBlocked {
		if p.open != "" && u.State != api.AgentStatusBlocked && gateSource(src) && !u.Subagent {
			p.resolve(p.open, src, u.State) // a hook past the request resolves it
		}
		switch u.State {
		case api.AgentStatusExited:
			next.Last = ""
			if next.State == api.AgentStatusDone || next.State == api.AgentStatusError {
				next.Last = next.State
			}
			next.Kind, next.Request = "", nil
		case api.AgentStatusBlocked:
			if next.State == api.AgentStatusBlocked && next.Kind == u.Kind && u.Request == nil {
				break // the same prompt reported again without its request (Claude's Notification)
			}
			next.Last, next.Kind, next.Request = "", u.Kind, nil
			if u.Kind == api.BlockedKindPermission {
				next.Request = u.Request
			}
			if gateSource(src) {
				p.resolve(p.open, src, u.State) // a new prompt replaces the open one
			}
			if next.Request != nil {
				if next.Request.ID == "" {
					r := *next.Request
					s.reqN++
					r.ID = fmt.Sprintf("%s-%d", s.reqPrefix, s.reqN)
					next.Request = &r
				}
				p.open = next.Request.ID
				opened = p.open
			}
		default:
			next.Last, next.Kind, next.Request = "", "", nil
		}
		next.State = u.State
	}
	if u.Unbind {
		p.bound = false
	}

	if same(p.st, next) {
		return false, opened
	}
	next.Source = src
	s.commitLocked(p, next)
	return true, opened
}

// turnEnd reports whether a state ends a turn: done or error. Not idle: Codex 0.166 reports
// working, then idle, while it starts, before its first hook (SessionStart fires with the first
// prompt), so idle would mark a pane whose hooks work.
func turnEnd(s api.AgentStatus) bool {
	return s == api.AgentStatusDone || s == api.AgentStatusError
}

// paneLocked returns the pane's record, made on first use. The caller holds s.mu.
func (s *Store) paneLocked(paneID, harness string, src api.StateSource) *pane {
	p := s.panes[paneID]
	if p == nil {
		p = &pane{lastT: map[api.StateSource]int64{}, st: api.AgentState{Pane: paneID, Harness: harness, State: api.AgentStatusNone, Source: src}, changed: make(chan struct{})}
		s.panes[paneID] = p
	}
	return p
}

// commitLocked stores next as the pane's state under a new revision, publishes it and wakes
// the waiters on Changed. The caller holds s.mu.
func (s *Store) commitLocked(p *pane, next api.AgentState) {
	next.Revision = p.st.Revision + 1
	next.At = s.now().UTC().Format(time.RFC3339Nano)
	p.st = next
	s.wakeLocked(p)
	s.publish(structure.Event{Type: EventAgentState, ID: next.Pane, Revision: next.Revision, Data: api.NonNil(next)})
}

// wakeLocked wakes the waiters on Changed. The caller holds s.mu.
func (s *Store) wakeLocked(p *pane) {
	close(p.changed)
	p.changed = make(chan struct{})
}

// interrupted reports whether an OSC idle ends a turn a stronger source holds as working or
// blocked: the owner interrupted it, which fires no hook. It applies only once interruptGrace
// has passed since the stronger source last spoke, and never to a permission request a hook or
// server opened: only a hook, a server, a new session or the exit closes that (any program in
// the pane can write OSC 7501, and a send's Enter on a live dialog would approve it).
func (p *pane) interrupted(src api.StateSource, t int64, u Update) bool {
	if src != api.StateSourceOsc || u.State != api.AgentStatusIdle {
		return false
	}
	if p.st.State != api.AgentStatusWorking && p.st.State != api.AgentStatusBlocked {
		return false
	}
	if p.open != "" {
		return false
	}
	return t-p.lastT[p.turnSource] >= interruptGrace.Milliseconds()
}

// resolve closes request id, closed by src into state ("" when the session that asked is gone).
func (p *pane) resolve(id string, src api.StateSource, state api.AgentStatus) {
	if id == "" {
		return
	}
	if !p.isResolved(id) {
		p.resolved = append(p.resolved, resolution{id, src, state})
		if len(p.resolved) > maxResolved {
			p.resolved = p.resolved[1:]
		}
	}
	if p.open == id {
		p.open = ""
	}
}

func (p *pane) isResolved(id string) bool {
	_, ok := p.resolution(id)
	return ok
}

func (p *pane) resolution(id string) (resolution, bool) {
	for _, r := range p.resolved {
		if r.id == id {
			return r, true
		}
	}
	return resolution{}, false
}

// Seen turns done into idle: a client focused the pane, or the owner sent input.
func (s *Store) Seen(paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.State != api.AgentStatusDone {
		return
	}
	next := p.st
	next.State = api.AgentStatusIdle
	s.commitLocked(p, next)
}

// Interrupt records that the owner typed data into the pane (pane.input). An interrupt key
// (InterruptKey: Esc or Ctrl-C for Claude Code) typed while the agent works ends the turn on
// the display: Claude fires no hook when the owner interrupts it. It never touches an open
// permission request, and the next report from any source wins, so a key that interrupted
// nothing is corrected by the harness's next report. Display only: CheckSend refuses until the
// harness reports again, since the key may have gone to a shell whose harness was killed. It
// reports whether the state changed.
func (s *Store) Interrupt(paneID string, data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.Revision == 0 || p.st.State != api.AgentStatusWorking || p.open != "" || !InterruptKey(p.st.Harness, data) {
		return false
	}
	next := p.st
	next.State, next.Last, next.Kind, next.Request = api.AgentStatusIdle, "", "", nil
	next.Source = api.StateSourceServer
	p.turnSource = ""
	p.ownerStop = true
	s.commitLocked(p, next)
	return true
}

// Prompt records that a shell prompt (an OSC 133 A mark) came back in the pane's live output
// after the last status report: the harness no longer runs in the foreground. A working or
// blocked state that terminal output set (osc or screen) shows exited, as an OSC clear does, so
// session.send never types into the shell. A permission request Flightdeck answered closes too.
// A request nobody answered stays open, with its state: terminal output never ends it. The
// binding stays, so the session's next hook takes the pane back. It reports whether the state
// changed.
func (s *Store) Prompt(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.Revision == 0 {
		return false
	}
	answered := p.open != "" && p.took == p.open
	if answered {
		p.resolve(p.open, api.StateSourceOsc, api.AgentStatusExited)
	}
	if p.open != "" {
		return false
	}
	byOutput := p.st.Source == api.StateSourceOsc || p.st.Source == api.StateSourceScreen
	if p.st.State != api.AgentStatusWorking && p.st.State != api.AgentStatusBlocked || !byOutput && !answered {
		return false
	}
	next := p.st
	next.State, next.Last, next.Kind, next.Request = api.AgentStatusExited, "", "", nil
	next.Source = api.StateSourceOsc
	p.turnSource = ""
	s.commitLocked(p, next)
	return true
}

// Exited records that the pane's program ended: a harness in the pane is gone with it. The last
// done or error stays in Last; working and blocked are dropped, and the session no longer holds
// the pane. A pane with no harness state is left alone.
func (s *Store) Exited(paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.panes[paneID]; p != nil {
		s.exitLocked(p)
	}
}

// exitLocked marks the pane's harness gone. The caller holds s.mu.
func (s *Store) exitLocked(p *pane) {
	p.resolve(p.open, "", api.AgentStatusExited)
	p.bound = false
	p.oscLast, p.answering, p.answered, p.took, p.ownerStop = "", "", "", "", false
	if p.st.State == api.AgentStatusNone || p.st.State == api.AgentStatusExited {
		return // an OSC clear may have shown the exit already
	}
	next := p.st
	next.Last = ""
	if next.State == api.AgentStatusDone || next.State == api.AgentStatusError {
		next.Last = next.State
	}
	next.State, next.Kind, next.Request = api.AgentStatusExited, "", nil
	s.commitLocked(p, next)
}

// Changed returns a channel that is closed at the pane's next accepted change. A pane with no
// state yet gets a record, so the channel still fires when its first state arrives.
func (s *Store) Changed(paneID string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paneLocked(paneID, "", "").changed
}

// Forget drops a pane that is gone. It emits nothing: the pane's own deletion tells clients.
func (s *Store) Forget(paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.panes, paneID)
}

// Get returns one pane's state.
func (s *Store) Get(paneID string) (api.AgentState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.panes[paneID]; p != nil && p.st.Revision > 0 {
		return p.st, true
	}
	return api.AgentState{}, false
}

// Snapshot returns the state of every pane, sorted by pane id.
func (s *Store) Snapshot() []api.AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.AgentState, 0, len(s.panes))
	for _, p := range s.panes {
		if p.st.Revision > 0 { // a record Changed made, with no state yet
			out = append(out, api.NonNil(p.st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

// same compares the visible fields, ignoring source, revision and time.
func same(a, b api.AgentState) bool {
	if a.Harness != b.Harness || a.Session != b.Session || a.State != b.State || a.Last != b.Last ||
		a.Kind != b.Kind || a.Turn != b.Turn || a.Version != b.Version || a.Hooks != b.Hooks {
		return false
	}
	if (a.Warning == nil) != (b.Warning == nil) || (a.Warning != nil && *a.Warning != *b.Warning) {
		return false
	}
	if (a.Request == nil) != (b.Request == nil) {
		return false
	}
	if a.Request != nil {
		x, y := a.Request, b.Request
		if x.ID != y.ID || x.Tool != y.Tool || x.Command != y.Command || x.Cwd != y.Cwd ||
			string(x.Input) != string(y.Input) || x.InputTruncated != y.InputTruncated || len(x.Files) != len(y.Files) {
			return false
		}
		for i := range x.Files {
			if x.Files[i] != y.Files[i] {
				return false
			}
		}
	}
	return true
}
