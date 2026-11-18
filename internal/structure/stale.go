package structure

import "slices"

// SetStale sets what a session's running agent loaded that a newer render replaced
// (instructions-mcp-flight-rules.md 13.1); nil or empty when nothing is. It reports whether the
// record changed. The flag is not saved: the hab works it out again after a start.
func (s *Store) SetStale(id string, stale []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil || slices.Equal(ss.Stale, stale) {
		return false
	}
	if len(stale) == 0 {
		stale = nil
	}
	ss.Stale = slices.Clone(stale)
	ss.Revision++
	s.commit([]Event{s.sessionEvent(ss)})
	return true
}
