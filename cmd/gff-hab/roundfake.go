package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
)

// The far sides of the capcom and group modes (rounds.md 5.2) are built by later parts: the
// Capcom service (Part 14 A2, capcom-service.md 9.5) and the group record (Part 15 B3,
// groups.md 5.1). Until they exist, this hab offers no Capcom service and publishes no groups,
// so a capcom round is refused at creation and a group round finds no group. A build with the
// testseam tag can stand in for both from files (roundseam.go), so the rounds side is tested
// end to end.

// roundWords are the capability words rounds add to this hab's catalog: round-lock, and
// capcom-service while a Capcom service runs here.
func roundWords() []string {
	w := []string{capRoundLock}
	if roundCapcomSeam != "" {
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

// fakeServiceActor is the stand-in Capcom service's actor id on the hab with device id self.
func fakeServiceActor(self string) string {
	return "capcom-service-" + strings.TrimPrefix(shortID(self), "SHA256:")
}

// withServiceActor adds the Capcom service's actor id to this hab's capcom section while it is
// the home host and runs the service (capcom-service.md 9.5).
func withServiceActor(c *api.CatalogCapcom, self string) *api.CatalogCapcom {
	if c == nil || roundCapcomSeam == "" || c.Home != self {
		return c
	}
	cc := *c
	cc.Service = fakeServiceActor(self)
	return &cc
}

// roundCapcomService is the channels Capcom hook: nil (no service) unless a test stands one in.
// The stand-in answers each round message with the first line of its file ("accepted",
// "declined off", "refused Capcom moved to x"), repeats are answered duplicate, every message
// it took is appended as a JSON line to <file>.got, and a second line "outcome <word>" sends
// that outcome a moment after an accepted message.
func roundCapcomService(chs func() *channels.Service, logf func(string, ...any)) func(api.ChannelMessage) (api.ChannelState, string) {
	if roundCapcomSeam == "" {
		return nil
	}
	var mu sync.Mutex
	taken := map[string]bool{}
	return func(m api.ChannelMessage) (api.ChannelState, string) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := os.ReadFile(roundCapcomSeam)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		word, reason, _ := strings.Cut(strings.TrimSpace(lines[0]), " ")
		if f, err := os.OpenFile(roundCapcomSeam+".got", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			j, _ := json.Marshal(m)
			f.Write(append(j, '\n'))
			f.Close()
		}
		state := api.ChannelState(word)
		if state == api.ChannelStateAccepted && taken[m.ID] {
			return api.ChannelStateDuplicate, ""
		}
		if state == api.ChannelStateAccepted {
			taken[m.ID] = true
			if len(lines) > 1 {
				if o, ok := strings.CutPrefix(strings.TrimSpace(lines[1]), "outcome "); ok {
					id := m.ID
					time.AfterFunc(300*time.Millisecond, func() {
						if s := chs(); s != nil {
							if err := s.Outcome(id, o, time.Now()); err != nil {
								logf("fake capcom: outcome %s: %v", id, err)
							}
						}
					})
				}
			}
		}
		return state, reason
	}
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
