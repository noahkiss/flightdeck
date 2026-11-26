package control

import "github.com/noahkiss/flightdeck/api"

// Devices runs the hab's trust store (transport.md 5, 6.6). gff-hab implements it.
type Devices interface {
	List() ([]api.Device, error)
	Revoke(query string) error
}

// hintNoPair answers devices.pair: pairing over the API is not built (transport.md 6.1).
const hintNoPair = "%s is not built; pair a device with gff-hab pair on the host"

// handleDevices answers the devices.* methods. They manage who may log in to this hab, so only
// the owner's own clients call them: never a peer link (a peer cannot admit a key here,
// transport.md 6.2), and never the pane route, which does not list them.
func (c *conn) handleDevices(m message) {
	seq := c.currentSeq()
	ds := c.s.cfg.Devices
	c.mu.Lock()
	kind := c.kind
	c.mu.Unlock()
	var call func() (any, error)
	var e *Error
	switch m.Method {
	case "devices.list":
		var p api.DevicesListParams
		if e = c.decodeUp(m, &p); e == nil && ds != nil {
			call = func() (any, error) {
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
			call = func() (any, error) { return api.Empty{}, ds.Revoke(p.ID) }
		}
	case "devices.pair":
		e = appErr(api.ErrorKindUnavailable, hintNoPair, m.Method)
	default:
		c.reply(m, seq, nil, protoErr(codeMethodNotFound, "method %q does not exist", m.Method))
		return
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
	res, err := call()
	seq = c.currentSeq()
	if err != nil {
		c.reply(m, seq, nil, actionErr(m.Method, err))
		return
	}
	c.reply(m, seq, api.NonNil(withSeq(res, seq)), nil)
}
