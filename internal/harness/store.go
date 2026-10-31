package harness

import (
	"sort"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// EventAgentState is the event type of an agent state change.
const EventAgentState api.EventType = "agent.state"

// maxResolved bounds the remembered resolved request ids per pane.
const maxResolved = 64

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
}

type pane struct {
	st         api.AgentState
	bound      bool                      // a session lives in the pane
	turnSource api.StateSource           // the strongest source that spoke this turn
	lastT      map[api.StateSource]int64 // last accepted client clock per source
	open       string                    // the open permission request id
	resolved   []string                  // recently resolved request ids, oldest first
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
	s := &Store{panes: map[string]*pane{}, publish: cfg.Publish, now: cfg.Now, logf: cfg.Logf}
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
// whether the pane's state changed. Unknown harnesses and events are logged and dropped.
func (s *Store) Hook(paneID string, env Envelope) bool {
	u, err := Normalise(env)
	if err != nil {
		s.logf("hook %s for pane %s dropped: %v", env.Event, paneID, err)
		return false
	}
	return s.Apply(paneID, env.Harness, api.StateSourceHook, env.T, u)
}

// Apply applies one update from source src, stamped t by the client clock (ms). The rules, from
// the spec's State event model:
//
//   - Binding: while a session lives in the pane, events from another harness or session are
//     dropped (a nested agent). A session start binds a free pane; Claude Code's own session
//     start rebinds, since its entrypoint guard already keeps nested runs out. A subagent never
//     binds.
//   - Ordering: an event older than the last accepted one from the same source is late and
//     changes nothing, except a resolution of the open request, which applies regardless of t.
//     A blocked state for a request already resolved is dropped.
//   - Precedence: inside one turn a weaker source cannot overwrite a stronger one. A new turn
//     resets it.
//   - Exit keeps the last done or error in Last and drops working and blocked.
//
// A change is accepted and published only when a visible field changes.
func (s *Store) Apply(paneID, harness string, src api.StateSource, t int64, u Update) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil {
		p = &pane{lastT: map[api.StateSource]int64{}, st: api.AgentState{Pane: paneID, Harness: harness, State: api.AgentStatusNone, Source: src}}
		s.panes[paneID] = p
	}

	// Binding.
	if p.bound && (harness != p.st.Harness || (u.Session != "" && p.st.Session != "" && u.Session != p.st.Session)) {
		rebind := u.Bind && !u.Subagent && harness == p.st.Harness && harness == detect.KindClaude
		if !rebind {
			s.logf("pane %s: dropped %s event for %s session %q; bound to %s session %q", paneID, harness, harness, u.Session, p.st.Harness, p.st.Session)
			return false
		}
	}

	// Ordering.
	resolvesOpen := u.Resolves != "" && u.Resolves == p.open
	if u.State == api.AgentStatusBlocked && u.Request != nil && u.Request.ID != "" && contains(p.resolved, u.Request.ID) {
		return false // a late blocked for a request already resolved
	}
	late := t < p.lastT[src]
	if late && !resolvesOpen {
		return false
	}

	// Precedence.
	if u.NewTurn {
		p.turnSource = ""
	}
	if p.turnSource != "" && rank[src] < rank[p.turnSource] && !resolvesOpen {
		return false
	}
	if rank[src] > rank[p.turnSource] {
		p.turnSource = src
	}
	if !late {
		p.lastT[src] = t
	}

	next := p.st
	if harness != next.Harness || (u.Bind && !u.Subagent && u.Session != next.Session) {
		// A new harness or session in the pane: nothing of the old one carries over.
		next = api.AgentState{Pane: paneID, Revision: next.Revision, State: next.State}
		p.open = ""
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
	if u.Version != "" {
		next.Version = u.Version
		next.Warning = nil
		if w := detect.CheckVersion(harness, u.Version); w != nil {
			next.Warning = &api.VersionWarning{Version: w.Version, Tested: w.Tested, Reason: w.Reason}
		}
	}
	if resolvesOpen {
		p.resolve(p.open)
		if next.State == api.AgentStatusBlocked {
			next.State, next.Kind, next.Request = api.AgentStatusWorking, "", nil
		}
	}
	if u.State != "" && !(late && resolvesOpen) {
		if next.State == api.AgentStatusBlocked && u.State != api.AgentStatusBlocked && p.open != "" {
			p.resolve(p.open) // leaving blocked resolves the open request
		}
		switch u.State {
		case api.AgentStatusExited:
			next.Last = ""
			if next.State == api.AgentStatusDone || next.State == api.AgentStatusError {
				next.Last = next.State
			}
			next.Kind, next.Request = "", nil
		case api.AgentStatusBlocked:
			next.Last, next.Kind, next.Request = "", u.Kind, nil
			if u.Kind == api.BlockedKindPermission {
				next.Request = u.Request
			}
			p.open = ""
			if u.Request != nil {
				p.open = u.Request.ID
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
		return false
	}
	next.Source = src
	next.Revision = p.st.Revision + 1
	next.At = s.now().UTC().Format(time.RFC3339Nano)
	p.st = next
	s.publish(structure.Event{Type: EventAgentState, ID: paneID, Revision: next.Revision, Data: api.NonNil(next)})
	return true
}

func (p *pane) resolve(id string) {
	if id == "" {
		return
	}
	if !contains(p.resolved, id) {
		p.resolved = append(p.resolved, id)
		if len(p.resolved) > maxResolved {
			p.resolved = p.resolved[1:]
		}
	}
	if p.open == id {
		p.open = ""
	}
}

// Seen turns done into idle: a client focused the pane, or the owner sent input.
func (s *Store) Seen(paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.State != api.AgentStatusDone {
		return
	}
	p.st.State = api.AgentStatusIdle
	p.st.Revision++
	p.st.At = s.now().UTC().Format(time.RFC3339Nano)
	s.publish(structure.Event{Type: EventAgentState, ID: paneID, Revision: p.st.Revision, Data: api.NonNil(p.st)})
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
	if p := s.panes[paneID]; p != nil {
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
		out = append(out, api.NonNil(p.st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

// same compares the visible fields, ignoring source, revision and time.
func same(a, b api.AgentState) bool {
	if a.Harness != b.Harness || a.Session != b.Session || a.State != b.State || a.Last != b.Last ||
		a.Kind != b.Kind || a.Turn != b.Turn || a.Version != b.Version {
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
			string(x.Input) != string(y.Input) || len(x.Files) != len(y.Files) {
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
