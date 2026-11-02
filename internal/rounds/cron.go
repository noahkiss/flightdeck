package rounds

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"time"
)

// Cron is a parsed 5-field cron expression (rounds.md 4.1): minute, hour, day of month, month,
// day of week. Each field is a bit set of the values it matches.
type Cron struct {
	expr                   string
	minute, hour, dom, mon uint64
	dow                    uint64
	// domAny and dowAny follow the classic cron rule: when both day fields are restricted, a
	// day matches when either matches; when one starts with "*", both must match.
	domAny, dowAny bool
}

type cronField struct {
	name     string
	min, max int
	names    []string // index 0 is the value min
}

var (
	monthNames = []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
	dayNames   = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}

	minuteField = cronField{name: "minute", min: 0, max: 59}
	hourField   = cronField{name: "hour", min: 0, max: 23}
	domField    = cronField{name: "day of month", min: 1, max: 31}
	monthField  = cronField{name: "month", min: 1, max: 12, names: monthNames}
	// Day of week takes 0 to 7; 7 is Sunday, as 0 is.
	dowField = cronField{name: "day of week", min: 0, max: 7, names: dayNames}
)

// ErrNeverRuns refuses a cron expression no date ever matches (0 0 31 2 *).
var ErrNeverRuns = errors.New("never runs")

// ParseCron parses a 5-field cron expression. Lists, ranges, steps and the three-letter month
// and day names are accepted, in any case. Seconds, "@" forms such as @reboot or @daily, and
// the non-standard L, W, # and ? are refused. It does not check that the expression ever
// matches; Schedule does, with a zone.
func ParseCron(expr string) (*Cron, error) {
	f := strings.Fields(expr)
	if len(f) == 1 && strings.HasPrefix(f[0], "@") {
		return nil, fmt.Errorf("cron %q: @ forms are not accepted; write the 5 fields or use a preset", expr)
	}
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), have %d", expr, len(f))
	}
	c := &Cron{expr: strings.Join(f, " ")}
	var err error
	if c.minute, err = minuteField.parse(f[0]); err != nil {
		return nil, err
	}
	if c.hour, err = hourField.parse(f[1]); err != nil {
		return nil, err
	}
	if c.dom, err = domField.parse(f[2]); err != nil {
		return nil, err
	}
	if c.mon, err = monthField.parse(f[3]); err != nil {
		return nil, err
	}
	if c.dow, err = dowField.parse(f[4]); err != nil {
		return nil, err
	}
	if c.dow&(1<<7) != 0 {
		c.dow = c.dow&^(1<<7) | 1
	}
	c.domAny = strings.HasPrefix(f[2], "*")
	c.dowAny = strings.HasPrefix(f[4], "*")
	return c, nil
}

// String returns the expression with its fields separated by single spaces.
func (c *Cron) String() string { return c.expr }

func (cf cronField) parse(s string) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(s, ",") {
		b, err := cf.parseItem(item)
		if err != nil {
			return 0, fmt.Errorf("cron %s field %q: %w", cf.name, s, err)
		}
		set |= b
	}
	return set, nil
}

func (cf cronField) parseItem(item string) (uint64, error) {
	if item == "" {
		return 0, errors.New("empty list item")
	}
	rng, stepText, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, ok := digits(stepText)
		if !ok || n < 1 {
			return 0, fmt.Errorf("bad step %q", stepText)
		}
		if n > cf.max-cf.min {
			return 0, fmt.Errorf("step %d is larger than the field", n)
		}
		step = n
	}
	lo, hi := cf.min, cf.max
	switch {
	case rng == "*":
	case strings.Contains(rng, "-"):
		a, b, _ := strings.Cut(rng, "-")
		var err error
		if lo, err = cf.value(a); err != nil {
			return 0, err
		}
		if hi, err = cf.value(b); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("range %q runs backwards", rng)
		}
	default:
		v, err := cf.value(rng)
		if err != nil {
			return 0, err
		}
		lo = v
		if !hasStep {
			hi = v // a single value; "a/s" runs from a to the end of the field
		}
	}
	var set uint64
	for v := lo; v <= hi; v += step {
		set |= 1 << uint(v)
	}
	return set, nil
}

func (cf cronField) value(s string) (int, error) {
	for i, n := range cf.names {
		if strings.EqualFold(s, n) {
			return cf.min + i, nil
		}
	}
	v, ok := digits(s)
	if !ok {
		return 0, fmt.Errorf("bad value %q", s)
	}
	if v < cf.min || v > cf.max {
		return 0, fmt.Errorf("value %d is outside %d-%d", v, cf.min, cf.max)
	}
	return v, nil
}

func has(set uint64, v int) bool { return set&(1<<uint(v)) != 0 }

// dayMatches applies the day-of-month, month and day-of-week fields to one civil date.
func (c *Cron) dayMatches(d time.Time) bool {
	if !has(c.mon, int(d.Month())) {
		return false
	}
	domOK := has(c.dom, d.Day())
	dowOK := has(c.dow, int(d.Weekday()))
	if c.domAny || c.dowAny {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// searchDays bounds the search for the next matching day. 29 February on one weekday recurs
// within 28 years in the Gregorian calendar, except across a skipped leap year; 40 years
// covers that.
const searchDays = 40 * 366

// next returns the first slot strictly after the instant after, in loc, under the DST rules of
// rounds.md 4.1:
//
//   - A local time that does not exist (the hour skipped in spring) runs at the first valid
//     local minute after it.
//   - A local time that happens twice (the hour repeated in autumn) runs once, at its first
//     occurrence.
//
// The walk is over civil (wall-clock) minutes from the minute after after's local time; ok is
// false when nothing matches within the search bound.
func (c *Cron) next(after time.Time, loc *time.Location) (time.Time, bool) {
	start := civilOf(after.In(loc)).Add(time.Minute)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < searchDays; i++ {
		d := day.AddDate(0, 0, i)
		if !c.dayMatches(d) {
			continue
		}
		for h := bitsFrom(c.hour, 0); h >= 0; h = bitsFrom(c.hour, h+1) {
			for m := bitsFrom(c.minute, 0); m >= 0; m = bitsFrom(c.minute, m+1) {
				civil := d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
				if civil.Before(start) {
					continue
				}
				t, ok := firstInstant(civil, loc)
				if ok && t.After(after) {
					return t.UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}

// bitsFrom returns the lowest set bit at or above from, or -1.
func bitsFrom(set uint64, from int) int {
	if from >= 64 {
		return -1
	}
	rest := set >> uint(from)
	if rest == 0 {
		return -1
	}
	return from + bits.TrailingZeros64(rest)
}

// civilOf returns t's wall clock, truncated to the minute, as a UTC time used for civil
// arithmetic only.
func civilOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// instants returns every instant whose wall clock in loc is the civil minute civil, earliest
// first: none in a spring gap, two in an autumn repeat.
func instants(civil time.Time, loc *time.Location) []time.Time {
	// Collect the zone's offsets near the civil time; one of them is in force at each
	// matching instant. Zones change offset at most a few times a year, so a two-day window
	// sampled every three hours finds every offset that could apply.
	var offsets []int
	for k := -8; k <= 8; k++ {
		_, off := civil.Add(time.Duration(k) * 3 * time.Hour).In(loc).Zone()
		seen := false
		for _, o := range offsets {
			seen = seen || o == off
		}
		if !seen {
			offsets = append(offsets, off)
		}
	}
	var out []time.Time
	for _, off := range offsets {
		t := civil.Add(-time.Duration(off) * time.Second).In(loc)
		if civilOf(t).Equal(civil) && t.Second() == 0 {
			dup := false
			for _, o := range out {
				dup = dup || o.Equal(t)
			}
			if !dup {
				out = append(out, t)
			}
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Before(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// firstInstant maps a civil minute to the instant a slot at that minute runs: its first
// occurrence, or, in a gap, the first valid minute after it. A gap longer than two days (a
// zone that skipped a whole day) is still bridged.
func firstInstant(civil time.Time, loc *time.Location) (time.Time, bool) {
	for i := 0; i < 3*24*60; i++ {
		if ts := instants(civil.Add(time.Duration(i)*time.Minute), loc); len(ts) > 0 {
			return ts[0], true
		}
	}
	return time.Time{}, false
}
