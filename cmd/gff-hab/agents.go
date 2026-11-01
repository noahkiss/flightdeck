package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// answerReadBack is how long an answer waits to see the permission request close.
const answerReadBack = 3 * time.Second

// sendStart is how long a delivered message holds its pane for the turn to start: until the
// agent leaves idle, done or error, another session.send to the pane is refused.
const sendStart = 3 * time.Second

// queuer queues writes to a pane's program in order; linkQueuer is one.
type queuer interface {
	// QueueInputs queues the writes back to back, with no other client's write between them,
	// and returns one wait per write.
	QueueInputs(ctx context.Context, pane string, data ...[]byte) []func() error
}

// linkQueuer queues writes through the holder link.
type linkQueuer struct{ lk *link.Link }

func (q linkQueuer) QueueInputs(ctx context.Context, pane string, data ...[]byte) []func() error {
	waits := make([]func() error, len(data))
	u, ok := parseUUID(pane)
	if !ok {
		for i := range waits {
			waits[i] = func() error { return control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", pane) }
		}
		return waits
	}
	for i, wait := range q.lk.QueueInputs(ctx, u, data...) {
		waits[i] = func() error { return inputErr(pane, wait()) }
	}
	return waits
}

// agentService serves session.send and approval.answer through the harness adapters
// (harness-adapters.md, Input delivery and Permission answers).
type agentService struct {
	store  *structure.Store
	agents *harness.Store
	gate   *harness.Gate
	input  queuer
	logf   func(string, ...any)

	mu      sync.Mutex
	sending map[string]bool // panes a session.send holds until the turn starts
}

var _ control.Agents = (*agentService)(nil)

// claim marks the pane as taking a message. It reports false when a send already holds it.
func (a *agentService) claim(pane string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending[pane] {
		return false
	}
	if a.sending == nil {
		a.sending = map[string]bool{}
	}
	a.sending[pane] = true
	return true
}

func (a *agentService) unclaim(pane string) {
	a.mu.Lock()
	delete(a.sending, pane)
	a.mu.Unlock()
}

// Send delivers a message as a bracketed paste, then Enter, only while the agent is idle, done
// or stopped on an error, with no permission request open. A busy agent refuses it with
// conflict; the hab queues nothing. One send at a time per pane: a second one while the first
// has not started its turn (sendStart) is refused with conflict too.
func (a *agentService) Send(ctx context.Context, client string, p api.SessionSendParams) error {
	if len(p.Attachments) > 0 {
		return control.Errorf(api.ErrorKindUnavailable, "session.send with attachments is not built yet; Part 6 stage 5 builds it")
	}
	ss, ok := a.store.Session(p.Session)
	if !ok {
		return control.Errorf(api.ErrorKindNotFound, "session %s does not exist", p.Session)
	}
	if ss.Pane == "" || ss.State == api.SessionStateExited {
		return control.Errorf(api.ErrorKindConflict, "session %s has exited; resume it first", p.Session)
	}
	if !a.claim(ss.Pane) {
		return control.Errorf(api.ErrorKindConflict, "another message to session %s is still starting its turn; send again when the agent is idle or done", p.Session)
	}
	held := false
	defer func() {
		if !held {
			a.unclaim(ss.Pane)
		}
	}()
	st, err := a.agents.CheckSend(ss.Pane)
	if err != nil {
		return agentErr(err)
	}
	writes, err := harness.MessageWrites(st.Harness, p.Text)
	if err != nil {
		return agentErr(err)
	}
	if err := a.write(ctx, ss.Pane, writes...); err != nil {
		return err
	}
	a.agents.Seen(ss.Pane)
	a.logf("session %s: message sent (%s, client %s)", p.Session, p.Origin, client)
	held = true
	go a.holdForTurn(ss.Pane)
	return nil
}

// holdForTurn keeps the pane claimed until its agent leaves idle, done and error, or sendStart
// passes, then frees it for the next send.
func (a *agentService) holdForTurn(pane string) {
	defer a.unclaim(pane)
	deadline := time.NewTimer(sendStart)
	defer deadline.Stop()
	for {
		changed := a.agents.Changed(pane)
		st, ok := a.agents.Get(pane)
		if !ok || (st.State != api.AgentStatusIdle && st.State != api.AgentStatusDone && st.State != api.AgentStatusError) {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			return
		}
	}
}

// Answer answers the pane's open permission request. A blocking permission hook that waits for
// this request takes the answer as a structured decision; else the dialog gets keys. Either
// way Answer then waits up to answerReadBack to see the request resolved, and reports success
// only then (harness-adapters.md, Permission answers). It never answers on its own: a decision
// the hook could not take is reported, not retried as keys.
func (a *agentService) Answer(ctx context.Context, client string, p api.ApprovalAnswerParams) error {
	if p.Choice == api.ApprovalChoiceAnswer {
		return control.Errorf(api.ErrorKindUnavailable, "answering a question is not built yet; answer it in the terminal")
	}
	allow := p.Choice == api.ApprovalChoiceAllow
	st, err := a.agents.CheckAnswer(p.Pane, p.Request)
	if err != nil {
		return agentErr(err)
	}
	mark := a.agents.OSCMark(p.Pane)
	changed := a.agents.Changed(p.Pane)
	how := "through the permission hook"
	if written, ok := a.gate.Answer(p.Pane, p.Request, harness.Decision{Allow: allow}); ok {
		select {
		case err := <-written:
			if err != nil {
				return control.Errorf(api.ErrorKindUnavailable, "the permission hook for request %s ended before it took the answer (%v); %s shows its own dialog now: answer again", p.Request, err, st.Harness)
			}
		case <-ctx.Done():
			return control.Errorf(api.ErrorKindUnavailable, "the permission hook for request %s did not take the answer: %v", p.Request, ctx.Err())
		}
	} else {
		how = "by keys"
		if d := a.gate.Settle(p.Pane, p.Request); d > 0 {
			// A hook for this request gave up just now: the dialog is still being drawn, and a
			// key sent now would land in the prompt.
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", p.Pane, ctx.Err())
			}
			if st, err = a.agents.CheckAnswer(p.Pane, p.Request); err != nil {
				return agentErr(err)
			}
			changed = a.agents.Changed(p.Pane)
		}
		keys, err := harness.PermissionKeys(st.Harness, allow)
		if err != nil {
			return agentErr(err)
		}
		if err := a.write(ctx, p.Pane, keys); err != nil {
			return err
		}
	}
	a.logf("pane %s: request %s answered %s %s (client %s, via %s)", p.Pane, p.Request, p.Choice, how, client, p.Via)
	return a.readBack(ctx, p, allow, how, mark, changed)
}

// readBack waits for the request to resolve after an answer. Success needs a resolution by a
// hook or server, the harness reporting work going on over OSC 7501, or, for a deny, the turn
// ending in idle (the dialog's Esc interrupts it). An exit, a session change, or a request
// still open after answerReadBack is unavailable.
func (a *agentService) readBack(ctx context.Context, p api.ApprovalAnswerParams, allow bool, how string, mark uint64, changed <-chan struct{}) error {
	deadline := time.NewTimer(answerReadBack)
	defer deadline.Stop()
	for {
		switch a.agents.Outcome(p.Pane, p.Request, mark) {
		case harness.OutcomeAnswered, harness.OutcomeContinued:
			return nil
		case harness.OutcomeInterrupted:
			if !allow {
				return nil
			}
			return control.Errorf(api.ErrorKindUnavailable, "request %s closed because the turn was interrupted; the tool did not run", p.Request)
		case harness.OutcomeGone:
			return control.Errorf(api.ErrorKindUnavailable, "request %s closed without the answer: the agent exited or its session changed", p.Request)
		}
		select {
		case <-changed:
			changed = a.agents.Changed(p.Pane)
		case <-deadline.C:
			return control.Errorf(api.ErrorKindUnavailable, "answer sent to pane %s %s, but request %s is still open after %v; answer it in the terminal", p.Pane, how, p.Request, answerReadBack)
		case <-ctx.Done():
			return control.Errorf(api.ErrorKindUnavailable, "answer sent to pane %s %s; %v", p.Pane, how, ctx.Err())
		}
	}
}

// write queues the writes to the pane back to back, then waits for all of them.
func (a *agentService) write(ctx context.Context, pane string, writes ...[]byte) error {
	var first error
	for _, wait := range a.input.QueueInputs(ctx, pane, writes...) {
		if err := wait(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// holdPermission is the hook server's Holder: it holds a Claude PermissionRequest hook posted
// with -wait while the gate is on, a client that can answer is connected (approvers), and the
// request this very envelope opened is still open. A hook whose envelope opened nothing (a
// nested session's, which binding dropped) never waits, so it can never take another
// request's answer.
func (a *agentService) holdPermission(approvers func(pane string) int) hook.Holder {
	return func(pane string, env harness.Envelope, opened string) hook.Wait {
		if opened == "" || !a.gate.Enabled() || env.Harness != detect.KindClaude || env.Event != "PermissionRequest" || approvers(pane) == 0 {
			return nil
		}
		if _, err := a.agents.CheckAnswer(pane, opened); err != nil {
			return nil
		}
		return func(ctx context.Context) ([]byte, func(error)) {
			d, written, ok := a.gate.Wait(ctx, pane, opened)
			if !ok {
				return nil, nil
			}
			out := map[string]string{"behavior": "deny", "message": "The owner denied this request from Flightdeck."}
			if d.Allow {
				out = map[string]string{"behavior": "allow"}
			}
			b, _ := json.Marshal(out)
			return b, written
		}
	}
}

// agentErr gives an adapter refusal its API kind.
func agentErr(err error) error {
	switch {
	case errors.Is(err, harness.ErrBusy), errors.Is(err, harness.ErrNotOpen):
		return control.Errorf(api.ErrorKindConflict, "%v", err)
	case errors.Is(err, harness.ErrNoAgent), errors.Is(err, harness.ErrUnsupported):
		return control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	return control.Errorf(api.ErrorKindInvalid, "%v", err)
}

// permissionWait picks how long a blocking permission hook waits for the app's answer:
// -permission-wait, else $GFF_HAB_PERMISSION_WAIT (a Go duration, or seconds), else 0, which
// keeps the hook off. The owner turns it on (harness-adapters.md, Open decisions).
func permissionWait(flagValue time.Duration, getenv func(string) string) (time.Duration, error) {
	d, from := flagValue, "-permission-wait"
	if d == 0 {
		v := getenv("GFF_HAB_PERMISSION_WAIT")
		if v == "" {
			return 0, nil
		}
		from = "GFF_HAB_PERMISSION_WAIT"
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			n, nerr := strconv.Atoi(v)
			if nerr != nil {
				return 0, fmt.Errorf("%s=%q: want a duration such as 30s", from, v)
			}
			d = time.Duration(n) * time.Second
		}
		if d == 0 {
			return 0, nil
		}
	}
	if d < time.Second || d > harness.MaxPermissionWait {
		return 0, fmt.Errorf("%s %v: must be 1s to %v, or 0 for off", from, d, harness.MaxPermissionWait)
	}
	return d, nil
}
