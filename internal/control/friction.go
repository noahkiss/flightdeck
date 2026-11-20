package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/friction"
)

// Friction stores friction notes (diagnostics.md 9). gff-hab implements it. Note gets a checked
// note: its text is cut of surrounding space and within friction.MaxText characters.
type Friction interface {
	Note(ctx context.Context, client api.ClientKind, p api.FrictionParams) (api.FrictionNoted, error)
}

// handleFriction answers diag.friction on its own goroutine: the note reads a tail of the merged
// log files. It never logs the text; a refused note's failed-request line names only the
// method and the error kind (4.6).
func (c *conn) handleFriction(m message) {
	seq := c.currentSeq()
	var p api.FrictionParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	fr := c.s.cfg.Friction
	if fr == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "diag.friction needs the hab's state folder; the friction capability is absent"))
		return
	}
	if c.peer != nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "diag.friction does not run over a peer link: send the note to the session's own hab"))
		return
	}
	text, err := friction.CheckText(p.Text)
	if err != nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "diag.friction: %v", err))
		return
	}
	if len(p.Session) > friction.MaxID || len(p.Pane) > friction.MaxID {
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "diag.friction: a session or pane id is longer than %d bytes", friction.MaxID))
		return
	}
	p.Text = text
	c.mu.Lock()
	kind := c.kind
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(reqCtx(c.ctx, m), c.s.cfg.ActionTimeout)
		defer cancel()
		res, err := fr.Note(ctx, kind, p)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, res, nil)
	}()
}
