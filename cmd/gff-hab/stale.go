package main

// Session.stale (instructions-mcp-flight-rules.md 13.1): the hab reads each running Claude Code
// and Codex session's records in the background and flags the session when it works from an
// older render of its instruction file, so a client sees the flag without a flightrules.list
// per session. A harness with no render record is never read: nothing can be stale.

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/flightrules"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// The sweep's period, and the time one session's read may take.
const (
	staleEvery = 30 * time.Second
	staleRead  = 5 * time.Second
)

// staleInstructions is the Session.stale word for an instruction file.
const staleInstructions = "instructions"

// staleFlagged reports whether a harness's stale render is flagged on its session: Claude Code
// and Codex load the file once, at the start of the session, so only a restart picks a new one
// up. opencode reads a changed file before its next request (13), so a restart gains nothing.
func staleFlagged(harness string) bool {
	return harness == detect.KindClaude || harness == detect.KindCodex
}

// staleWatch keeps Session.stale.
type staleWatch struct {
	store *structure.Store
	agent func(pane string) (api.AgentState, bool)
	rules *flightRulesService
	state string // the hab's state directory, where the renderer keeps its records
	every time.Duration

	quit, done chan struct{}
}

// start begins the sweep. The flight rules service's mark should be w.listed, set when it is
// built.
func (w *staleWatch) start() {
	if w.every <= 0 {
		w.every = staleEvery
	}
	w.quit, w.done = make(chan struct{}), make(chan struct{})
	go w.run()
}

// Close stops the sweep.
func (w *staleWatch) Close() {
	close(w.quit)
	<-w.done
}

func (w *staleWatch) run() {
	defer close(w.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-w.quit
		cancel()
	}()
	t := time.NewTicker(w.every)
	defer t.Stop()
	for {
		w.sweep(ctx)
		select {
		case <-w.quit:
			return
		case <-t.C:
		}
	}
}

// harnessOf returns the agent of a running session whose stale render is flagged, with a render
// record to compare against.
func (w *staleWatch) harnessOf(ss api.Session) (api.AgentState, bool) {
	if ss.State != api.SessionStateRunning || ss.Pane == "" {
		return api.AgentState{}, false
	}
	st, ok := w.agent(ss.Pane)
	if !ok || st.Session == "" || !staleFlagged(st.Harness) {
		return api.AgentState{}, false
	}
	if _, ok := flightrules.Current(w.state, renderHarness(st.Harness))(); !ok {
		return api.AgentState{}, false
	}
	return st, true
}

// sweep works out the flag of every session once.
func (w *staleWatch) sweep(ctx context.Context) {
	for _, ss := range w.store.Snapshot(api.HostInfo{}).Sessions {
		if ctx.Err() != nil {
			return
		}
		if _, ok := w.harnessOf(ss); !ok {
			w.store.SetStale(ss.ID, nil)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, staleRead)
		stale, err := w.rules.stale(rctx, ss.ID)
		cancel()
		if err != nil {
			continue // a read cut short goes on at the next sweep; the flag stays
		}
		w.set(ss.ID, stale)
	}
}

// listed takes the flag from a flightrules.list, so a client that lists sees the session agree.
func (w *staleWatch) listed(session, harness string, stale bool) {
	ss, ok := w.store.Session(session)
	if !ok {
		return
	}
	if st, ok := w.harnessOf(ss); !ok || st.Harness != harness {
		w.store.SetStale(session, nil)
		return
	}
	w.set(session, stale)
}

func (w *staleWatch) set(session string, stale bool) {
	if stale {
		w.store.SetStale(session, []string{staleInstructions})
	} else {
		w.store.SetStale(session, nil)
	}
}
