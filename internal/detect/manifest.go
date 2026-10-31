// Package detect tells which harness runs in a pane, and what a harness screen looks like it is
// doing (docs/specs/harness-adapters.md, Detection). It reads herdr's detection manifests as
// data: the process part matches a pane's foreground processes against each manifest's id and
// aliases; the screen part runs a manifest's rules over the screen text. The screen part is a
// fallback that never decides a permission answer.
//
// The manifests are vendored in manifests/ with their licence and source commit. A refresh
// writes newer copies into the hab's data directory; the vendored copies stay the fallback.
package detect

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// EngineVersion is the manifest shape this matcher reads. A manifest whose min_engine_version is
// higher is skipped. Version 3 adds the top_non_empty_lines region to version 2.
const EngineVersion = 3

// ScreenState is the coarse state a screen rule reports.
type ScreenState string

const (
	ScreenIdle    ScreenState = "idle"
	ScreenWorking ScreenState = "working"
	ScreenBlocked ScreenState = "blocked"
	ScreenUnknown ScreenState = "unknown"
)

// Manifest is one parsed and compiled agent manifest.
type Manifest struct {
	ID               string
	Version          string
	MinEngineVersion int
	UpdatedAt        string
	Aliases          []string
	Rules            []Rule
	Raw              []byte // the file as read, for diffs
}

// Rule is one compiled screen rule. A rule matches when every matcher in its gate holds over its
// region.
type Rule struct {
	ID         string
	State      ScreenState
	Priority   int
	Region     string
	Visible    bool // the rule's visible_* flag for its own state
	SkipUpdate bool // a match must not change the pane's state (an overlay such as a viewer)
	gate       gate
}

type gate struct {
	contains  []string // lower case; each must appear in the lower-cased text
	regex     []*regexp.Regexp
	lineRegex []*regexp.Regexp // each must match at least one line
	all       []gate
	any       []gate
	not       []gate
}

// The TOML shapes. Unknown keys are refused, so a manifest written for a newer engine that
// forgot to raise min_engine_version fails loudly instead of matching wrongly.
type rawManifest struct {
	ID               string    `toml:"id"`
	Version          string    `toml:"version"`
	MinEngineVersion int       `toml:"min_engine_version"`
	UpdatedAt        string    `toml:"updated_at"`
	Aliases          []string  `toml:"aliases"`
	Rules            []rawRule `toml:"rules"`
}

type rawGate struct {
	All       []rawGate `toml:"all"`
	Any       []rawGate `toml:"any"`
	Not       []rawGate `toml:"not"`
	Contains  []string  `toml:"contains"`
	Regex     []string  `toml:"regex"`
	LineRegex []string  `toml:"line_regex"`
}

type rawRule struct {
	ID              string `toml:"id"`
	State           string `toml:"state"`
	Priority        int    `toml:"priority"`
	Region          string `toml:"region"`
	VisibleIdle     bool   `toml:"visible_idle"`
	VisibleBlocker  bool   `toml:"visible_blocker"`
	VisibleWorking  bool   `toml:"visible_working"`
	SkipStateUpdate bool   `toml:"skip_state_update"`
	rawGate
}

// validID is the shape of a manifest id.
var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ErrEngine marks a manifest that needs a newer matcher than this one.
var ErrEngine = errors.New("needs a newer detection engine")

// Parse reads one manifest. It refuses unknown keys, unknown states and regions, rules with no
// matcher, regular expressions Go cannot compile, and a min_engine_version above EngineVersion
// (that error wraps ErrEngine).
func Parse(data []byte) (*Manifest, error) {
	var rm rawManifest
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&rm)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	if !validID.MatchString(rm.ID) {
		// The id names the file a refresh writes, so it must be a plain file name.
		return nil, fmt.Errorf("id %q is not lower-case letters, digits, - and _", rm.ID)
	}
	if err := checkVersion(rm.Version); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", rm.ID, err)
	}
	if rm.MinEngineVersion > EngineVersion {
		return nil, fmt.Errorf("manifest %s: min_engine_version %d: %w (this one is %d)", rm.ID, rm.MinEngineVersion, ErrEngine, EngineVersion)
	}
	m := &Manifest{
		ID:               rm.ID,
		Version:          strings.TrimSpace(rm.Version),
		MinEngineVersion: rm.MinEngineVersion,
		UpdatedAt:        rm.UpdatedAt,
		Aliases:          rm.Aliases,
		Raw:              append([]byte(nil), data...),
	}
	for _, rr := range rm.Rules {
		r, err := compileRule(rr, rm.MinEngineVersion)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: rule %s: %w", rm.ID, rr.ID, err)
		}
		m.Rules = append(m.Rules, r)
	}
	return m, nil
}

func compileRule(rr rawRule, minEngine int) (Rule, error) {
	if rr.ID == "" {
		return Rule{}, errors.New("no id")
	}
	r := Rule{ID: rr.ID, Priority: rr.Priority, Region: strings.TrimSpace(rr.Region), SkipUpdate: rr.SkipStateUpdate}
	switch ScreenState(rr.State) {
	case ScreenIdle, ScreenWorking, ScreenBlocked, ScreenUnknown:
		r.State = ScreenState(rr.State)
	case "":
		r.State = ScreenUnknown
	default:
		return Rule{}, fmt.Errorf("unknown state %q", rr.State)
	}
	if r.Region == "" {
		r.Region = "whole_recent"
	}
	if !validRegion(r.Region) {
		return Rule{}, fmt.Errorf("unknown region %q", r.Region)
	}
	if strings.HasPrefix(r.Region, "top_non_empty_lines(") && minEngine < 3 {
		return Rule{}, errors.New("top_non_empty_lines needs min_engine_version 3")
	}
	if r.SkipUpdate && r.State != ScreenUnknown {
		return Rule{}, errors.New("skip_state_update needs state unknown")
	}
	switch r.State {
	case ScreenIdle:
		r.Visible = rr.VisibleIdle
	case ScreenWorking:
		r.Visible = rr.VisibleWorking
	case ScreenBlocked:
		r.Visible = rr.VisibleBlocker
	}
	if !hasPositive(rr.rawGate) {
		return Rule{}, errors.New("no matcher")
	}
	g, err := compileGate(rr.rawGate)
	if err != nil {
		return Rule{}, err
	}
	r.gate = g
	return r, nil
}

func hasPositive(g rawGate) bool {
	return len(g.Contains) > 0 || len(g.Regex) > 0 || len(g.LineRegex) > 0 || len(g.All) > 0 || len(g.Any) > 0
}

func compileGate(rg rawGate) (gate, error) {
	var g gate
	for _, c := range rg.Contains {
		g.contains = append(g.contains, strings.ToLower(c))
	}
	var err error
	if g.regex, err = compileAll(rg.Regex); err != nil {
		return gate{}, err
	}
	if g.lineRegex, err = compileAll(rg.LineRegex); err != nil {
		return gate{}, err
	}
	for _, sets := range []struct {
		in  []rawGate
		out *[]gate
	}{{rg.All, &g.all}, {rg.Any, &g.any}, {rg.Not, &g.not}} {
		for _, n := range sets.in {
			if !hasPositive(n) && len(n.Not) == 0 {
				return gate{}, errors.New("empty nested gate")
			}
			cg, err := compileGate(n)
			if err != nil {
				return gate{}, err
			}
			*sets.out = append(*sets.out, cg)
		}
	}
	return g, nil
}

func compileAll(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(goRegex(p))
		if err != nil {
			return nil, fmt.Errorf("regex %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// match reports whether every part of g holds over text. lower is text in lower case.
func (g *gate) match(text, lower string, lines []string) bool {
	for _, c := range g.contains {
		if !strings.Contains(lower, c) {
			return false
		}
	}
	for _, re := range g.regex {
		if !re.MatchString(text) {
			return false
		}
	}
	for _, re := range g.lineRegex {
		hit := false
		for _, l := range lines {
			if re.MatchString(l) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for i := range g.all {
		if !g.all[i].match(text, lower, lines) {
			return false
		}
	}
	if len(g.any) > 0 {
		hit := false
		for i := range g.any {
			if g.any[i].match(text, lower, lines) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for i := range g.not {
		if g.not[i].match(text, lower, lines) {
			return false
		}
	}
	return true
}

// Input is what the screen rules read: the visible screen text (recent lines, top to bottom),
// the last OSC title and the last OSC 9;4 progress report. Empty strings mean none seen.
type Input struct {
	Screen      string
	OSCTitle    string
	OSCProgress string
}

// ScreenResult is the outcome of the screen rules for one pane.
type ScreenResult struct {
	Matched    bool        // some rule matched; false means State is unknown
	State      ScreenState // the matching rule's state
	Rule       string      // the matching rule's id
	Visible    bool        // the rule says the state is visible on screen
	SkipUpdate bool        // keep the pane's previous state (an overlay covers the screen)
}

// Screen runs m's rules over in. The rule with the highest priority wins; among equal
// priorities, the first in the file wins. No match gives state unknown.
func (m *Manifest) Screen(in Input) ScreenResult {
	best := -1
	for i := range m.Rules {
		r := &m.Rules[i]
		if best >= 0 && r.Priority <= m.Rules[best].Priority {
			continue // cannot win: a rule at this priority or higher already matched
		}
		text := region(in, r.Region)
		if r.gate.match(text, strings.ToLower(text), splitLines(text)) {
			best = i
		}
	}
	if best < 0 {
		return ScreenResult{State: ScreenUnknown}
	}
	r := m.Rules[best]
	return ScreenResult{Matched: true, State: r.State, Rule: r.ID, Visible: r.Visible, SkipUpdate: r.SkipUpdate}
}

// Names returns the manifest id and aliases, in lower case.
func (m *Manifest) Names() []string {
	out := []string{strings.ToLower(m.ID)}
	for _, a := range m.Aliases {
		out = append(out, strings.ToLower(a))
	}
	return out
}

// checkVersion requires a dotted numeric version such as 2026.10.11.1.
func checkVersion(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("no version")
	}
	for _, seg := range strings.Split(v, ".") {
		if seg == "" {
			return fmt.Errorf("version %q has an empty segment", v)
		}
		if _, err := strconv.ParseUint(seg, 10, 64); err != nil {
			return fmt.Errorf("version %q is not dotted numbers", v)
		}
	}
	return nil
}

// CompareVersions compares two dotted numeric versions segment by segment. Missing segments
// count as zero, so 1.2 equals 1.2.0. A segment that is not a number counts as zero.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(strings.TrimSpace(a), "."), strings.Split(strings.TrimSpace(b), ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y uint64
		if i < len(as) {
			x, _ = strconv.ParseUint(as[i], 10, 64)
		}
		if i < len(bs) {
			y, _ = strconv.ParseUint(bs[i], 10, 64)
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}
