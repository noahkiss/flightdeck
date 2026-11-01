package usage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// The Claude Code versions whose status-line input the parser was written and tested against.
// rate_limits is in the status-line input of recent versions only; an older version sends none,
// and the account then has no reading.
const (
	ClaudeTestedMin = "2.1.300"
	ClaudeTestedMax = "2.1.316"
)

// claudeWindows maps the known status-line rate_limits keys to window names and lengths. Another
// key whose value parses is a window too, named by claudeWindowName. A spend limit's length comes
// from its period (claudePeriods).
var claudeWindows = map[string]struct {
	name   string
	length time.Duration
}{
	"five_hour":   {"5h", 5 * time.Hour},
	"seven_day":   {"7d", 7 * 24 * time.Hour},
	"spend_limit": {"spend", 0},
}

// Number words and units of a rate_limits key such as seven_day_opus.
var (
	claudeNumbers = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "twelve": 12, "fourteen": 14, "thirty": 30}
	claudeUnits = map[string]struct {
		short string
		d     time.Duration
	}{
		"minute": {"m", time.Minute}, "minutes": {"m", time.Minute},
		"hour": {"h", time.Hour}, "hours": {"h", time.Hour},
		"day": {"d", 24 * time.Hour}, "days": {"d", 24 * time.Hour},
		"week": {"w", 7 * 24 * time.Hour}, "weeks": {"w", 7 * 24 * time.Hour},
	}
)

// claudePeriods maps a spend limit's period to the start of the period that ends at the reset
// time. A period not listed gives the window no length or start.
var claudePeriods = map[string]func(reset time.Time) time.Time{
	"daily":   func(r time.Time) time.Time { return r.AddDate(0, 0, -1) },
	"weekly":  func(r time.Time) time.Time { return r.AddDate(0, 0, -7) },
	"monthly": monthBefore,
}

// monthBefore is the same day and time one calendar month before r, in UTC, with the day
// clamped to the length of that month: 31 March gives 28 (or 29) February, not 3 March as
// AddDate would.
func monthBefore(r time.Time) time.Time {
	r = r.UTC()
	y, m, d := r.Date()
	last := time.Date(y, m, 0, 0, 0, 0, 0, time.UTC).Day() // the last day of the month before
	return time.Date(y, m-1, min(d, last), r.Hour(), r.Minute(), r.Second(), r.Nanosecond(), time.UTC)
}

// claudeWindowName names the window of a rate_limits key: a known key by claudeWindows, any
// other by its words, a leading length shortened: seven_day_opus is "7d opus". It also returns
// the length the key states (7 days for seven_day_opus), or 0 when the key states none. It
// refuses a key that is not lower-case words of letters and digits joined by "_", or is longer
// than 40 bytes.
func claudeWindowName(k string) (string, time.Duration, bool) {
	if w, ok := claudeWindows[k]; ok {
		return w.name, w.length, true
	}
	if !lowerWords(k, '_', 40) {
		return "", 0, false
	}
	words := strings.Split(k, "_")
	var length time.Duration
	if len(words) >= 2 {
		n, ok := claudeNumbers[words[0]]
		if !ok {
			n, _ = strconv.Atoi(words[0])
		}
		if u, ok := claudeUnits[words[1]]; ok && n > 0 {
			words = append([]string{strconv.Itoa(n) + u.short}, words[2:]...)
			if n <= 3660 { // in any unit; a larger n states no usable length
				length = time.Duration(n) * u.d
			}
		}
	}
	return strings.Join(words, " "), length, true
}

// lowerWords reports whether s is 1 to limit bytes of words of a-z and 0-9, each separated by
// one sep.
func lowerWords(s string, sep byte, limit int) bool {
	if s == "" || len(s) > limit || s[0] == sep || s[len(s)-1] == sep {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == sep && s[i-1] != sep:
		default:
			return false
		}
	}
	return true
}

// ClaudeStatus is the result of reading one status-line input.
type ClaudeStatus struct {
	Reading
	Version string // the version field, when present
}

// ParseClaudeStatus reads the rate_limits of one Claude Code status-line input (the JSON the
// statusLine command gets on stdin). at is when Claude Code ran the status line; it becomes
// each window's read_at. host names the hab, for the account key. With no rate_limits (an
// API-key account, a version before rate_limits, or before the first answer) the reading has no
// windows: the account reports none.
func ParseClaudeStatus(payload []byte, at time.Time, host string) (st ClaudeStatus, err error) {
	var in struct {
		TranscriptPath string          `json:"transcript_path"`
		Version        string          `json:"version"`
		RateLimits     json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return ClaudeStatus{}, fmt.Errorf("status-line input: %w", err)
	}
	st = ClaudeStatus{Version: in.Version}
	st.Harness = detect.KindClaude
	dataDir := claudeDataDir(in.TranscriptPath)
	st.Key = Key(host, detect.KindClaude, dataDir)
	st.At = at
	var known []string // drift in a known window: it marks the account degraded
	defer func() {
		switch {
		case len(st.Windows) == 0 && dataDir == "":
			// No data directory names no real account: the drift is logged only, so it makes
			// no account of its own.
		case len(st.Windows) == 0:
			st.Degrade = st.Drift
		default:
			st.Degrade = known
		}
	}()
	drift := func(k, s string) {
		st.Drift = append(st.Drift, s)
		if _, ok := claudeWindows[k]; ok {
			known = append(known, s)
		}
	}
	var limits map[string]json.RawMessage
	if len(in.RateLimits) > 0 && json.Unmarshal(in.RateLimits, &limits) != nil {
		// The input names the account, so a rate_limits of another shape marks it degraded.
		st.Drift = append(st.Drift, "rate_limits is not an object")
		return st, nil
	}
	readAt := at.UTC().Format(time.RFC3339)
	keys := make([]string, 0, len(limits))
	for k := range limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := limits[k]
		if string(raw) == "null" {
			continue
		}
		name, length, ok := claudeWindowName(k)
		if !ok {
			drift(k, "a rate_limits key is not a window name")
			continue
		}
		var w struct {
			Used   *float64 `json:"used_percentage"`
			Resets *float64 `json:"resets_at"`
			Period string   `json:"period"`
		}
		if err := json.Unmarshal(raw, &w); err != nil || w.Used == nil {
			drift(k, "rate_limits."+k+" has no used_percentage number")
			continue
		}
		used, ok := pct(*w.Used)
		if !ok {
			drift(k, "rate_limits."+k+".used_percentage is out of range")
			continue
		}
		uw := api.UsageWindow{Name: name, UsedPct: used, ReadAt: readAt}
		if w.Resets != nil {
			r, ok := epoch(*w.Resets)
			if !ok {
				drift(k, "rate_limits."+k+".resets_at is not Unix seconds")
				continue
			}
			uw.ResetsAt = r
		}
		if name == "spend" && lowerWords(w.Period, ' ', 24) {
			uw.Name = "spend " + w.Period
		}
		span(&uw, length)
		if start, ok := claudePeriods[w.Period]; ok && name == "spend" {
			if r, err := time.Parse(time.RFC3339, uw.ResetsAt); err == nil {
				s := start(r)
				uw.LengthS = int64(r.Sub(s) / time.Second)
				uw.StartsAt = s.Format(time.RFC3339)
			}
		}
		st.Windows = append(st.Windows, uw)
	}
	return st, nil
}

// claudeDataDir is the Claude Code data directory a transcript path lies in: the part before
// /projects/. It is only hashed into the account key.
func claudeDataDir(transcript string) string {
	transcript = filepath.ToSlash(transcript)
	if i := strings.LastIndex(transcript, "/projects/"); i > 0 {
		return transcript[:i]
	}
	return ""
}
