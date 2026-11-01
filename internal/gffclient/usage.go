package gffclient

// Usage: each harness account's limit windows, from every hab, for the usage row
// (gff-client.md 9.1, 15.6; app.md 14). Readings merge by account key; the newest wins.

import (
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// usageEntry is the latest reading of one account and the host it came from.
type usageEntry struct {
	host string
	acct api.UsageAccount
	at   time.Time // the newest window's read_at
}

// usageBook holds the latest reading of each account across hosts.
type usageBook struct {
	byKey map[string]usageEntry
	order []string // account keys in first-seen order, so the row does not reorder
}

// put records a reading from host. An older reading of the same account than the one held is
// dropped (app.md 14.1: the newest reading wins).
func (b *usageBook) put(host string, a api.UsageAccount) {
	if b.byKey == nil {
		b.byKey = map[string]usageEntry{}
	}
	at := readAt(a)
	if old, ok := b.byKey[a.Key]; ok && old.at.After(at) {
		return
	}
	if _, ok := b.byKey[a.Key]; !ok {
		b.order = append(b.order, a.Key)
	}
	b.byKey[a.Key] = usageEntry{host: host, acct: a, at: at}
}

// entries returns the accounts in first-seen order.
func (b *usageBook) entries() []usageEntry {
	out := make([]usageEntry, 0, len(b.order))
	for _, k := range b.order {
		out = append(out, b.byKey[k])
	}
	return out
}

// hostShown reports, for each entry, whether the row names its host: when another entry has
// the same harness and label, so the two would read alike (app.md 14.5 labels each by hab).
func hostShown(es []usageEntry) []bool {
	n := map[[2]string]int{}
	for _, e := range es {
		n[[2]string{e.acct.Harness, e.acct.Label}]++
	}
	out := make([]bool, len(es))
	for i, e := range es {
		out[i] = n[[2]string{e.acct.Harness, e.acct.Label}] > 1
	}
	return out
}

func readAt(a api.UsageAccount) time.Time {
	var t time.Time
	for _, w := range a.Windows {
		if r, err := time.Parse(time.RFC3339, w.ReadAt); err == nil && r.After(t) {
			t = r
		}
	}
	return t
}

// windowLength reads a window's length from its name: 5h, 7d, 30m, 1w. A model's window has no
// length in its name, so it has no pace.
func windowLength(name string) (time.Duration, bool) {
	if len(name) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(name[:len(name)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[name[len(name)-1]]
	if unit == 0 {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// usageLevel is how close an account is to a limit, for the row's colour: 0 clear, 1 at 80 per
// cent or more of a window, 2 at 95 or more (app.md 14.3 thresholds).
type usageLevel int

// usageView is what the usage row shows for one account.
type usageView struct {
	text  string
	level usageLevel
	stale bool
}

// windowState is one window worked out against now.
type windowState struct {
	name    string
	used    int  // whole percent, at most 100
	full    bool // at or above 100
	resetIn time.Duration
	hasRst  bool
	gone    bool // the reset time passed with no reading since
	pace    int  // where even spending would be, in percent; -1 with no length or reset
	stale   bool
	readAt  time.Time
}

func windowAt(w api.UsageWindow, now time.Time) windowState {
	s := windowState{name: w.Name, used: min(max(w.UsedPct, 0), 100), full: w.UsedPct >= 100, pace: -1}
	read, rerr := time.Parse(time.RFC3339, w.ReadAt)
	if rerr == nil {
		s.readAt = read
	}
	length, hasLen := windowLength(w.Name)
	if rst, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		s.hasRst = true
		s.resetIn = rst.Sub(now)
		if rerr == nil && !rst.After(now) && !read.After(rst) {
			s.gone = true
		}
		if hasLen && s.resetIn > 0 {
			gone := length - s.resetIn
			s.pace = int(min(max(gone*100/length, 0), 100))
		}
	}
	// A stale reading: older than the window, or than an hour (app.md 14.5). Its pace would
	// rise with now while its figure stays old, so a stale window has none.
	if rerr == nil {
		age := now.Sub(read)
		if age > time.Hour || (hasLen && age > length) {
			s.stale = true
			s.pace = -1
		}
	}
	return s
}

// usageText is the usage row's text for one account (app.md 14.2 status line):
// "claude Max 5x · 5h 42% resets 1h 58m · 7d 61% resets 4d 23h · over pace on 7d". A window
// that reached its limit says so first; a window spent faster than an even pace is over pace;
// with a pace and neither, the account is on pace. host, when not "", names the hab the
// reading came from: "claude on two · ...".
func usageText(a api.UsageAccount, host string, now time.Time) usageView {
	head := a.Harness
	if a.Label != "" {
		head += " " + a.Label
	}
	if host != "" {
		head += " on " + host
	}
	parts := []string{head}
	var v usageView
	if len(a.Windows) == 0 {
		return usageView{text: head + " · no limits reported"}
	}
	var full, over []string
	paced := false
	stale := true
	var newest time.Time
	for _, w := range a.Windows {
		s := windowAt(w, now)
		if s.readAt.After(newest) {
			newest = s.readAt
		}
		if !s.stale {
			stale = false
		}
		switch {
		case s.gone:
			parts = append(parts, s.name+" reset at "+now.Add(s.resetIn).In(now.Location()).Format("15:04")+"; no reading since")
			continue
		case s.hasRst:
			parts = append(parts, s.name+" "+strconv.Itoa(s.used)+"% resets "+durText(s.resetIn))
		default:
			parts = append(parts, s.name+" "+strconv.Itoa(s.used)+"%")
		}
		switch {
		case s.used >= 95:
			v.level = max(v.level, 2)
		case s.used >= 80:
			v.level = max(v.level, 1)
		}
		if s.full {
			t := s.name + " limit reached"
			if s.hasRst {
				t += " · resets in " + durText(s.resetIn)
			}
			full = append(full, t)
			continue
		}
		if s.pace >= 0 {
			paced = true
			if s.used > s.pace {
				over = append(over, s.name)
			}
		}
	}
	switch {
	case len(full) > 0:
		parts = append(parts, full...)
	case len(over) > 0:
		parts = append(parts, "over pace on "+strings.Join(over, ", "))
	case paced:
		parts = append(parts, "on pace")
	}
	if stale && !newest.IsZero() {
		v.stale = true
		parts = append(parts, "as of "+newest.In(now.Location()).Format("15:04"))
	}
	v.text = strings.Join(parts, " · ")
	return v
}

// durText is a time to reset: "1h 58m", "4d 23h", "12m", "under 1m".
func durText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under 1m"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h " + strconv.Itoa(int(d%time.Hour/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d " + strconv.Itoa(int(d%(24*time.Hour)/time.Hour)) + "h"
}
