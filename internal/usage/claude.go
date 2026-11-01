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

// claudeWindows maps the known status-line rate_limits keys to window names. Another key whose
// value parses is a window too, named by claudeWindowName.
var claudeWindows = map[string]string{
	"five_hour":   "5h",
	"seven_day":   "7d",
	"spend_limit": "spend",
}

// Number words and units of a rate_limits key such as seven_day_opus.
var (
	claudeNumbers = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "twelve": 12, "fourteen": 14, "thirty": 30}
	claudeUnits = map[string]string{"minute": "m", "minutes": "m", "hour": "h", "hours": "h",
		"day": "d", "days": "d", "week": "w", "weeks": "w"}
)

// claudeWindowName names the window of a rate_limits key: a known key by claudeWindows, any
// other by its words, a leading length shortened: seven_day_opus is "7d opus". It refuses a key
// that is not lower-case words of letters and digits joined by "_", or is longer than 40 bytes.
func claudeWindowName(k string) (string, bool) {
	if n, ok := claudeWindows[k]; ok {
		return n, true
	}
	if !lowerWords(k, '_', 40) {
		return "", false
	}
	words := strings.Split(k, "_")
	if len(words) >= 2 {
		n, ok := claudeNumbers[words[0]]
		if !ok {
			n, _ = strconv.Atoi(words[0])
		}
		if u, ok := claudeUnits[words[1]]; ok && n > 0 {
			words = append([]string{strconv.Itoa(n) + u}, words[2:]...)
		}
	}
	return strings.Join(words, " "), true
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
	Version string   // the version field, when present
	Drift   []string // what the parser could not read, one line each; never a reading
}

// ParseClaudeStatus reads the rate_limits of one Claude Code status-line input (the JSON the
// statusLine command gets on stdin). at is when Claude Code ran the status line; it becomes
// each window's read_at. host names the hab, for the account key. With no rate_limits (an
// API-key account, a version before rate_limits, or before the first answer) the reading has no
// windows: the account reports none.
func ParseClaudeStatus(payload []byte, at time.Time, host string) (ClaudeStatus, error) {
	var in struct {
		TranscriptPath string                     `json:"transcript_path"`
		Version        string                     `json:"version"`
		RateLimits     map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return ClaudeStatus{}, fmt.Errorf("status-line input: %w", err)
	}
	st := ClaudeStatus{Version: in.Version}
	st.Harness = detect.KindClaude
	st.Key = Key(host, detect.KindClaude, claudeDataDir(in.TranscriptPath))
	readAt := at.UTC().Format(time.RFC3339)
	keys := make([]string, 0, len(in.RateLimits))
	for k := range in.RateLimits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := in.RateLimits[k]
		if string(raw) == "null" {
			continue
		}
		name, ok := claudeWindowName(k)
		if !ok {
			st.Drift = append(st.Drift, "a rate_limits key is not a window name")
			continue
		}
		var w struct {
			Used   *float64 `json:"used_percentage"`
			Resets *float64 `json:"resets_at"`
			Period string   `json:"period"`
		}
		if err := json.Unmarshal(raw, &w); err != nil || w.Used == nil {
			st.Drift = append(st.Drift, "rate_limits."+k+" has no used_percentage number")
			continue
		}
		used, ok := pct(*w.Used)
		if !ok {
			st.Drift = append(st.Drift, "rate_limits."+k+".used_percentage is out of range")
			continue
		}
		uw := api.UsageWindow{Name: name, UsedPct: used, ReadAt: readAt}
		if w.Resets != nil {
			r, ok := epoch(*w.Resets)
			if !ok {
				st.Drift = append(st.Drift, "rate_limits."+k+".resets_at is not Unix seconds")
				continue
			}
			uw.ResetsAt = r
		}
		if name == "spend" && lowerWords(w.Period, ' ', 24) {
			uw.Name = "spend " + w.Period
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
