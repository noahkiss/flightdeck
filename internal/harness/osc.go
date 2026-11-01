package harness

import (
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/progstatus"
)

var manifests = sync.OnceValue(detect.Vendored)

// Report applies one program status report (OSC 7501) from the pane's output with source osc,
// stamped t by gff-hab's clock (ms). It reports whether the pane's state changed.
//
//   - Only the root record counts: a report with an id is a record below the program (a
//     subagent, a job) and never moves the pane.
//   - app names the harness. It must resolve to a detection manifest, so a program such as a
//     package manager that also reports status does not show as an agent. A report with no
//     app counts for the harness the pane already has.
//   - clear from the pane's own harness means it exited: the pane becomes exited and the
//     session no longer holds it, as when the process is reaped. A lost SessionEnd then never
//     leaves a shell that looks like an idle agent.
//   - The report goes through Apply, so hooks outrank it inside a turn, except for an OSC idle
//     that ends an interrupted turn (see interruptGrace).
//   - A replayed report (live false: bytes the program sent before this gff-hab followed the
//     pane) applies with source screen, the weakest, so the next hook or live report wins. The
//     link hands on only the last report of a replay.
func (s *Store) Report(paneID string, r progstatus.Report, t int64, live bool) bool {
	if r.ID != "" {
		return false
	}
	harness := ""
	if r.App != "" {
		harness = manifests().Kind(strings.ToLower(r.App))
		if harness == "" {
			return false
		}
	} else {
		st, ok := s.Get(paneID)
		if !ok || st.Harness == "" {
			return false
		}
		harness = st.Harness
	}
	if r.State == "clear" {
		return s.cleared(paneID, harness)
	}
	src := api.StateSourceOsc
	if !live {
		src = api.StateSourceScreen
	}
	u := Update{State: api.AgentStatus(r.State)}
	if u.State == api.AgentStatusBlocked {
		u.Kind = api.BlockedKind(r.Kind)
	}
	return s.Apply(paneID, harness, src, t, u)
}

// cleared marks the pane exited when harness is the one it holds.
func (s *Store) cleared(paneID, harness string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.Revision == 0 || p.st.Harness != harness {
		return false
	}
	rev := p.st.Revision
	s.exitLocked(p)
	return p.st.Revision != rev
}
