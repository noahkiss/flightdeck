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
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxChannelTime)
		defer cancel()
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
