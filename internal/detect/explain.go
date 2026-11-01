package detect

import "strings"

// RuleTrace is one screen rule as an explanation shows it: what it reads and whether it holds.
type RuleTrace struct {
	ID       string
	State    ScreenState
	Priority int
	Region   string
	Matched  bool
	Text     string // the region's text, as the rule read it
}

// ScreenTrace is Screen with its working shown: every rule, in file order, and the result.
type ScreenTrace struct {
	Result ScreenResult
	Winner int // index into Rules of the rule that won, or -1
	Rules  []RuleTrace
}

// ExplainScreen runs every rule of m over in, where Screen stops at the first rule that cannot
// win. Its Result is always the one Screen gives.
func (m *Manifest) ExplainScreen(in Input) ScreenTrace {
	t := ScreenTrace{Winner: -1, Result: ScreenResult{State: ScreenUnknown}}
	for i := range m.Rules {
		r := &m.Rules[i]
		text := region(in, r.Region)
		ok := r.gate.match(text, strings.ToLower(text), splitLines(text))
		t.Rules = append(t.Rules, RuleTrace{ID: r.ID, State: r.State, Priority: r.Priority, Region: r.Region, Matched: ok, Text: text})
		if ok && (t.Winner < 0 || r.Priority > m.Rules[t.Winner].Priority) {
			t.Winner = i
		}
	}
	if t.Winner >= 0 {
		r := m.Rules[t.Winner]
		t.Result = ScreenResult{Matched: true, State: r.State, Rule: r.ID, Visible: r.Visible, SkipUpdate: r.SkipUpdate}
	}
	return t
}

// VisibleText cuts what the rules read from a pane's plain text (scrollback, then the active
// screen, one line per row): the last rows lines, with trailing blank lines dropped. The
// manifests are written for the live bottom of the screen, and the blank rows under a fresh
// prompt would push it out of every bottom_lines(N) region.
func VisibleText(plain string, rows int) string {
	ls := splitLines(plain)
	if rows > 0 && len(ls) > rows {
		ls = ls[len(ls)-rows:]
	}
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	return strings.Join(ls, "\n")
}

// ProcessTrace is one foreground process as an explanation shows it.
type ProcessTrace struct {
	Process
	Leader bool
	Names  []string // the names it offers, in the order they are tried
	Match  string   // the manifest id the first matching name resolves to, or ""
}

// ExplainProcess is DetectProcess with its working shown: every process in the order it is
// tried, the names each offers, and which manifest each one's first matching name resolves to.
// The Detection and error are always the ones DetectProcess gives.
func (s *Set) ExplainProcess(fg Foreground) ([]ProcessTrace, Detection, error) {
	var out []ProcessTrace
	det, err := Detection{}, ErrNoAgent
	for i, p := range append([]Process{fg.Leader}, fg.Members...) {
		if i == 0 && p.PID == 0 && p.Name == "" && len(p.Args) == 0 {
			continue // the group's leader is gone; its members still count
		}
		pt := ProcessTrace{Process: p, Leader: i == 0, Names: processNames(p)}
		for _, n := range pt.Names {
			if id, ok := s.names[n]; ok {
				pt.Match = id
				if err != nil {
					det, err = Detection{ID: id, PID: p.PID, Name: n}, nil
				}
				break
			}
		}
		out = append(out, pt)
	}
	return out, det, err
}
