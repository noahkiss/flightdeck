package board

// The status board's Groups section (groups.md 5.4, gff-client.md 7.7): one card per agent
// group, from its home host's snapshot while that host is online, else from another hab's
// catalog copy.

import (
	"cmp"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/merge"
)

// GroupOverShown is how long an ended or dropped group stays on the board after its end.
const GroupOverShown = 24 * time.Hour

// GroupRow is one group on its home host.
type GroupRow struct {
	Host  string // the home host's label
	ID    string
	Title string
	State string
	// Items and Landed count the plan's items that are not dropped, and those landed.
	Items, Landed, Members int
	Coordinator            string // the coordinator's name, when known
	// PausedBy names the pause reasons: "you", "usage", or both.
	PausedBy []string
	Waits    []string // what holds the group now, in the record's words; empty from a catalog copy
	Created  time.Time
	Ended    time.Time // zero while the group is live, and in a catalog copy
	// ItemRows are the plan's items with their states; empty from a catalog copy.
	ItemRows []GroupItemRow
	// Stale: the home host is not online, so the row is another hab's catalog copy or the
	// host's last snapshot. Since is when that copy was last confirmed.
	Stale bool
	Since time.Time
}

// GroupItemRow is one item of a group's card.
type GroupItemRow struct {
	Title, State, Lead, Reason string
}

// Over reports whether the group ended or was dropped.
func (g GroupRow) Over() bool { return g.State == "ended" || g.State == "dropped" }

// GroupCopy is one host's groups as another hab's catalog holds them (groups.md 5.1).
type GroupCopy struct {
	Groups []api.CatalogGroup
	SeenAt time.Time
}

// GroupCatalogs finds a host's catalog copy of its groups by device id.
type GroupCatalogs func(deviceID string) (GroupCopy, bool)

// GroupRows builds the group rows of every host, by the rules RoundRows uses: an online host
// gives its own records, live; a host that is not online gives the catalog copy confirmed
// after this client lost it (or when the client never reached it), else its last snapshot.
// A host that failed for good gives none. Ended and dropped groups show for GroupOverShown
// after their end; a catalog copy carries no end time, so an over group there shows while the
// copy holds it. Rows come in board order (SortGroups).
func GroupRows(hs []hosts.Host, cats GroupCatalogs, now time.Time) []GroupRow {
	var out []GroupRow
	shown := merge.Shown(hs)
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
		if !h.Online() {
			var cp GroupCopy
			ok := false
			if id := h.Identity(); id != "" && cats != nil {
				cp, ok = cats(id)
			}
			held := h.Welcome.DeviceID != ""
			if ok && (!held || cp.SeenAt.After(h.Since.Add(CatalogSkew))) {
				for _, c := range cp.Groups {
					out = append(out, catalogGroup(h.Label, c, cp.SeenAt))
				}
				continue
			}
		}
		for _, g := range h.Structure.Groups {
			r := liveGroup(h.Label, g)
			if !h.Online() {
				r.Stale, r.Since = true, h.Since
			}
			if r.Over() && !r.Ended.IsZero() && now.Sub(r.Ended) >= GroupOverShown {
				continue
			}
			out = append(out, r)
		}
	}
	SortGroups(out)
	return out
}

func liveGroup(host string, g api.Group) GroupRow {
	r := GroupRow{Host: host, ID: g.ID, Title: g.Title, State: string(g.State), Coordinator: g.Coordinator.Name,
		Waits: g.Waits, Created: parseTime(g.Created), Ended: parseTime(g.Ended), PausedBy: pausedBy(&g.Pause)}
	if r.Over() && r.Ended.IsZero() {
		r.Ended = parseTime(g.Updated)
	}
	for _, it := range g.Items {
		if it.State != "dropped" {
			r.Items++
		}
		if it.State == "landed" {
			r.Landed++
		}
		r.ItemRows = append(r.ItemRows, GroupItemRow{Title: it.Title, State: string(it.State), Lead: it.Lead, Reason: it.Reason})
	}
	r.Members = len(g.Members)
	return r
}

func catalogGroup(host string, c api.CatalogGroup, seen time.Time) GroupRow {
	r := GroupRow{Host: host, ID: c.ID, Title: c.Title, State: c.State, Items: c.Items, Landed: c.Landed, Members: c.Members,
		PausedBy: pausedBy(c.Pause), Stale: true, Since: seen}
	if c.Coordinator != nil {
		r.Coordinator = c.Coordinator.Name
	}
	return r
}

func pausedBy(p *api.GroupPause) []string {
	var out []string
	if p == nil {
		return nil
	}
	if p.User != "" {
		out = append(out, "you")
	}
	if p.Usage != "" {
		out = append(out, "usage")
	}
	return out
}

// groupRank is a group's place on the board: confirming, running, paused, ending, other
// words, then ended and dropped.
func groupRank(s string) int {
	switch s {
	case "confirming":
		return 0
	case "running":
		return 1
	case "paused":
		return 2
	case "ending":
		return 3
	case "ended", "dropped":
		return 5
	}
	return 4
}

// SortGroups orders live groups first, newest first inside each state; host and id break ties.
func SortGroups(rows []GroupRow) {
	slices.SortStableFunc(rows, func(a, b GroupRow) int {
		if c := cmp.Compare(groupRank(a.State), groupRank(b.State)); c != 0 {
			return c
		}
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// GroupTitles maps each group id to its title, across every host's snapshot, for the member
// mark on session rows: a member's group lives on its home host.
func GroupTitles(hs []hosts.Host) map[string]string {
	out := map[string]string{}
	for _, h := range hs {
		for _, g := range h.Structure.Groups {
			out[g.ID] = g.Title
		}
	}
	return out
}

// MatchGroup reports whether a group row matches the sidebar filter: title or host name, case
// folded.
func MatchGroup(g GroupRow, filter string) bool {
	return Match(Row{Name: g.Title, Key: Key{Host: g.Host}}, filter)
}
