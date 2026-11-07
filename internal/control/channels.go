package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// EventChannelMessage is the event type of a changed record of a message this hab sent. Its id
// is the message id.
const EventChannelMessage api.EventType = "channel.message"

// Channels carries agent messages (mesh.md 6). gff-hab implements it with internal/channels. An
// error that is an *Error keeps its kind.
type Channels interface {
	Send(ctx context.Context, p api.ChannelSendParams) (api.ChannelSent, error)
	Status(ctx context.Context, p api.ChannelStatusParams) (api.ChannelStatusResult, error)
	Wait(ctx context.Context, p api.ChannelWaitParams) (api.ChannelWaitResult, error)
	// Push and Pull serve peer links only; from is the link's hello peer field.
	Push(from api.PeerHello, p api.MailPushParams) (api.MailPushResult, error)
	Pull(from api.PeerHello, p api.MailPullParams) (api.MailPullResult, error)
}

// peerCallKey marks the context of a channel call that came over a peer link.
type peerCallKey struct{}

// WithPeerCall marks ctx as a call that came over a peer link (mesh.md 6.6).
func WithPeerCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, peerCallKey{}, true)
}

// PeerCall reports whether ctx is a call that came over a peer link: a channel.wait there waits
// on this hab's own agents only, so a wait is never forwarded twice (mesh.md 6.6).
func PeerCall(ctx context.Context) bool {
	v, _ := ctx.Value(peerCallKey{}).(bool)
	return v
}

// callerPeerKey carries the hello of the peer hab a call came from.
type callerPeerKey struct{}

// WithCallerPeer marks ctx as a call from the peer hab whose hello is p.
func WithCallerPeer(ctx context.Context, p api.PeerHello) context.Context {
	return context.WithValue(ctx, callerPeerKey{}, p)
}

// CallerPeer returns the hello of the peer hab a call came over, with ok false for a call from
// any other client. session.create's report_to names a session on that hab
// (harness-adapters.md, Spawn): the device comes from the link, never from the params.
func CallerPeer(ctx context.Context) (api.PeerHello, bool) {
	p, ok := ctx.Value(callerPeerKey{}).(api.PeerHello)
	return p, ok
}

// maxChannelTime bounds one channel call: the longest wait plus a margin.
const maxChannelTime = 610 * time.Second

func isChannelMethod(m string) bool {
	switch m {
	case "channel.send", "channel.status", "channel.wait", "mail.push", "mail.pull":
		return true
	}
	return false
}

// handleChannels answers the channel and mail methods off the read loop: a wait may take
// minutes.
func (c *conn) handleChannels(m message) {
	seq := c.currentSeq()
	ch := c.s.cfg.Channels
	var call func(ctx context.Context) (any, error)
	var e *Error
	switch m.Method {
	case "channel.send":
		var p api.ChannelSendParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return ch.Send(ctx, p) }
		}
	case "channel.status":
		var p api.ChannelStatusParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return ch.Status(ctx, p) }
		}
	case "channel.wait":
		var p api.ChannelWaitParams
		if e = c.decodeUp(m, &p); e == nil {
			for _, w := range p.For {
				if !isWord("AgentStatus", string(w)) {
					e = appErr(api.ErrorKindInvalid, "for %q is not one of %v", w, api.Enums["AgentStatus"])
				}
			}
			call = func(ctx context.Context) (any, error) { return ch.Wait(ctx, p) }
		}
	case "mail.push", "mail.pull":
		if c.peer == nil {
			e = appErr(api.ErrorKindInvalid, "%s needs a peer link: client.kind hab with the hello's peer field", m.Method)
			break
		}
		from := *c.peer
		if m.Method == "mail.push" {
			var p api.MailPushParams
			if e = c.decodeUp(m, &p); e == nil {
				call = func(context.Context) (any, error) { return ch.Push(from, p) }
			}
		} else {
			var p api.MailPullParams
			if e = c.decodeUp(m, &p); e == nil {
				call = func(context.Context) (any, error) { return ch.Pull(from, p) }
			}
		}
	}
	if e == nil && ch == nil {
		e = appErr(api.ErrorKindUnavailable, "%s needs channels, which this hab is running without; the channels capability is absent", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	fromPeer := c.peer != nil
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxChannelTime)
		defer cancel()
		if fromPeer {
			ctx = WithPeerCall(ctx)
		}
		res, err := call(ctx)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		s := api.Seq{Seq: seq}
		switch r := res.(type) {
		case api.ChannelSent:
			r.Seq = s
			res = api.NonNil(r)
		case api.ChannelStatusResult:
			r.Seq = s
			res = api.NonNil(r)
		case api.ChannelWaitResult:
			r.Seq = s
			res = api.NonNil(r)
		case api.MailPushResult:
			r.Seq = s
			res = api.NonNil(r)
		case api.MailPullResult:
			r.Seq = s
			res = api.NonNil(r)
		}
		c.reply(m, seq, res, nil)
	}()
}
