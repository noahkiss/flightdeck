package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// LogbookSync serves Logbook sync to peer links (logbook.md 7): logbook.pull and logbook.push.
// gff-hab implements it with internal/lbsync. from is the link's hello peer field. An error that
// is an *Error keeps its kind.
type LogbookSync interface {
	Pull(ctx context.Context, from api.PeerHello, p api.LogbookPullParams) (api.LogbookPullResult, error)
	Push(ctx context.Context, from api.PeerHello, p api.LogbookPushParams) (api.LogbookPushResult, error)
}

// maxLogbookSyncTime bounds one page: a 4 MiB push folds in well under it.
const maxLogbookSyncTime = 60 * time.Second

// handleLogbookSync answers logbook.pull and logbook.push off the read loop: a page reads or
// writes the store.
func (c *conn) handleLogbookSync(m message) {
	seq := c.currentSeq()
	ls := c.s.cfg.LogbookSync
	var call func(ctx context.Context) (any, error)
	var e *Error
	switch {
	case c.peer == nil:
		e = appErr(api.ErrorKindInvalid, "%s needs a peer link: client.kind hab with the hello's peer field", m.Method)
	case m.Method == "logbook.pull":
		var p api.LogbookPullParams
		if e = c.decodeUp(m, &p); e == nil {
			from := *c.peer
			call = func(ctx context.Context) (any, error) { return ls.Pull(ctx, from, p) }
		}
	default:
		var p api.LogbookPushParams
		if e = c.decodeUp(m, &p); e == nil {
			from := *c.peer
			call = func(ctx context.Context) (any, error) { return ls.Push(ctx, from, p) }
		}
	}
	if e == nil && ls == nil {
		e = appErr(api.ErrorKindUnavailable, "%s needs Logbook, which this hab is running without; the logbook capability is absent", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxLogbookSyncTime)
		defer cancel()
		res, err := call(ctx)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		s := api.Seq{Seq: seq}
		switch r := res.(type) {
		case api.LogbookPullResult:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookPushResult:
			r.Seq = s
			res = api.NonNil(r)
		}
		c.reply(m, seq, res, nil)
	}()
}
