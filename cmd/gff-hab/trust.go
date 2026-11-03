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

// Times of the trust watch and answer. Tests shorten them.
var (
	trustPoll     = 500 * time.Millisecond // how often a watched pane's screen is read
	trustNoDialog = 10 * time.Minute       // a watch that saw no dialog ends after this
	trustReadBack = 3 * time.Second        // how long an answer waits for the dialog to go
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
	// write sends bytes to a pane's program, as the hab.
	write func(ctx context.Context, pane string, data []byte) error
	// live reports whether the pane still runs a session's program.
	live func(pane string) bool
	// refuse ends the messages that wait for the pane's session and counts them; nil: none.
	refuse func(pane, reason string) (int, error)
	logf   func(string, ...any)

	mu        sync.Mutex
	watching  map[string]bool // panes watched now
	answering map[string]bool // panes a trust.answer runs on now
	declined  map[string]bool // panes whose prompt the owner answered exit
	done      chan struct{}
	closed    bool
}

var _ control.Trust = (*trustService)(nil)

func newTrustService(agents *harness.Store, screen func(string) ([]string, error), write func(context.Context, string, []byte) error, live func(string) bool, logf func(string, ...any)) *trustService {
	return &trustService{agents: agents, screen: screen, write: write, live: live, logf: logf,
		watching: map[string]bool{}, answering: map[string]bool{}, declined: map[string]bool{}, done: make(chan struct{})}
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

// run reads the pane's screen every trustPoll until the harness reports a state of its own,
// the pane goes, the dialog went, or trustNoDialog passes with no dialog.
func (t *trustService) run(pane, kind, folder string) {
	defer func() {
		t.mu.Lock()
		delete(t.watching, pane)
		t.mu.Unlock()
	}()
	start := time.Now()
	seen := false
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
		shown := err == nil && harness.TrustDialog(kind, strings.Join(rows, "\n"))
		switch {
		case shown:
			if !seen {
				t.logf("pane %s: %s asks to trust folder %s", pane, kind, folder)
			}
			seen = true
			t.agents.SetTrust(pane, kind, folder, true)
		case seen:
			t.agents.SetTrust(pane, kind, folder, false)
			return
		case time.Since(start) > trustNoDialog:
			return
		}
	}
}

// Declined reports whether the owner answered the pane's trust prompt with exit.
func (t *trustService) Declined(pane string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.declined[canonicalPane(pane)]
}

// Answer serves trust.answer (app.md 7.10): the pane must show blocked:trust and its screen
// the dialog, with one cursor row and one row with the chosen label; the keys move the cursor
// there and press Enter. It returns once the dialog left the screen.
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
		t.mu.Unlock()
	}()
	rows, err := t.screen(pane)
	if err != nil {
		return out, control.Errorf(api.ErrorKindUnavailable, "pane %s: reading the screen: %v", pane, err)
	}
	trust := p.Choice == api.TrustChoiceTrust
	keys, err := harness.TrustKeys(st.Harness, rows, trust)
	switch {
	case errors.Is(err, harness.ErrNoTrustDialog), errors.Is(err, harness.ErrTrustLayout):
		return out, control.Errorf(api.ErrorKindConflict, "pane %s: %v; answer it in the terminal", pane, err)
	case err != nil:
		return out, agentErr(err)
	}
	folder := ""
	if st.Trust != nil {
		folder = st.Trust.Folder
	}
	if !trust {
		// Before the keys: the exit ends the session, and a message must never be refused as
		// "exited" when its reason is the owner's no.
		t.mu.Lock()
		t.declined[pane] = true
		t.mu.Unlock()
	}
	if err := t.write(ctx, pane, keys); err != nil {
		if !trust {
			t.mu.Lock()
			delete(t.declined, pane)
			t.mu.Unlock()
		}
		return out, err
	}
	t.logf("pane %s: trust prompt for %s answered %s (client %s)", pane, folder, p.Choice, client)
	if !trust && t.refuse != nil {
		n, err := t.refuse(pane, reasonUntrusted)
		if err != nil {
			t.logf("pane %s: refusing the waiting messages: %v", pane, err)
		}
		out.Dropped = n
	}
	// Read back: the dialog leaves the screen, or the pane goes with the harness.
	deadline := time.Now().Add(trustReadBack)
	for {
		rows, err := t.screen(pane)
		if err != nil || !t.live(pane) || !harness.TrustDialog(st.Harness, strings.Join(rows, "\n")) {
			t.agents.SetTrust(pane, st.Harness, folder, false)
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, control.Errorf(api.ErrorKindUnavailable, "pane %s: the keys went in, but the trust dialog still shows after %s; answer it in the terminal", pane, trustReadBack)
		}
		select {
		case <-ctx.Done():
			return out, control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, ctx.Err())
		case <-time.After(trustPoll / 5):
		}
	}
}

// trustLive reports whether a pane still holds a running session, from the structure store.
func trustLive(sessionByPane func(string) (api.Session, bool)) func(string) bool {
	return func(pane string) bool {
		ss, ok := sessionByPane(pane)
		return ok && ss.State != api.SessionStateExited && ss.Pane == pane
	}
}

// errUntrusted is the refusal of a message to a session whose folder the owner did not trust.
var errUntrusted = fmt.Errorf("agent not running: %s", reasonUntrusted)
