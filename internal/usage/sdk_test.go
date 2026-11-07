package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Hand-written rate_limit_info objects, in the shapes the Claude Agent SDK's
// rate_limit_event carries (capcom-service.md 10.1).
func TestSDKReading(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	reset := float64(time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC).Unix())
	five := float64(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC).Unix())
	b, _ := json.Marshal(map[string]any{
		"status": "allowed_warning", "rateLimitType": "seven_day", "utilization": 0.9, "resetsAt": reset,
		"unifiedWindows": map[string]any{
			"five_hour":      map[string]any{"utilization": 0.426, "resetsAt": five, "status": "allowed"},
			"seven_day":      map[string]any{"utilization": 0.418, "resetsAt": reset},
			"seven_day_opus": map[string]any{"utilization": 1.2, "resetsAt": reset, "status": "rejected"},
		},
	})
	r := SDKReading("claude-k", b, at)
	if len(r.Drift) != 0 || r.Harness != "claude" || r.Key != "claude-k" {
		t.Fatalf("%+v", r)
	}
	got := map[string]int{}
	for _, w := range r.Windows {
		got[w.Name] = w.UsedPct
	}
	if len(r.Windows) != 3 || got["5h"] != 42 || got["7d"] != 41 || got["7d opus"] != 120 {
		t.Fatalf("windows %+v", r.Windows)
	}
	for _, w := range r.Windows {
		if w.Name == "7d" && (w.LengthS != 7*24*3600 || w.StartsAt != "2026-11-02T09:00:00Z" || w.ReadAt != "2026-11-07T10:00:00Z") {
			t.Fatalf("7d window %+v", w)
		}
	}

	// The top-level window alone, as an SDK without unifiedWindows sends it.
	b, _ = json.Marshal(map[string]any{"status": "allowed", "rateLimitType": "seven_day", "utilization": 0.25, "resetsAt": reset})
	r = SDKReading("claude-k", b, at)
	if len(r.Windows) != 1 || r.Windows[0].Name != "7d" || r.Windows[0].UsedPct != 25 || r.Windows[0].StartsAt == "" {
		t.Fatalf("%+v", r)
	}

	// No window (an API key's session): nothing to store.
	r = SDKReading("claude-k", json.RawMessage(`{"status":"allowed"}`), at)
	if len(r.Windows) != 0 || len(r.Degrade) != 0 {
		t.Fatalf("%+v", r)
	}

	// A reset time in milliseconds is drift, and with no window the account is degraded.
	b, _ = json.Marshal(map[string]any{"rateLimitType": "seven_day", "utilization": 0.25, "resetsAt": reset * 1000})
	if r = SDKReading("claude-k", b, at); len(r.Windows) != 0 || len(r.Degrade) != 1 {
		t.Fatalf("%+v", r)
	}
	if r = SDKReading("claude-k", json.RawMessage(`[1]`), at); len(r.Degrade) != 1 {
		t.Fatalf("%+v", r)
	}
}

// TestCodexEngineReading: the Capcom engine on Codex sends the rollout's newest token_count
// record of each limit id, hand-written here in the shape of CLI 0.167 (ordinal, credits,
// individual_limit and the other new fields beside the windows). They read as a rollout's end:
// the account's own windows first, the plan label, each window read at its record's time, all
// under the key given, as a Codex account. Another shape is drift that degrades; an empty list
// gives nothing.
func TestCodexEngineReading(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "codex-engine-0.167.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := CodexEngineReading(Key("hab-a", "codex", "/state/capcom/engine"), b)
	golden(t, "codex-engine-0.167.0", result{Account: asAccount(t, r, "/state/capcom/engine"), Drift: r.Drift, Degrade: r.Degrade})
	if r.Harness != "codex" || r.At.IsZero() {
		t.Fatalf("%+v", r)
	}
	for _, raw := range []string{`{}`, `[1]`, `{"rollout":3}`, `{"status":"allowed","rateLimitType":"seven_day"}`} {
		if r := CodexEngineReading("codex-k", json.RawMessage(raw)); len(r.Windows) != 0 || len(r.Degrade) != 1 || r.Key != "codex-k" {
			t.Errorf("%s: %+v", raw, r)
		}
	}
	if r := CodexEngineReading("codex-k", json.RawMessage(`{"rollout":[]}`)); len(r.Windows) != 0 || len(r.Drift) != 0 {
		t.Errorf("an empty list: %+v", r)
	}
}
