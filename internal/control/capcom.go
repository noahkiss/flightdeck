package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// Capcom serves capcom.open, capcom.attach, capcom.detach and capcom.assign (capcom.md 7, 8.3). gff-hab
// implements it. An error that is an *Error keeps its kind.
type Capcom interface {
	Open(ctx context.Context, p api.CapcomOpenParams) (api.CapcomOpened, error)
	Attach(ctx context.Context, p api.CapcomAttachParams) (api.CapcomAttached, error)
	Detach(p api.CapcomDetachParams) (api.CapcomAttached, error)
	Assign(ctx context.Context, p api.CapcomAssignParams) (api.CapcomAssigned, error)
}

// ContextSender is an Agents that may deliver a message with no text of the user's: a Capcom
// session's pending context alone (capcom.md 8.4). gff-hab's Capcom wrapper implements it, and
// any wrapper around it passes the answer through.
type ContextSender interface {
	// SendsContext reports whether a send to session with no text and no attachments may go
	// on: it still has something to deliver, or it is keyed (so a retry after the context
	// left gets the first answer from the idempotency table; with nothing waiting, the
	// sender refuses it there). False when no Capcom wrapper is underneath.
	SendsContext(session string, keyed bool) bool
}

// sendsContext reports whether an empty send to session may go on to the Agents service.
func (c *conn) sendsContext(session, key string) bool {
	cs, ok := c.s.cfg.Agents.(ContextSender)
	return ok && cs.SendsContext(session, key != "")
}

// checkContext refuses a context ref with no ref or an unknown kind.
func checkContext(method string, r api.CapcomContextRef) *Error {
	if !isWord("CapcomContextKind", string(r.Kind)) {
		return appErr(api.ErrorKindInvalid, "%s: kind %q is not one of %v", method, r.Kind, api.Enums["CapcomContextKind"])
	}
	if r.Ref == "" {
		return appErr(api.ErrorKindInvalid, "%s: %s context needs a ref", method, r.Kind)
	}
	return nil
}

// Resumer resumes an exited session (session.resume). Actions may implement it; without it,
// session.resume answers unavailable. An error that is an *Error keeps its kind.
type Resumer interface {
	ResumeSession(ctx context.Context, session string) (pane string, err error)
}

const hintNoCapcom = "%s needs the Capcom host, which this hab is running without; the capcom capability is absent"

// dispatchCapcom answers capcom.open, capcom.attach, capcom.detach and capcom.assign.
func (c *conn) dispatchCapcom(m message, s api.Seq) (any, *Error) {
	cc := c.s.cfg.Capcom
	ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
	defer cancel()
	if m.Method == "capcom.open" {
		var p api.CapcomOpenParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if cc == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCapcom, m.Method)
		}
		for _, r := range p.Context {
			if e := checkContext(m.Method, r); e != nil {
				return nil, e
			}
		}
		res, err := cc.Open(ctx, p)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		if res.Started && c.s.cfg.Size != nil {
			c.s.cfg.Size.Created(c.clientID, res.Pane) // the opener owns the new pane (spec 6.5)
		}
		res.Seq = s
		return res, nil
	}
	if m.Method == "capcom.detach" {
		var p api.CapcomDetachParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if cc == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCapcom, m.Method)
		}
		if p.Session == "" {
			return nil, appErr(api.ErrorKindInvalid, "capcom.detach needs a session")
		}
		if e := checkContext(m.Method, api.CapcomContextRef{Kind: p.Kind, Ref: p.Ref, Host: p.Host}); e != nil {
			return nil, e
		}
		res, err := cc.Detach(p)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		res.Seq = s
		return api.NonNil(res), nil
	}
	if m.Method == "capcom.attach" {
		var p api.CapcomAttachParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if cc == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCapcom, m.Method)
		}
		if p.Session == "" {
			return nil, appErr(api.ErrorKindInvalid, "capcom.attach needs a session")
		}
		if e := checkContext(m.Method, api.CapcomContextRef{Kind: p.Kind, Ref: p.Ref, Host: p.Host}); e != nil {
			return nil, e
		}
		res, err := cc.Attach(ctx, p)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		res.Seq = s
		return api.NonNil(res), nil
	}
	var p api.CapcomAssignParams
	if e := c.decodeUp(m, &p); e != nil {
		return nil, e
	}
	if cc == nil {
		return nil, appErr(api.ErrorKindUnavailable, hintNoCapcom, m.Method)
	}
	if p.Host == "" {
		return nil, appErr(api.ErrorKindInvalid, "capcom.assign needs a host: self, a peer label or a device id")
	}
	res, err := cc.Assign(ctx, p)
	if err != nil {
		return nil, actionErr(m.Method, err)
	}
	res.Seq = s
	return res, nil
}

// dispatchResume answers session.resume.
func (c *conn) dispatchResume(m message, s api.Seq) (any, *Error) {
	var p api.SessionRef
	if e := c.decodeUp(m, &p); e != nil {
		return nil, e
	}
	if p.Session == "" {
		return nil, appErr(api.ErrorKindInvalid, "session.resume needs a session")
	}
	r, ok := c.s.cfg.Actions.(Resumer)
	if !ok {
		return nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
	defer cancel()
	pane, err := r.ResumeSession(ctx, p.Session)
	if err != nil {
		return nil, actionErr(m.Method, err)
	}
	if c.s.cfg.Size != nil {
		c.s.cfg.Size.Created(c.clientID, pane)
	}
	return api.SessionCreated{Seq: s, Session: p.Session, Pane: pane}, nil
}
