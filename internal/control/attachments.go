package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// Attachments stores the files the owner sends to a session (app.md 4.4). gff-hab implements it
// with internal/attach. Its calls write and read files, so they run off the connection's read
// loop. An error that is an *Error keeps its kind.
type Attachments interface {
	Put(ctx context.Context, p api.AttachmentPutParams) (api.AttachmentStored, error)
	Write(ctx context.Context, p api.AttachmentWriteParams) (api.AttachmentWritten, error)
	Get(ctx context.Context, p api.AttachmentGetParams) (api.AttachmentData, error)
}

// maxRunningAttachments bounds the attachment.* calls one connection may have running. Each
// holds up to 1 MiB until its reply is queued.
const maxRunningAttachments = 4

// handleAttachment checks an attachment.* call and runs it on its own goroutine.
func (c *conn) handleAttachment(m message) {
	seq := c.currentSeq()
	as := c.s.cfg.Attachments
	if as == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%s needs the attachment store, which this hab is running without; the attachments capability is absent", m.Method))
		return
	}
	var run func(ctx context.Context) (any, error)
	s := api.Seq{Seq: seq}
	switch m.Method {
	case "attachment.put":
		var p api.AttachmentPutParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.Session == "" || p.Name == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "attachment.put needs a session and a name"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := as.Put(ctx, p)
			r.Seq = s
			return r, err
		}
	case "attachment.write":
		var p api.AttachmentWriteParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.ID == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "attachment.write needs an id"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := as.Write(ctx, p)
			r.Seq = s
			return r, err
		}
	case "attachment.get":
		var p api.AttachmentGetParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.ID == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "attachment.get needs an id"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := as.Get(ctx, p)
			if r.Data == nil {
				r.Data = []byte{}
			}
			r.Seq = s
			return r, err
		}
	default:
		c.reply(m, seq, nil, protoErr(codeMethodNotFound, "method %q does not exist", m.Method))
		return
	}
	c.mu.Lock()
	if c.attachments >= maxRunningAttachments {
		c.mu.Unlock()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d attachment calls already run on this connection", maxRunningAttachments))
		return
	}
	c.attachments++
	c.mu.Unlock()
	go func() {
		// A write runs to its end when the connection drops: the upload then waits for the
		// next run from a new connection, or is dropped when idle.
		ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
		defer cancel()
		res, err := c.runAttachment(ctx, m.Method, run)
		c.mu.Lock()
		c.attachments--
		c.mu.Unlock()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, res, nil)
	}()
}

// runAttachment runs one call and turns a panic into an error: a bug in the store fails that
// call, never the hab and every session's connection with it.
func (c *conn) runAttachment(ctx context.Context, method string, run func(context.Context) (any, error)) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			c.s.cfg.Logf("control: %s panicked: %v", method, r)
			res, err = nil, appErr(api.ErrorKindUnavailable, "%s failed inside the hab: %v", method, r)
		}
	}()
	return run(ctx)
}
