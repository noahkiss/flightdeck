package control

import (
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// DebugMax is the longest debug window diag.debug opens (diagnostics.md 4.5): a window always
// ends by itself.
const DebugMax = 24 * time.Hour

// Debug turns gff-hab's debug lines on until a time, or off (diagnostics.md 4.5). gff-hab
// implements it. Set gets a checked time: zero, or within DebugMax of now.
type Debug interface {
	Set(until time.Time) (api.DebugState, error)
}

// handleDebug answers diag.debug. A time already past turns debug off; one more than DebugMax
// ahead is refused.
func (c *conn) handleDebug(m message) {
	seq := c.currentSeq()
	var p api.DebugParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	d := c.s.cfg.Debug
	if d == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "diag.debug needs the hab's state folder; the debug capability is absent"))
		return
	}
	if c.peer != nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "diag.debug does not run over a peer link: send it to that hab"))
		return
	}
	var until time.Time
	if p.Until != "" {
		t, err := time.Parse(time.RFC3339, p.Until)
		if err != nil {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "diag.debug: until is not an RFC 3339 time"))
			return
		}
		now := time.Now()
		if t.Sub(now) > DebugMax+time.Minute { // a minute of clock skew between client and hab
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "diag.debug: until is more than %s ahead", DebugMax))
			return
		}
		if t.After(now) {
			until = t
		}
	}
	res, err := d.Set(until)
	if err != nil {
		c.reply(m, seq, nil, actionErr(m.Method, err))
		return
	}
	res.Seq = api.Seq{Seq: seq}
	c.reply(m, seq, res, nil)
}
