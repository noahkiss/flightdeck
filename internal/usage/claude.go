package usage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

// claudeWindows maps a status-line rate_limits key to a window name. Any other key is left out
// and reported as drift once.
var claudeWindows = map[string]string{
	"five_hour":   "5h",
	"seven_day":   "7d",
	"spend_limit": "spend",
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
		name, ok := claudeWindows[k]
		if !ok {
			st.Drift = append(st.Drift, "rate_limits."+k+" is not known")
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
		if name == "spend" && w.Period != "" {
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
