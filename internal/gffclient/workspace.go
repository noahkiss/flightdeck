package gffclient

// The workspace: tabs, the sidebar of every session on every host, the status board, and the
// bars (gff-client.md 3.1, 4, 5.1, 6, 7). Structure comes from the habs through internal/hosts
// and is ranked by internal/board; everything here is presentation, kept in this process.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/modview"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// SidebarWidth is the sidebar's width in columns, separator included (gff-client.md 4.1).
const SidebarWidth = 28

// minSidebarHost is the narrowest host that shows the sidebar beside the panes; a narrower
// one shows it only while it has focus, over the panes (4.2).
const minSidebarHost = 80

type uiMode int

const (
	modePanes   uiMode = iota // keys go to the focused pane
	modeSidebar               // keys move in the sidebar
	modeBoard                 // the status board fills the screen
	modePrompt                // a line of text or a yes or no, on the bottom bar
	modeThread                // the Capcom service's thread fills the pane area (capcom.go)
)

// prompt is a question on the bottom bar.
type prompt struct {
	label string
	text  []rune
	yesNo bool
	hosts bool // Tab picks the next online host
	host  string
	done  func(text string, host string)
	back  uiMode
}

// workspaceUI is the workspace's presentation state.
type workspaceUI struct {
	c        *client
	mode     uiMode
	bars     bool // the tab bar, sidebar and bottom bar show
	sidebar  bool // the sidebar shows beside the panes
	group    board.GroupBy
	filter   string
	filterOn bool
	sel      board.Key // the selected row, kept across refreshes
	order    board.Order
	rows     []board.Row // every row, ordered
	dirty    bool
	seen     map[string]string // host/pane -> the done state's time the owner saw
	focusAt  map[string]string // host/session -> the last session.focus sent
	resuming map[string]bool   // host/session -> a session.resume is out
	pending  []pendingStart    // startup tabs' programs waiting for their hosts (saved.go)
	msg      string            // one line on the bottom bar until the next key
	prompt   *prompt
	thread   *threadState // the Capcom thread shown in modeThread

	usage   usageBook   // each account's latest usage reading, from every host
	sys     *sysReading // the last system info reading, or nil
	sysAt   time.Time   // when the last reading started
	sysBusy bool        // a reading is out
	pasting bool        // inside a bracketed paste that came to the sidebar, board or a prompt

	// Rounds (rounds.go).
	showRuns bool             // run sessions show; off hides those that do not need the owner
	rounds   []board.RoundRow // every round on every host, ordered
	onRound  bool             // the board's selection is on a round row
	boardTop int              // the first board line under the head, so the selection shows
	selRound board.RoundKey
	cats     map[string]catCopy      // device id -> the newest catalog copy
	catBusy  map[string]int          // host -> 1 a catalog read is out, 2 and another is due
	heavy    map[string]heavyCopy    // device id -> the heavy-run lock's section (heavy.go)
	governor map[string]governorCopy // device id -> the usage governor's section (governor.go)
	pendRun  pendingRun

	// Modules (modules.go).
	onMod    bool             // the board's selection is on a module target
	selMod   modStop          // that target
	modFold  map[modStop]bool // sections the owner folded or unfolded, by card and ref
	modSeen  modSeen          // the selected target as last drawn
	modMoved bool             // the owner moved the selection since it was drawn
}

func (u *workspaceUI) init(c *client) {
	u.c, u.bars, u.sidebar, u.dirty = c, true, true, true
	u.group = board.GroupProject
	u.seen = map[string]string{}
	u.focusAt = map[string]string{}
	u.resuming = map[string]bool{}
	u.showRuns = true
	u.cats, u.catBusy = map[string]catCopy{}, map[string]int{}
	u.heavy = map[string]heavyCopy{}
	u.governor = map[string]governorCopy{}
	u.order.Now = func() time.Time { return c.cfg.Now() }
	if c.ws && len(c.cfg.Panes) == 0 {
		u.mode = modeSidebar // nothing on screen yet: start where sessions are picked
	}
}

func (u *workspaceUI) changed() { u.dirty = true }

func (u *workspaceUI) tick() {
	now := u.c.cfg.Now()
	for _, t := range u.c.tabs {
		if !t.bell.IsZero() && now.After(t.bell) {
			t.bell = time.Time{}
		}
	}
	u.c.eachView(func(_ *tab, v *view) { v.dirty = true })
	u.readStats()
	u.c.saveWorkspace() // at most once a second, and only when the workspace changed
}

// refresh rebuilds the rows when the hosts changed.
func (u *workspaceUI) refresh() {
	if !u.dirty {
		return
	}
	u.dirty = false
	hs := u.c.cfg.Hosts.Hosts()
	u.rows = board.Rows(hs, u.seenFn)
	u.order.Sort(u.rows)
	u.rounds = board.RoundRows(hs, u.catalog)
	board.SortRounds(u.rounds)
	u.follow()
	u.markSeen()
	u.openPending()
}

func (u *workspaceUI) seenFn(host, pane string, a *api.AgentState) bool {
	return u.seen[host+"/"+pane] == a.At
}

// markSeen marks the focused view's done state seen while the owner looks at it, and reports
// focus to the hab (gff-client.md 7.1). The hab's session.focus is a later stage; until it is
// built, the mark stays in gff.
func (u *workspaceUI) markSeen() {
	v := u.c.focused()
	if v == nil || u.mode != modePanes {
		return
	}
	r, ok := rowOf(u.rows, v)
	if !ok {
		return
	}
	if r.Agent != nil && r.Agent.State == api.AgentStatusDone && u.seen[r.Host+"/"+r.Pane] != r.Agent.At {
		u.seen[r.Host+"/"+r.Pane] = r.Agent.At
		u.dirty = true
	}
	if r.Session != "" && !r.Offline() {
		k := r.Host + "/" + r.Session
		at := ""
		if r.Agent != nil {
			at = r.Agent.At
		}
		if u.focusAt[k] != "seen "+at {
			u.focusAt[k] = "seen " + at
			conn, sess := v.h.conn, r.Session
			go func() {
				ctx, cancel := context.WithTimeout(u.c.ctx, callTimeout)
				defer cancel()
				conn.API().SessionFocus(ctx, api.SessionFocusParams{Session: sess, Active: true})
			}()
		}
	}
}

func (u *workspaceUI) focusChanged() { u.dirty = true }

// visible returns the rows the sidebar and board list: filtered, grouped.
func (u *workspaceUI) visible() []board.Group {
	var rows []board.Row
	for _, r := range u.rows {
		if board.Match(r, u.filter) && board.ShowRuns(r, u.showRuns) {
			rows = append(rows, r)
		}
	}
	if u.mode == modeBoard || u.prompt != nil && u.prompt.back == modeBoard {
		return board.Groups(rows, board.GroupNone) // the board labels its rank sections instead
	}
	return board.Groups(rows, u.group)
}

// flat lists the visible rows in display order.
func (u *workspaceUI) flat() []board.Row {
	var out []board.Row
	for _, g := range u.visible() {
		out = append(out, g.Rows...)
	}
	return out
}

func (u *workspaceUI) selected() (board.Row, int, bool) {
	rows := u.flat()
	for i, r := range rows {
		if r.Key == u.sel {
			return r, i, true
		}
	}
	if len(rows) > 0 {
		u.sel = rows[0].Key
		return rows[0], 0, true
	}
	return board.Row{}, -1, false
}

func (u *workspaceUI) move(d int) {
	if u.mode == modeBoard {
		u.moveBoard(d)
		return
	}
	rows := u.flat()
	_, i, ok := u.selected()
	if !ok {
		return
	}
	i = min(max(i+d, 0), len(rows)-1)
	u.sel = rows[i].Key
}

// event handles one hab event beyond the structure: bells and output marks on tabs
// (gff-client.md 5.1, 15.7).
func (u *workspaceUI) event(hs *hostState, e api.Event) {
	switch e.Type {
	case api.EventPaneBell:
		until := u.c.cfg.Now().Add(time.Second)
		for i, t := range u.c.tabs {
			for _, v := range t.views {
				if v.h == hs && v.spec.ID == e.ID {
					v.bellUntil, v.dirty = until, true
					if i != u.c.cur {
						t.bell = until
					}
				}
			}
		}
	case api.EventCatalogUpdated:
		u.listCatalog(hs)
	case api.EventCapcomItem, api.EventCapcomTyping:
		u.threadEvent(hs, e)
	case api.EventUsageUpdated:
		var a api.UsageAccount
		if json.Unmarshal(e.Data, &a) == nil && a.Key != "" {
			u.usage.put(hs.label, a)
		}
	case api.EventPaneOutput:
		var o api.PaneOutput
		if json.Unmarshal(e.Data, &o) != nil {
			return
		}
		for i, t := range u.c.tabs {
			if i == u.c.cur {
				continue
			}
			for _, v := range t.views {
				// Output the view already applied, before its tab was hidden, is not news.
				if v.h == hs && v.spec.ID == e.ID && (!v.haveOffset || o.Offset > v.offset) {
					t.activity = true
				}
			}
		}
	}
}

// passOK reports whether the panes may take the whole host: the bars are hidden and no
// overlay is up.
func (u *workspaceUI) passOK() bool { return !u.bars && u.mode == modePanes }

// sidebarShown reports whether the sidebar takes columns beside the panes.
func (u *workspaceUI) sidebarShown() bool {
	if !u.bars {
		return false
	}
	if u.mode == modeSidebar || u.prompt != nil && u.prompt.back == modeSidebar {
		return true
	}
	return u.sidebar && u.c.w >= minSidebarHost
}

func (u *workspaceUI) paneArea() rect {
	c := u.c
	if !u.bars {
		return rect{0, 0, c.w, c.h}
	}
	x := 0
	if u.sidebarShown() {
		x = min(SidebarWidth, c.w-1)
	}
	top := u.top()
	return rect{x, top, max(c.w-x, 1), max(c.h-top-1, 2)}
}

// top is the first row under the bars on top: the tab bar, and the usage row when it shows.
func (u *workspaceUI) top() int {
	if u.usageShown() {
		return 2
	}
	return 1
}

// usageShown reports whether the usage row shows: the config has it on and the host has the
// rows for it beside a pane.
func (u *workspaceUI) usageShown() bool { return u.bars && u.c.cfg.Bar.Usage && u.c.h >= 8 }

// command handles an action of the workspace; false leaves it to the pane actions.
func (u *workspaceUI) command(act string) bool {
	c := u.c
	u.msg = ""
	switch act {
	case actBack, actSession, actPaneMode, actTabMode, actLock, actUnlock:
		// Mode keys: the key reader moved the mode, and the bar shows it.
	case actRunSessions:
		u.toggleRuns()
	case actWarnings:
		if w := u.warnings(); len(w) > 0 {
			u.msg = strings.Join(w, " · ")
		} else {
			u.msg = "no warnings"
		}
	case actSidebar:
		u.bars = true
		if u.mode == modeSidebar {
			u.mode = modePanes
		} else {
			u.mode = modeSidebar
		}
		c.layoutChanged()
	case actBars:
		u.bars = !u.bars
		if !u.bars {
			u.mode = modePanes
		}
		c.layoutChanged()
	case actBoard:
		if u.mode == modeBoard {
			u.mode = modePanes
		} else {
			u.mode = modeBoard
		}
		c.canvas.valid = false
	case actNewTab:
		c.tabs = append(c.tabs, &tab{name: c.freeTabName(len(c.tabs) + 1), split: c.cfg.Split})
		u.switchTab(len(c.tabs) - 1)
		u.mode = modeSidebar
	case actRenameTab:
		u.renameTab()
	case actCloseTab:
		u.closeTab()
	case actLastTab:
		for i, t := range c.tabs {
			if t == c.last {
				u.switchTab(i)
				break
			}
		}
	case actMoveTabLeft, actMoveTabRight:
		j := c.cur - 1
		if act == actMoveTabRight {
			j = c.cur + 1
		}
		if j >= 0 && j < len(c.tabs) {
			c.tabs[c.cur], c.tabs[j] = c.tabs[j], c.tabs[c.cur]
			c.cur = j
			u.dirty = true
		}
	case actNextTab:
		u.switchTab((c.cur + 1) % len(c.tabs))
	case actPrevTab:
		u.switchTab((c.cur + len(c.tabs) - 1) % len(c.tabs))
	case actNewPane, actNewRight, actNewDown, actNewLeft, actNewUp:
		u.newPane(map[string]place{actNewPane: placeStack, actNewRight: placeRight, actNewDown: placeDown,
			actNewLeft: placeLeft, actNewUp: placeUp}[act])
	case actCloseView:
		if v := c.focused(); v != nil {
			c.removeView(c.tab(), v)
			c.layoutChanged()
		}
	default:
		if n, ok := strings.CutPrefix(act, actTabN); ok {
			if i, err := strconv.Atoi(n); err == nil && i >= 1 && i <= len(c.tabs) {
				u.switchTab(i - 1)
			}
			return true
		}
		return false
	}
	return true
}

// switchTab shows tab i. The hidden tab's views close their data streams and keep their
// parsers and offsets; the shown tab's views reopen from theirs (gff-client.md 11.3).
func (u *workspaceUI) switchTab(i int) {
	c := u.c
	if i == c.cur {
		return
	}
	for _, v := range c.tab().views {
		c.closeStream(v)
	}
	c.last = c.tab()
	c.cur = i
	t := c.tab()
	t.activity, t.bell = false, time.Time{}
	for _, v := range t.views {
		c.openStream(v)
		v.sentCols, v.sentRows = 0, 0
	}
	if u.mode == modeBoard {
		u.mode = modePanes
	}
	c.layoutChanged()
	u.dirty = true
}

// renameTab asks for the current tab's new name. An empty name gives the tab a number again.
func (u *workspaceUI) renameTab() {
	t := u.c.tab()
	u.ask(&prompt{label: "rename tab " + t.name, text: []rune(t.name), done: func(name, _ string) {
		name = strings.Join(strings.Fields(name), " ")
		if name == "" {
			t.name = ""
			name = u.c.freeTabName(slicesIndexTab(u.c.tabs, t) + 1)
		}
		t.name = name
	}})
}

// closeTab closes the current tab: its views close and their programs keep running, in the
// sidebar. The tab shown before it shows, else the one on its left. Closing the only tab leaves
// an empty tab.
func (u *workspaceUI) closeTab() {
	c := u.c
	t := c.tab()
	for _, v := range append([]*view(nil), t.views...) {
		c.removeView(t, v)
	}
	if len(c.tabs) == 1 {
		c.tabs[0] = &tab{name: "1", split: c.cfg.Split}
		c.last = nil
		u.mode = modeSidebar
		c.layoutChanged()
		u.dirty = true
		return
	}
	i := c.cur
	c.tabs = append(c.tabs[:i], c.tabs[i+1:]...)
	next := max(i-1, 0)
	for j, x := range c.tabs {
		if x == c.last {
			next = j
		}
	}
	c.cur = next
	c.last = nil
	nt := c.tab()
	nt.activity, nt.bell = false, time.Time{}
	for _, v := range nt.views {
		c.openStream(v)
		v.sentCols, v.sentRows = 0, 0
	}
	c.layoutChanged()
	u.dirty = true
}

// open shows a row's session: it focuses a view that shows it already, else adds one to the
// current tab, or to a new tab (gff-client.md 4.1).
func (u *workspaceUI) open(r board.Row, newTab bool) {
	c := u.c
	u.pendRun = pendingRun{} // the owner opened a session: a Run now's run no longer jumps into view
	switch {
	case r.Offline():
		u.msg = r.Host + " is offline"
		return
	case u.capcomService(r):
		// The Capcom service has no pane: its thread shows as text (capcom-service.md 7.7).
		u.openThread(r)
		return
	case (r.Pane == "" || r.State == api.SessionStateExited) && r.Session == "":
		u.msg = r.Name + " has exited"
		return
	case r.Pane == "" || r.State == api.SessionStateExited:
		u.resume(r, newTab)
		return
	}
	if !newTab {
		for i, t := range c.tabs {
			for j, v := range t.views {
				if v.h.label == r.Host && v.spec.ID == r.Pane {
					u.switchTab(i)
					t.focus = j
					u.mode = modePanes
					c.layoutChanged()
					u.dirty = true
					return
				}
			}
		}
	}
	hs := c.hosts[r.Host]
	if hs == nil {
		return
	}
	if newTab {
		c.tabs = append(c.tabs, &tab{name: c.freeTabName(len(c.tabs) + 1), split: c.cfg.Split})
		u.switchTab(len(c.tabs) - 1)
	}
	t := c.tab()
	c.addView(t, PaneSpec{ID: r.Pane, Label: r.Name, Host: r.Host, Session: r.Session}, hs)
	t.focus = len(t.views) - 1
	t.zoom = false
	u.mode = modePanes
	c.layoutChanged()
	u.dirty = true
}

// resumable reports whether a view shows a session that has exited and can be resumed: its
// host is online and the hab keeps its record (gff-client.md 6.2).
func (u *workspaceUI) resumable(v *view) bool {
	if v.spec.Session == "" || v.spec.Observe {
		return false
	}
	r, ok := rowOf(u.rows, v)
	return ok && !r.Offline() && (r.State == api.SessionStateExited || r.Pane == "")
}

// exitedWord is the frame note of a resumable view: "exited" and the status, from the view's
// last frame, else from the hab's record.
func (u *workspaceUI) exitedWord(v *view) string {
	if v.exited != nil {
		return "exited" + exitText(v.exited)
	}
	if r, ok := rowOf(u.rows, v); ok && r.Exit != nil {
		return "exited" + exitText(&api.ExitedFrame{Code: r.Exit.Code, Signal: r.Exit.Signal})
	}
	return "exited"
}

// resumeView resumes the session a focused view shows, on Enter.
func (u *workspaceUI) resumeView(v *view) {
	if r, ok := rowOf(u.rows, v); ok {
		u.resume(r, false)
	}
}

// resume asks the session's hab to start it again (session.resume): the harness with its
// resume id, or the command it was started with. Nothing resumes on its own; the owner asks.
// Every view of the session follows it to the new pane; with none, the new pane opens as Open
// would.
func (u *workspaceUI) resume(r board.Row, newTab bool) {
	c := u.c
	hs := c.hosts[r.Host]
	if hs == nil {
		return
	}
	k := r.Host + "/" + r.Session
	if u.resuming[k] {
		u.msg = "resuming " + r.Name + " on " + r.Host
		return
	}
	u.resuming[k] = true
	u.msg = "resuming " + r.Name + " on " + r.Host
	conn := hs.conn
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		defer cancel()
		res, err := conn.API().SessionResume(ctx, api.SessionRef{Session: r.Session})
		c.run(func() {
			delete(u.resuming, k)
			if err != nil {
				u.msg = "resume " + r.Name + " on " + r.Host + ": " + callErr(err)
				return
			}
			if u.msg == "resuming "+r.Name+" on "+r.Host {
				u.msg = ""
			}
			r.Pane, r.State, r.Exit = res.Pane, api.SessionStateRunning, nil
			shown := u.repointSession(r.Host, r.Session, res.Pane)
			switch {
			case u.mode == modePrompt && !shown:
				// A prompt has the keys: the view goes in the current tab, the keys stay.
				if hs := c.hosts[r.Host]; hs != nil {
					c.addView(c.tab(), PaneSpec{ID: res.Pane, Label: r.Name, Host: r.Host, Session: r.Session}, hs)
					c.layoutChanged()
				}
			case u.mode != modePrompt:
				u.open(r, newTab && !shown)
			}
			u.dirty = true
		})
	}()
}

// repointSession moves every view of a session on host to pane, and reports whether any view
// shows the session.
func (u *workspaceUI) repointSession(host, session, pane string) bool {
	c := u.c
	shown := false
	for _, t := range c.tabs {
		for _, v := range t.views {
			if v.h.label != host || v.spec.Session != session {
				continue
			}
			shown = true
			if v.spec.ID != pane {
				c.repoint(t, v, pane)
			}
		}
	}
	return shown
}

// follow moves each view of a session to the session's current pane, when it runs in another
// one: a resume from another client, or one whose answer this client missed.
func (u *workspaceUI) follow() {
	type move struct{ host, session, pane string }
	var moves []move
	u.c.eachView(func(_ *tab, v *view) {
		if v.spec.Session == "" {
			return
		}
		if r, ok := rowOf(u.rows, v); ok && r.Pane != "" && r.Pane != v.spec.ID && r.State != api.SessionStateExited {
			moves = append(moves, move{v.h.label, v.spec.Session, r.Pane})
		}
	})
	for _, m := range moves {
		u.repointSession(m.host, m.session, m.pane)
	}
}

// keys handles input while the sidebar, the board or a prompt has the keys. A bracketed paste
// is never keys: its text goes into a prompt or the filter, and elsewhere it is dropped.
func (u *workspaceUI) keys(p []byte) {
	for len(p) > 0 {
		if u.pasting {
			text := p
			p = nil
			if j := bytes.Index(text, pasteEnd); j >= 0 {
				text, p = text[:j], text[j+len(pasteEnd):]
				u.pasting = false
			}
			u.paste(text)
			continue
		}
		if bytes.HasPrefix(p, pasteStart) {
			u.pasting = true
			p = p[len(pasteStart):]
			continue
		}
		k, n := readKey(p)
		p = p[n:]
		u.key(k)
	}
}

// paste adds pasted text, its printable characters only, to the prompt or the filter.
func (u *workspaceUI) paste(text []byte) {
	for _, r := range string(text) {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			continue
		}
		switch {
		case u.mode == modePrompt && !u.prompt.yesNo:
			u.prompt.text = append(u.prompt.text, r)
		case u.filterOn:
			u.filter += string(r)
		}
	}
}

// key names one key of host input.
type key struct {
	name string // up, down, left, right, enter, esc, tab, backspace, or "" for a rune
	r    rune
}

// readKey reads one key from p: an arrow, Enter, Esc, Tab, Backspace or a character. A lone
// Esc at the end of a read is Esc.
func readKey(p []byte) (key, int) {
	switch p[0] {
	case '\r', '\n':
		return key{name: "enter"}, 1
	case '\t':
		return key{name: "tab"}, 1
	case 0x7f, 0x08:
		return key{name: "backspace"}, 1
	case 0x03:
		return key{name: "esc"}, 1
	case 0x1b:
		if len(p) == 1 || (p[1] != '[' && p[1] != 'O') {
			return key{name: "esc"}, 1
		}
		for i := 2; i < len(p); i++ {
			if c := p[i]; c >= 0x40 && c <= 0x7e {
				switch c {
				case 'A':
					return key{name: "up"}, i + 1
				case 'B':
					return key{name: "down"}, i + 1
				case 'C':
					return key{name: "right"}, i + 1
				case 'D':
					return key{name: "left"}, i + 1
				}
				return key{name: "other"}, i + 1
			}
		}
		return key{name: "other"}, len(p)
	}
	r, n := utf8.DecodeRune(p)
	if r < 0x20 {
		return key{name: "other"}, n
	}
	return key{r: r}, n
}

func (u *workspaceUI) key(k key) {
	if u.mode == modePrompt {
		u.promptKey(k)
		return
	}
	u.msg = ""
	if u.mode == modeThread && u.thread != nil {
		u.threadKey(k)
		return
	}
	if u.filterOn {
		switch k.name {
		case "enter", "esc":
			u.filterOn = false
			if k.name == "esc" {
				u.filter = ""
			}
		case "backspace":
			if r := []rune(u.filter); len(r) > 0 {
				u.filter = string(r[:len(r)-1])
			}
		case "":
			u.filter += string(k.r)
		}
		return
	}
	name := k.name
	if name == "" {
		name = string(k.r)
	}
	m := ModeSidebar
	if u.mode == modeBoard {
		m = ModeBoard
	}
	act := u.c.cfg.Keymap.lookup(m, name)
	if u.mode == modeBoard && act != actUp && act != actDown && act != actBack && u.modKey(k, act) {
		return
	}
	if u.mode == modeBoard && u.onMod && (act == actRename || act == actClose) {
		return // the selection is on a module target, which has no name or pane
	}
	switch act {
	case actUp:
		u.move(-1)
	case actDown:
		u.move(1)
	case actBack:
		u.mode = modePanes
		u.c.layoutChanged()
	case actOpen, actOpenTab:
		newTab := act == actOpenTab
		if rr, ok := u.selectedRound(); ok {
			u.openRound(rr, newTab)
		} else if r, _, ok := u.selected(); ok {
			u.open(r, newTab)
		}
	case actRunNow:
		u.runNow()
	case actPause:
		u.pauseRound()
	case actRunSessions:
		u.toggleRuns()
	case actNew:
		u.newSession()
	case actRename:
		u.rename()
	case actClose:
		u.closeSession()
	case actFilter:
		u.filterOn = true
	case actGroup:
		u.group = (u.group + 1) % 3
	}
}

func (u *workspaceUI) ask(p *prompt) {
	p.back = u.mode
	u.prompt = p
	u.mode = modePrompt
}

func (u *workspaceUI) promptKey(k key) {
	p := u.prompt
	end := func() {
		u.prompt = nil
		u.mode = p.back
	}
	if p.yesNo {
		end()
		if k.r == 'y' || k.r == 'Y' {
			p.done("y", p.host)
		}
		return
	}
	switch k.name {
	case "esc":
		end()
	case "enter":
		end()
		p.done(string(p.text), p.host)
	case "backspace":
		if len(p.text) > 0 {
			p.text = p.text[:len(p.text)-1]
		}
	case "tab":
		if p.hosts {
			p.host = u.nextHost(p.host)
		}
	case "":
		p.text = append(p.text, k.r)
	}
}

// nextHost is the online host after label, in the configured order.
func (u *workspaceUI) nextHost(label string) string {
	hs := u.c.cfg.Hosts.Hosts()
	var online []string
	for _, h := range hs {
		if h.Online() {
			online = append(online, h.Label)
		}
	}
	if len(online) == 0 {
		return label
	}
	for i, l := range online {
		if l == label {
			return online[(i+1)%len(online)]
		}
	}
	return online[0]
}

// newSession asks for a host and a project directory, then a command, and has that hab start
// it (gff-client.md 6.1). The new pane opens in the tab that was current when it started, at
// its view's size there.
func (u *workspaceUI) newSession() {
	host := ""
	if r, _, ok := u.selected(); ok && !r.Offline() {
		host = r.Host
	}
	if host == "" {
		host = u.nextHost("")
	}
	if host == "" {
		u.msg = "no host is online"
		return
	}
	u.ask(&prompt{label: "new session · Tab: host · directory (empty: none)", hosts: true, host: host,
		done: func(dir, host string) {
			u.ask(&prompt{label: "command on " + host + " (empty: shell)", host: host, done: func(cmd, host string) {
				u.create(host, strings.TrimSpace(dir), strings.TrimSpace(cmd))
			}})
		}})
}

func (u *workspaceUI) create(host, dir, cmd string) {
	p := api.SessionCreateParams{ProjectDir: dir}
	label := "shell"
	if cmd != "" {
		p.Argv = []string{"/bin/sh", "-c", cmd}
		if f := strings.Fields(cmd); len(f) > 0 {
			label = f[0]
		}
	}
	u.start(host, p, label, nil, placeEnd)
}

// newPane starts the hab's shell in a new pane beside the focused one (gff-client.md 5.4): on
// its host, in its folder (cwd_of), stacked onto it or split on one side. With no view in the
// tab, it starts on the selected row's host, else the first online one, in the hab's default
// folder.
func (u *workspaceUI) newPane(how place) {
	c := u.c
	p := api.SessionCreateParams{}
	host := ""
	anchor := c.focused()
	if anchor != nil && anchor.h != nil {
		host = anchor.h.label
		p.CwdOf = anchor.spec.ID
	} else {
		if r, _, ok := u.selected(); ok && !r.Offline() {
			host = r.Host
		}
		if host == "" {
			host = u.nextHost("")
		}
	}
	if host == "" {
		u.msg = "no host is online"
		return
	}
	u.start(host, p, "shell", anchor, how)
}

// start has host start p in a new pane and shows it in the current tab against anchor, as how
// says. The pane starts at its view's size in that tab, and its view goes there when the call
// returns, even if another tab shows by then. The keys move to it only from that tab's sidebar,
// or from its panes when the anchor still has focus; a prompt or the board keep them.
func (u *workspaceUI) start(host string, p api.SessionCreateParams, label string, anchor *view, how place) {
	u.startIn(u.c.tab(), host, p, label, anchor, how)
}

// startIn is start into tab t.
func (u *workspaceUI) startIn(t *tab, host string, p api.SessionCreateParams, label string, anchor *view, how place) {
	c := u.c
	// The same source as the tab bar and the host prompt: the host set's model. The client's
	// own hostState can trail it by one queued update.
	hm, ok := c.cfg.Hosts.Host(host)
	if !ok || !hm.Online() {
		u.msg = host + " is offline"
		return
	}
	hs := c.host(hm)
	r := c.paneArea()
	p.Cols, p.Rows = t.sizeIfPut(anchor, how, r.w, r.h)
	u.msg = "starting on " + host
	conn := hs.conn
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		defer cancel()
		res, err := conn.API().SessionCreate(ctx, p)
		c.run(func() {
			if err != nil {
				u.msg = "start on " + host + ": " + callErr(err)
				return
			}
			u.msg = ""
			found := false
			for _, open := range c.tabs {
				if open == t {
					found = true
					break
				}
			}
			if !found {
				u.msg = label + " on " + host + " started after its tab closed"
				return
			}
			hs := c.hosts[host]
			if hs == nil {
				return
			}
			at := anchor
			if at != nil && t.leafOf(at) == nil {
				at = nil // the anchor's view closed meanwhile: the end of the tab
			}
			focusedAnchor := at != nil && t.focusedView() == at
			v := c.addViewAt(t, PaneSpec{ID: res.Pane, Label: label, Host: host, Session: res.Session}, hs, at, how)
			switch {
			case t == c.tab() && (u.mode == modeSidebar && how == placeEnd || u.mode == modePanes && focusedAnchor):
				t.focus = slicesIndex(t.views, v)
				t.zoom = false
				u.mode = modePanes
			case t != c.tab():
				u.msg = label + " on " + host + " is in tab " + t.name
			}
			c.layoutChanged()
			u.dirty = true
		})
	}()
}

func slicesIndexTab(ts []*tab, t *tab) int {
	for i, x := range ts {
		if x == t {
			return i
		}
	}
	return 0
}

func slicesIndex(vs []*view, v *view) int {
	for i, x := range vs {
		if x == v {
			return i
		}
	}
	return 0
}

func (u *workspaceUI) rename() {
	r, _, ok := u.selected()
	switch {
	case !ok:
		return
	case r.Session == "":
		u.msg = "an unfiled pane has no session to rename"
		return
	case r.Offline():
		u.msg = r.Host + " is offline"
		return
	}
	u.ask(&prompt{label: "rename " + r.Name + " on " + r.Host, text: []rune(r.Name), host: r.Host,
		done: func(name, _ string) {
			name = strings.TrimSpace(name)
			if name == "" || name == r.Name {
				return
			}
			u.call(r.Host, "rename "+r.Name, func(ctx context.Context, a api.Client) error {
				_, err := a.SessionRename(ctx, api.SessionRenameParams{Session: r.Session, Name: name})
				return err
			})
		}})
}

// closeSession ends a running session's program, after a yes, or removes an exited session's
// record (gff-client.md 5.2: ending a program confirms first).
func (u *workspaceUI) closeSession() {
	r, _, ok := u.selected()
	if !ok {
		return
	}
	if r.Offline() {
		u.msg = r.Host + " is offline"
		return
	}
	if r.Pane != "" && r.State != api.SessionStateExited {
		u.ask(&prompt{label: "end " + r.Name + " on " + r.Host + "? the program gets a hang-up (y/n)", yesNo: true,
			done: func(string, string) {
				// The program may have ended while the question was up.
				if now, ok := u.row(r.Key); !ok || now.State == api.SessionStateExited {
					u.msg = r.Name + " has ended"
					return
				}
				u.call(r.Host, "end "+r.Name, func(ctx context.Context, a api.Client) error {
					_, err := a.PaneClose(ctx, api.PaneRef{Pane: r.Pane})
					return err
				})
			}})
		return
	}
	if r.Session == "" {
		return
	}
	u.ask(&prompt{label: "remove " + r.Name + " from " + r.Host + "? (y/n)", yesNo: true,
		done: func(string, string) {
			u.call(r.Host, "remove "+r.Name, func(ctx context.Context, a api.Client) error {
				_, err := a.SessionClose(ctx, api.SessionRef{Session: r.Session})
				return err
			})
		}})
}

// row returns the current row of key k.
func (u *workspaceUI) row(k board.Key) (board.Row, bool) {
	u.dirty = true
	u.refresh()
	for _, r := range u.rows {
		if r.Key == k {
			return r, true
		}
	}
	return board.Row{}, false
}

// call makes one call to a host off the loop and shows a failure on the bottom bar. The
// result itself arrives as events.
func (u *workspaceUI) call(host, what string, f func(context.Context, api.Client) error) {
	c := u.c
	hs := c.hosts[host]
	if hs == nil {
		return
	}
	conn := hs.conn
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		defer cancel()
		if err := f(ctx, conn.API()); err != nil {
			c.run(func() { u.msg = what + ": " + callErr(err) })
		}
	}()
}

func callErr(err error) string {
	if errors.Is(err, habconn.ErrOffline) {
		return "offline"
	}
	return err.Error()
}

// agentWord is the agent state a view's frame shows.
func (u *workspaceUI) agentWord(v *view) string {
	r, ok := rowOf(u.rows, v)
	if !ok || r.Agent == nil {
		return ""
	}
	return r.StateWord()
}

// Styles of the bars, from the host's own palette.
var (
	barStyle   = vt.Style{Inverse: true}
	activeTab  = vt.Style{Bold: true}
	dimStyle   = vt.Style{Faint: true}
	boldStyle  = vt.Style{Bold: true}
	selStyle   = vt.Style{Inverse: true}
	rankStyles = map[board.Rank]vt.Style{
		board.RankBlocked: {Bold: true, Fg: palette(1)},
		board.RankError:   {Fg: palette(1)},
		board.RankDone:    {Fg: palette(2)},
		board.RankWorking: {Fg: palette(3)},
		board.RankIdle:    {},
		board.RankOther:   {Faint: true},
	}
	rankMarks = map[board.Rank]string{
		board.RankBlocked: "!", board.RankError: "x", board.RankDone: "*",
		board.RankWorking: "~", board.RankIdle: "-", board.RankOther: " ",
	}
)

// exitedMark is the state mark of an exited session.
const exitedMark = "_"

// stateMark is a row's state mark: its rank's, or the exited mark.
func stateMark(r board.Row) string {
	if r.State == api.SessionStateExited {
		return exitedMark
	}
	return rankMarks[r.Rank]
}

func palette(i uint8) vt.Color { return vt.Color{Kind: vt.ColorPalette, Index: i} }

// inView says where a row's pane shows: 2 in the current tab, 1 in another tab, 0 nowhere.
func (u *workspaceUI) inView(r board.Row) int {
	best := 0
	for i, t := range u.c.tabs {
		for _, v := range t.views {
			if v.h.label == r.Host && v.spec.ID == r.Pane && r.Pane != "" {
				if i == u.c.cur {
					return 2
				}
				best = 1
			}
		}
	}
	return best
}

func (u *workspaceUI) drawSidebar(a rect) {
	c := u.c
	y := a.y
	line := func(s string, st vt.Style) {
		if y < a.y+a.h {
			c.canvas.text(a.x, y, a.w, s, st)
			y++
		}
	}
	head := " sessions · " + [...]string{"by need", "by project", "by host"}[u.group]
	line(head, boldStyle)
	if u.filterOn || u.filter != "" {
		line(" /"+u.filter, vt.Style{})
	}
	if !u.showRuns {
		line(" "+roundBadge+" run sessions hidden", dimStyle)
	}
	hs := c.cfg.Hosts.Hosts()
	var problems []string
	for _, h := range hs {
		if p := h.Problem(); p != "" {
			s := " ○ " + h.Label + " " + p
			if !h.Since.IsZero() && p == "offline" {
				s += " " + ago(c.cfg.Now(), h.Since)
			}
			problems = append(problems, s)
		}
	}
	end := a.y + a.h - len(problems)
	sel, _, _ := u.selected()
	groups := u.visible()
	if len(u.rows) == 0 {
		line(" no sessions · n: new", dimStyle)
	}
	for _, g := range groups {
		if y >= end {
			break
		}
		if g.Title != "" && u.group != board.GroupNone {
			line(" "+g.Title, boldStyle)
		}
		for _, r := range g.Rows {
			if y >= end {
				break
			}
			mark := [...]string{" ", "·", "▸"}[u.inView(r)]
			name := middle(r.Name, a.w-4-min(len(r.Host), 8))
			if r.RunSession() {
				name = roundBadge + middle(r.Name, a.w-5-min(len(r.Host), 8))
			}
			text := mark + stateMark(r) + " " + name
			host := r.Host
			if len([]rune(host)) > 8 {
				host = string([]rune(host)[:8])
			}
			pad := a.w - len([]rune(text)) - len([]rune(host)) - 1
			text += strings.Repeat(" ", max(pad, 1)) + host
			st := rankStyles[r.Rank]
			if r.Offline() {
				st = dimStyle
			}
			if r.Key == sel.Key && (u.mode == modeSidebar || u.mode == modePrompt) {
				st = selStyle
			}
			line(text, st)
		}
	}
	y = max(y, end)
	for _, p := range problems {
		line(p, dimStyle)
	}
}

func (u *workspaceUI) drawBoard(a rect) {
	c := u.c
	if a.h <= 0 {
		return
	}
	head := " status board · Enter opens · Esc returns"
	if !u.showRuns {
		head += " · " + roundBadge + " run sessions hidden"
	}
	c.canvas.text(a.x, a.y, a.w, head, boldStyle)
	// The rest scrolls under the head so the selected row stays on screen: an action on the
	// board always acts on a row the owner can see.
	type boardLine struct {
		s     string
		st    vt.Style
		spans []modview.Span // a module line: drawn span by span
	}
	var lines []boardLine
	selLine := -1
	line := func(s string, st vt.Style) {
		if st == selStyle {
			selLine = len(lines)
		}
		lines = append(lines, boardLine{s: s, st: st})
	}
	spans := func(ss []modview.Span, sel bool) {
		if sel {
			selLine = len(lines)
		}
		lines = append(lines, boardLine{spans: ss})
	}
	defer func() {
		avail := a.h - 1
		if selLine >= 0 {
			if selLine < u.boardTop {
				u.boardTop = selLine
			}
			if selLine >= u.boardTop+avail {
				u.boardTop = selLine - avail + 1
			}
		}
		u.boardTop = max(0, min(u.boardTop, len(lines)-avail))
		for i := 0; i < avail && u.boardTop+i < len(lines); i++ {
			l := lines[u.boardTop+i]
			if l.spans == nil {
				c.canvas.text(a.x, a.y+1+i, a.w, l.s, l.st)
				continue
			}
			x := a.x
			for _, s := range l.spans {
				n := min(len([]rune(s.Text)), a.x+a.w-x)
				c.canvas.text(x, a.y+1+i, n, s.Text, modStyle(s.Style))
				x += n
			}
			c.canvas.text(x, a.y+1+i, a.x+a.w-x, "", vt.Style{})
		}
	}()
	u.fixBoardSel()
	sel, _, _ := u.selected()
	section := ""
	now := c.cfg.Now()
	for _, g := range u.visible() {
		for _, r := range g.Rows {
			if s := r.Rank.Section(); s != section {
				section = s
				line(" "+s, boldStyle)
			}
			proj := r.Project.Name
			harness := ""
			if r.Agent != nil {
				harness = r.Agent.Harness
			}
			when := ""
			if !r.At.IsZero() {
				when = ago(now, r.At)
			}
			if r.Offline() {
				when = "offline " + ago(now, r.HostSince)
			}
			name, ctx := middle(r.Name, 20), r.Context()
			if r.RunSession() {
				name = roundBadge + middle(r.Name, 19)
				if ctx == "" {
					ctx = "round " + r.RoundName
				}
			}
			text := fmt.Sprintf("%s %-20s %-14s %-10s %-9s %-18s %-10s %s", stateMark(r), name,
				middle(proj, 14), middle(r.Host, 10), middle(harness, 9), middle(r.StateWord(), 18), when, ctx)
			st := rankStyles[r.Rank]
			if r.Offline() {
				st = dimStyle
			}
			if r.Key == sel.Key && !u.onRound && !u.onMod {
				st = selStyle
			}
			line(text, st)
		}
	}
	if len(u.rows) == 0 {
		line(" no sessions on any online host", dimStyle)
	}
	if rounds := u.visibleRounds(); len(rounds) > 0 {
		line(" Rounds", boldStyle)
		for _, r := range rounds {
			st := vt.Style{}
			if r.Stale || r.Paused || r.Completed {
				st = dimStyle
			}
			if u.onRound && r.RoundKey == u.selRound {
				st = selStyle
			}
			line(roundText(now, r), st)
		}
	}
	if hl := u.heavyLines(); len(hl) > 0 {
		line(" Heavy runs", boldStyle)
		for _, l := range hl {
			line(l, vt.Style{})
		}
	}
	if gl := u.governorLines(now); len(gl) > 0 {
		line(" Usage pacing", boldStyle)
		for _, l := range gl {
			line(l, vt.Style{})
		}
	}
	u.drawCards(a.w, line, spans)
}

// middle shortens s to n columns, cutting the middle so the end stays readable (4.1).
func middle(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(r) <= n {
		return s
	}
	if n < 3 {
		return string(r[:n])
	}
	head := (n - 1) / 2
	tail := n - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// ago is a short duration: 5s, 3m, 2h, 4d.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return ""
	case d < time.Minute:
		return strconv.Itoa(max(int(d/time.Second), 0)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}
