package control

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// Logbook serves the logbook.* methods (logbook.md 10.3). gff-hab implements it with
// internal/lbsvc over its Logbook replica. A write's actor is the client: a person by the
// client's label (logbook.md 2.5). An error that is an *Error keeps its kind.
type Logbook interface {
	Find(ctx context.Context, p api.LogbookFindParams) (api.LogbookRecords, error)
	Get(ctx context.Context, p api.LogbookGetParams) (api.LogbookRecordResult, error)
	List(ctx context.Context, p api.LogbookListParams) (api.LogbookRecords, error)
	Ready(ctx context.Context, p api.LogbookReadyParams) (api.LogbookRecords, error)
	History(ctx context.Context, p api.LogbookHistoryParams) (api.LogbookHistory, error)
	Write(ctx context.Context, a api.LogbookActor, p api.LogbookWriteParams) (api.LogbookWritten, error)
	Resolve(ctx context.Context, a api.LogbookActor, p api.LogbookResolveParams) (api.LogbookWritten, error)
	Projects(ctx context.Context) (api.LogbookProjects, error)
	Status(ctx context.Context) (api.LogbookStatus, error)
}

// EventLogbookUpdated and EventLogbookConflict are Logbook's events: host scope only.
const (
	EventLogbookUpdated  = api.EventLogbookUpdated
	EventLogbookConflict = api.EventLogbookConflict
)

func isLogbookMethod(m string) bool {
	switch m {
	case "logbook.find", "logbook.get", "logbook.list", "logbook.ready", "logbook.history", "logbook.write",
		"logbook.resolve", "logbook.projects", "logbook.status":
		return true
	}
	return false
}

// handleLogbook answers the logbook.* methods off the read loop: a write waits for SQLite's
// write lock, up to 5 s while the CLI writes (logbook.md 4.3).
func (c *conn) handleLogbook(m message) {
	seq := c.currentSeq()
	lb := c.s.cfg.Logbook
	c.mu.Lock()
	who := api.LogbookActor{Kind: "person", Device: c.label}
	c.mu.Unlock()
	var call func(ctx context.Context) (any, error)
	var e *Error
	write := m.Method == "logbook.write" || m.Method == "logbook.resolve"
	switch m.Method {
	case "logbook.find":
		var p api.LogbookFindParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Find(ctx, p) }
		}
	case "logbook.get":
		var p api.LogbookGetParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Get(ctx, p) }
		}
	case "logbook.list":
		var p api.LogbookListParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.List(ctx, p) }
		}
	case "logbook.ready":
		var p api.LogbookReadyParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Ready(ctx, p) }
		}
	case "logbook.history":
		var p api.LogbookHistoryParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.History(ctx, p) }
		}
	case "logbook.write":
		var p api.LogbookWriteParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Write(ctx, who, p) }
		}
	case "logbook.resolve":
		var p api.LogbookResolveParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Resolve(ctx, who, p) }
		}
	case "logbook.projects":
		var p api.LogbookProjectsParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Projects(ctx) }
		}
	case "logbook.status":
		var p api.LogbookStatusParams
		if e = c.decodeUp(m, &p); e == nil && lb != nil {
			call = func(ctx context.Context) (any, error) { return lb.Status(ctx) }
		}
	}
	switch {
	case e != nil:
	case call == nil:
		e = appErr(api.ErrorKindUnavailable, "%s needs Logbook, which this hab is running without; the logbook capability is absent", m.Method)
	case write && c.peer != nil:
		// Origins are habs: a client's write becomes an op on the hab it talks to, and peers get
		// it by sync (logbook.md 2.5, D3).
		e = appErr(api.ErrorKindInvalid, "%s does not run over a peer link: write on the hab you talk to, and sync carries the ops", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxReadTime)
		defer cancel()
		res, err := call(ctx)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		s := api.Seq{Seq: seq}
		switch r := res.(type) {
		case api.LogbookRecords:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookRecordResult:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookHistory:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookWritten:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookProjects:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookStatus:
			r.Seq = s
			res = api.NonNil(r)
		}
		c.reply(m, seq, res, nil)
	}()
}
