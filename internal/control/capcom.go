package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// Capcom serves capcom.open and capcom.assign (capcom.md 7, 8.3). gff-hab implements it. An
// error that is an *Error keeps its kind.
type Capcom interface {
	Open(ctx context.Context, p api.CapcomOpenParams) (api.CapcomOpened, error)
	Assign(ctx context.Context, p api.CapcomAssignParams) (api.CapcomAssigned, error)
}

// Resumer resumes an exited session (session.resume). Actions may implement it; without it,
// session.resume answers unavailable. An error that is an *Error keeps its kind.
type Resumer interface {
	ResumeSession(ctx context.Context, session string) (pane string, err error)
}

const hintNoCapcom = "%s needs the Capcom host, which this hab is running without; the capcom capability is absent"

// dispatchCapcom answers capcom.open and capcom.assign.
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
