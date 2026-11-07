package gffclient

// The workspace's bars: the tab bar with the needs-you count, the warnings, the hosts and the
// clock; the usage row; and the shortcut bar with the keys of the mode and system info
// (gff-client.md 3.1, 8, 9).

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/hosts"
	gffversion "github.com/noahkiss/flightdeck/internal/version"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// draw draws the bars and, as the mode asks, the sidebar or the board. It returns where the
// cursor goes when a prompt has the keys.
func (u *workspaceUI) draw() (int, int, bool) {
	u.refresh()
	if !u.bars {
		if u.mode == modeBoard {
			u.drawBoard(rect{0, 0, u.c.w, u.c.h})
		}
		return 0, 0, false
	}
	c := u.c
	u.drawTabBar()
	top := u.top()
	if u.usageShown() {
		u.drawUsage(1)
	}
	if u.mode == modeBoard {
		u.drawBoard(rect{0, top, c.w, max(c.h-top-1, 1)})
	} else if u.sidebarShown() {
		sw := min(SidebarWidth, c.w-1)
		u.drawSidebar(rect{0, top, sw - 1, max(c.h-top-1, 1)})
		for y := top; y < c.h-1; y++ {
			c.canvas.set(sw-1, y, hcell{text: "│", style: dimStyle})
		}
	}
	if len(c.tab().views) == 0 && u.mode != modeBoard {
		a := u.paneArea()
		c.canvas.text(a.x+1, a.y+1, a.w-1, "empty tab: open a session from the sidebar", dimStyle)
	}
	return u.drawBottom()
}

func (u *workspaceUI) drawTabBar() {
	c := u.c
	c.canvas.text(0, 0, c.w, "", barStyle)
	x := 0
	needTabs := map[*tab]bool{}
	for _, t := range c.tabs {
		for _, v := range t.views {
			if r, ok := rowOf(u.rows, v); ok && r.Rank.NeedsYou() && !r.Offline() {
				needTabs[t] = true
			}
		}
	}
	now := c.cfg.Now()
	for i, t := range c.tabs {
		s := " " + strconv.Itoa(i+1) + ":" + t.name
		switch {
		case needTabs[t]:
			s += "!"
		case now.Before(t.bell):
			s += "*"
		case t.activity:
			s += "~"
		}
		s += " "
		st := barStyle
		if i == c.cur {
			st = activeTab
		}
		c.canvas.text(x, 0, min(len([]rune(s)), max(c.w-x, 0)), s, st)
		x += len([]rune(s))
	}
	online, total := 0, 0
	for _, h := range c.cfg.Hosts.Hosts() {
		total++
		if h.Online() {
			online++
		}
	}
	parts := []string{fmt.Sprintf("%d need you", board.NeedsYou(u.rows))}
	if n := len(u.warnings()); n == 1 {
		parts = append(parts, "1 warning")
	} else if n > 1 {
		parts = append(parts, strconv.Itoa(n)+" warnings")
	}
	parts = append(parts, fmt.Sprintf("%d/%d hosts", online, total))
	if c.cfg.Workspace != "" {
		parts = append(parts, c.cfg.Workspace)
	}
	if c.cfg.Bar.Clock != "" {
		parts = append(parts, now.Format(c.cfg.Bar.Clock))
	}
	right := " " + strings.Join(parts, " · ") + " "
	if rx := c.w - len([]rune(right)); rx > x {
		c.canvas.text(rx, 0, c.w-rx, right, barStyle)
	}
}

// warnings lists what the warnings badge counts (gff-client.md 9.1): each host that is not
// online, with its problem: offline, connecting, update needed, host key changed; then each
// online host whose hab or holder build is superseded.
func (u *workspaceUI) warnings() []string {
	return hostWarnings(u.c.cfg.Hosts.Hosts(), u.c.cfg.Version, u.c.cfg.Now())
}

// hostWarnings is warnings over a list of hosts, with own as gff's release version.
//
// A hab is superseded when its welcome's release version is older than the newest release
// version among gff's own and the online habs'; a dev build or a hab that sends no version is
// never compared. A holder build is superseded when a pane's holder_build differs from its
// hab's build: gff-hab drains a holder of another build (hab-holder.md 4.3), so such a pane
// runs on an older holder until it ends.
func hostWarnings(hs []hosts.Host, own string, now time.Time) []string {
	var out []string
	newest := ""
	if gffversion.IsRelease(own) {
		newest = own
	}
	for _, h := range hs {
		v := h.Welcome.Version
		if !h.Online() || !gffversion.IsRelease(v) {
			continue
		}
		if c, _ := gffversion.Compare(v, newest); newest == "" || c > 0 {
			newest = v
		}
	}
	for _, h := range hs {
		p := h.Problem()
		if p == "" {
			continue
		}
		s := h.Label + " " + p
		if p == "offline" && !h.Since.IsZero() {
			s += " " + ago(now, h.Since)
		}
		out = append(out, s)
	}
	for _, h := range hs {
		if !h.Online() {
			continue
		}
		if c, ok := gffversion.Compare(h.Welcome.Version, newest); ok && c < 0 {
			out = append(out, h.Label+" hab "+h.Welcome.Version+" superseded by "+newest)
		}
		n := 0
		for _, p := range h.Structure.Panes {
			if h.Welcome.Build != "" && p.HolderBuild != "" && p.HolderBuild != h.Welcome.Build {
				n++
			}
		}
		switch {
		case n == 1:
			out = append(out, h.Label+" holder build superseded: 1 pane")
		case n > 1:
			out = append(out, h.Label+" holder build superseded: "+strconv.Itoa(n)+" panes")
		}
	}
	return out
}

// usageCounts is the per-harness counts of the usage row, across every online host: "claude
// 2 working 1 needs you".
func usageCounts(rows []board.Row) string {
	type count struct{ working, needs int }
	counts := map[string]*count{}
	var names []string
	for _, r := range rows {
		if r.Agent == nil || r.Offline() || r.Agent.Harness == "" {
			continue
		}
		n := counts[r.Agent.Harness]
		if n == nil {
			n = &count{}
			counts[r.Agent.Harness] = n
			names = append(names, r.Agent.Harness)
		}
		switch {
		case r.Rank.NeedsYou():
			n.needs++
		case r.Rank == board.RankWorking:
			n.working++
		}
	}
	slices.Sort(names)
	var parts []string
	for _, h := range names {
		n := counts[h]
		var p []string
		if n.working > 0 {
			p = append(p, strconv.Itoa(n.working)+" working")
		}
		if n.needs > 0 {
			p = append(p, strconv.Itoa(n.needs)+" needs you")
		}
		if len(p) > 0 {
			parts = append(parts, h+" "+strings.Join(p, " "))
		}
	}
	return strings.Join(parts, " · ")
}

// Usage row styles: a warning colour near a limit, grey when stale.
var usageStyles = map[usageLevel]vt.Style{0: {}, 1: {Fg: palette(3)}, 2: {Bold: true, Fg: palette(1)}}

// drawUsage draws the usage row at row y: each account's windows, then the per-harness counts
// on the right. With no reading from any host, it says so: no usage is not zero usage.
func (u *workspaceUI) drawUsage(y int) {
	c := u.c
	c.canvas.text(0, y, c.w, "", vt.Style{})
	counts := usageCounts(u.rows)
	right := c.w
	if counts != "" {
		s := " " + counts + " "
		right = max(c.w-len([]rune(s)), 0)
		c.canvas.text(right, y, c.w-right, s, dimStyle)
	}
	es := u.usage.entries()
	if len(es) == 0 {
		c.canvas.text(0, y, right, " no usage reported", dimStyle)
		return
	}
	now := c.cfg.Now()
	named := hostShown(es)
	x := 0
	for i, e := range es {
		host := ""
		if named[i] {
			host = e.host
		}
		v := usageText(e.acct, host, now)
		s := " " + v.text + " "
		if i > 0 {
			s = "│" + s
		}
		st := usageStyles[v.level]
		if v.stale {
			st = dimStyle
		}
		if x >= right {
			break
		}
		c.canvas.text(x, y, right-x, s, st)
		x += len([]rune(s))
	}
}

// barMode is the mode whose keys the shortcut bar shows: the key reader's when it is in a mode,
// else the sidebar's or the board's while it has focus, else normal.
func (u *workspaceUI) barMode() Mode {
	if m := u.c.keys.mode; m != ModeNormal {
		return m
	}
	switch u.mode {
	case modeSidebar:
		return ModeSidebar
	case modeBoard:
		return ModeBoard
	}
	return ModeNormal
}

// barText is the shortcut bar's left part in w columns: the keys of the mode, from the loaded
// keymap (gff-client.md 8.1). A mode other than normal, the sidebar and the board is named
// first.
func (u *workspaceUI) barText(w int) string {
	m := u.barMode()
	lead := ""
	switch m {
	case ModeSession, ModePane, ModeTab, ModeLocked:
		lead = " " + string(m) + " ·"
	}
	return lead + fitHints(u.c.cfg.Keymap.hints(m), w-len([]rune(lead)), u.c.cfg.Bar.MaxHints)
}

func (u *workspaceUI) drawBottom() (int, int, bool) {
	c := u.c
	y := c.h - 1
	if p := u.prompt; p != nil {
		s := " " + p.label
		if p.hosts {
			s = " [" + p.host + "] " + p.label
		}
		if !p.yesNo {
			s += ": " + string(p.text)
		}
		c.canvas.text(0, y, c.w, s, vt.Style{})
		return min(len([]rune(s)), c.w-1), y, true
	}
	c.canvas.text(0, y, c.w, "", barStyle)
	info, st := u.infoText()
	left := c.w
	if info != "" && len([]rune(info))+2 < c.w/2 {
		s := " " + info + " "
		left = c.w - len([]rune(s))
		c.canvas.text(left, y, c.w-left, s, st)
	}
	s := ""
	if u.msg != "" {
		s = " " + u.msg
	} else {
		s = u.barText(left - 1)
	}
	c.canvas.text(0, y, max(left-1, 0), s, barStyle)
	return 0, 0, false
}

// infoText is the system info for the bar and its style: dim, with the reading's age, when its
// host is offline (9.2).
func (u *workspaceUI) infoText() (string, vt.Style) {
	r := u.sys
	if r == nil {
		return "", barStyle
	}
	s := infoText(r.stats, u.c.cfg.Bar.Info)
	if s == "" {
		return "", barStyle
	}
	if r.host == "" {
		return s, barStyle
	}
	s = r.host + " " + s
	if hm, ok := u.c.cfg.Hosts.Host(r.host); !ok || !hm.Online() {
		return s + " " + ago(u.c.cfg.Now(), r.got), vt.Style{Inverse: true, Faint: true}
	}
	return s, barStyle
}

// statsHost is the host whose figures the bar shows: the focused pane's host when it offers
// host stats, else "" for this machine. The capability comes from the host's last welcome, so
// an offline host that offered it keeps its last figures, dimmed.
func (u *workspaceUI) statsHost() string {
	v := u.c.focused()
	if v == nil {
		return ""
	}
	hm, ok := u.c.cfg.Hosts.Host(v.h.label)
	if !ok || !hasCap(hm.Welcome, "host-stats") {
		return ""
	}
	return hm.Label
}

// readStats starts a reading of the figures when the bar shows and the last one is at least
// statsEvery old (9.2). It reads off the loop.
func (u *workspaceUI) readStats() {
	c := u.c
	now := c.cfg.Now()
	if !u.bars || u.sysBusy || (!u.sysAt.IsZero() && now.Sub(u.sysAt) < statsEvery) {
		return
	}
	host := u.statsHost()
	hs := c.hosts[host]
	if host != "" {
		hm, _ := c.cfg.Hosts.Host(host)
		if !hm.Online() {
			return // its last figures stay, dimmed with their age
		}
		if hs == nil {
			host = ""
		}
	}
	u.sysAt, u.sysBusy = now, true
	var conn *hostState
	if host != "" {
		conn = hs
	}
	read := c.cfg.Stats
	go func() {
		var st api.HostStats
		var err error
		if conn != nil {
			ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
			st, err = conn.conn.API().HabStats(ctx, api.HabStatsParams{})
			cancel()
		} else {
			st, err = read()
		}
		c.run(func() {
			u.sysBusy = false
			if err != nil {
				return
			}
			u.sys = &sysReading{stats: st, host: host, got: c.cfg.Now()}
		})
	}()
}

// listUsage asks a host that came online for its usage readings. A hab that does not serve
// usage yet answers unavailable, and the row keeps what it has.
func (u *workspaceUI) listUsage(hs *hostState) {
	c := u.c
	conn, label := hs.conn, hs.label
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		res, err := conn.API().UsageList(ctx, api.UsageListParams{})
		cancel()
		if err != nil {
			return
		}
		c.run(func() {
			for _, a := range res.Accounts {
				if a.Key != "" {
					u.usage.put(label, a)
				}
			}
		})
	}()
}
