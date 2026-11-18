package main

// session.restart (instructions-mcp-flight-rules.md 13.1): a running session whose agent is idle
// or done exits its harness and starts again through the harness's own resume, so it loads its
// instructions anew and keeps its conversation. The owner starts each restart; there is no
// restart of several sessions. The holder runs one program per pane, so the harness starts again
// in a new pane of the same session, as session.resume does.

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// The waits for the old harness to exit: after the hang-up, then after a TERM.
const (
	restartHupWait  = 10 * time.Second
	restartTermWait = 5 * time.Second
)

// restarter serves session.restart.
type restarter struct {
	store *structure.Store
	// agent is the agent state of a pane.
	agent func(pane string) (api.AgentState, bool)
	// ready reports why the session could not start again once its harness exits (its resume
	// start or folder not known), before anything is stopped. nil: no check.
	ready func(ctx context.Context, session string, st api.AgentState) error
	// signal sends sig to a pane's child.
	signal func(ctx context.Context, pane string, sig hp.Sig) error
	// resume starts an exited session again: session.resume's path.
	resume func(ctx context.Context, session string) (string, error)
	logf   func(string, ...any)

	hupWait, termWait time.Duration
	op                sync.Mutex // one restart at a time, on the whole hab
}

// Restart exits a session's harness and starts it again with its resume id. It refuses while
// another restart runs, and unless the agent is idle or done.
func (r *restarter) Restart(ctx context.Context, session string) (string, error) {
	if !r.op.TryLock() {
		return "", control.Errorf(api.ErrorKindConflict, "another restart is running: restart one session at a time")
	}
	defer r.op.Unlock()
	ss, ok := r.store.Session(session)
	switch {
	case !ok:
		return "", control.Errorf(api.ErrorKindNotFound, "session %s does not exist", session)
	case ss.Round != nil && ss.Round.Watcher:
		return "", control.Errorf(api.ErrorKindConflict, "session %s is a watcher session: gff-hab starts its watcher (rounds.md 4.4)", ss.Name)
	case ss.State != api.SessionStateRunning || ss.Pane == "":
		return "", control.Errorf(api.ErrorKindConflict, "session %s is not running: session.resume starts it", ss.Name)
	}
	st, err := r.restartable(ss)
	if err != nil {
		return "", err
	}
	if r.ready != nil {
		if err := r.ready(ctx, session, st); err != nil {
			return "", err
		}
	}
	pane := ss.Pane
	r.logf("restart: session %s: %s %s is %s; hanging up pane %s", ss.Name, st.Harness, st.Session, st.State, pane)
	if err := r.exit(ctx, session, pane); err != nil {
		return "", err
	}
	newPane, err := r.resume(ctx, session)
	if err != nil {
		kind := errKind(err)
		if kind == "" {
			kind = api.ErrorKindUnavailable
		}
		r.logf("restart: session %s: %s exited; starting it again failed: %v", ss.Name, st.Harness, err)
		return "", control.Errorf(kind, "session %s: %s exited, and starting it again failed: %s. Its conversation %s is untouched: session.resume, or the harness's resume in its folder, starts it again", ss.Name, st.Harness, errText(err), st.Session)
	}
	r.logf("restart: session %s: %s %s started again in pane %s", ss.Name, st.Harness, st.Session, newPane)
	return newPane, nil
}

// restartable returns the agent state of a running session that may restart now.
func (r *restarter) restartable(ss api.Session) (api.AgentState, error) {
	st, ok := r.agent(ss.Pane)
	switch {
	case !ok || !slices.Contains(harness.Harnesses, st.Harness):
		return st, control.Errorf(api.ErrorKindUnavailable, "session %s runs no harness gff-hab can resume", ss.Name)
	case st.State != api.AgentStatusIdle && st.State != api.AgentStatusDone:
		return st, control.Errorf(api.ErrorKindConflict, "session %s: the agent is %s; a restart waits until it is idle or done", ss.Name, st.State)
	case st.Session == "":
		return st, control.Errorf(api.ErrorKindUnavailable, "session %s: %s has not reported its session id, so it cannot resume", ss.Name, st.Harness)
	}
	return st, nil
}

// exit hangs up the pane's program, the signal a pane close sends, and waits for it to end;
// then a TERM. A program that outlives both is left running and the restart fails.
func (r *restarter) exit(ctx context.Context, session, pane string) error {
	for _, step := range []struct {
		sig  hp.Sig
		wait time.Duration
	}{{hp.SigHUP, r.hupWait}, {hp.SigTERM, r.termWait}} {
		// The agent may have taken a prompt since the check: never stop a turn.
		if st, ok := r.agent(pane); step.sig == hp.SigHUP && ok && st.State != api.AgentStatusIdle && st.State != api.AgentStatusDone {
			return control.Errorf(api.ErrorKindConflict, "session %s: the agent became %s; nothing was stopped", session, st.State)
		}
		if err := r.signal(ctx, pane, step.sig); err != nil && !r.ended(session, pane) {
			r.logf("restart: session %s: signal %d to pane %s: %v", session, step.sig, pane, err)
		}
		if r.wait(ctx, session, pane, step.wait) {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return control.Errorf(api.ErrorKindUnavailable, "session %s: the harness did not exit after a hang-up and a TERM; it still runs, and nothing was restarted", session)
}

// ended reports whether the session's program in pane has ended.
func (r *restarter) ended(session, pane string) bool {
	ss, ok := r.store.Session(session)
	return !ok || ss.Pane != pane || ss.State != api.SessionStateRunning
}

// wait polls until the program in pane ends, for at most d.
func (r *restarter) wait(ctx context.Context, session, pane string, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if r.ended(session, pane) {
			return true
		}
		select {
		case <-ctx.Done():
			return r.ended(session, pane)
		case <-t.C:
			return r.ended(session, pane)
		case <-tick.C:
		}
	}
}

var _ control.Restarter = (*actions)(nil)

// RestartSession serves session.restart (restart.go).
func (a *actions) RestartSession(ctx context.Context, session string) (string, error) {
	if a.restart == nil {
		return "", control.Errorf(api.ErrorKindUnavailable, "this hab cannot restart sessions")
	}
	return a.restart.Restart(ctx, session)
}

// restartReady checks, before anything stops, that session.resume can start a session again
// with the agent's resume id in a known folder. A Capcom session resumes through Capcom, which
// keeps its own record.
func restartReady(store *structure.Store, book *resumeBook, folder func(context.Context, string) string,
	claudeTranscript func(string) bool) func(context.Context, string, api.AgentState) error {
	return func(ctx context.Context, session string, st api.AgentState) error {
		if ss, ok := store.Session(session); ok && ss.Kind == api.SessionKindCapcom {
			return nil
		}
		e, _ := book.get(session)
		e.Seen, e.Native = st.Harness, st.Session
		if _, ok, err := resumeStart(e, claudeTranscript); err != nil {
			return control.Errorf(api.ErrorKindUnavailable, "session %s: %v", session, err)
		} else if !ok {
			return control.Errorf(api.ErrorKindUnavailable, "session %s: how it starts again is not known", session)
		}
		if folder == nil || folder(ctx, session) == "" {
			return control.Errorf(api.ErrorKindUnavailable, "session %s: its folder is not known", session)
		}
		return nil
	}
}
