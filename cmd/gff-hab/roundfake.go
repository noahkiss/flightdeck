package main

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// The group record is built by Part 15 B3 (groups.md 5.1). A build with the testseam tag can
// stand in for the catalog's groups section from a file (roundseam.go), so the rounds side of a
// group round is tested end to end. The capcom mode's far side is the Capcom service
// (capcomevents.go, capcom-service.md 9.5).

// roundWords are the capability words rounds add to this hab's catalog: round-lock, and
// capcom-service while this hab holds the Capcom mark and runs the service (9.5).
func roundWords(eng *capcomEngine) []string {
	w := []string{capRoundLock}
	if eng != nil && eng.holds() {
		w = append(w, capCapcomService)
	}
	return w
}

// roundGroups is the catalog's groups section: none until Part 15, or the test file's.
func roundGroups() []api.CatalogGroup {
	if roundGroupsSeam == "" {
		return nil
	}
	b, err := os.ReadFile(roundGroupsSeam)
	if err != nil {
		return nil
	}
	var gs []api.CatalogGroup
	if json.Unmarshal(b, &gs) != nil {
		return nil
	}
	return gs
}

// withServiceActor adds the Capcom service's actor id, its session id, to this hab's capcom
// section while it is the home host and runs the service (capcom-service.md 9.5).
func withServiceActor(c *api.CatalogCapcom, self string, eng *capcomEngine) *api.CatalogCapcom {
	if c == nil || eng == nil || c.Home != self {
		return c
	}
	id := eng.Session()
	if id == "" {
		return c
	}
	cc := *c
	cc.Service = id
	return &cc
}

// roundLock is the round lock's in-use reason for a session, "" when free (rounds.md 6.1): what
// holds a round's message to a coordinator on this hab (rounds.md 5.2).
func (s *roundsService) roundLock(ss api.Session) string {
	st, ok := s.cfg.agent(ss.Pane)
	why := agentUse(st, ok)
	if why == "" && s.cfg.sendHeld(ss.Pane) {
		why = lockSending
	}
	s.mu.Lock()
	if why == "" {
		why = s.clientUseLocked(ss.ID, ss.Pane)
	}
	if _, held := s.byPane[ss.Pane]; why == "" && held {
		why = lockRound
	}
	s.mu.Unlock()
	return strings.TrimPrefix(why, "in use: ")
}
