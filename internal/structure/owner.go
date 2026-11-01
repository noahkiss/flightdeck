package structure

import (
	"github.com/noahkiss/flightdeck/api"
)

// SetOwner records a change of pane id's size owner (hab-logic-api.md 6.5). It raises the pane's
// revision and emits one pane.owner event with o. A pane the store does not know is ignored and
// reported false. The owner is not saved: a new gff-hab starts a new epoch with no owners. So
// SetOwner never writes the state file, and a caller that holds a lock (panesize does) never
// waits on the disk. The raised revision reaches the file with the next saved change; revisions
// are only compared within an epoch (spec 3.5), so a crash before then loses nothing.
func (s *Store) SetOwner(id string, o api.PaneOwner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.panes[id]
	if ps == nil || s.closed {
		return false
	}
	ps.owner = o.Owner
	ps.rec.Revision++
	s.emit([]Event{{PaneOwner, id, ps.rec.Revision, api.NonNil(o)}})
	return true
}
