package gffclient

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
)

// Reopen a closed view, and a shell in an exited view (gff-client.md 5.8).

// maxClosed is how many closed views gff keeps to reopen.
const maxClosed = 10

// closedView is a view the owner closed: its session, its tab and where it sat there.
type closedView struct {
	t    *tab
	spec PaneSpec
	at   spot
}

// closeView takes v out of t and keeps it to reopen; the program keeps running.
func (u *workspaceUI) closeView(t *tab, v *view) {
	c := u.c
	e := closedView{t: t, spec: v.spec, at: t.spotOf(v)}
	e.spec.Host = v.h.label
	if len(u.closed) == maxClosed {
		u.closed = append(u.closed[:0], u.closed[1:]...)
	}
	u.closed = append(u.closed, e)
	c.removeView(t, v)
}

// rowFor finds the hab's row of a closed view's session, or of its pane when it had none.
func (u *workspaceUI) rowFor(spec PaneSpec) (board.Row, bool) {
	for _, r := range u.rows {
		if r.Host != spec.Host {
			continue
		}
		if spec.Session != "" && r.Session == spec.Session || spec.Session == "" && r.Pane == spec.ID {
			return r, true
		}
	}
	return board.Row{}, false
}

// reopen shows the view closed last again, in its old place. An entry whose session the hab
// no longer lists, or whose host left, is dropped and the next one tried. One whose session a
// view shows already is dropped and that view takes the focus. An offline host keeps its entry.
func (u *workspaceUI) reopen() {
	c := u.c
	for len(u.closed) > 0 {
		e := u.closed[len(u.closed)-1]
		hm, known := c.cfg.Hosts.Host(e.spec.Host)
		if known && !hm.Online() {
			u.msg = e.spec.Host + " is offline"
			return
		}
		u.closed = u.closed[:len(u.closed)-1]
		r, listed := u.rowFor(e.spec)
		if !known || !listed {
			continue
		}
		spec := e.spec
		if r.Pane != "" {
			spec.ID = r.Pane // the session moved on: a resume started a new pane
		}
		for i, t := range c.tabs {
			for j, v := range t.views {
				if v.h.label == spec.Host && (spec.Session != "" && v.spec.Session == spec.Session || v.spec.ID == spec.ID) {
					u.switchTab(i)
					t.focus = j
					t.openPath(v)
					u.mode = modePanes
					c.layoutChanged()
					u.dirty = true
					return
				}
			}
		}
		at := e.at
		ti := slicesIndexTab(c.tabs, e.t)
		if ti < 0 {
			ti, at = c.cur, spot{how: placeEnd} // the tab closed: the end of the current one
		}
		u.switchTab(ti)
		t := c.tab()
		v := c.newView(t, spec, c.host(hm), func(v *view) { t.putBack(v, at) })
		t.focus = slicesIndex(t.views, v)
		t.openPath(v)
		t.zoom = false
		u.mode = modePanes
		c.layoutChanged()
		u.dirty = true
		return
	}
	u.msg = "nothing to reopen"
}

// exitedKey runs the exited table's action for the first key of p it binds, in a focused view
// whose session has exited (gff-client.md 5.8). Other keys go nowhere.
func (u *workspaceUI) exitedKey(v *view, p []byte) {
	km := u.c.keys.km
	for len(p) > 0 {
		name, _, n, ok := commandKey(p)
		if !ok || n <= 0 {
			return
		}
		p = p[n:]
		switch km.lookup(ModeExited, name) {
		case actResume:
			u.resumeView(v)
			return
		case actShell:
			u.shellView(v)
			return
		}
	}
}

// shellView starts the hab's shell in v's place, in the folder of the exited pane it shows
// (cwd_of). When the call answers, v shows the new session; the exited one stays in the
// sidebar.
func (u *workspaceUI) shellView(v *view) {
	c := u.c
	host := v.h.label
	if u.shelling[v] {
		u.msg = "starting a shell on " + host
		return
	}
	hm, ok := c.cfg.Hosts.Host(host)
	if !ok || !hm.Online() {
		u.msg = host + " is offline"
		return
	}
	p := api.SessionCreateParams{CwdOf: v.spec.ID}
	p.Cols, p.Rows = v.sentCols, v.sentRows
	if p.Cols <= 0 || p.Rows <= 0 {
		p.Cols, p.Rows = v.cols, v.rows
	}
	u.shelling[v] = true
	u.msg = "starting a shell on " + host
	conn := c.host(hm).conn
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		defer cancel()
		res, err := conn.API().SessionCreate(ctx, p)
		c.run(func() {
			delete(u.shelling, v)
			if err != nil {
				u.msg = "shell on " + host + ": " + callErr(err)
				return
			}
			if u.msg == "starting a shell on "+host {
				u.msg = ""
			}
			for _, t := range c.tabs {
				if t.leafOf(v) != nil {
					v.spec.Session, v.spec.Label = res.Session, "shell"
					c.repoint(t, v, res.Pane)
					u.dirty = true
					return
				}
			}
			u.msg = "shell on " + host + " started after its view closed"
		})
	}()
}
