package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Heavy is the host's heavy-run lock (groups.md 8). gff-hab implements it with internal/heavy.
// An error that is an *Error keeps its kind.
type Heavy interface {
	// Take asks for the lock or takes over a ticket for the connection whose context is owner,
	// and answers at once. The ticket goes when owner ends, unless another call took it over.
	Take(owner context.Context, p api.HeavyTakeParams) (api.HeavyTaken, error)
	// Wait waits until the ticket holds the lock or ctx ends, and answers its state then.
	Wait(ctx context.Context, ticket string) (api.HeavyTaken, error)
	Release(p api.HeavyReleaseParams) error
	Status() api.HeavyStatus
}

// handleHeavy answers the heavy.* methods. heavy.take runs off the read loop: its wait may take
// minutes. The connection's own context owns each ticket it takes, so the ticket goes when the
// connection drops.
func (c *conn) handleHeavy(m message) {
	seq := c.currentSeq()
	h := c.s.cfg.Heavy
	switch m.Method {
	case "heavy.take", "heavy.release", "heavy.status":
	default:
		result, e := c.dispatch(m, seq)
		c.reply(m, seq, result, e)
		return
	}
	if h == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%s needs the heavy-run lock, which this hab is running without; the heavy capability is absent", m.Method))
		return
	}
	switch m.Method {
	case "heavy.status":
		var p api.HeavyStatusParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		res := h.Status()
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, api.NonNil(res), nil)
	case "heavy.release":
		var p api.HeavyReleaseParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if err := h.Release(p); err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, api.Empty{Seq: api.Seq{Seq: seq}}, nil)
	case "heavy.take":
		var p api.HeavyTakeParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		go func() {
			res, err := h.Take(c.ctx, p)
			if err == nil && res.State != "holding" && p.WaitMS > 0 {
				ctx, cancel := context.WithTimeout(c.ctx, time.Duration(p.WaitMS)*time.Millisecond)
				res, err = h.Wait(ctx, res.Ticket)
				cancel()
			}
			seq := c.currentSeq()
			if err != nil {
				c.reply(m, seq, nil, actionErr(m.Method, err))
				return
			}
			res.Seq = api.Seq{Seq: seq}
			c.reply(m, seq, api.NonNil(res), nil)
		}()
	}
}
