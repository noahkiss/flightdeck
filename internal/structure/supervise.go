package structure

import (
	"fmt"

	"github.com/noahkiss/flightdeck/api"
)

// Context supervision of group members (groups.md 9, stage B6): the session record keeps the
// member's context use, the compactions counted toward group.fresh_after_compacts, and the
// session a fresh session replaced. Each setter commits only a change.

func copyPct(p *int) *int {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// SetContextPct sets a session's context use in whole percent; nil when unknown. It reports
// whether the record changed.
func (s *Store) SetContextPct(id string, pct *int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return false
	}
	if (pct == nil && ss.ContextPct == nil) || (pct != nil && ss.ContextPct != nil && *pct == *ss.ContextPct) {
		return false
	}
	ss.ContextPct = copyPct(pct)
	ss.Revision++
	s.commit([]Event{s.sessionEvent(ss)})
	return true
}

// AddCompact counts one compaction of a member and returns the count.
func (s *Store) AddCompact(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return 0, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	ss.Compacts++
	ss.Revision++
	s.commit([]Event{s.sessionEvent(ss)})
	return ss.Compacts, nil
}

// SetReplaced records on a fresh session the session it replaced (groups.md 9.3).
func (s *Store) SetReplaced(id, old string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if ss.Replaced == old {
		return nil
	}
	ss.Replaced = old
	ss.Revision++
	s.commit([]Event{s.sessionEvent(ss)})
	return nil
}

// SetGroup sets a session's group and envelope. Nil for both takes a replaced member out of
// its group (groups.md 9.3): its record keeps its conversation, so it stays resumable, but it
// is no member any more; a failed swap puts them back.
func (s *Store) SetGroup(id string, g *api.SessionGroup, env *api.GroupEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	ss.Group, ss.Envelope = copyGroup(g), copyEnvelope(env)
	if g == nil {
		ss.ContextPct = nil
	}
	ss.Revision++
	s.commit([]Event{s.sessionEvent(ss)})
	return nil
}

// Members lists the running sessions with a pane that are group members.
func (s *Store) Members() []api.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.Session
	for _, ss := range s.sessions {
		if ss.Group != nil && ss.State == stateRunning && ss.Pane != "" {
			out = append(out, sessionAPI(ss))
		}
	}
	return out
}

// SetSwap marks (m) or clears (nil) a fresh-session swap on its old session (groups.md 9.5).
func (s *Store) SetSwap(id string, m *SwapMark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if m == nil && ss.Swap == nil {
		return nil
	}
	if m != nil {
		c := *m
		c.Envelope = copyEnvelope(m.Envelope)
		m = &c
	}
	ss.Swap = m
	s.commit(nil)
	return nil
}

// Swaps lists the swap marks, by old session id.
func (s *Store) Swaps() map[string]SwapMark {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]SwapMark{}
	for _, ss := range s.sessions {
		if ss.Swap != nil {
			m := *ss.Swap
			m.Envelope = copyEnvelope(ss.Swap.Envelope)
			out[ss.ID] = m
		}
	}
	return out
}
