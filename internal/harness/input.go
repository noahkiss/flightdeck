package harness

import (
	"errors"
	"fmt"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// Why an input or a permission answer is refused (harness-adapters.md, Input delivery and
// Permission answers). gff-hab maps each to an API error kind.
var (
	// ErrNoAgent: the pane has no harness, or it exited. Start or resume one.
	ErrNoAgent = errors.New("no agent runs in the pane")
	// ErrBusy: the agent is working or waits for the owner, so a message would land mid-turn.
	ErrBusy = errors.New("the agent is busy")
	// ErrNotOpen: the request is not the open permission request of the pane.
	ErrNotOpen = errors.New("no such open permission request")
	// ErrUnsupported: the harness has no adapter for this input.
	ErrUnsupported = errors.New("not supported for this harness")
)

// CheckSend returns the pane's state when a message may be delivered now: the agent is idle,
// done or stopped on an error, and no permission request is open. Otherwise it says why not,
// wrapping ErrNoAgent or ErrBusy.
func (s *Store) CheckSend(paneID string) (api.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.Revision == 0 {
		return api.AgentState{}, fmt.Errorf("%w: no agent state for pane %s; start or resume the harness", ErrNoAgent, paneID)
	}
	st := p.st
	if p.open != "" && st.State != api.AgentStatusExited {
		// Whatever the state says: Enter on a live permission dialog would approve it.
		return st, fmt.Errorf("%w: %s waits for permission request %s; answer that first", ErrBusy, st.Harness, p.open)
	}
	switch st.State {
	case api.AgentStatusIdle, api.AgentStatusDone:
		return st, nil
	case api.AgentStatusWorking:
		return st, fmt.Errorf("%w: %s is working; send when it is idle or done", ErrBusy, st.Harness)
	case api.AgentStatusBlocked:
		what := string(st.Kind)
		if what == "" {
			what = "the owner"
		}
		return st, fmt.Errorf("%w: %s waits for %s; answer that first", ErrBusy, st.Harness, what)
	case api.AgentStatusError:
		// The turn failed and stopped: the harness is at its prompt again.
		return st, nil
	}
	return st, fmt.Errorf("%w: the pane's agent state is %s; start or resume the harness", ErrNoAgent, st.State)
}

// CheckAnswer returns the pane's state when request is its open permission request, reported by
// a hook or a server, never by the screen or OSC 7501 alone (harness-adapters.md, rule 2).
func (s *Store) CheckAnswer(paneID, request string) (api.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.st.Revision == 0 {
		return api.AgentState{}, fmt.Errorf("%w: pane %s has no agent state", ErrNotOpen, paneID)
	}
	st := p.st
	switch {
	case st.State != api.AgentStatusBlocked || st.Kind != api.BlockedKindPermission:
		return st, fmt.Errorf("%w: pane %s is %s, not blocked on a permission", ErrNotOpen, paneID, describe(st))
	case st.Source != api.StateSourceHook && st.Source != api.StateSourceServer:
		return st, fmt.Errorf("%w: pane %s looks blocked from %s output only; answer in the terminal", ErrNotOpen, paneID, st.Source)
	case st.Request == nil || st.Request.ID == "" || p.open != st.Request.ID:
		return st, fmt.Errorf("%w: pane %s has no open request with an id", ErrNotOpen, paneID)
	case st.Request.ID != request:
		return st, fmt.Errorf("%w: request %s is not open on pane %s; the open one is %s", ErrNotOpen, request, paneID, st.Request.ID)
	}
	return st, nil
}

// Outcome is what became of a permission request after an answer was sent (Store.Outcome).
type Outcome int

const (
	// OutcomeOpen: the request is still open, and nothing says the turn went on.
	OutcomeOpen Outcome = iota
	// OutcomeAnswered: a hook or server closed the request (PostToolUse, a resolution, the next
	// prompt or the end of the turn).
	OutcomeAnswered
	// OutcomeContinued: the request is not closed yet, but the harness reported working over
	// OSC 7501 after the answer: the dialog is gone and a long tool runs.
	OutcomeContinued
	// OutcomeInterrupted: an OSC idle closed it; the owner left the dialog and the turn stopped.
	OutcomeInterrupted
	// OutcomeGone: the harness exited or its session changed, or the request is unknown.
	OutcomeGone
)

// OSCMark returns the pane's count of OSC 7501 reports, for Outcome.
func (s *Store) OSCMark(paneID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.panes[paneID]; p != nil {
		return p.oscN
	}
	return 0
}

// Outcome says what became of request since mark (from OSCMark, taken before the answer).
func (s *Store) Outcome(paneID, request string, mark uint64) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	switch {
	case p == nil:
		return OutcomeGone
	case p.open == request:
		if p.oscN > mark && p.oscLast == api.AgentStatusWorking {
			return OutcomeContinued
		}
		return OutcomeOpen
	}
	r, ok := p.resolution(request)
	switch {
	case !ok || r.state == "" || r.state == api.AgentStatusExited:
		return OutcomeGone
	case r.src == api.StateSourceHook || r.src == api.StateSourceServer:
		return OutcomeAnswered
	case r.src == api.StateSourceOsc && r.state == api.AgentStatusIdle:
		return OutcomeInterrupted
	}
	return OutcomeGone
}

func describe(st api.AgentState) string {
	if st.Kind != "" {
		return string(st.State) + ":" + string(st.Kind)
	}
	return string(st.State)
}

// Bracketed paste markers.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// MessageWrites returns the writes that deliver text as one message to harness: a bracketed
// paste of the text, then Enter as its own write. A multi-line message stays one message.
// Control characters other than newline and tab are removed, so the text can neither end the
// paste early nor press keys; CR LF and CR become LF.
func MessageWrites(harness, text string) ([][]byte, error) {
	switch harness {
	case detect.KindClaude:
	default:
		return nil, fmt.Errorf("%w: sending a message to %s", ErrUnsupported, harness)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f {
			return -1
		}
		return r
	}, text)
	if strings.TrimSpace(clean) == "" {
		return nil, errors.New("the message is empty")
	}
	return [][]byte{[]byte(pasteStart + clean + pasteEnd), []byte("\r")}, nil
}

// PermissionKeys returns the keys that choose allow or deny in harness's permission dialog.
// Claude Code's dialog lists "1. Yes" first and takes Esc as No.
func PermissionKeys(harness string, allow bool) ([]byte, error) {
	switch harness {
	case detect.KindClaude:
		if allow {
			return []byte("1"), nil
		}
		return []byte("\x1b"), nil
	}
	return nil, fmt.Errorf("%w: answering a permission in %s's dialog", ErrUnsupported, harness)
}
