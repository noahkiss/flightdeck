package usage

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// sdkWindow is one window of the Claude Agent SDK's rate-limit info: utilization is a fraction
// of the limit (above 1 past it), resetsAt Unix seconds.
type sdkWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *float64 `json:"resetsAt"`
}

// SDKReading reads the rate_limit_info of a Claude Agent SDK rate_limit_event, as the Capcom
// engine hands it on (capcom-service.md 10.1). Two shapes carry windows: unifiedWindows, a
// window per key (five_hour, seven_day, seven_day_opus, ...), and the top-level utilization
// and resetsAt of the window rateLimitType names. A window in unifiedWindows wins over the
// top-level one of the same name. What does not parse is drift, as for the status line. at
// is when the reading arrived; it stamps each window.
func SDKReading(key string, raw json.RawMessage, at time.Time) Reading {
	r := Reading{Key: key, Harness: "claude", At: at}
	var info struct {
		RateLimitType  string               `json:"rateLimitType"`
		Utilization    *float64             `json:"utilization"`
		ResetsAt       *float64             `json:"resetsAt"`
		UnifiedWindows map[string]sdkWindow `json:"unifiedWindows"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		r.Drift = []string{fmt.Sprintf("rate_limit_info: %v", err)}
		r.Degrade = r.Drift
		return r
	}
	readAt := at.UTC().Format(time.RFC3339)
	seen := map[string]bool{}
	add := func(k string, w sdkWindow) {
		name, length, ok := claudeWindowName(k)
		if !ok || name == "spend" || seen[name] {
			return
		}
		if w.Utilization == nil || w.ResetsAt == nil {
			return // a window the server did not send this time
		}
		used, ok1 := pct(*w.Utilization * 100)
		reset, ok2 := epoch(*w.ResetsAt)
		if !ok1 || !ok2 {
			r.Drift = append(r.Drift, fmt.Sprintf("rate_limit_info %s: utilization %v, resetsAt %v", k, *w.Utilization, *w.ResetsAt))
			return
		}
		seen[name] = true
		uw := api.UsageWindow{Name: name, UsedPct: used, ResetsAt: reset, ReadAt: readAt}
		span(&uw, length)
		r.Windows = append(r.Windows, uw)
	}
	keys := make([]string, 0, len(info.UnifiedWindows))
	for k := range info.UnifiedWindows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k, info.UnifiedWindows[k])
	}
	if info.RateLimitType != "" && info.RateLimitType != "overage" {
		add(info.RateLimitType, sdkWindow{Utilization: info.Utilization, ResetsAt: info.ResetsAt})
	}
	// Drift with no window marks the account degraded; drift beside a window is logged only.
	if len(r.Windows) == 0 {
		r.Degrade = r.Drift
	}
	return r
}
