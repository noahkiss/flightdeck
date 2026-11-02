package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Confirm serves the confirmations (capcom.md 6.1). gff-hab implements it with internal/confirm.
// An error that is an *Error keeps its kind.
type Confirm interface {
	Request(ctx context.Context, p api.ConfirmRequestParams) (api.Confirmation, error)
	Answer(ctx context.Context, client string, p api.ConfirmAnswerParams) (api.Confirmation, error)
	List(all bool) []api.Confirmation
	Get(id string) (api.Confirmation, error)
}

// maxConfirmTime bounds one confirm call: the longest wait, or a run, plus a margin.
const maxConfirmTime = 130 * time.Second

func isConfirmMethod(m string) bool {
	switch m {
	case "confirm.request", "confirm.answer", "confirm.list", "confirm.get":
		return true
	}
	return false
}

// handleConfirm answers the confirm methods off the read loop: a request waits for the user, and
// a confirm runs the start.
func (c *conn) handleConfirm(m message) {
	seq := c.currentSeq()
	cf := c.s.cfg.Confirm
	var call func(ctx context.Context) (api.Confirmation, error)
	var e *Error
	switch m.Method {
	case "confirm.request":
		var p api.ConfirmRequestParams
		if e = c.decodeUp(m, &p); e == nil && cf != nil {
			call = func(ctx context.Context) (api.Confirmation, error) { return cf.Request(ctx, p) }
		}
	case "confirm.answer":
		var p api.ConfirmAnswerParams
		if e = c.decodeUp(m, &p); e == nil && cf != nil {
			c.mu.Lock()
			kind := c.kind
			c.mu.Unlock()
			if kind == api.ClientKindHab {
				// A peer link carries an agent's calls from another hab, never the user's answer.
				e = appErr(api.ErrorKindInvalid, "confirm.answer is the user's answer; a peer link cannot send it")
			}
			call = func(ctx context.Context) (api.Confirmation, error) { return cf.Answer(ctx, c.clientID, p) }
		}
	case "confirm.list":
		var p api.ConfirmListParams
		if e = c.decodeUp(m, &p); e == nil && cf != nil {
			c.reply(m, seq, api.NonNil(api.ConfirmList{Seq: api.Seq{Seq: seq}, Confirmations: cf.List(p.All)}), nil)
			return
		}
	case "confirm.get":
		var p api.ConfirmGetParams
		if e = c.decodeUp(m, &p); e == nil && cf != nil {
			call = func(context.Context) (api.Confirmation, error) { return cf.Get(p.ID) }
		}
	}
	if e == nil && call == nil {
		e = appErr(api.ErrorKindUnavailable, "%s needs confirmations, which this hab is running without; the confirm capability is absent", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxConfirmTime)
		defer cancel()
		res, err := call(ctx)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		if m.Method == "confirm.request" {
			c.reply(m, seq, api.NonNil(api.ConfirmRequested{Seq: api.Seq{Seq: seq}, Confirmation: res}), nil)
			return
		}
		c.reply(m, seq, api.NonNil(api.ConfirmationResult{Seq: api.Seq{Seq: seq}, Confirmation: res}), nil)
	}()
}

// CreateSession runs session.create for the hab itself, as a confirmed start does (capcom.md
// 6.1): through the same idempotency table as a client's create, so a repeat with the same key
// starts nothing new. No client owns the new pane's size.
func (s *Server) CreateSession(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
	if e := checkKey("session.create", p.IdempotencyKey); e != nil {
		return api.SessionCreated{}, e
	}
	if s.cfg.Actions == nil {
		return api.SessionCreated{}, appErr(api.ErrorKindUnavailable, hintNoActions, "session.create")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ActionTimeout)
	defer cancel()
	res, e := s.createIdem(ctx, p, func(string) {})
	if e != nil {
		return api.SessionCreated{}, e
	}
	return res, nil
}

// createIdem runs a create under its idempotency key. created hears the new pane of a create
// that ran.
func (s *Server) createIdem(ctx context.Context, p api.SessionCreateParams, created func(pane string)) (api.SessionCreated, *Error) {
	key, bare := p.IdempotencyKey, p
	bare.IdempotencyKey, bare.Cols, bare.Rows = "", 0, 0 // a retry may come from a rotated screen
	res, e := s.idem(ctx, "session.create", key, bare, func() (any, *Error) {
		session, pane, err := s.cfg.Actions.CreateSession(ctx, p)
		if err != nil {
			return nil, actionErr("session.create", err)
		}
		created(pane)
		return api.SessionCreated{Session: session, Pane: pane}, nil
	})
	if e != nil {
		return api.SessionCreated{}, e
	}
	return res.(api.SessionCreated), nil
}
