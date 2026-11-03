package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// The folder trust card (app.md 7.10): gff-hab watches the screen of each pane it starts with a
// harness until the harness first reports, shows its folder trust prompt as blocked:trust, and
// answers it from the dialog on screen when a client asks (trust.answer).

// Times and counts of the trust watch and answer. Tests shorten them.
var (
	trustPoll     = 500 * time.Millisecond // how often a watched pane's screen is read
	trustNoDialog = 10 * time.Minute       // a watch that saw no dialog ends after this
	trustStart    = time.Minute            // after the dialog went, the harness reports within this
	trustReadBack = 5 * time.Second        // how long an answer waits for its outcome
	trustCursor   = time.Second            // how long trust waits for the cursor to reach its row
	trustQuiet    = time.Second            // client input this recent refuses an answer
	trustSettle   = 2 * time.Second        // after an answer, the watch shows no prompt again
	trustKeyGap   = 30 * time.Millisecond  // between two keys of one answer
	trustMisses   = 3                      // reads in a row without the dialog before it counts as gone
)

// trustCapability is the capability word of a hab that serves trust.answer.
const trustCapability = "trust"

// reasonUntrusted is why messages to a session whose folder the owner did not trust are refused.
const reasonUntrusted = "the owner did not trust the folder; the agent exited"

// trustService watches new harness panes for a folder trust prompt and answers it.
type trustService struct {
	agents *harness.Store
	// screen reads a pane's visible rows.
	screen func(pane string) ([]string, error)
	// write sends one key to a pane's program, as the hab.
	write func(ctx context.Context, pane string, data []byte) error
	// live reports whether the pane still runs a session's program.
	live func(pane string) bool
	// refuse ends the messages that wait for the pane's session and counts them; nil: none.
	refuse func(pane, reason string) (int, error)
	logf   func(string, ...any)

	mu        sync.Mutex
	watching  map[string]bool      // panes watched now
	answering map[string]bool      // panes a trust.answer runs on now
	answered  map[string]time.Time // when the last answer to a pane ended
	exiting   map[string]bool      // panes an exit answer runs on: their messages wait
	declined  map[string]bool      // panes whose prompt the owner answered exit, and that exited
	queued    map[string]int       // client input to a pane not yet written
	typed     map[string]time.Time // when client input last reached a pane
	done      chan struct{}
	closed    bool
}

var _ control.Trust = (*trustService)(nil)

func newTrustService(agents *harness.Store, screen func(string) ([]string, error), write func(context.Context, string, []byte) error, live func(string) bool, logf func(string, ...any)) *trustService {
	return &trustService{agents: agents, screen: screen, write: write, live: live, logf: logf,
		watching: map[string]bool{}, answering: map[string]bool{}, answered: map[string]time.Time{},
		exiting: map[string]bool{}, declined: map[string]bool{}, queued: map[string]int{}, typed: map[string]time.Time{},
		done: make(chan struct{})}
}

// Close ends every watch.
func (t *trustService) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.done)
	}
}

// forget drops what the service holds about a pane that is gone.
func (t *trustService) forget(pane string) {
	pane = canonicalPane(pane)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.answered, pane)
	delete(t.declined, pane)
	delete(t.typed, pane)
	delete(t.queued, pane)
}

// inputStart and inputEnd bracket a client's pane.input: an answer waits until the pane is quiet.
func (t *trustService) inputStart(pane string) {
	pane = canonicalPane(pane)
	t.mu.Lock()
	t.queued[pane]++
	t.typed[pane] = time.Now()
	t.mu.Unlock()
}

func (t *trustService) inputEnd(pane string) {
	pane = canonicalPane(pane)
	t.mu.Lock()
	if t.queued[pane]--; t.queued[pane] <= 0 {
		delete(t.queued, pane)
	}
	t.typed[pane] = time.Now()
	t.mu.Unlock()
}

// busyInput reports why client input refuses an answer now, or "".
func (t *trustService) busyInput(pane string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.typed[pane]
	if t.queued[pane] > 0 && time.Since(at) < 10*time.Second {
		return "client input to the pane is still being written"
	}
	if ok && time.Since(at) < trustQuiet {
		return "a client typed into the pane a moment ago"
	}
	return ""
}

// show shows the prompt unless an answer runs or one ended just now: a screen read from before
// an answer's keys must not bring the prompt back. The check and the change are one step.
func (t *trustService) show(pane, kind, folder string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.answering[pane] || time.Since(t.answered[pane]) < trustSettle {
		return false
	}
	t.agents.SetTrust(pane, kind, folder, true)
	return true
}

// watch starts watching a pane gff-hab started with harness kind in folder, when that harness
// has a trust prompt Flightdeck knows.
func (t *trustService) watch(pane, kind, folder string) {
	if !slices.Contains(harness.TrustHarnesses, kind) {
		return
	}
	pane = canonicalPane(pane)
	t.mu.Lock()
	if t.closed || t.watching[pane] {
		t.mu.Unlock()
		return
	}
	t.watching[pane] = true
	t.mu.Unlock()
	go t.run(pane, kind, folder)
}

// run reads the pane's screen every trustPoll until the harness reports a state of its own or
// the pane goes. The dialog counts as gone only after trustMisses reads in a row without it, and
// may come back. A watch that saw no dialog ends after trustNoDialog; once the dialog went, the
// harness has trustStart to report, else the pane shows its hooks missing.
func (t *trustService) run(pane, kind, folder string) {
	defer func() {
		t.mu.Lock()
		delete(t.watching, pane)
		t.mu.Unlock()
	}()
	start := time.Now()
	var seen, gone time.Time
	misses, showing := 0, false
	tick := time.NewTicker(trustPoll)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
		}
		if st, ok := t.agents.Get(pane); ok && st.Source != api.StateSourceScreen {
			return // the harness speaks for itself now
		}
		if !t.live(pane) {
			return
		}
		rows, err := t.screen(pane)
		if err == nil && harness.TrustDialog(kind, strings.Join(rows, "\n")) {
			misses, gone = 0, time.Time{}
			if seen.IsZero() {
				t.logf("pane %s: %s asks to trust folder %s", pane, kind, folder)
			}
			seen = time.Now()
			if t.show(pane, kind, folder) {
				showing = true
			}
			continue
		}
		misses++
		switch {
		case showing && misses >= trustMisses:
			t.agents.SetTrust(pane, kind, folder, false)
			showing, gone = false, time.Now()
		case !seen.IsZero() && gone.IsZero() && !showing && misses >= trustMisses:
			gone = time.Now() // an answer cleared the prompt
		case !gone.IsZero() && time.Since(gone) > trustStart:
			t.agents.TrustStalled(pane)
			return
		case seen.IsZero() && time.Since(start) > trustNoDialog:
			return
		}
	}
}

// Declined reports whether the owner answered the pane's trust prompt with exit and the agent
// exited (declined), or such an answer runs now (pending): a message then waits.
func (t *trustService) Declined(pane string) (declined, pending bool) {
	pane = canonicalPane(pane)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.declined[pane], t.exiting[pane]
}

// Answer serves trust.answer (app.md 7.10). The pane must show blocked:trust and its screen
// the dialog, with one cursor row and one row with the chosen label, and no client input may be
// on its way to the pane. Exit sends Esc, which exits whatever row the cursor is on. Trust moves
// the cursor one key at a time, reads the screen again, and sends Enter only when the cursor sits
// on "Yes, I trust this folder". It returns once the outcome shows: the agent exited for exit;
// the dialog gone and the agent still running for trust.
func (t *trustService) Answer(ctx context.Context, client string, p api.TrustAnswerParams) (api.TrustAnswered, error) {
	var out api.TrustAnswered
	pane := canonicalPane(p.Pane)
	st, ok := t.agents.Trusting(pane)
	if !ok {
		return out, control.Errorf(api.ErrorKindConflict, "pane %s shows no folder trust prompt", pane)
	}
	t.mu.Lock()
	if t.answering[pane] {
		t.mu.Unlock()
		return out, control.Errorf(api.ErrorKindConflict, "another answer to the trust prompt of pane %s is on its way", pane)
	}
	t.answering[pane] = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.answering, pane)
		delete(t.exiting, pane)
		t.answered[pane] = time.Now()
		t.mu.Unlock()
	}()
	if why := t.busyInput(pane); why != "" {
		return out, control.Errorf(api.ErrorKindConflict, "pane %s: %s; answer again", pane, why)
	}
	trust := p.Choice == api.TrustChoiceTrust
	rows, err := t.screen(pane)
	if err != nil {
		return out, control.Errorf(api.ErrorKindUnavailable, "pane %s: reading the screen: %v", pane, err)
	}
	moves, err := harness.TrustMoves(st.Harness, rows, trust)
	if err != nil {
		return out, trustErr(pane, err)
	}
	folder := ""
	if st.Trust != nil {
		folder = st.Trust.Folder
	}
	if trust {
		if err := t.pick(ctx, pane, st.Harness, moves); err != nil {
			return out, err
		}
	} else {
		key, err := harness.TrustExitKey(st.Harness)
		if err != nil {
			return out, agentErr(err)
		}
		t.mu.Lock()
		t.exiting[pane] = true // messages wait: never refused as "exited" when the reason is the owner's no
		t.mu.Unlock()
		if err := t.write(ctx, pane, key); err != nil {
			return out, err
		}
	}
	t.logf("pane %s: trust prompt for %s answered %s (client %s)", pane, folder, p.Choice, client)
	if err := t.outcome(ctx, pane, st.Harness, trust); err != nil {
		return out, err
	}
	if trust {
		t.agents.SetTrust(pane, st.Harness, folder, false)
		return out, nil
	}
	t.mu.Lock()
	t.declined[pane] = true
	t.mu.Unlock()
	if t.refuse != nil {
		n, err := t.refuse(pane, reasonUntrusted)
		if err != nil {
			t.logf("pane %s: refusing the waiting messages: %v", pane, err)
		}
		out.Dropped = n
	}
	return out, nil
}

// pick moves the cursor to "Yes, I trust this folder" one key at a time, reads the screen again,
// and presses Enter only when the cursor sits there and no client input came meanwhile.
func (t *trustService) pick(ctx context.Context, pane, kind string, moves [][]byte) error {
	for _, k := range moves {
		if err := t.write(ctx, pane, k); err != nil {
			return err
		}
		if err := sleepCtx(ctx, trustKeyGap); err != nil {
			return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, err)
		}
	}
	deadline := time.Now().Add(trustCursor)
	for {
		rows, err := t.screen(pane)
		if err != nil {
			return control.Errorf(api.ErrorKindUnavailable, "pane %s: reading the screen: %v", pane, err)
		}
		on, err := harness.TrustCursorOn(kind, rows, true)
		if err != nil {
			return trustErr(pane, err)
		}
		if on {
			break
		}
		if time.Now().After(deadline) {
			return control.Errorf(api.ErrorKindConflict, "pane %s: the cursor did not reach \"Yes, I trust this folder\"; nothing was chosen; answer it in the terminal", pane)
		}
		if err := sleepCtx(ctx, trustPoll/10); err != nil {
			return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, err)
		}
	}
	if why := t.busyInput(pane); why != "" {
		return control.Errorf(api.ErrorKindConflict, "pane %s: %s; nothing was chosen; answer again", pane, why)
	}
	return t.write(ctx, pane, harness.KeyEnter)
}

// outcome waits up to trustReadBack for the answer's outcome. Exit: the agent's session ends.
// Trust: the dialog stays off the screen for two reads in a row, and the agent still runs.
func (t *trustService) outcome(ctx context.Context, pane, kind string, trust bool) error {
	deadline := time.Now().Add(trustReadBack)
	misses := 0
	for {
		live := t.live(pane)
		switch {
		case !trust && !live:
			return nil
		case trust && !live:
			return control.Errorf(api.ErrorKindUnavailable, "pane %s: the agent exited after the trust answer; start it again", pane)
		case trust:
			rows, err := t.screen(pane)
			if err == nil && !harness.TrustDialog(kind, strings.Join(rows, "\n")) {
				misses++
			} else {
				misses = 0
			}
			if misses >= 2 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if trust {
				return control.Errorf(api.ErrorKindUnavailable, "pane %s: the keys went in, but the trust dialog still shows after %s; answer it in the terminal", pane, trustReadBack)
			}
			return control.Errorf(api.ErrorKindUnavailable, "pane %s: Esc went in, but the agent still runs after %s; answer it in the terminal", pane, trustReadBack)
		}
		if err := sleepCtx(ctx, trustPoll/5); err != nil {
			return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, err)
		}
	}
}

// trustErr maps a refusal of the dialog's layout to conflict.
func trustErr(pane string, err error) error {
	if errors.Is(err, harness.ErrNoTrustDialog) || errors.Is(err, harness.ErrTrustLayout) {
		return control.Errorf(api.ErrorKindConflict, "pane %s: %v; answer it in the terminal", pane, err)
	}
	return agentErr(err)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// trustLive reports whether a pane still holds a running session, from the structure store.
func trustLive(sessionByPane func(string) (api.Session, bool)) func(string) bool {
	return func(pane string) bool {
		ss, ok := sessionByPane(pane)
		return ok && ss.State != api.SessionStateExited && ss.Pane == pane
	}
}

// trustInput notes each client's pane.input for the trust service: an answer waits until the
// pane is quiet.
type trustInput struct {
	control.Input
	trust *trustService
}

func (i trustInput) QueueInput(ctx context.Context, client, pane string, data []byte) func() error {
	i.trust.inputStart(pane)
	wait := i.Input.QueueInput(ctx, client, pane, data)
	return func() error {
		defer i.trust.inputEnd(pane)
		return wait()
	}
}

// errUntrusted is the refusal of a message to a session whose folder the owner did not trust.
var errUntrusted = fmt.Errorf("agent not running: %s", reasonUntrusted)
