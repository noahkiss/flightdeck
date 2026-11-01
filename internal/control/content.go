package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// EventContentUpdated is the event type of a file a session showed (content.show). Its id is
// the item id.
const EventContentUpdated api.EventType = "content.updated"

// Content serves rich content for the chat view (app.md 4.7). gff-hab implements it with
// internal/content. Its calls read transcripts and run git, so they run off the connection's
// read loop. An error that is an *Error keeps its kind.
type Content interface {
	Show(ctx context.Context, p api.ContentShowParams) (api.ContentShown, error)
	List(ctx context.Context, p api.ContentListParams) (api.ContentList, error)
	Read(ctx context.Context, p api.ContentReadParams) (api.ContentData, error)
	Diff(ctx context.Context, p api.ContentDiffParams) (api.ContentDiff, error)
}

// maxRunningContent bounds the content.* calls one connection may have running. A content.read
// holds up to 1 MiB until its reply is queued.
const maxRunningContent = 4

// handleContent checks a content.* call and runs it on its own goroutine.
func (c *conn) handleContent(m message) {
	seq := c.currentSeq()
	if c.s.cfg.Content == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%s needs the content service, which this hab is running without; the content capability is absent", m.Method))
		return
	}
	var run func(ctx context.Context) (any, error)
	cs := c.s.cfg.Content
	s := api.Seq{Seq: seq}
	switch m.Method {
	case "content.show":
		var p api.ContentShowParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.Path == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "content.show needs a path"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := cs.Show(ctx, p)
			r.Seq = s
			return r, err
		}
	case "content.list":
		var p api.ContentListParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.Session == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "content.list needs a session"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := cs.List(ctx, p)
			if r.Items == nil {
				r.Items = []api.ContentItem{}
			}
			r.Seq = s
			return r, err
		}
	case "content.read":
		var p api.ContentReadParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.Session == "" || p.ID == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "content.read needs a session and an id"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := cs.Read(ctx, p)
			if r.Data == nil {
				r.Data = []byte{}
			}
			r.Seq = s
			return r, err
		}
	case "content.diff":
		var p api.ContentDiffParams
		if e := c.decodeUp(m, &p); e != nil {
			c.reply(m, seq, nil, e)
			return
		}
		if p.Session == "" || p.ID == "" {
			c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "content.diff needs a session and an id"))
			return
		}
		run = func(ctx context.Context) (any, error) {
			r, err := cs.Diff(ctx, p)
			r.Seq = s
			return r, err
		}
	}
	c.mu.Lock()
	if c.contents >= maxRunningContent {
		c.mu.Unlock()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d content calls already run on this connection", maxRunningContent))
		return
	}
	c.contents++
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, c.s.cfg.ActionTimeout)
		defer cancel()
		res, err := run(ctx)
		c.mu.Lock()
		c.contents--
		c.mu.Unlock()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, res, nil)
	}()
}
