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
}

const hintNoCapcomThread = "%s needs the Capcom service, which this hab is not running: set [capcom] engine on the home host; the capcom-thread capability is absent"

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
