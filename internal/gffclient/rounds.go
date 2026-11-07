package gffclient

// Rounds in the workspace (rounds.md 10.3, gff-client.md 7.5): the run-session badge and its
// filter, the board's Rounds list across every host, Run now and Pause or Unpause, and the
// catalog copies that give an offline host's rounds.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/control"
)

// roundBadge marks a run session's row in the sidebar and on the board.
const roundBadge = "↻"

// runOpenWait is how long a Run now waits for its run's session to open it (app.md 21.3).
const runOpenWait = 2 * time.Minute

// pendingRun is a Run now whose session opens when it shows, until the wait lapses or the
// owner opens a session meanwhile.
type pendingRun struct {
	host, run string
	until     time.Time
}

// catCopy is one host's catalog copy, kept from the habs' catalog.list answers.
type catCopy struct {
	rounds []api.CatalogRound
	groups []api.CatalogGroup
	seenAt time.Time
}

// catalog finds the newest catalog copy of a device id (board.Catalogs).
func (u *workspaceUI) catalog(id string) (board.CatalogCopy, bool) {
	c, ok := u.cats[id]
	if !ok {
		return board.CatalogCopy{}, false
	}
	return board.CatalogCopy{Rounds: c.rounds, SeenAt: c.seenAt}, true
}

// listCatalog reads a hab's catalog.list off the loop and keeps each host's newest copy. One
// read per host at a time; a change during a read reads once more after it.
func (u *workspaceUI) listCatalog(hs *hostState) {
	c := u.c
	hm, ok := c.cfg.Hosts.Host(hs.label)
	if !ok || !hasCap(hm.Welcome, "catalog") {
		return
	}
	if u.catBusy[hs.label] > 0 {
		u.catBusy[hs.label] = 2
		return
	}
	u.catBusy[hs.label] = 1
	conn, label := hs.conn, hs.label
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		res, err := conn.API().CatalogList(ctx, api.CatalogListParams{})
		cancel()
		c.run(func() {
			again := u.catBusy[label] == 2
			delete(u.catBusy, label)
			if err == nil {
				ids := map[string]bool{}
				labels := map[string]string{}
				for _, h := range c.cfg.Hosts.Hosts() {
					if id := h.Identity(); id != "" {
						ids[id] = true
						labels[id] = h.Label
					}
				}
				for _, e := range res.Hosts {
					u.keepCatalog(e, ids)
					u.keepHeavy(label, e, labels)
				}
				u.dirty = true
			}
			if again {
				if hs := c.hosts[label]; hs != nil {
					u.listCatalog(hs)
				}
			}
		})
	}()
}

// keepCatalog keeps a copy when it was confirmed later than the one held. It keeps only
// copies of hosts in gff's host list (ids holds their identities), as only those show. It
// skips the answering hab's own entry: gff is connected to that hab, so its snapshot is newer,
// and the entry's time is by that hab's clock.
func (u *workspaceUI) keepCatalog(e api.CatalogEntry, ids map[string]bool) {
	id := e.Catalog.DeviceID
	if id == "" || e.Own || !ids[id] {
		return
	}
	seen, err := time.Parse(time.RFC3339Nano, e.SeenAt)
	if err != nil {
		return
	}
	if old, ok := u.cats[id]; ok && !seen.After(old.seenAt) {
		return
	}
	u.cats[id] = catCopy{rounds: e.Catalog.Rounds, groups: e.Catalog.Groups, seenAt: seen}
}

// visibleRounds lists the round rows the board shows: those matching the filter.
func (u *workspaceUI) visibleRounds() []board.RoundRow {
	var out []board.RoundRow
	for _, r := range u.rounds {
		if board.MatchRound(r, u.filter) {
			out = append(out, r)
		}
	}
	return out
}

// fixBoardSel keeps the board's selection on a row that shows: a gone round moves it to the
// first round, and with no session row it moves to the rounds.
func (u *workspaceUI) fixBoardSel() {
	stops := u.modStops()
	u.fixModSel(stops)
	if u.onMod {
		u.onRound = false
		return
	}
	rounds := u.visibleRounds()
	if u.onRound {
		for _, r := range rounds {
			if r.RoundKey == u.selRound {
				return
			}
		}
		if len(rounds) > 0 {
			u.selRound = rounds[0].RoundKey
			return
		}
		u.onRound = false
	}
	if len(u.flat()) == 0 && len(rounds) > 0 {
		u.onRound, u.selRound = true, rounds[0].RoundKey
	}
	if !u.onRound && len(u.flat()) == 0 && len(stops) > 0 {
		u.onMod, u.selMod = true, stops[0]
	}
}

// selectedRound is the selected round row on the board.
func (u *workspaceUI) selectedRound() (board.RoundRow, bool) {
	if u.mode != modeBoard {
		return board.RoundRow{}, false
	}
	u.fixBoardSel()
	if !u.onRound {
		return board.RoundRow{}, false
	}
	for _, r := range u.visibleRounds() {
		if r.RoundKey == u.selRound {
			return r, true
		}
	}
	return board.RoundRow{}, false
}

// moveBoard moves the board's selection over its session rows, then its round rows, then the
// module cards' targets.
func (u *workspaceUI) moveBoard(d int) {
	u.fixBoardSel()
	rows, rounds, stops := u.flat(), u.visibleRounds(), u.modStops()
	n := len(rows) + len(rounds) + len(stops)
	if n == 0 {
		return
	}
	i := 0
	if u.onMod {
		i = len(rows) + len(rounds) + max(0, slices.Index(stops, u.selMod))
	} else if u.onRound {
		for j, r := range rounds {
			if r.RoundKey == u.selRound {
				i = len(rows) + j
			}
		}
	} else if _, j, ok := u.selected(); ok {
		i = j
	}
	i = min(max(i+d, 0), n-1)
	u.onMod, u.modMoved = false, true
	switch {
	case i < len(rows):
		u.onRound, u.sel = false, rows[i].Key
	case i < len(rows)+len(rounds):
		u.onRound, u.selRound = true, rounds[i-len(rows)].RoundKey
	default:
		u.onRound, u.onMod, u.selMod = false, true, stops[i-len(rows)-len(rounds)]
	}
}

// toggleRuns hides or shows run sessions in the sidebar and on the board. Run sessions that
// need the owner always show.
func (u *workspaceUI) toggleRuns() {
	u.showRuns = !u.showRuns
	if u.showRuns {
		u.msg = "run sessions shown"
	} else {
		u.msg = "run sessions hidden, but those that need you"
	}
}

// roundUsable says why a round row takes no action, or "".
func roundUsable(r board.RoundRow) string {
	if r.Stale {
		return r.Host + " is offline"
	}
	return ""
}

// runNow starts a run of the selected round (round.run). Its session opens when it shows.
func (u *workspaceUI) runNow() {
	r, ok := u.selectedRound()
	if !ok {
		u.msg = "select a round to run"
		return
	}
	if why := roundUsable(r); why != "" {
		u.msg = why
		return
	}
	u.msg = "starting a run of " + r.Name + " on " + r.Host
	u.roundCall(r, "run "+r.Name, func(ctx context.Context, a api.Client) (func(), error) {
		res, err := a.RoundRun(ctx, api.RoundRef{ID: r.Round})
		if err != nil {
			return nil, err
		}
		return func() {
			u.msg = fmt.Sprintf("%s run %d %s on %s", r.Name, res.Run.Number, res.Run.State, r.Host)
			u.pendRun = pendingRun{host: r.Host, run: res.Run.ID, until: u.c.cfg.Now().Add(runOpenWait)}
			u.dirty = true
		}, nil
	})
}

// pauseRound pauses the selected round, or unpauses a paused one.
func (u *workspaceUI) pauseRound() {
	r, ok := u.selectedRound()
	if !ok {
		u.msg = "select a round to pause"
		return
	}
	if why := roundUsable(r); why != "" {
		u.msg = why
		return
	}
	what := "pause "
	if r.Paused {
		what = "unpause "
	}
	u.roundCall(r, what+r.Name, func(ctx context.Context, a api.Client) (func(), error) {
		var err error
		if r.Paused {
			_, err = a.RoundUnpause(ctx, api.RoundRef{ID: r.Round})
		} else {
			_, err = a.RoundPause(ctx, api.RoundRef{ID: r.Round})
		}
		return nil, err
	})
}

// roundCall makes one round call off the loop. A refusal shows on the bottom row in the hab's
// words; done, when given, runs on the loop after a success.
func (u *workspaceUI) roundCall(r board.RoundRow, what string, f func(context.Context, api.Client) (func(), error)) {
	c := u.c
	hs := c.hosts[r.Host]
	if hs == nil {
		u.msg = r.Host + " is offline"
		return
	}
	conn := hs.conn
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		defer cancel()
		done, err := f(ctx, conn.API())
		c.run(func() {
			switch {
			case err != nil:
				u.msg = what + ": " + roundErr(err)
			case done != nil:
				done()
			}
		})
	}()
}

// roundErr is a refusal in the hab's words, without the error kind.
func roundErr(err error) string {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Message
	}
	return callErr(err)
}

// openRound opens the selected round's newest run's session, when it has one on screen.
func (u *workspaceUI) openRound(r board.RoundRow, newTab bool) {
	if r.LastSession == "" {
		u.msg = r.Name + " has no run session"
		return
	}
	for _, row := range u.rows {
		if row.Host == r.Host && row.Session == r.LastSession {
			u.open(row, newTab)
			return
		}
	}
	u.msg = r.Name + "'s last run session is gone"
}

// openPending opens a Run now's session once it shows and has a pane.
func (u *workspaceUI) openPending() {
	p := u.pendRun
	if p.run == "" {
		return
	}
	if u.c.cfg.Now().After(p.until) {
		u.pendRun = pendingRun{}
		return
	}
	for _, r := range u.rows {
		if r.Host == p.host && r.Round != nil && r.Round.Run == p.run && r.Pane != "" &&
			r.State != api.SessionStateExited && !r.Offline() {
			u.pendRun = pendingRun{}
			u.open(r, false)
			return
		}
	}
}

// nextText is a round's next run in the viewer's zone: "today 09:00", "tomorrow 09:00", else
// the short date; with the round's own clock beside it when that differs (rounds.md 4.1).
func nextText(now, t time.Time, zone string) string {
	if t.IsZero() {
		return "-"
	}
	lt, ln := t.In(time.Local), now.In(time.Local)
	y1, m1, d1 := lt.Date()
	y2, m2, d2 := ln.Date()
	days := int(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC).Sub(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC)) / (24 * time.Hour))
	var s string
	switch days {
	case 0:
		s = "today " + lt.Format("15:04")
	case 1:
		s = "tomorrow " + lt.Format("15:04")
	default:
		s = lt.Format("Jan 2 15:04")
	}
	if zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			if rt := t.In(loc); rt.Format("15:04") != lt.Format("15:04") {
				s += " (" + rt.Format("15:04") + " " + zone + ")"
			}
		}
	}
	return s
}

// roundText is one round row of the board.
func roundText(now time.Time, r board.RoundRow) string {
	mark := r.Mark()
	if r.Reason != "" && (r.Paused || r.Completed) {
		mark += ": " + r.Reason
	}
	if r.Stale {
		mark += " · stale " + ago(now, r.Since)
	}
	next := r.Next
	if r.Paused || r.Completed {
		next = time.Time{} // it will not run then
	}
	// The trigger comes last: a zone name is long, and a cut one is wrong. Why it no longer
	// compiles follows it.
	trigger := r.Trigger
	if r.TriggerError != "" {
		trigger += ": " + r.TriggerError
	}
	return fmt.Sprintf("  %-20s %-7s %-10s %-28s %-22s %s", middle(r.Name, 20), middle(string(r.Mode), 7),
		middle(r.Host, 10), middle(nextText(now, next, r.Zone), 28), middle(mark, 22), trigger)
}
