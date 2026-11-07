package harness

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// A harness's folder trust prompt (app.md 7.10). Claude Code and Codex ask whether to trust a
// folder they have not seen before they report any state, by hook or OSC 7501, so only the
// screen shows it.

// trustScreenRules are Flightdeck's own screen rules for trust prompts, in the detection
// manifest format. The vendored Claude manifest has no rule for the dialog, the vendored Codex
// rule does not ask for an option row, and a refresh replaces the vendored files, so the rules
// live here.
const trustScreenRules = `id = "flightdeck-trust"
version = "2026.11.6.1"
min_engine_version = 3
updated_at = "2026-11-06T00:00:00Z"

[[rules]]
id = "claude_trust_folder"
state = "blocked"
priority = 1000
region = "whole_recent"
visible_blocker = true
contains = ["yes, i trust this folder"]
any = [
  { contains = ["quick safety check"] },
  { contains = ["do you trust the files in this folder"] },
]

# Codex 0.167: "Trust this folder? Codex can read, edit, and run files here", then "1. Trust and
# continue" and "2. Back to Agent Command Center". Older builds: "Do you trust the contents of
# this directory?" with "1. Yes, continue" and "2. No, quit".
[[rules]]
id = "codex_trust_folder"
state = "blocked"
priority = 1000
region = "top_non_empty_lines(20)"
visible_blocker = true
any = [
  { all = [ { contains = ["folder access"] }, { contains = ["trust this folder?"] }, { contains = ["codex can read, edit, and run files here"] }, { contains = ["trust and continue"] } ] },
  { all = [ { regex = ['\A> You are in [^\r\n]+(?:\r?\n|$)'] }, { contains = ["do you trust the contents of this directory?"] }, { contains = ["yes, continue"] } ] },
]
`

var trustRules = sync.OnceValue(func() *detect.Manifest {
	m, err := detect.Parse([]byte(trustScreenRules))
	if err != nil {
		panic("harness: trust screen rules: " + err.Error())
	}
	return m
})

// TrustHarnesses are the harnesses whose trust prompt gff-hab reports and answers.
var TrustHarnesses = []string{detect.KindClaude, detect.KindCodex}

// trustRule is the screen rule of each harness's trust prompt.
var trustRule = map[string]string{
	detect.KindClaude: "claude_trust_folder",
	detect.KindCodex:  "codex_trust_folder",
}

// TrustDialog reports whether screen shows harness's folder trust prompt.
func TrustDialog(harness, screen string) bool {
	rule, ok := trustRule[harness]
	if !ok {
		return false
	}
	r := trustRules().Screen(detect.Input{Screen: screen})
	return r.Matched && r.Rule == rule
}

// Why a trust answer is refused. gff-hab maps both to conflict.
var (
	// ErrNoTrustDialog: the screen does not show the trust prompt now.
	ErrNoTrustDialog = errors.New("the pane no longer shows the trust dialog")
	// ErrTrustLayout: the dialog shows, but not with one cursor row and one row with the label.
	ErrTrustLayout = errors.New("the trust dialog does not show the expected options")
)

// trustLabels are the option labels of each harness's trust prompt, lower case: the trust row
// (true) and the other row (false). Claude Code 2.1.317 lists "No, exit" first and selected,
// then "Yes, I trust this folder", with no digits; earlier builds list "1. Yes, I trust this
// folder" first. Codex 0.167 lists "1. Trust and continue" first and selected, then "2. Back to
// Agent Command Center"; earlier builds "1. Yes, continue" and "2. No, quit".
var trustLabels = map[string]map[bool][]string{
	detect.KindClaude: {true: {"yes, i trust this folder"}, false: {"no, exit"}},
	detect.KindCodex:  {true: {"trust and continue", "yes, continue"}, false: {"back to agent command center", "no, quit"}},
}

// The keys of Claude Code's trust dialog. Each goes to the pane as its own write: a terminal UI
// may read one chunk of several keys as a paste.
var (
	KeyDown  = []byte("\x1b[B")
	KeyUp    = []byte("\x1b[A")
	KeyEnter = []byte("\r")
	// KeyEsc answers the dialog with exit whatever row the cursor is on: Claude Code 2.1.317
	// exits at Esc (checked live, 2026-11-02), and older builds say "Esc to exit".
	KeyEsc = []byte("\x1b")
)

// TrustMoves returns the arrow keys that move the cursor of harness's trust dialog to the row
// of the chosen answer, read from the visible rows, one key per entry; none when the cursor is
// there. The dialog must show, with exactly one cursor row (❯) among its options and exactly one
// row with the label; otherwise nothing is sent. The caller reads the screen again and sends
// Enter only when TrustCursorOn says the cursor sits on the label: a key typed in the terminal
// meanwhile must never turn one answer into the other.
func TrustMoves(harness string, rows []string, trust bool) ([][]byte, error) {
	cursor, target, err := trustLayout(harness, rows, trust)
	if err != nil {
		return nil, err
	}
	var keys [][]byte
	for d := target - cursor; d != 0; {
		if d > 0 {
			keys = append(keys, KeyDown)
			d--
		} else {
			keys = append(keys, KeyUp)
			d++
		}
	}
	return keys, nil
}

// TrustCursorOn reports whether the cursor of harness's trust dialog sits on the row of the
// chosen answer.
func TrustCursorOn(harness string, rows []string, trust bool) (bool, error) {
	cursor, target, err := trustLayout(harness, rows, trust)
	return err == nil && cursor == target, err
}

// TrustExitKey returns the key that answers harness's trust dialog with exit, whatever row
// the cursor is on. Codex has none: Esc and Ctrl-C go back from its dialog, and Codex runs on
// with the folder untrusted (0.167, checked live 2026-11-06), so exit is refused for it.
func TrustExitKey(harness string) ([]byte, error) {
	if harness != detect.KindClaude {
		return nil, fmt.Errorf("%w: answering a trust prompt with exit in %s; answer it in the terminal", ErrUnsupported, harness)
	}
	return KeyEsc, nil
}

// trustLayout finds the cursor row and the chosen answer's row among the dialog's options.
func trustLayout(harness string, rows []string, trust bool) (cursor, target int, err error) {
	labels, ok := trustLabels[harness]
	if !ok {
		return 0, 0, fmt.Errorf("%w: answering a trust prompt in %s", ErrUnsupported, harness)
	}
	if !TrustDialog(harness, strings.Join(rows, "\n")) {
		return 0, 0, ErrNoTrustDialog
	}
	cursor, target = -1, -1
	n := 0 // options seen: every row with a label of either answer
	for _, row := range rows {
		text, marked := trustOption(row)
		if text == "" {
			continue
		}
		yes, no := hasLabel(text, labels[true]), hasLabel(text, labels[false])
		if !yes && !no {
			continue
		}
		if marked {
			if cursor >= 0 {
				return 0, 0, ErrTrustLayout
			}
			cursor = n
		}
		if (trust && yes) || (!trust && no) {
			if target >= 0 {
				return 0, 0, ErrTrustLayout
			}
			target = n
		}
		n++
	}
	if cursor < 0 || target < 0 {
		return 0, 0, ErrTrustLayout
	}
	return cursor, target, nil
}

// trustOption returns an option row's text, lower case, without its cursor and number, and
// whether the cursor marks it.
func trustOption(row string) (string, bool) {
	t := strings.TrimSpace(strings.Trim(row, " │|\t"))
	marked := false
	for _, c := range []string{"❯", "›", ">"} {
		if strings.HasPrefix(t, c) {
			t, marked = strings.TrimSpace(strings.TrimPrefix(t, c)), true
			break
		}
	}
	if len(t) > 2 && t[0] >= '1' && t[0] <= '9' && (t[1] == '.' || t[1] == ')') {
		t = strings.TrimSpace(t[2:])
	}
	return strings.ToLower(t), marked
}

// hasLabel reports whether text is one of labels: the row starts with it.
func hasLabel(text string, labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(text, l) {
			return true
		}
	}
	return false
}

// SetTrust shows the pane's harness waiting on its folder trust prompt (shown), or, once the
// prompt went, starting (working) until the harness reports. Both have source screen, the
// weakest: the harness's first hook or report wins. It never covers a state a harness reported
// by hook, OSC 7501 or its server, and the prompt going changes only a pane that shows it. It
// reports whether the state changed.
func (s *Store) SetTrust(paneID, harness, folder string, shown bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if !shown {
		if p == nil || p.st.State != api.AgentStatusBlocked || p.st.Kind != api.BlockedKindTrust {
			return false
		}
		next := p.st
		next.State, next.Kind = api.AgentStatusWorking, ""
		next.Source = api.StateSourceScreen
		s.commitLocked(p, next)
		return true
	}
	if p != nil && p.st.Revision > 0 && (p.bound || p.hooked != "" || (p.st.Source != api.StateSourceScreen && p.st.State != api.AgentStatusNone)) {
		return false // the harness already speaks for itself
	}
	p = s.paneLocked(paneID, harness, api.StateSourceScreen)
	next := p.st
	next.Harness = harness
	next.State, next.Kind, next.Request, next.Last, next.Error = api.AgentStatusBlocked, api.BlockedKindTrust, nil, "", ""
	next.Trust = &api.TrustPrompt{Folder: folder}
	if p.st.Revision > 0 && same(p.st, next) {
		return false
	}
	next.Source = api.StateSourceScreen
	s.commitLocked(p, next)
	return true
}

// Trusting reports whether the pane shows the folder trust prompt, and its state.
func (s *Store) Trusting(paneID string) (api.AgentState, bool) {
	st, ok := s.Get(paneID)
	return st, ok && st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindTrust
}

// TrustStalled ends the starting state a trust prompt left: the prompt went, and the harness
// has not reported since (no hook, no OSC 7501), so its hooks are likely missing. The pane shows
// none with hooks missing, so a waiting message is refused with that reason rather than held as
// busy. Only a pane still in that state changes. It reports whether the state changed.
func (s *Store) TrustStalled(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panes[paneID]
	if p == nil || p.bound || p.hooked != "" || p.st.Source != api.StateSourceScreen || p.st.State != api.AgentStatusWorking {
		return false
	}
	next := p.st
	next.State, next.Hooks = api.AgentStatusNone, api.HookHealthMissing
	s.logf("pane %s: error: %s reported no state after its trust prompt; install its hook config (gff-hab hook -print %s)", paneID, p.st.Harness, p.st.Harness)
	s.commitLocked(p, next)
	return true
}
