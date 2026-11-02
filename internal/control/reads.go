package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Reads serves the read tools (capcom.md 4): transcript.list, transcript.search,
// transcript.read, notice.get and file.read. gff-hab implements it with internal/readtools.
// local is true for a call over a peer link: it is answered from this hab alone (one hop). An
// error that is an *Error keeps its kind.
type Reads interface {
	Sessions(ctx context.Context, local bool, p api.TranscriptListParams) (api.TranscriptSessions, error)
	Search(ctx context.Context, local bool, p api.TranscriptSearchParams) (api.TranscriptHits, error)
	Transcript(ctx context.Context, local bool, p api.TranscriptReadParams) (api.TranscriptPage, error)
	Notice(ctx context.Context, local bool, p api.NoticeGetParams) (api.NoticeRecord, error)
	File(ctx context.Context, local bool, p api.FileReadParams) (api.FileData, error)
}

// Logbook serves the Logbook reads (logbook.md 10.3). Nothing implements it until Logbook
// (Part 12) exists, so logbook.find and logbook.get answer unavailable.
type Logbook interface {
	Find(ctx context.Context, p api.LogbookFindParams) (api.LogbookRecords, error)
	Get(ctx context.Context, p api.LogbookGetParams) (api.LogbookRecordResult, error)
}

// maxReadTime bounds one read call: the per-host budget, the forward's grace and a margin.
const maxReadTime = 30 * time.Second

func isReadMethod(m string) bool {
	switch m {
	case "transcript.list", "transcript.search", "transcript.read", "notice.get", "file.read", "logbook.find", "logbook.get":
		return true
	}
	return false
}

// handleReads answers the read methods off the read loop: a search may take seconds on each
// host.
func (c *conn) handleReads(m message) {
	seq := c.currentSeq()
	rs, lb := c.s.cfg.Reads, c.s.cfg.Logbook
	local := c.peer != nil
	var call func(ctx context.Context) (any, error)
	var e *Error
	switch m.Method {
	case "transcript.list":
		var p api.TranscriptListParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context) (any, error) { return rs.Sessions(ctx, local, p) }
		}
	case "transcript.search":
		var p api.TranscriptSearchParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context) (any, error) { return rs.Search(ctx, local, p) }
		}
	case "transcript.read":
		var p api.TranscriptReadParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context) (any, error) { return rs.Transcript(ctx, local, p) }
		}
	case "notice.get":
		var p api.NoticeGetParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context) (any, error) { return rs.Notice(ctx, local, p) }
		}
	case "file.read":
		var p api.FileReadParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context) (any, error) { return rs.File(ctx, local, p) }
		}
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
	}
	if e == nil && call == nil {
		if m.Method == "logbook.find" || m.Method == "logbook.get" {
			e = appErr(api.ErrorKindUnavailable, "%s needs Logbook, which this hab does not have yet; the logbook capability is absent", m.Method)
		} else {
			e = appErr(api.ErrorKindUnavailable, "%s needs the read tools, which this hab is running without; the read-tools capability is absent", m.Method)
		}
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
		case api.TranscriptSessions:
			r.Seq = s
			res = api.NonNil(r)
		case api.TranscriptHits:
			r.Seq = s
			res = api.NonNil(r)
		case api.TranscriptPage:
			r.Seq = s
			res = api.NonNil(r)
		case api.NoticeRecord:
			r.Seq = s
			res = api.NonNil(r)
		case api.FileData:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookRecords:
			r.Seq = s
			res = api.NonNil(r)
		case api.LogbookRecordResult:
			r.Seq = s
			res = api.NonNil(r)
		}
		c.reply(m, seq, res, nil)
	}()
}
