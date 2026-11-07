package gffclient

// The usage governor on the status board (capcom-service.md 10.1): one line per consumer that
// is not at ok, or that runs unpaced, from the catalog's governor section.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// governorCopy is one host's governor section, from the newest catalog entry read.
type governorCopy struct {
	host    string
	section api.CatalogGovernor
	own     bool
	seenAt  time.Time
}

// keepGovernor keeps a host's governor section, by the rule keepHeavy follows: the answering
// hab's own entry wins while that host is online; a peer's copy is kept for a host in gff's
// host list when it is newer.
func (u *workspaceUI) keepGovernor(answering string, e api.CatalogEntry, labels map[string]string) {
	id := e.Catalog.DeviceID
	if id == "" {
		return
	}
	label := answering
	if !e.Own {
		var ok bool
		if label, ok = labels[id]; !ok {
			return
		}
	}
	seen, _ := time.Parse(time.RFC3339Nano, e.SeenAt)
	old, had := u.governor[id]
	if had && !e.Own && ((old.own && u.heavyOnline(old.host)) || !seen.After(old.seenAt)) {
		return
	}
	if e.Catalog.Governor == nil {
		delete(u.governor, id)
		return
	}
	u.governor[id] = governorCopy{host: label, section: *e.Catalog.Governor, own: e.Own, seenAt: seen}
}

// governorLines are the board's lines of the usage governor: each consumer that is not at ok,
// or is unpaced, by host and id.
func (u *workspaceUI) governorLines(now time.Time) []string {
	var cs []governorCopy
	for _, c := range u.governor {
		if c.own && !u.heavyOnline(c.host) {
			continue
		}
		cs = append(cs, c)
	}
	slices.SortFunc(cs, func(a, b governorCopy) int { return strings.Compare(a.host, b.host) })
	var out []string
	for _, c := range cs {
		for _, k := range c.section.Consumers {
			if t := governorText(k, now); t != "" {
				out = append(out, fmt.Sprintf("  %-10s %s", middle(c.host, 10), t))
			}
		}
	}
	return out
}

// governorText is one consumer's line, or "" when it runs at ok on a reading.
func governorText(k api.GovernorConsumer, now time.Time) string {
	var use string
	switch k.Basis {
	case "plan":
		use = fmt.Sprintf("%d%% of the week used, share %d%%", k.UsedPct, k.SharePct)
	case "usd":
		use = fmt.Sprintf("$%.2f of $%.2f today", float64(k.SpentCents)/100, float64(k.CapCents)/100)
	case "none":
		return fmt.Sprintf("%s unpaced: %s tokens today", k.ID, tokens(k.InputTokens+k.OutputTokens))
	}
	var pace string
	switch {
	case k.Pace == "wait":
		pace = "waits" + untilWord(k.Until, now)
	case k.Pace == "small":
		pace = "small model"
	case k.EventPace == "wait":
		pace = "event turns wait" + untilWord(k.EventUntil, now)
	default:
		return ""
	}
	return fmt.Sprintf("%s %s · %s", k.ID, pace, use)
}

func untilWord(at string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return ""
	}
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return " until " + t.Format("15:04")
	}
	return " until " + t.Format("Mon 15:04")
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
