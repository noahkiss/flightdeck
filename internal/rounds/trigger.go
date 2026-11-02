package rounds

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TriggerKind says what asks for a run (rounds.md 4).
type TriggerKind string

const (
	TriggerCron   TriggerKind = "cron"
	TriggerOnce   TriggerKind = "once"
	TriggerEvent  TriggerKind = "event" // a later stage (rounds.md 4.3); refused for now
	TriggerManual TriggerKind = "manual"
)

// Trigger is a round's one trigger. A preset is compiled to Cron before it is stored.
type Trigger struct {
	Kind TriggerKind `json:"kind"`
	// Cron is the 5-field expression of a cron trigger.
	Cron string `json:"cron,omitempty"`
	// Zone is the IANA zone a cron or once trigger is read in.
	Zone string `json:"zone,omitempty"`
	// At is the instant of a once trigger, in UTC.
	At *time.Time `json:"at,omitempty"`
}

// Equal reports whether two triggers ask for the same slots.
func (t Trigger) Equal(u Trigger) bool {
	if t.Kind != u.Kind || t.Cron != u.Cron || t.Zone != u.Zone || (t.At == nil) != (u.At == nil) {
		return false
	}
	return t.At == nil || t.At.Equal(*u.At)
}

// Schedule computes a trigger's slots. A manual trigger has none.
type Schedule struct {
	kind TriggerKind
	cron *Cron
	loc  *time.Location
	at   time.Time
}

// Schedule compiles the trigger. A cron expression that never matches is refused with
// ErrNeverRuns; an event trigger is refused until its stage is built.
func (t Trigger) Schedule() (*Schedule, error) {
	switch t.Kind {
	case TriggerCron:
		c, err := ParseCron(t.Cron)
		if err != nil {
			return nil, err
		}
		loc, err := LoadZone(t.Zone)
		if err != nil {
			return nil, err
		}
		s := &Schedule{kind: t.Kind, cron: c, loc: loc}
		// The check needs no particular start: a date that matches recurs within the
		// search bound from any day.
		if _, ok := c.next(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), loc); !ok {
			return nil, fmt.Errorf("cron %q: %w", t.Cron, ErrNeverRuns)
		}
		return s, nil
	case TriggerOnce:
		if t.At == nil || t.At.IsZero() {
			return nil, errors.New("once: no instant")
		}
		if t.Zone != "" {
			if _, err := LoadZone(t.Zone); err != nil {
				return nil, err
			}
		}
		return &Schedule{kind: t.Kind, at: t.At.UTC()}, nil
	case TriggerManual:
		return &Schedule{kind: t.Kind}, nil
	case TriggerEvent:
		return nil, errors.New("event triggers are not supported yet")
	default:
		return nil, fmt.Errorf("unknown trigger kind %q", t.Kind)
	}
}

// Next returns the first slot strictly after the instant after, in UTC. ok is false when there
// is none: a manual trigger, a once trigger already past, or a cron with no slot in reach.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	switch s.kind {
	case TriggerCron:
		return s.cron.next(after, s.loc)
	case TriggerOnce:
		if s.at.After(after) {
			return s.at, true
		}
	}
	return time.Time{}, false
}

// NextN returns up to n slots after the instant after, for "the next three runs".
func (s *Schedule) NextN(after time.Time, n int) []time.Time {
	var out []time.Time
	for len(out) < n {
		t, ok := s.Next(after)
		if !ok {
			break
		}
		out = append(out, t)
		after = t
	}
	return out
}

// A preset is an interval that cron expresses exactly, so its slots fall at the same clock
// times every day (rounds.md 4.1). There is no rolling interval anchored to creation time.
type preset struct {
	every time.Duration
	name  string
	cron  string
}

var presets = []preset{
	{time.Minute, "1m", "* * * * *"},
	{2 * time.Minute, "2m", "*/2 * * * *"},
	{5 * time.Minute, "5m", "*/5 * * * *"},
	{10 * time.Minute, "10m", "*/10 * * * *"},
	{15 * time.Minute, "15m", "*/15 * * * *"},
	{20 * time.Minute, "20m", "*/20 * * * *"},
	{30 * time.Minute, "30m", "*/30 * * * *"},
	{time.Hour, "1h", "0 * * * *"},
	{2 * time.Hour, "2h", "0 */2 * * *"},
	{3 * time.Hour, "3h", "0 */3 * * *"},
	{4 * time.Hour, "4h", "0 */4 * * *"},
	{6 * time.Hour, "6h", "0 */6 * * *"},
	{8 * time.Hour, "8h", "0 */8 * * *"},
	{12 * time.Hour, "12h", "0 */12 * * *"},
	{24 * time.Hour, "1d", "0 0 * * *"},
}

// PresetCron compiles a preset such as "5m", "1h" or "1d" to its cron expression. Equal spans
// in another unit work too ("60m" is "1h"). A span cron cannot express is refused, naming the
// nearest presets that work.
func PresetCron(every string) (string, error) {
	d, err := parseSpan(every)
	if err != nil {
		return "", err
	}
	for _, p := range presets {
		if p.every == d {
			return p.cron, nil
		}
	}
	var below, above string
	for _, p := range presets {
		if p.every < d {
			below = p.name
		} else if above == "" {
			above = p.name
		}
	}
	var near []string
	for _, n := range []string{below, above} {
		if n != "" {
			near = append(near, n)
		}
	}
	return "", fmt.Errorf("every %s: cron cannot express it; nearest presets: %s", every, strings.Join(near, ", "))
}

// PresetName is the preset a cron expression compiles from, such as "1h", or "" when it is
// no preset's.
func PresetName(cron string) string {
	cron = strings.Join(strings.Fields(cron), " ")
	for _, p := range presets {
		if p.cron == cron {
			return p.name
		}
	}
	return ""
}

// maxSpan bounds a preset span well above the longest preset, so the multiplication cannot
// overflow and a long span is still refused with the nearest presets.
const maxSpan = 1000 * 24 * time.Hour

// parseSpan reads a whole number (digits only) and one unit: m, h or d.
func parseSpan(s string) (time.Duration, error) {
	bad := fmt.Errorf("every %q: want a number and m, h or d, such as 5m", s)
	if len(s) < 2 {
		return 0, bad
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[s[len(s)-1]]
	n, ok := digits(s[:len(s)-1])
	if unit == 0 || !ok || n < 1 || n > int(maxSpan/unit) {
		return 0, bad
	}
	return time.Duration(n) * unit, nil
}

// digits reads a decimal number of at most 9 digits with no sign: strconv.Atoi would also
// take "+5" and "-5".
func digits(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
