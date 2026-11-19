//go:build testseam

package structure

import "time"

// HoldForTest holds the store lock for d, as a store stuck in a call would. Only a build with
// the testseam tag has it: gff-hab's stall watchdog test blocks the store with it
// (docs/specs/diagnostics.md 4.7).
func (s *Store) HoldForTest(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	time.Sleep(d)
}
