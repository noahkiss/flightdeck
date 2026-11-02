package harness

import (
	"errors"
	"fmt"
	"regexp"
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
	// ErrAnswered: another answer to the request is on its way, or was sent.
	ErrAnswered = errors.New("the request is already answered")
	// ErrUnsupported: the harness has no adapter for this input.
	ErrUnsupported = errors.New("not supported for this harness")
)

// CheckSend returns the pane's state when a message may be delivered now: the agent is idle,
// done or stopped on an error, and no permission request is open. Otherwise it says why not,
// wrapping ErrNoAgent or ErrBusy. Like herdr, it trusts the displayed state, OSC 7501 included;
// only a hook or server opens and closes a permission request, so terminal output cannot end
// one a hook reported, unless Flightdeck already answered it.
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
	if p.ownerStop {
		return st, fmt.Errorf("%w: the owner interrupted %s; send when it reports it is at its prompt", ErrBusy, st.Harness)
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

// CheckAnswer returns the pane's state when request is its open permission request. Only a hook
// or server opens a request, so OSC 7501 or the screen alone never enables an answer
// (harness-adapters.md, rule 2), and neither can close one: a request stays answerable through
// a waiting hook whatever the terminal output says (an OSC clear). Whether keys may answer it
// is Shown.
func (s *Store) CheckAnswer(paneID, request string) (api.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	return s.checkAnswerLocked(p, paneID, request)
}

func (s *Store) checkAnswerLocked(p *pane, paneID, request string) (api.AgentState, error) {
	if p == nil || p.st.Revision == 0 {
		return api.AgentState{}, fmt.Errorf("%w: pane %s has no agent state", ErrNotOpen, paneID)
	}
	st := p.st
	switch {
	case p.open == "":
		return st, fmt.Errorf("%w: pane %s is %s, with no permission request open by its hooks", ErrNotOpen, paneID, describe(st))
	case p.open != request:
		return st, fmt.Errorf("%w: request %s is not open on pane %s; the open one is %s", ErrNotOpen, request, paneID, p.open)
	}
	return st, nil
}

// BeginAnswer is CheckAnswer, and claims the request for this answer: a second answer while
// one is on its way, or after one was sent, is refused with ErrAnswered, so it never sends keys
// into a dialog the first answer already closed. EndAnswer releases the claim.
func (s *Store) BeginAnswer(paneID, request string) (api.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	st, err := s.checkAnswerLocked(p, paneID, request)
	if err != nil {
		return st, err
	}
	switch request {
	case p.answering:
		return st, fmt.Errorf("%w: another answer to request %s is on its way", ErrAnswered, request)
	case p.answered:
		return st, fmt.Errorf("%w: request %s was answered and waits for %s; answer it in the terminal", ErrAnswered, request, st.Harness)
	}
	p.answering = request
	return st, nil
}

// Sent records that the answer BeginAnswer claimed reached the harness, or is about to: the
// keys go out next, or the waiting hook took the decision. From now on the next OSC 7501 idle
// or shell prompt may close the request (Store.Apply, Store.Prompt), since Claude fires no hook
// after a deny. Unsent withdraws it. EndAnswer still releases the claim.
func (s *Store) Sent(paneID, request string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.panes[paneID]; p != nil && p.answering == request {
		p.took = request
	}
}

// Unsent withdraws Sent: the keys could not be written, or the request was still open when the
// read-back gave up, so the answer may have landed somewhere else and the dialog may still be
// live. Terminal output again never closes the request.
func (s *Store) Unsent(paneID, request string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.panes[paneID]; p != nil && p.took == request {
		p.took = ""
	}
}

// EndAnswer releases BeginAnswer's claim. sent says the answer reached the harness, by keys or
// through its hook: the request then takes no other answer. Else another may try.
func (s *Store) EndAnswer(paneID, request string, sent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.answering != request {
		return
	}
	p.answering = ""
	if sent {
		p.answered = request
	}
}

// Shown reports whether the pane's display still shows request as its permission dialog, as a
// hook or server reported it, with no OSC 7501 report since that says otherwise. Only then may
// keys answer it: a key typed after the dialog closed lands in the prompt. A request that
// replaced another still open and unanswered is never shown: the harness may still show the
// first dialog (parallel tool calls), and a key would answer that one.
func (s *Store) Shown(paneID, request string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil {
		return false
	}
	st := p.st
	return st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindPermission && gateSource(st.Source) &&
		st.Request != nil && st.Request.ID == request && p.open == request && !p.stacked
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
	// OutcomeInterrupted: no hook closed the request, but the harness reported idle over OSC
	// 7501 after the answer: the dialog's Esc stopped the turn. That idle closes the request,
	// since Flightdeck answered it (Store.Sent).
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
		switch {
		case p.oscN <= mark:
		case p.oscLast == api.AgentStatusWorking:
			return OutcomeContinued
		case p.oscLast == api.AgentStatusIdle:
			return OutcomeInterrupted
		}
		return OutcomeOpen
	}
	r, ok := p.resolution(request)
	switch {
	case !ok || r.state == "" || r.state == api.AgentStatusExited:
		return OutcomeGone
	case gateSource(r.src):
		return OutcomeAnswered
	case r.src == api.StateSourceOsc && r.state == api.AgentStatusIdle:
		return OutcomeInterrupted // an OSC idle closed the answered request: the turn stopped
	}
	return OutcomeGone
}

func describe(st api.AgentState) string {
	if st.Kind != "" {
		return string(st.State) + ":" + string(st.Kind)
	}
	return string(st.State)
}

// InterruptKey reports whether data, typed by the owner, is the key that interrupts harness's
// turn: Esc or Ctrl-C for Claude Code, as plain bytes or in the kitty keyboard encoding. A paste
// is never a key.
func InterruptKey(harness string, data []byte) bool {
	if harness != detect.KindClaude || strings.HasPrefix(string(data), pasteStart) {
		return false
	}
	switch string(data) {
	case "\x1b", "\x03", "\x1b[27u", "\x1b[99;5u":
		return true
	}
	return false
}

// Bracketed paste markers.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// CleanText is the text a message write sends: CR LF and CR become LF, and control characters
// other than newline and tab (C0, DEL and C1) are removed. Text that must be judged as the agent
// will see it, such as a channel envelope, goes through it first.
func CleanText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f {
			return -1
		}
		return r
	}, text)
}

// MessageWrites returns the writes that deliver text as one message to harness: a bracketed
// paste of the text, then Enter as its own write. A multi-line message stays one message.
// The text goes through CleanText, so it can neither end the paste early nor press keys.
func MessageWrites(harness, text string) ([][]byte, error) {
	switch harness {
	case detect.KindClaude, detect.KindCodex:
	default:
		return nil, fmt.Errorf("%w: sending a message to %s", ErrUnsupported, harness)
	}
	clean := CleanText(text)
	if strings.TrimSpace(clean) == "" {
		return nil, errors.New("the message is empty")
	}
	return [][]byte{[]byte(pasteStart + clean + pasteEnd), []byte("\r")}, nil
}

// ImageMessageWrites delivers a message with images the way harness takes an image from its
// prompt (app.md 4.4).
//
//   - Claude Code splits a paste into lines and reads each line that is one absolute .png,
//     .jpg, .gif or .webp path as an image; other lines stay text. So the images go as path
//     lines after the text, in the one paste MessageWrites makes.
//   - Codex attaches an image when a paste is one image path and nothing else. So each image
//     goes as its own paste, then the text as one more, then Enter. A space, typed outside the
//     pastes, follows each image: should Codex keep a path as text, the path and what follows
//     stay apart.
//
// text holds the owner's text and any paths of files that are not images.
func ImageMessageWrites(harness, text string, images []string) ([][]byte, error) {
	if len(images) == 0 {
		return MessageWrites(harness, text)
	}
	switch harness {
	case detect.KindClaude:
		t := strings.TrimRight(text, "\n")
		if strings.TrimSpace(t) != "" {
			t += "\n\n"
		}
		return MessageWrites(harness, t+strings.Join(images, "\n"))
	case detect.KindCodex:
	default:
		return nil, fmt.Errorf("%w: sending a message to %s", ErrUnsupported, harness)
	}
	var out [][]byte
	for _, p := range images {
		clean := CleanText(strings.ReplaceAll(p, "\n", ""))
		if clean == "" {
			continue
		}
		out = append(out, []byte(pasteStart+clean+pasteEnd), []byte(" "))
	}
	if clean := CleanText(text); strings.TrimSpace(clean) != "" {
		out = append(out, []byte(pasteStart+clean+pasteEnd))
	}
	if len(out) == 0 {
		return nil, errors.New("the message is empty")
	}
	return append(out, []byte("\r")), nil
}

// ToolPlan is Claude Code's plan approval tool. Its dialog is not the usual Yes and No.
const ToolPlan = "ExitPlanMode"

// planManual is the label of the plan dialog option that approves the plan and leaves edits to
// ask, as before plan mode.
const planManual = "manually approve edits"

// optionLine is one numbered option of a select, inside a box or not, with the cursor or not.
var optionLine = regexp.MustCompile(`^[\s│]*(?:[❯›>]\s*)?([1-9])[.)]\s+(.+?)[\s│]*$`)

// PlanApproveKey returns the key that picks "Yes, manually approve edits" in Claude Code's
// plan dialog, read by its label from the visible rows. The dialog's first option clears the
// context and turns on auto-accept, so "1" never approves a plan. It reports false when no row
// or more than one row carries that label: the hab then sends nothing.
func PlanApproveKey(rows []string) ([]byte, bool) {
	var key []byte
	for _, row := range rows {
		m := optionLine.FindStringSubmatch(row)
		if m == nil || !strings.Contains(strings.ToLower(m[2]), planManual) {
			continue
		}
		if key != nil {
			return nil, false
		}
		key = []byte(m[1])
	}
	return key, key != nil
}

// PermissionKeys returns the keys that choose allow or deny in harness's permission dialog.
// Claude Code's dialog lists "1. Yes" first and takes Esc as No. Codex's lists "1. Yes,
// proceed" first, and takes Esc as "No, and tell Codex what to do differently", which stops
// the turn as Claude's Esc does.
func PermissionKeys(harness string, allow bool) ([]byte, error) {
	switch harness {
	case detect.KindClaude, detect.KindCodex:
		if allow {
			return []byte("1"), nil
		}
		return []byte("\x1b"), nil
	}
	return nil, fmt.Errorf("%w: answering a permission in %s's dialog", ErrUnsupported, harness)
}
