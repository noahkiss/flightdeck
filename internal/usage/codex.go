package usage

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// The Codex versions whose rollout token_count shape the parser was written and tested against.
// The rollout is not a documented format; a version outside this range still reads, and drift
// is reported for what does not parse.
const (
	CodexTestedMin = "0.159.0"
	CodexTestedMax = "0.166.0"
)

// codexMainLimit is the limit id of the account's own windows. Windows of another limit id (a
// model family with its own limit) are named with that id first.
const codexMainLimit = "codex"

// CodexUsage is the result of reading the end of one rollout.
type CodexUsage struct {
	Reading
	Version string   // session_meta cli_version, when the part read holds it
	Drift   []string // what the parser could not read, one line each; never a reading
}

// codexSnapshot is a token_count event's rate_limits.
type codexSnapshot struct {
	LimitID   string       `json:"limit_id"`
	LimitName *string      `json:"limit_name"`
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
	PlanType  *string      `json:"plan_type"`
}

type codexWindow struct {
	Used     *float64 `json:"used_percent"`
	Minutes  *int64   `json:"window_minutes"`
	ResetsAt *float64 `json:"resets_at"`
	// ResetsIn is the field before resets_at, in older versions: seconds after the event.
	ResetsIn *float64 `json:"resets_in_seconds"`
}

// ParseCodexRollout reads the token_count events in the end of a Codex rollout and returns the
// latest windows of each limit id. data may start mid-line: a first line that is not JSON is
// skipped. host and codexHome name the account key. Each window's read_at is the timestamp of
// the line that carried it, so a reading from an old rollout is never newer than it was.
func ParseCodexRollout(data []byte, host, codexHome string) CodexUsage {
	u := CodexUsage{}
	u.Harness = detect.KindCodex
	u.Key = Key(host, detect.KindCodex, codexHome)
	type latest struct {
		snap codexSnapshot
		at   time.Time
	}
	limits := map[string]latest{}
	var plan string
	var planAt time.Time
	drift := map[string]bool{}
	note := func(s string) {
		if !drift[s] {
			drift[s] = true
			u.Drift = append(u.Drift, s)
		}
	}
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil // the last line may still be written: a complete JSON line still counts
		}
		if bytes.Contains(line, []byte(`"session_meta"`)) {
			var m struct {
				Type    string `json:"type"`
				Payload struct {
					CLIVersion string `json:"cli_version"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &m) == nil && m.Type == "session_meta" && m.Payload.CLIVersion != "" {
				u.Version = m.Payload.CLIVersion
			}
			continue
		}
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var l struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type       string          `json:"type"`
				RateLimits json.RawMessage `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "event_msg" || l.Payload.Type != "token_count" {
			continue
		}
		if len(l.Payload.RateLimits) == 0 || string(l.Payload.RateLimits) == "null" {
			continue // an API-key login, or a version before rate limits
		}
		at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			note("token_count line has no RFC 3339 timestamp")
			continue
		}
		var s codexSnapshot
		if err := json.Unmarshal(l.Payload.RateLimits, &s); err != nil {
			note("rate_limits is not an object")
			continue
		}
		if s.LimitID == "" {
			s.LimitID = codexMainLimit // versions before limit ids had one limit
		}
		if prev, ok := limits[s.LimitID]; !ok || !at.Before(prev.at) {
			limits[s.LimitID] = latest{s, at}
		}
		if s.PlanType != nil && *s.PlanType != "" && !at.Before(planAt) {
			plan, planAt = *s.PlanType, at
		}
	}
	ids := make([]string, 0, len(limits))
	for id := range limits {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { // the account's own windows first
		if (ids[i] == codexMainLimit) != (ids[j] == codexMainLimit) {
			return ids[i] == codexMainLimit
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		l := limits[id]
		prefix := ""
		if id != codexMainLimit {
			prefix = id
			if l.snap.LimitName != nil && *l.snap.LimitName != "" {
				prefix = *l.snap.LimitName
			}
			prefix += " "
		}
		for _, w := range []*codexWindow{l.snap.Primary, l.snap.Secondary} {
			if w == nil {
				continue
			}
			uw, problem := codexUsageWindow(w, l.at)
			if problem != "" {
				note(problem)
				continue
			}
			uw.Name = prefix + uw.Name
			u.Windows = append(u.Windows, uw)
		}
	}
	u.Label = planLabel(plan)
	return u
}

// codexUsageWindow turns one rate-limit window into a usage window read at at, or says why it
// cannot.
func codexUsageWindow(w *codexWindow, at time.Time) (api.UsageWindow, string) {
	if w.Used == nil {
		return api.UsageWindow{}, "a rate-limit window has no used_percent"
	}
	used, ok := pct(*w.Used)
	if !ok {
		return api.UsageWindow{}, "a rate-limit window's used_percent is out of range"
	}
	var minutes int64
	if w.Minutes != nil {
		minutes = *w.Minutes
	}
	name := windowName(minutes)
	if name == "" {
		return api.UsageWindow{}, "a rate-limit window has no window_minutes"
	}
	uw := api.UsageWindow{Name: name, UsedPct: used, ReadAt: at.UTC().Format(time.RFC3339)}
	switch {
	case w.ResetsAt != nil:
		r, ok := epoch(*w.ResetsAt)
		if !ok {
			return api.UsageWindow{}, "a rate-limit window's resets_at is not Unix seconds"
		}
		uw.ResetsAt = r
	case w.ResetsIn != nil && *w.ResetsIn >= 0:
		uw.ResetsAt = at.Add(time.Duration(*w.ResetsIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return uw, ""
}

// planLabel turns a plan type such as "plus" or "pro" into a label: "Plus", "Pro".
func planLabel(p string) string {
	if p == "" || len(p) > 32 {
		return ""
	}
	r, n := utf8.DecodeRuneInString(p)
	return string(unicode.ToUpper(r)) + p[n:]
}
