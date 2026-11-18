package main

// Session.stale (instructions-mcp-flight-rules.md 13.1): the hab reads each running Claude Code
// and Codex session's records in the background and flags the session when it works from an
// older render of its instruction file, so a client sees the flag without a flightrules.list
// per session. A harness with no render record is never read: nothing can be stale.

import (
	"context"
	"slices"
	"sync"
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

	mu sync.Mutex
	// since holds when each session last restarted. Until the new program records a load, the
	// newest entry is the old program's: only a stale entry seen at or after this time counts.
	since map[string]time.Time

	quit, done chan struct{}
}

// restarted notes a restart that began at at (restart.go), and clears the flag.
func (w *staleWatch) restarted(session string, at time.Time) {
	w.mu.Lock()
	if w.since == nil {
		w.since = map[string]time.Time{}
	}
	w.since[session] = at
	w.mu.Unlock()
	w.store.SetStale(session, nil)
}

// stale reports whether entries show the session working from an older render, loaded since
// its last restart.
func (w *staleWatch) stale(session string, entries []api.FlightRule) bool {
	w.mu.Lock()
	since, restarted := w.since[session]
	w.mu.Unlock()
	for _, e := range entries {
		if !e.Stale {
			continue
		}
		if !restarted {
			return true
		}
		if t, err := time.Parse(time.RFC3339Nano, e.LastSeen); err == nil && !t.Before(since) {
			return true
		}
	}
	return false
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
	sessions := w.store.Snapshot(api.HostInfo{}).Sessions
	w.mu.Lock()
	for id := range w.since {
		if !slices.ContainsFunc(sessions, func(s api.Session) bool { return s.ID == id }) {
			delete(w.since, id)
		}
	}
	w.mu.Unlock()
	for _, ss := range sessions {
		if ctx.Err() != nil {
			return
		}
		if _, ok := w.harnessOf(ss); !ok {
			w.store.SetStale(ss.ID, nil)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, staleRead)
		entries, err := w.rules.entries(rctx, ss.ID)
		cancel()
		if err != nil {
			continue // a read cut short goes on at the next sweep; the flag stays
		}
		w.set(ss.ID, w.stale(ss.ID, entries))
	}
}

// listed takes the flag from a flightrules.list, so a client that lists sees the session agree.
func (w *staleWatch) listed(session, harness string, entries []api.FlightRule) {
	ss, ok := w.store.Session(session)
	if !ok {
		return
	}
	if st, ok := w.harnessOf(ss); !ok || st.Harness != harness {
		w.store.SetStale(session, nil)
		return
	}
	w.set(session, w.stale(session, entries))
}

func (w *staleWatch) set(session string, stale bool) {
	if stale {
		w.store.SetStale(session, []string{staleInstructions})
	} else {
		w.store.SetStale(session, nil)
	}
}
