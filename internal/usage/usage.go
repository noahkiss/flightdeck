// Package usage turns what each harness reports about its account's rate-limit windows into
// usage readings, and keeps the latest reading per account (docs/specs/harness-adapters.md,
// Usage; app.md 14). It reads only what a harness already hands over: Claude Code's status-line
// input and Codex's rollout token_count events. It never reads a credential file and never
// calls a usage endpoint.
package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Key names an account. No harness source gives an account id, so the key is a hash of the hab,
// the harness and the harness's data directory: two accounts on one hab with separate data
// directories get two keys. It is stable across restarts and says nothing about the account.
func Key(host, harness, dataDir string) string {
	h := sha256.Sum256([]byte(host + "\x00" + harness + "\x00" + dataDir))
	return harness + "-" + hex.EncodeToString(h[:6])
}

// pct turns a reported percentage into whole percent, rounded down: 99.6 is not yet full. It
// refuses a value that is not a finite number at or above zero; a value above 100 stays (a
// spend limit can pass its limit).
func pct(v float64) (int, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e6 {
		return 0, false
	}
	return int(math.Floor(v)), true
}

// epoch turns Unix seconds into an RFC 3339 time in UTC. It refuses a value before 2001 or past
// 2200: such a number is a field whose unit changed.
func epoch(s float64) (string, bool) {
	if math.IsNaN(s) || s < 978307200 || s > 7258118400 {
		return "", false
	}
	return time.Unix(int64(s), 0).UTC().Format(time.RFC3339), true
}

// windowName names a window by its length: 300 minutes is 5h, 10080 is 7d.
func windowName(minutes int64) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes%1440 == 0:
		return fmt.Sprintf("%dd", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

// span sets a window's length and, when its reset time is known, its start: the reset time less
// the length. Callers pass only a length the harness's own data defines.
func span(w *api.UsageWindow, length time.Duration) {
	if length <= 0 {
		return
	}
	w.LengthS = int64(length / time.Second)
	if r, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		w.StartsAt = r.Add(-length).UTC().Format(time.RFC3339)
	}
}

// Reading is what one report from a harness says about its account.
type Reading struct {
	Key     string
	Harness string
	Label   string // plan name; empty when the source gives none
	Windows []api.UsageWindow
	// Drift is what the parser could not read, one line each; never a window. A reading with
	// drift marks its account degraded (app.md 14.5); one without clears the mark.
	Drift []string
}
