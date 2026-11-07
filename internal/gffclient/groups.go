package gffclient

// Agent groups on the status board (groups.md 5.4, gff-client.md 7.7): one card per group
// below the sessions, from each home host's snapshot, or from another hab's catalog copy
// while the home host is offline.

import (
	"fmt"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// groupCatalog finds the newest catalog copy of a device id's groups (board.GroupCatalogs).
func (u *workspaceUI) groupCatalog(id string) (board.GroupCopy, bool) {
	c, ok := u.cats[id]
	if !ok {
		return board.GroupCopy{}, false
	}
	return board.GroupCopy{Groups: c.groups, SeenAt: c.seenAt}, true
}

// groupLine is one line of the Groups section.
type groupLine struct {
	text string
	st   vt.Style
}

// groupLines are the Groups section's lines: per group a header line, then its items while
// the group is live and its record is at hand.
func (u *workspaceUI) groupLines(now time.Time) []groupLine {
	var out []groupLine
	for _, g := range u.groups {
		if !board.MatchGroup(g, u.filter) {
			continue
		}
		st := vt.Style{}
		if g.Stale || g.Over() {
			st = dimStyle
		}
		out = append(out, groupLine{groupText(now, g), st})
		if g.Over() || g.Stale {
			continue
		}
		for _, it := range g.ItemRows {
			text := fmt.Sprintf("    · %-22s %-9s %s", middle(it.Title, 22), middle(it.State, 9), it.Lead)
			if it.Reason != "" {
				text += " · " + it.Reason
			}
			out = append(out, groupLine{strings.TrimRight(text, " "), dimStyle})
		}
	}
	return out
}

// groupText is a group's header line: title, state, home host, then the counts, the elapsed
// time, the pause reasons, the waits and the coordinator.
func groupText(now time.Time, g board.GroupRow) string {
	parts := []string{fmt.Sprintf("%d/%d landed", g.Landed, g.Items), fmt.Sprintf("%d members", g.Members)}
	if !g.Created.IsZero() {
		end := now
		if !g.Ended.IsZero() {
			end = g.Ended
		}
		parts = append(parts, ago(end, g.Created))
	}
	if len(g.PausedBy) > 0 {
		parts = append(parts, "paused by "+strings.Join(g.PausedBy, " and "))
	}
	if len(g.Waits) > 0 {
		parts = append(parts, "waiting: "+strings.Join(g.Waits, ", "))
	}
	if g.Coordinator != "" {
		parts = append(parts, "coordinator "+g.Coordinator)
	}
	if g.Stale {
		parts = append(parts, "offline "+ago(now, g.Since))
	}
	return fmt.Sprintf("  %-24s %-10s %-10s %s", middle(g.Title, 24), middle(g.State, 10), middle(g.Host, 10),
		strings.Join(parts, " · "))
}
