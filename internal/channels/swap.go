package channels

import (
	"context"
	"time"
)

// A group member's fresh-session swap (groups.md 9.3, 12.8; mesh.md 6.5): the hab holds new
// deliveries to the old session while it renames and replaces it, then moves the messages that
// wait for it to its named successor, in their order. The sender sees them delivered to the
// same address.

// Hold stops deliveries to the session with id sessionID until the returned release runs. It
// waits, until ctx ends, for a delivery attempt in flight to that session to finish, so no
// write into the old pane races the swap. Holds nest by count.
func (s *Service) Hold(ctx context.Context, sessionID string) (release func(), err error) {
	s.mu.Lock()
	if s.held == nil {
		s.held = map[string]int{}
	}
	s.held[sessionID]++
	s.mu.Unlock()
	release = func() {
		s.mu.Lock()
		if s.held[sessionID]--; s.held[sessionID] <= 0 {
			delete(s.held, sessionID)
		}
		s.mu.Unlock()
		s.Poke()
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		s.mu.Lock()
		busy := false
		for _, it := range s.st.Inbox {
			if it.SessionID == sessionID && it.busy {
				busy = true
			}
		}
		s.mu.Unlock()
		if !busy {
			return release, nil
		}
		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// Move gives every message that waits for the session with id from to the session with id to,
// which now holds the name the messages were sent to; their order stays, after what waits for
// to already (its seed). It returns how many moved. The caller holds from (Hold), so none is in
// a delivery attempt.
func (s *Service) Move(from, to string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept, moved []*inItem
	for _, it := range s.st.Inbox {
		if it.SessionID == from && !it.busy {
			it.SessionID = to
			moved = append(moved, it)
		} else {
			kept = append(kept, it)
		}
	}
	n := len(moved)
	if n == 0 {
		return 0, nil
	}
	s.st.Inbox = append(kept, moved...)
	if err := s.commitLocked(); err != nil {
		return 0, err
	}
	s.Poke()
	return n, nil
}

// heldLocked reports whether deliveries to the session with id sessionID are held. The caller
// holds s.mu.
func (s *Service) heldLocked(sessionID string) bool { return s.held[sessionID] > 0 }
