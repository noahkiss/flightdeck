package control

import (
	"github.com/noahkiss/flightdeck/api"
)

// EventNoticeRaised is the event type of a notice raised through notice.raise. Its id is the
// notice id.
const EventNoticeRaised api.EventType = "notice.raised"

// PushCapability is in the welcome while Push.Enabled reports true, checked at each hello.
const PushCapability = "push"

// Push serves the push tokens, focus reports and notices (app.md 8, 17; hab-logic-api.md 3.9).
// gff-hab implements it with internal/push. An error that is an *Error keeps its kind.
type Push interface {
	// Enabled reports whether push is on: config loaded and validated, and the key loaded. It
	// may turn true after start. Without it the push.* methods answer unavailable and the
	// welcome lacks PushCapability; session.focus and notice.raise still work.
	Enabled() bool
	Register(client string, p api.PushRegisterParams) error
	RegisterActivity(client string, p api.PushActivityParams) error
	Unregister(client string, p api.PushUnregisterParams) error
	// Focus records a session.focus report from client.
	Focus(client string, p api.SessionFocusParams) error
	// Raise raises a notice. The result's seq is filled in by the caller.
	Raise(client string, p api.NoticeRaiseParams) (api.NoticeRaised, error)
}

const hintNoPush = "%s needs push, which is not set up on this hab; the push capability is absent (docs/push.md)"

// dispatchPush answers push.register, push.register_activity, push.unregister, session.focus
// and notice.raise.
func (c *conn) dispatchPush(m message, s api.Seq) (any, *Error) {
	ps := c.s.cfg.Push
	if ps == nil {
		return nil, appErr(api.ErrorKindUnavailable, "%s needs the push service, which this hab is running without", m.Method)
	}
	var err error
	switch m.Method {
	case "push.register":
		var p api.PushRegisterParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if !ps.Enabled() {
			return nil, appErr(api.ErrorKindUnavailable, hintNoPush, m.Method)
		}
		if p.DeviceID == "" || p.Token == "" {
			return nil, appErr(api.ErrorKindInvalid, "push.register needs a device_id and a token")
		}
		if !isWord("PushEnvironment", string(p.Environment)) {
			return nil, appErr(api.ErrorKindInvalid, "environment %q is not one of %v", p.Environment, api.Enums["PushEnvironment"])
		}
		for _, k := range p.Kinds {
			if !isWord("PushKind", string(k)) {
				return nil, appErr(api.ErrorKindInvalid, "kind %q is not one of %v", k, api.Enums["PushKind"])
			}
		}
		err = ps.Register(c.clientID, p)
	case "push.register_activity":
		var p api.PushActivityParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if !ps.Enabled() {
			return nil, appErr(api.ErrorKindUnavailable, hintNoPush, m.Method)
		}
		if p.DeviceID == "" || p.Session == "" || p.Token == "" {
			return nil, appErr(api.ErrorKindInvalid, "push.register_activity needs a device_id, a session and a token")
		}
		err = ps.RegisterActivity(c.clientID, p)
	case "push.unregister":
		var p api.PushUnregisterParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if !ps.Enabled() {
			return nil, appErr(api.ErrorKindUnavailable, hintNoPush, m.Method)
		}
		if p.DeviceID == "" || p.Token == "" {
			return nil, appErr(api.ErrorKindInvalid, "push.unregister needs a device_id and a token")
		}
		err = ps.Unregister(c.clientID, p)
	case "session.focus":
		var p api.SessionFocusParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if p.Session == "" {
			return nil, appErr(api.ErrorKindInvalid, "session.focus needs a session")
		}
		err = ps.Focus(c.clientID, p)
	case "notice.raise":
		var p api.NoticeRaiseParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if p.Title == "" {
			return nil, appErr(api.ErrorKindInvalid, "notice.raise needs a title")
		}
		if p.Level != "" && !isWord("NoticeLevel", string(p.Level)) {
			return nil, appErr(api.ErrorKindInvalid, "level %q is not one of %v", p.Level, api.Enums["NoticeLevel"])
		}
		res, err := ps.Raise(c.clientID, p)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		res.Seq = s
		return res, nil
	}
	if err != nil {
		return nil, actionErr(m.Method, err)
	}
	return api.Empty{Seq: s}, nil
}

// Connected reports whether client has an open control connection.
func (s *Server) Connected(client string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.clientID == client && t.live != nil {
			return true
		}
	}
	return false
}

// ClientLabel returns the label an open connection of client gave in its hello, else its
// client name, else "".
func (s *Server) ClientLabel(client string) string {
	s.mu.Lock()
	var live []*conn
	for _, t := range s.tokens {
		if t.clientID == client && t.live != nil {
			live = append(live, t.live)
		}
	}
	s.mu.Unlock()
	for _, c := range live {
		c.mu.Lock()
		l := c.label
		c.mu.Unlock()
		if l != "" {
			return l
		}
	}
	return ""
}
