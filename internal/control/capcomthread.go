package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// CapcomThread serves the Capcom service's calls (capcom-service.md 7.4, 11). gff-hab
// implements it while [capcom] engine is set. An error that is an *Error keeps its kind.
type CapcomThread interface {
	ThreadPage(p api.CapcomThreadPageParams) (api.CapcomThreadPage, error)
	Interrupt() error
	ServiceStatus() (api.CapcomServiceStatus, error)
	Pause() (api.CapcomServiceStatus, error)
	Resume(ctx context.Context) (api.CapcomServiceStatus, error)
	// CardAnswer answers a thread card for the client device (6.4, 7.4).
	CardAnswer(ctx context.Context, client string, p api.CapcomCardAnswerParams) (api.CapcomCardAnswered, error)
	RemoveSite(site string) (api.CapcomServiceStatus, error)
	// DeviceContext takes the client device's latest context (8.1).
	DeviceContext(client string, p api.DeviceContextPutParams) error
}

// dispatchDeviceContext answers device.context.put (capcom-service.md 8.1). Device context goes
// only to the home hab: never over a peer link.
func (c *conn) dispatchDeviceContext(m message, s api.Seq) (any, *Error) {
	var p api.DeviceContextPutParams
	if e := c.decodeUp(m, &p); e != nil {
		return nil, e
	}
	ct := c.s.cfg.CapcomThread
	if ct == nil {
		return nil, appErr(api.ErrorKindUnavailable, hintNoCapcomThread, m.Method)
	}
	if c.peer != nil {
		return nil, appErr(api.ErrorKindInvalid, "device.context.put does not run over a peer link: device context stays on the Capcom home host")
	}
	c.mu.Lock()
	client := c.label
	c.mu.Unlock()
	if err := ct.DeviceContext(client, p); err != nil {
		return nil, actionErr(m.Method, err)
	}
	return api.Empty{Seq: s}, nil
}

const hintNoCapcomThread = "%s needs the Capcom service, which this hab is not running: set [capcom] engine on the home host; the capcom-thread capability is absent"

// handleCapcomCard answers capcom.card.answer off the read loop: an undo writes to Logbook,
// which may wait for SQLite's write lock (logbook.md 4.3).
func (c *conn) handleCapcomCard(m message) {
	seq := c.currentSeq()
	var p api.CapcomCardAnswerParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	ct := c.s.cfg.CapcomThread
	if ct == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, hintNoCapcomThread, m.Method))
		return
	}
	if c.peer != nil {
		// An undo writes to Logbook as a person: only on the hab the person talks to (logbook.md D3).
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "capcom.card.answer does not run over a peer link: answer on the Capcom home host"))
		return
	}
	if p.Item == "" || p.Choice == "" {
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "capcom.card.answer: item and choice are required"))
		return
	}
	c.mu.Lock()
	client := c.label
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxReadTime)
		defer cancel()
		res, err := ct.CardAnswer(ctx, client, p)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, api.NonNil(res), nil)
	}()
}

// dispatchCapcomThread answers capcom.thread.page, capcom.interrupt, capcom.status,
// capcom.pause and capcom.resume.
func (c *conn) dispatchCapcomThread(m message, s api.Seq) (any, *Error) {
	ct := c.s.cfg.CapcomThread
	if m.Method == "capcom.thread.page" {
		var p api.CapcomThreadPageParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if ct == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCapcomThread, m.Method)
		}
		if p.Limit < 0 {
			return nil, appErr(api.ErrorKindInvalid, "capcom.thread.page: limit %d is below 0", p.Limit)
		}
		res, err := ct.ThreadPage(p)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		res.Seq = s
		return api.NonNil(res), nil
	}
	if m.Method == "capcom.site.remove" {
		var p api.CapcomSiteParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if ct == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoCapcomThread, m.Method)
		}
		if p.Site == "" {
			return nil, appErr(api.ErrorKindInvalid, "capcom.site.remove: no site")
		}
		res, err := ct.RemoveSite(p.Site)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		res.Seq = s
		return api.NonNil(res), nil
	}
	var p api.CapcomServiceParams
	if e := c.decodeUp(m, &p); e != nil {
		return nil, e
	}
	if ct == nil {
		return nil, appErr(api.ErrorKindUnavailable, hintNoCapcomThread, m.Method)
	}
	var res api.CapcomServiceStatus
	var err error
	switch m.Method {
	case "capcom.interrupt":
		if err := ct.Interrupt(); err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.Empty{Seq: s}, nil
	case "capcom.status":
		res, err = ct.ServiceStatus()
	case "capcom.pause":
		res, err = ct.Pause()
	default:
		ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
		defer cancel()
		res, err = ct.Resume(ctx)
	}
	if err != nil {
		return nil, actionErr(m.Method, err)
	}
	res.Seq = s
	return api.NonNil(res), nil
}
