package control

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Devices runs the hab's trust store and the pairing window that devices.pair opens
// (transport.md 5, 6.1.7). gff-hab implements it. Only the connection that opened a window
// answers its device.
type Devices interface {
	List() ([]api.Device, error)
	Revoke(query string) error
	// Pair opens a pairing window. by.Ctx ends when the calling connection does: the window
	// closes then, so a window never outlives the only connection that may answer it.
	Pair(by Caller, p api.DevicesPairParams) (api.PairingOffer, error)
	// Confirm waits for a device, or answers it. It returns when ctx ends, with the state then.
	Confirm(ctx context.Context, by Caller, p api.DevicesConfirmParams) (api.DevicePairing, error)
}

// Caller is the connection that sends a devices.* call.
type Caller struct {
	// Conn is the connection, unique in this process. A window answers only the connection that
	// opened it, not its client id: a client that reconnects with its reconnect token takes the
	// id while the old connection may still be draining (transport.md 6.1.7).
	Conn   uint64
	Client string          // the connection's client id, for the log
	Ctx    context.Context // ends when the connection does
}

// connIDs numbers the connections of this process, for Caller.Conn.
var connIDs atomic.Uint64

// maxConfirmWait bounds one devices.confirm: a window lives at most five minutes.
const maxConfirmWait = 6 * time.Minute

// handleDevices answers the devices.* methods. They manage who may log in to this hab, so only
// the owner's own clients call them: never a peer link (a peer cannot admit a key here,
// transport.md 6.2), and never the pane route, which does not list them. devices.confirm
// waits, so each call runs on its own goroutine.
func (c *conn) handleDevices(m message) {
	seq := c.currentSeq()
	ds := c.s.cfg.Devices
	by := c.caller()
	c.mu.Lock()
	kind := c.kind
	c.mu.Unlock()
	var call func(ctx context.Context) (any, error)
	var e *Error
	switch m.Method {
	case "devices.list":
		var p api.DevicesListParams
		if e = c.decodeUp(m, &p); e == nil && ds != nil {
			call = func(context.Context) (any, error) {
				devs, err := ds.List()
				return api.DeviceList{Devices: devs}, err
			}
		}
	case "devices.revoke":
		var p api.DevicesRevokeParams
		if e = c.decodeUp(m, &p); e == nil && ds != nil {
			if p.ID == "" {
				e = appErr(api.ErrorKindInvalid, "devices.revoke needs id: a device id or label")
				break
			}
			call = func(context.Context) (any, error) { return api.Empty{}, ds.Revoke(p.ID) }
		}
	case "devices.pair":
		var p api.DevicesPairParams
		if e = c.decodeUp(m, &p); e == nil && ds != nil {
			call = func(context.Context) (any, error) { return ds.Pair(by, p) }
		}
	case "devices.confirm":
		var p api.DevicesConfirmParams
		if e = c.decodeUp(m, &p); e == nil && ds != nil {
			if p.Code != "" && p.Decline {
				e = appErr(api.ErrorKindInvalid, "devices.confirm takes a code or decline, not both")
				break
			}
			call = func(ctx context.Context) (any, error) { return ds.Confirm(ctx, by, p) }
		}
	}
	if e == nil && kind == api.ClientKindHab {
		e = appErr(api.ErrorKindDenied, "%s is the owner's call; a peer link cannot send it", m.Method)
	}
	if e == nil && call == nil {
		e = appErr(api.ErrorKindUnavailable, "%s needs the hab's state folder; the devices capability is absent", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(reqCtx(c.ctx, m), maxConfirmWait)
		defer cancel()
		res, err := call(ctx)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, api.NonNil(withSeq(res, seq)), nil)
	}()
}

// caller is c as a devices.* caller.
func (c *conn) caller() Caller { return Caller{Conn: c.id, Client: c.clientID, Ctx: c.ctx} }
