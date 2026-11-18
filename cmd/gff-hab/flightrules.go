package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/flightrules"
	"github.com/noahkiss/flightdeck/internal/instructions"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// flightRulesIdle is how long a session's ledger stays in memory with no call.
const flightRulesIdle = 10 * time.Minute

// flightRulesService serves flightrules.list and flightrules.text (instructions-mcp-flight-rules.md
// 11). A session's ledger is found like its chat: the native session id its hooks reported names
// the transcript. Only Claude Code has a ledger source yet.
type flightRulesService struct {
	session func(id string) (api.Session, bool)
	agent   func(pane string) (api.AgentState, bool)
	find    func(harness, native string) (string, error)
	state   string // the hab's state directory, where the instruction renderer keeps its records
	now     func() time.Time

	mu      sync.Mutex
	ledgers map[string]*sessionLedger // by gff-hab session id
}

type sessionLedger struct {
	harness, native string
	s               *flightrules.Session // nil while the transcript is not written
	used            time.Time
}

// source builds the ledger source of a harness, or nil when it has none yet. A new harness's
// source is one more case here.
func (f *flightRulesService) source(harness, path string) (flightrules.Source, func(string, string) []string) {
	switch harness {
	case detect.KindClaude:
		return flightrules.NewClaude(path), flightrules.Rendered(f.state, instructions.Claude)
	}
	return nil, nil
}

// ledger returns a session's ledger, opening it on first use. It is nil with no error while
// the transcript is not written yet.
func (f *flightRulesService) ledger(id string) (*sessionLedger, error) {
	sess, ok := f.session(id)
	if !ok {
		return nil, control.Errorf(api.ErrorKindNotFound, "no session %s", id)
	}
	var harness, native string
	if sess.Pane != "" {
		if st, ok := f.agent(sess.Pane); ok && st.Session != "" {
			harness, native = st.Harness, st.Session
		}
	}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evict(now)
	sl := f.ledgers[id]
	if native == "" {
		// With no agent (it left, or the pane is gone) the last ledger stays readable.
		if sl != nil {
			sl.used = now
			return sl, nil
		}
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s has no agent session yet: its harness has not reported one", id)
	}
	if sl != nil && sl.native == native && sl.s != nil {
		sl.used = now
		return sl, nil
	}
	if src, _ := f.source(harness, ""); src == nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s runs %s, which has no flight rules reader yet", id, harness)
	}
	path, err := f.find(harness, native)
	if errors.Is(err, transcript.ErrNoTranscript) {
		sl = &sessionLedger{harness: harness, native: native, used: now}
		f.ledgers[id] = sl
		return sl, nil
	}
	if err != nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", id, err)
	}
	src, configured := f.source(harness, path)
	sl = &sessionLedger{harness: harness, native: native, s: flightrules.NewSession(src, configured), used: now}
	f.ledgers[id] = sl
	return sl, nil
}

// evict drops the ledgers of gone sessions and those unused for flightRulesIdle. Called with
// f.mu held.
func (f *flightRulesService) evict(now time.Time) {
	for id, sl := range f.ledgers {
		if _, ok := f.session(id); !ok || now.Sub(sl.used) > flightRulesIdle {
			delete(f.ledgers, id)
		}
	}
}

// List implements control.FlightRules.
func (f *flightRulesService) List(ctx context.Context, p api.FlightRulesParams) (api.FlightRules, error) {
	sl, err := f.ledger(p.Session)
	if err != nil {
		return api.FlightRules{}, err
	}
	out := api.FlightRules{Session: p.Session, Harness: sl.harness, Entries: []api.FlightRule{}}
	if sl.s == nil {
		return out, nil
	}
	out.Entries, out.Warnings, err = sl.s.List(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		// A long transcript's first read ran out of time. The source keeps its place, so the
		// next list goes on from there; this one answers with what it read.
		out.Warnings = append(out.Warnings, "the transcript was not read to the end in time; list again for the rest")
	case err != nil:
		return api.FlightRules{}, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", p.Session, err)
	}
	return out, nil
}

// Text implements control.FlightRules.
func (f *flightRulesService) Text(ctx context.Context, p api.FlightRuleTextParams) (api.FlightRuleText, error) {
	sl, err := f.ledger(p.Session)
	if err != nil {
		return api.FlightRuleText{}, err
	}
	if sl.s == nil {
		return api.FlightRuleText{}, control.Errorf(api.ErrorKindNotFound, "no entry %s in session %s", p.ID, p.Session)
	}
	text, size, cut, err := sl.s.Text(ctx, p.ID)
	switch {
	case errors.Is(err, flightrules.ErrNoEntry):
		return api.FlightRuleText{}, control.Errorf(api.ErrorKindNotFound, "no entry %s with text in session %s", p.ID, p.Session)
	case err != nil:
		return api.FlightRuleText{}, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", p.Session, err)
	}
	return api.FlightRuleText{ID: p.ID, Text: text, Bytes: size, Truncated: cut}, nil
}
