package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/opencode"
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
	// answering, if set, hears of an answer about to go out and, through done, whether it did:
	// the push notifier names the client when it withdraws the banner (app.md 7.5).
	answering func(pane, request, client string, allow bool) (done func(ok bool))
	// opencode reaches the private server of an opencode pane gff-hab started: sends and
	// answers go through its API, not the terminal. Nil: none.
	opencode *opencodeServers

	mu      sync.Mutex
	sending map[string]bool // panes a session.send holds until the turn starts

	// holdMu makes a held hook's state change and its gate wait one step: an answer takes it
	// before it looks for the wait, so it never finds the request open but the wait missing.
	holdMu sync.Mutex
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
	if c, ok := a.opencodeClient(ss.Pane, st.Harness); ok {
		if err := a.promptOpencode(ctx, c, st, p.Text); err != nil {
			return err
		}
	} else {
		writes, err := harness.MessageWrites(st.Harness, p.Text)
		if err != nil {
			return agentErr(err)
		}
		if taken, err := a.writeAll(ctx, ss.Pane, writes...); err != nil {
			if taken == 0 {
				return err
			}
			// The paste is in the prompt but Enter was not sent: a retry must not paste it again.
			e := control.Errorf(api.ErrorKindUnavailable, "the message reached the prompt of session %s but Enter was not sent (%v); press Enter in the terminal, or clear the prompt and send again with a new key", p.Session, err)
			return control.Partial(e)
		}
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
// this request takes the answer as a structured decision; else the dialog gets keys, but only
// while the display still shows it. Either way Answer then waits up to answerReadBack to see
// the request resolved, and reports success only then (harness-adapters.md, Permission
// answers). It never answers on its own: a decision the hook could not take is reported, not
// retried as keys. One answer per request: a second one gets conflict, never keys.
func (a *agentService) Answer(ctx context.Context, client string, p api.ApprovalAnswerParams) (err error) {
	if p.Choice == api.ApprovalChoiceAnswer {
		return control.Errorf(api.ErrorKindUnavailable, "answering a question is not built yet; answer it in the terminal")
	}
	allow := p.Choice == api.ApprovalChoiceAllow
	if u, ok := parseUUID(p.Pane); ok {
		p.Pane = u.String() // the store keys panes by the canonical id
	}
	st, err := a.agents.BeginAnswer(p.Pane, p.Request)
	if err != nil {
		return agentErr(err)
	}
	if a.answering != nil {
		done := a.answering(p.Pane, p.Request, client, allow)
		defer func() { done(err == nil) }()
	}
	sent := false
	defer func() { a.agents.EndAnswer(p.Pane, p.Request, sent) }()
	mark := a.agents.OSCMark(p.Pane)
	changed := a.agents.Changed(p.Pane)
	how := "through the permission hook"
	a.holdMu.Lock() // a hook that opened this request has registered its wait by now
	written, ok := a.gate.Answer(p.Pane, p.Request, harness.Decision{Allow: allow})
	a.holdMu.Unlock()
	oc, server := a.opencode.Client(p.Pane)
	if !ok && server && st.Harness == detect.KindOpencode {
		how = "through the opencode server"
		sent = true // a reply that fails may still have reached the server
		a.agents.Sent(p.Pane, p.Request)
		if err := a.replyOpencode(ctx, oc, p, st, allow); err != nil {
			a.agents.Unsent(p.Pane, p.Request)
			return err
		}
	} else if ok {
		select {
		case err := <-written:
			if err != nil {
				return control.Errorf(api.ErrorKindUnavailable, "the permission hook for request %s ended before it took the answer (%v); %s shows its own dialog now: answer again", p.Request, err, st.Harness)
			}
			a.agents.Sent(p.Pane, p.Request)
		case <-ctx.Done():
			sent = true // the hook may still write it: never answer twice
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
		if !a.agents.Shown(p.Pane, p.Request) {
			// The request is open by its hooks, but the terminal says the harness is gone (an
			// OSC clear, perhaps forged): keys would land in a shell. Or it replaced a request
			// nobody answered, whose dialog may still be on screen: keys would answer that one.
			return control.Errorf(api.ErrorKindConflict, "pane %s may not show request %s on screen (the terminal said otherwise, or it came while another request was open); answer it in the terminal", p.Pane, p.Request)
		}
		keys, err := harness.PermissionKeys(st.Harness, allow)
		if err != nil {
			return agentErr(err)
		}
		sent = true // a write that fails may still have reached the dialog
		a.agents.Sent(p.Pane, p.Request)
		if err := a.write(ctx, p.Pane, keys); err != nil {
			a.agents.Unsent(p.Pane, p.Request)
			return err
		}
	}
	sent = true
	a.logf("pane %s: request %s answered %s %s (client %s, via %s)", p.Pane, p.Request, p.Choice, how, client, p.Via)
	err = a.readBack(ctx, p, allow, how, mark, changed)
	if err != nil {
		// The request may still be open with its dialog live, the answer having landed
		// elsewhere: terminal output must not close it.
		a.agents.Unsent(p.Pane, p.Request)
	}
	return err
}

// opencodeClient returns the client of the pane's opencode server when the pane runs opencode
// and gff-hab follows its server: messages to it go through the server, not the terminal.
func (a *agentService) opencodeClient(pane, harnessKind string) (*opencode.Client, bool) {
	if harnessKind != detect.KindOpencode {
		return nil, false
	}
	return a.opencode.Client(pane)
}

// opencodeTimeout bounds one call to an opencode server. Tests shorten it.
var opencodeTimeout = 5 * time.Second

// promptOpencode sends a message to the pane's opencode session through its server's prompt
// API (harness-adapters.md, opencode, built choices). The TUI is a client of the same server
// and shows the message as the owner's turn.
func (a *agentService) promptOpencode(ctx context.Context, c *opencode.Client, st api.AgentState, text string) error {
	if strings.TrimSpace(text) == "" {
		return control.Errorf(api.ErrorKindInvalid, "the message is empty")
	}
	if st.Session == "" {
		return control.Errorf(api.ErrorKindUnavailable, "opencode has no session yet; type the first message in its terminal")
	}
	ctx, cancel := context.WithTimeout(ctx, opencodeTimeout)
	defer cancel()
	// wrote turns true once any of the request reached the transport's writer: before that, the
	// server cannot have it.
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	if err := c.Prompt(ctx, st.Session, text); err != nil {
		if opencode.NotFound(err, opencode.TagSessionNotFound) {
			return permanent(control.Errorf(api.ErrorKindConflict, "opencode session %s is gone: %v", st.Session, err))
		}
		if promptMayHaveLanded(err, wrote.Load()) {
			// The server may have queued it: a retry must not send it again.
			e := control.Errorf(api.ErrorKindUnavailable, "the message may have reached opencode session %s, but its server gave no answer (%v); check the session before sending again with a new key", st.Session, err)
			return control.Partial(e)
		}
		if promptRefusedForGood(err) {
			return permanent(opencodeUnavailable(err))
		}
		return opencodeUnavailable(err)
	}
	return nil
}

// promptMayHaveLanded reports whether a failed prompt call may still have reached the server:
// a 5xx answer, or no answer to a request that was written, in part or whole. A 4xx refused it;
// a request never written (no connection, or a context that ended first) cannot have landed.
func promptMayHaveLanded(err error, wrote bool) bool {
	var ae *opencode.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return wrote
}

// promptRefusedForGood reports whether the server refused a prompt in a way a retry cannot fix:
// a 4xx other than 408, 409 and 429 (a 404 here is API drift; a gone session is handled first).
func promptRefusedForGood(err error) bool {
	var ae *opencode.APIError
	if !errors.As(err, &ae) || ae.Status < 400 || ae.Status >= 500 {
		return false
	}
	switch ae.Status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	}
	return true
}

// errPermanent marks a failure that sending again cannot fix. It rides beside the API error, so
// session.send answers the same kind; channel delivery refuses the message rather than hold it.
var errPermanent = errors.New("permanent")

type permanentError struct{ e error }

func (p permanentError) Error() string   { return p.e.Error() }
func (p permanentError) Unwrap() []error { return []error{p.e, errPermanent} }

func permanent(err error) error { return permanentError{err} }

// replyOpencode answers a permission request through the pane's opencode server: once for
// allow, reject for deny. A request a child session asked is answered under that session. A
// request the server no longer has was answered elsewhere (in the TUI) or ended.
func (a *agentService) replyOpencode(ctx context.Context, c *opencode.Client, p api.ApprovalAnswerParams, st api.AgentState, allow bool) error {
	session := firstNonEmpty(a.opencode.requestSession(p.Pane, p.Request), st.Session)
	decision, message := opencode.ReplyOnce, ""
	if !allow {
		decision, message = opencode.ReplyReject, "The owner denied this request from Flightdeck."
	}
	ctx, cancel := context.WithTimeout(ctx, opencodeTimeout)
	defer cancel()
	if err := c.Reply(ctx, session, p.Request, decision, message); err != nil {
		if opencode.NotFound(err, opencode.TagPermissionNotFound, opencode.TagSessionNotFound) {
			return control.Errorf(api.ErrorKindConflict, "opencode has no open request %s: it was answered in the terminal, or it ended", p.Request)
		}
		return opencodeUnavailable(err)
	}
	return nil
}

// opencodeUnavailable maps a failed opencode call to unavailable. A 404 without a known tag is
// a route the server lacks: the API moved under the adapter.
func opencodeUnavailable(err error) error {
	if opencode.Drift(err) {
		return control.Errorf(api.ErrorKindUnavailable, "the opencode server lacks this API route; its version differs from the tested %s (API drift): %v", opencode.TestedVersion, err)
	}
	return control.Errorf(api.ErrorKindUnavailable, "%v", err)
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
	_, err := a.writeAll(ctx, pane, writes...)
	return err
}

// writeAll is write, and also reports how many writes in a row, from the first, were taken.
func (a *agentService) writeAll(ctx context.Context, pane string, writes ...[]byte) (taken int, err error) {
	for _, wait := range a.input.QueueInputs(ctx, pane, writes...) {
		werr := wait()
		if werr == nil && err == nil {
			taken++
		}
		if werr != nil && err == nil {
			err = werr
		}
	}
	return taken, err
}

// holdPermission is the hook server's Holder: it holds a Claude Code or Codex PermissionRequest hook posted
// with -wait while the gate is on, a client that can answer is connected (approvers), and the
// request this very envelope opened is still open. A hook whose envelope opened nothing (a
// nested session's, which binding dropped) never waits, so it can never take another
// request's answer. The envelope's state change and the wait's registration happen under
// holdMu, so an answer that comes the moment the state is published finds the wait.
func (a *agentService) holdPermission(approvers func(pane string) int) hook.Holder {
	return func(pane string, env harness.Envelope, sink func() string) hook.Wait {
		a.holdMu.Lock()
		defer a.holdMu.Unlock()
		opened := sink()
		if opened == "" || !a.gate.Enabled() || !hook.Waits(env.Harness) || env.Event != "PermissionRequest" || approvers(pane) == 0 {
			return nil
		}
		if _, err := a.agents.CheckAnswer(pane, opened); err != nil {
			return nil
		}
		h := a.gate.Hold(pane, opened)
		return func(ctx context.Context) ([]byte, func(error)) {
			d, written, ok := h.Wait(ctx)
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

// releaseUnwatched ends the gate's waits on every pane no client that can answer watches any
// more, so the harness shows its own dialog at once instead of after the full wait. The control
// server calls it when a connection closes or changes its subscription.
func (a *agentService) releaseUnwatched(approvers func(pane string) int) {
	for _, pane := range a.gate.Panes() {
		if approvers(pane) == 0 {
			a.gate.Release(pane)
			a.logf("pane %s: no client can answer its permission request now; the hook stops waiting", pane)
		}
	}
}

// agentErr gives an adapter refusal its API kind.
func agentErr(err error) error {
	switch {
	case errors.Is(err, harness.ErrBusy), errors.Is(err, harness.ErrNotOpen), errors.Is(err, harness.ErrAnswered):
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
