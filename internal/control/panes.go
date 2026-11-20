package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// PaneCaller says who makes a pane.read or pane.wait call on the control connection. Peer is
// true for a peer link (client kind hab): an agent on another hab, under agent.read_panes. Any
// other client is the owner. Conn names the connection, which the wait bound counts by.
type PaneCaller struct {
	Peer bool
	Conn string
}

// Panes serves pane.read and pane.wait (spec 3.11), and pane.list, pane.type and pane.new
// (spec 3.12).
type Panes interface {
	Read(ctx context.Context, from PaneCaller, p api.PaneReadParams) (api.PaneText, error)
	Wait(ctx context.Context, from PaneCaller, p api.PaneWaitParams) (api.PaneWaitResult, error)
	List(ctx context.Context, from PaneCaller, p api.PaneListParams) (api.PaneList, error)
	Type(ctx context.Context, from PaneCaller, p api.PaneTypeParams) (api.PaneTyped, error)
	New(ctx context.Context, from PaneCaller, p api.PaneNewParams) (api.PaneStarted, error)
}

// IsPaneMethod says whether method is served by Panes.
func IsPaneMethod(method string) bool {
	switch method {
	case "pane.read", "pane.wait", "pane.list", "pane.type", "pane.new":
		return true
	}
	return false
}

// maxPaneReadTime bounds one pane.read: a peer's answer at most. A pane.wait has its own time
// (at most 10 minutes) and runs until it ends or the connection closes.
const (
	maxPaneReadTime = 30 * time.Second
	maxPaneWaitTime = 11 * time.Minute
)

// handlePanes runs a pane.read or pane.wait call on its own goroutine: a wait lasts minutes, and
// the read loop goes on meanwhile.
func (c *conn) handlePanes(m message) {
	seq := c.currentSeq()
	c.mu.Lock()
	from := PaneCaller{Peer: c.kind == api.ClientKindHab, Conn: c.clientID}
	c.mu.Unlock()
	ps := c.s.cfg.Panes
	var call func(ctx context.Context) (any, error)
	var e *Error
	limit := maxPaneReadTime
	switch m.Method {
	case "pane.read":
		var p api.PaneReadParams
		if e = c.decodeUp(m, &p); e == nil && ps != nil {
			call = func(ctx context.Context) (any, error) {
				r, err := ps.Read(ctx, from, p)
				return r, err
			}
		}
	case "pane.wait":
		limit = maxPaneWaitTime
		var p api.PaneWaitParams
		if e = c.decodeUp(m, &p); e == nil && ps != nil {
			call = func(ctx context.Context) (any, error) {
				r, err := ps.Wait(ctx, from, p)
				return r, err
			}
		}
	case "pane.list":
		var p api.PaneListParams
		if e = c.decodeUp(m, &p); e == nil && ps != nil {
			call = func(ctx context.Context) (any, error) {
				r, err := ps.List(ctx, from, p)
				return r, err
			}
		}
	case "pane.type":
		var p api.PaneTypeParams
		if e = c.decodeUp(m, &p); e == nil && ps != nil {
			call = func(ctx context.Context) (any, error) {
				r, err := ps.Type(ctx, from, p)
				return r, err
			}
		}
	case "pane.new":
		limit = c.s.cfg.ActionTimeout
		var p api.PaneNewParams
		if e = c.decodeUp(m, &p); e == nil && ps != nil {
			call = func(ctx context.Context) (any, error) {
				r, err := ps.New(ctx, from, p)
				return r, err
			}
		}
	}
	if e == nil && call == nil {
		e = appErr(api.ErrorKindUnavailable, "%s needs gff-hab serve with its holder link; the pane-read and pane-drive capabilities are absent", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(reqCtx(c.ctx, m), limit)
		defer cancel()
		res, err := call(ctx)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		s := api.Seq{Seq: seq}
		switch r := res.(type) {
		case api.PaneText:
			r.Seq = s
			res = api.NonNil(r)
		case api.PaneWaitResult:
			r.Seq = s
			res = api.NonNil(r)
		case api.PaneList:
			r.Seq = s
			res = api.NonNil(r)
		case api.PaneTyped:
			r.Seq = s
			res = api.NonNil(r)
		case api.PaneStarted:
			r.Seq = s
			res = api.NonNil(r)
		}
		c.reply(m, seq, res, nil)
	}()
}
