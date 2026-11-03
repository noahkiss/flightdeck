package harness

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// A harness's folder trust prompt (app.md 7.10). Claude Code asks whether to trust a folder it
// has not seen before it reports any state, by hook or OSC 7501, so only the screen shows it.

// trustScreenRules are Flightdeck's own screen rules for trust prompts, in the detection
// manifest format. The vendored Claude manifest has no rule for the dialog, and a refresh
// replaces the vendored files, so the rule lives here.
const trustScreenRules = `id = "flightdeck-trust"
version = "2026.11.2.1"
min_engine_version = 2
updated_at = "2026-11-02T00:00:00Z"

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
`

var trustRules = sync.OnceValue(func() *detect.Manifest {
	m, err := detect.Parse([]byte(trustScreenRules))
	if err != nil {
		panic("harness: trust screen rules: " + err.Error())
	}
	return m
})

// TrustHarnesses are the harnesses whose trust prompt gff-hab reports and answers.
var TrustHarnesses = []string{detect.KindClaude}

// TrustDialog reports whether screen shows harness's folder trust prompt.
func TrustDialog(harness, screen string) bool {
	if harness != detect.KindClaude {
		return false
	}
	r := trustRules().Screen(detect.Input{Screen: screen})
	return r.Matched && r.Rule == "claude_trust_folder"
}

// Why a trust answer is refused. gff-hab maps both to conflict.
var (
	// ErrNoTrustDialog: the screen does not show the trust prompt now.
	ErrNoTrustDialog = errors.New("the pane no longer shows the trust dialog")
	// ErrTrustLayout: the dialog shows, but not with one cursor row and one row with the label.
	ErrTrustLayout = errors.New("the trust dialog does not show the expected options")
)

// trustLabels are the option labels that answer Claude Code's trust prompt, lower case. Claude
// Code 2.1.317 lists "No, exit" first and selected, then "Yes, I trust this folder", with no
// digits; earlier builds list "1. Yes, I trust this folder" first.
var trustLabels = map[bool][]string{
	true:  {"yes, i trust this folder"},
	false: {"no, exit"},
}

// TrustKeys returns the keys that pick the trust answer in harness's dialog, read from the
// visible rows: Up or Down from the cursor row (❯) to the row with the chosen label, then
// Enter. The dialog must show, with exactly one cursor row among its options and exactly one row
// with the label; otherwise nothing is sent.
func TrustKeys(harness string, rows []string, trust bool) ([]byte, error) {
	if harness != detect.KindClaude {
		return nil, fmt.Errorf("%w: answering a trust prompt in %s", ErrUnsupported, harness)
	}
	if !TrustDialog(harness, strings.Join(rows, "\n")) {
		return nil, ErrNoTrustDialog
	}
	cursor, target := -1, -1
	var options []int // rows that hold an option: every row with a label of either answer
	for i, row := range rows {
		text, marked := trustOption(row)
		if text == "" {
			continue
		}
		yes, no := hasLabel(text, trustLabels[true]), hasLabel(text, trustLabels[false])
		if !yes && !no {
			continue
		}
		options = append(options, i)
		if marked {
			if cursor >= 0 {
				return nil, ErrTrustLayout
			}
			cursor = len(options) - 1
		}
		if (trust && yes) || (!trust && no) {
			if target >= 0 {
				return nil, ErrTrustLayout
			}
			target = len(options) - 1
		}
	}
	if cursor < 0 || target < 0 {
		return nil, ErrTrustLayout
	}
	var keys []byte
	for d := target - cursor; d != 0; {
		if d > 0 {
			keys = append(keys, "\x1b[B"...)
			d--
		} else {
			keys = append(keys, "\x1b[A"...)
			d++
		}
	}
	return append(keys, '\r'), nil
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
