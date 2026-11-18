package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// FlightRules serves a session's flight rules ledger (instructions-mcp-flight-rules.md 11).
// gff-hab implements it with internal/flightrules. An error that is an *Error keeps its kind.
type FlightRules interface {
	List(ctx context.Context, p api.FlightRulesParams) (api.FlightRules, error)
	Text(ctx context.Context, p api.FlightRuleTextParams) (api.FlightRuleText, error)
}

// handleFlightRules answers flightrules.list and flightrules.text on their own goroutine: the
// first list of a long transcript reads the whole file, and the read loop goes on meanwhile.
// They share the chat.page bound on running calls, since both read transcripts.
func (c *conn) handleFlightRules(m message) {
	seq := c.currentSeq()
	var session, id string
	switch m.Method {
	case "flightrules.list":
		var p api.FlightRulesParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		session = p.Session
	case "flightrules.text":
		var p api.FlightRuleTextParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		session, id = p.Session, p.ID
		if id == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "flightrules.text needs an id"))
			return
		}
	}
	fr := c.s.cfg.FlightRules
	switch {
	case session == "":
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "%s needs a session", m.Method))
		return
	case fr == nil:
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%s needs the flight rules reader, which this hab is running without; the flight-rules capability is absent", m.Method))
		return
	}
	c.mu.Lock()
	if c.chats >= maxRunningChat {
		c.mu.Unlock()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d transcript reads already run on this connection", maxRunningChat))
		return
	}
	c.chats++
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, c.s.cfg.ActionTimeout)
		defer cancel()
		var result any
		var err error
		if m.Method == "flightrules.list" {
			var r api.FlightRules
			r, err = fr.List(ctx, api.FlightRulesParams{Session: session})
			if r.Entries == nil {
				r.Entries = []api.FlightRule{}
			}
			r.Seq = api.Seq{Seq: seq}
			result = r
		} else {
			var r api.FlightRuleText
			r, err = fr.Text(ctx, api.FlightRuleTextParams{Session: session, ID: id})
			r.Seq = api.Seq{Seq: seq}
			result = r
		}
		c.mu.Lock()
		c.chats--
		c.mu.Unlock()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, result, nil)
	}()
}
