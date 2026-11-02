package board

// The status board's Rounds list (rounds.md 10.3, gff-client.md 7.5): every round on every
// host in one list, in next-run order.

import (
	"cmp"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/merge"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// RoundKey names a round across hosts.
type RoundKey struct {
	Host  string // hosts.Spec.Label
	Round string // the round id on that host
}

// RoundRow is one round on one host.
type RoundRow struct {
	RoundKey
	Name    string
	Mode    api.RoundMode
	Trigger string // in the catalog's words: "every 1h", "cron 0 9 * * 1-5 in Europe/Berlin", "by hand"
	Zone    string // the round's own zone, when known; empty from a catalog copy
	Project string // the project's name, when known
	// Next is the next slot; zero when there is none.
	Next      time.Time
	Paused    bool
	Completed bool
	// Reason is why the round paused or completed itself, when the hab says.
	Reason string
	// LastRun is the newest run's state; empty before the first run.
	LastRun api.RoundRunState
	// LastSession is the newest run's session id, when known.
	LastSession string
	// Stale: the host is not online, so the row is the last copy: its own last snapshot, or
	// another hab's catalog copy. No action reaches it.
	Stale bool
	// Since is when the copy was last confirmed: the host's drop, or the catalog's seen time.
	// Zero while the host is online.
	Since      time.Time
	HostStatus hosts.Status
}

// Mark is the row's mark in the spec's words: Paused, Completed, else the last run's state.
func (r RoundRow) Mark() string {
	switch {
	case r.Completed:
		return "completed"
	case r.Paused:
		return "paused"
	case r.LastRun != "":
		return string(r.LastRun)
	}
	return "no runs yet"
}

// CatalogSkew is the margin a catalog copy's time must clear past the drop to win over the
// snapshot gff held: the copy's time is by another hab's clock.
const CatalogSkew = 10 * time.Second

// CatalogCopy is one host's rounds as another hab's catalog holds them (rounds.md 9).
type CatalogCopy struct {
	Rounds []api.CatalogRound
	// SeenAt is when the hab that holds the copy last confirmed it with the host.
	SeenAt time.Time
}

// Catalogs finds a host's catalog copy by device id; nil means there are none.
type Catalogs func(deviceID string) (CatalogCopy, bool)

// RoundRows builds the round rows of every host. An online host gives its own rounds, live.
// A host that is not online gives its last copy, stale: the catalog copy another hab holds
// when that copy was confirmed after this client lost the host (or the client never reached
// it), else the rounds of its last snapshot. A host that failed for good gives none, as with
// sessions (gff-client.md 4.2). Rows come unordered; SortRounds sorts them.
func RoundRows(hs []hosts.Host, cats Catalogs) []RoundRow {
	var out []RoundRow
	shown := merge.Shown(hs)
	// One hab reached by two labels shows once. merge.Shown knows only welcomes; a label never
	// reached shows the hab it pins only when no shown label reached it, and only once.
	reached := map[string]bool{}
	for i, h := range hs {
		if shown[i] && h.DeviceID() != "" {
			reached[h.DeviceID()] = true
		}
	}
	for i, h := range hs {
		if !shown[i] || h.Status == hosts.Failed {
			continue
		}
		if h.DeviceID() == "" && h.Pinned != "" {
			if reached[h.Pinned] {
				continue
			}
			reached[h.Pinned] = true
		}
		if h.Online() {
			out = append(out, liveRounds(h, false, time.Time{})...)
			continue
		}
		var cp CatalogCopy
		ok := false
		if id := h.Identity(); id != "" && cats != nil {
			cp, ok = cats(id)
		}
		held := h.Welcome.DeviceID != ""
		if ok && (!held || cp.SeenAt.After(h.Since.Add(CatalogSkew))) {
			for _, c := range cp.Rounds {
				r := RoundRow{RoundKey: RoundKey{Host: h.Label, Round: c.ID}, Name: c.Name, Mode: c.Mode, Trigger: c.Trigger,
					Project: c.Project, Paused: c.Paused, Completed: c.Completed, LastRun: c.LastRun,
					Stale: true, Since: cp.SeenAt, HostStatus: h.Status}
				r.Next = parseTime(c.NextSlot)
				out = append(out, r)
			}
			continue
		}
		out = append(out, liveRounds(h, true, h.Since)...)
	}
	return out
}

func liveRounds(h hosts.Host, stale bool, since time.Time) []RoundRow {
	projects := map[string]string{}
	for _, p := range h.Structure.Projects {
		projects[p.ID] = p.Name
	}
	var out []RoundRow
	for _, x := range h.Structure.Rounds {
		r := RoundRow{RoundKey: RoundKey{Host: h.Label, Round: x.ID}, Name: x.Name, Mode: x.Mode,
			Trigger: rounds.TriggerWords(string(x.Trigger.Kind), x.Trigger.Cron, x.Trigger.Zone, x.Trigger.At),
			Zone:    x.Trigger.Zone, Project: projects[x.Project], Next: parseTime(x.NextSlot),
			Paused: x.Paused, Completed: x.Completed, Stale: stale, Since: since, HostStatus: h.Status}
		switch {
		case x.Completed:
			r.Reason = x.CompletedReason
		case x.Paused:
			r.Reason = x.PausedReason
		case x.TriggerError != "":
			r.Reason = x.TriggerError
		}
		if x.LastRun != nil {
			r.LastRun, r.LastSession = x.LastRun.State, x.LastRun.Session
		}
		out = append(out, r)
	}
	return out
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// roundGroup is a round's place in the order: what will run first, then what runs only by
// hand, then paused, then completed (app.md 21.2).
func roundGroup(r RoundRow) int {
	switch {
	case r.Completed:
		return 3
	case r.Paused:
		return 2
	case r.Next.IsZero():
		return 1
	}
	return 0
}

// SortRounds orders rows: the soonest next slot first across every host, then rounds with no
// next slot, then paused, then completed; the name, then the host, break ties.
func SortRounds(rows []RoundRow) {
	slices.SortStableFunc(rows, func(a, b RoundRow) int {
		if c := cmp.Compare(roundGroup(a), roundGroup(b)); c != 0 {
			return c
		}
		if c := a.Next.Compare(b.Next); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return cmp.Compare(a.Round, b.Round)
	})
}

// MatchRound reports whether a round row matches the sidebar filter: round, project or host
// name, case folded.
func MatchRound(r RoundRow, filter string) bool {
	return Match(Row{Name: r.Name, Project: api.Project{Name: r.Project}, Key: Key{Host: r.Host}}, filter)
}
