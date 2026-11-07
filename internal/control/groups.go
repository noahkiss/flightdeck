package control

import (
	"context"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// EventGroupUpdated is a group record's change (groups.md 15.1). Stored, with a revision; the
// groups are in the host snapshot, so it goes to host scopes only.
const EventGroupUpdated api.EventType = "group.updated"

// Groups serves the group.* methods (groups.md 15.1). gff-hab implements it. An error that is
// an *Error keeps its kind. caller is the calling session's name on this hab when the call came
// from a pane; the control connection passes "", a user.
type Groups interface {
	Create(ctx context.Context, p api.GroupCreateParams) (api.GroupCreated, error)
	Amend(ctx context.Context, p api.GroupAmendParams) (api.GroupCreated, error)
	Get(id string) (api.Group, error)
	List(state api.GroupState) []api.CatalogGroup
	Report(ctx context.Context, p api.GroupReportParams) (api.GroupReported, error)
	Note(ctx context.Context, p api.GroupNoteParams) error
	Pause(id string) (api.Group, error)
	Resume(id string) (api.Group, error)
	Close(ctx context.Context, id string) (api.Group, error)
	Retry(ctx context.Context, p api.GroupRetryParams) (api.Group, error)
	// MemberAction and MemberActionGet serve the sibling actions (groups.md 9.4, stage B6):
	// member.compact, member.handoff and member.action. A call from a peer hab carries its
	// hello (CallerPeer).
	MemberAction(ctx context.Context, kind api.MemberActionKind, p api.MemberActionParams) (api.MemberAction, error)
	MemberActionGet(ctx context.Context, id string) (api.MemberAction, error)
	// Sync takes a group home's push over a peer link; from is the link's hello.
	Sync(from api.PeerHello, p api.GroupSyncParams) error
}

// maxGroupTime bounds one group call: a create waits up to 120 s for its confirmation, and a
// retry starts a member on a peer.
const maxGroupTime = 150 * time.Second

// handleGroups answers the group methods off the read loop: a create may wait for its answer,
// and each change is saved before it answers.
func (c *conn) handleGroups(m message) {
	seq := c.currentSeq()
	gs := c.s.cfg.Groups
	if gs == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%s needs groups, which this hab is running without; the groups capability is absent", m.Method))
		return
	}
	var call func(ctx context.Context) (any, error)
	var e *Error
	ref := func() string {
		var p api.GroupRef
		e = c.decodeUp(m, &p)
		return p.Group
	}
	result := func(g api.Group, err error) (any, error) { return api.GroupResult{Group: g}, err }
	switch m.Method {
	case "group.create":
		var p api.GroupCreateParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return gs.Create(ctx, p) }
		}
	case "group.amend":
		var p api.GroupAmendParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return gs.Amend(ctx, p) }
		}
	case "group.get":
		id := ref()
		call = func(context.Context) (any, error) { return result(gs.Get(id)) }
	case "group.list":
		var p api.GroupListParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(context.Context) (any, error) { return api.GroupList{Groups: gs.List(p.State)}, nil }
		}
	case "group.report":
		var p api.GroupReportParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return gs.Report(ctx, p) }
		}
	case "group.note":
		var p api.GroupNoteParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return api.Empty{}, gs.Note(ctx, p) }
		}
	case "group.pause":
		id := ref()
		call = func(context.Context) (any, error) { return result(gs.Pause(id)) }
	case "group.resume":
		id := ref()
		call = func(context.Context) (any, error) { return result(gs.Resume(id)) }
	case "group.close":
		id := ref()
		call = func(ctx context.Context) (any, error) { return result(gs.Close(ctx, id)) }
	case "group.retry":
		var p api.GroupRetryParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) { return result(gs.Retry(ctx, p)) }
		}
	case "member.compact", "member.handoff":
		var p api.MemberActionParams
		kind := api.MemberActionKind(strings.TrimPrefix(m.Method, "member."))
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) {
				a, err := gs.MemberAction(ctx, kind, p)
				return api.MemberActionResult{Action: a}, err
			}
		}
	case "member.action":
		var p api.MemberActionRef
		if e = c.decodeUp(m, &p); e == nil {
			call = func(ctx context.Context) (any, error) {
				a, err := gs.MemberActionGet(ctx, p.ID)
				return api.MemberActionResult{Action: a}, err
			}
		}
	case "group.sync":
		if c.peer == nil {
			e = appErr(api.ErrorKindInvalid, "group.sync needs a peer link: client.kind hab with the hello's peer field")
			break
		}
		from := *c.peer
		var p api.GroupSyncParams
		if e = c.decodeUp(m, &p); e == nil {
			call = func(context.Context) (any, error) { return api.Empty{}, gs.Sync(from, p) }
		}
	default:
		e = appErr(api.ErrorKindNotFound, "no method %s", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxGroupTime)
		defer cancel()
		if c.peer != nil {
			ctx = WithCallerPeer(ctx, *c.peer) // a sibling action forwarded by the asker's hab
		}
		res, err := call(ctx)
		seq := c.currentSeq() // after the call: the events it caused come before its answer
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, api.NonNil(withSeq(res, seq)), nil)
	}()
}

// withSeq sets the seq of a group result.
func withSeq(res any, seq uint64) any {
	s := api.Seq{Seq: seq}
	switch r := res.(type) {
	case api.GroupCreated:
		r.Seq = s
		return r
	case api.GroupResult:
		r.Seq = s
		return r
	case api.GroupList:
		r.Seq = s
		return r
	case api.GroupReported:
		r.Seq = s
		return r
	case api.MemberActionResult:
		r.Seq = s
		return r
	case api.Empty:
		r.Seq = s
		return r
	}
	return res
}

func isGroupMethod(m string) bool {
	return strings.HasPrefix(m, "group.") || strings.HasPrefix(m, "member.")
}
