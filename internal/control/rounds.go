package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Event types of the rounds (rounds.md 13). round.updated is stored, with a revision, and the
// rounds are in the host snapshot; round.run_updated is ephemeral. Both go to host scopes only.
const (
	EventRoundUpdated    api.EventType = "round.updated"
	EventRoundRunUpdated api.EventType = "round.run_updated"
)

// Rounds serves the round.* methods (rounds.md 13). gff-hab implements it over internal/rounds.
// An error that is an *Error keeps its kind.
type Rounds interface {
	List() []api.Round
	Get(ref api.RoundRef) (api.Round, error)
	// Batch returns an event round's pending batch, nil when it has none (round.get with batch).
	Batch(ref api.RoundRef) (*api.RoundBatch, error)
	// Create stores a new round. createdBy is the calling session when an agent creates it from
	// its pane; the control connection passes "", a user.
	Create(ctx context.Context, p api.RoundCreateParams, createdBy string) (api.RoundCreated, error)
	Update(ctx context.Context, p api.RoundUpdateParams) (api.Round, error)
	Delete(ref api.RoundRef) error
	Pause(ref api.RoundRef) (api.Round, error)
	Unpause(ref api.RoundRef) (api.Round, error)
	Run(ref api.RoundRef) (api.RoundRun, error)
	Runs(p api.RoundRunsParams) (api.RoundRuns, error)
}

// maxRoundTime bounds one round call: a create with project_dir runs git.
const maxRoundTime = 30 * time.Second

func isRoundMethod(m string) bool {
	switch m {
	case "round.list", "round.get", "round.create", "round.update", "round.delete",
		"round.pause", "round.unpause", "round.run", "round.runs":
		return true
	}
	return false
}

// handleRounds answers the round methods off the read loop: a create may run git, and every
// change is saved to disk before it answers.
func (c *conn) handleRounds(m message) {
	seq := c.currentSeq()
	rs := c.s.cfg.Rounds
	var call func(ctx context.Context, now func() api.Seq) (any, error)
	var e *Error
	ref := func() api.RoundRef {
		var p api.RoundRef
		e = c.decodeUp(m, &p)
		return p
	}
	switch m.Method {
	case "round.list":
		var p api.RoundListParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(_ context.Context, now func() api.Seq) (any, error) {
				return api.RoundList{Seq: now(), Rounds: rs.List()}, nil
			}
		}
	case "round.get":
		var p api.RoundGetParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(_ context.Context, now func() api.Seq) (any, error) {
				r, err := rs.Get(p.RoundRef)
				res := api.RoundResult{Round: r}
				if err == nil && p.Batch {
					res.Batch, err = rs.Batch(p.RoundRef)
				}
				res.Seq = now()
				return res, err
			}
		}
	case "round.pause", "round.unpause":
		p := ref()
		if e == nil && rs != nil {
			call = func(_ context.Context, now func() api.Seq) (any, error) {
				var r api.Round
				var err error
				switch m.Method {
				case "round.pause":
					r, err = rs.Pause(p)
				default:
					r, err = rs.Unpause(p)
				}
				return api.RoundResult{Seq: now(), Round: r}, err
			}
		}
	case "round.delete":
		p := ref()
		if e == nil && rs != nil {
			call = func(_ context.Context, now func() api.Seq) (any, error) {
				err := rs.Delete(p)
				return api.Empty{Seq: now()}, err
			}
		}
	case "round.run":
		p := ref()
		if e == nil && rs != nil {
			call = func(_ context.Context, now func() api.Seq) (any, error) {
				r, err := rs.Run(p)
				return api.RoundRunResult{Seq: now(), Run: r}, err
			}
		}
	case "round.runs":
		var p api.RoundRunsParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(_ context.Context, now func() api.Seq) (any, error) {
				r, err := rs.Runs(p)
				r.Seq = now()
				return r, err
			}
		}
	case "round.create":
		var p api.RoundCreateParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context, now func() api.Seq) (any, error) {
				r, err := rs.Create(ctx, p, "")
				r.Seq = now()
				return r, err
			}
		}
	case "round.update":
		var p api.RoundUpdateParams
		if e = c.decodeUp(m, &p); e == nil && rs != nil {
			call = func(ctx context.Context, now func() api.Seq) (any, error) {
				r, err := rs.Update(ctx, p)
				return api.RoundResult{Seq: now(), Round: r}, err
			}
		}
	}
	if e == nil && call == nil {
		e = appErr(api.ErrorKindUnavailable, "%s needs rounds, which this hab is running without; the rounds capability is absent", m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxRoundTime)
		defer cancel()
		// The seq is read after the call: the events the call caused come before its answer.
		var seq uint64
		res, err := call(ctx, func() api.Seq { seq = c.currentSeq(); return api.Seq{Seq: seq} })
		if err != nil {
			seq = c.currentSeq()
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, api.NonNil(res), nil)
	}()
}
