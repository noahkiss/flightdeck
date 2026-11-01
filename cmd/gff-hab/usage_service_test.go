package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	habusage "github.com/noahkiss/flightdeck/internal/usage"
)

// tokenLine is a rollout token_count line of the account's own limit: 5h at used percent, read
// at second sec past 10:00.
func tokenLine(sec, used int) string {
	return fmt.Sprintf(`{"timestamp":"2026-01-01T10:%02d:%02d.000Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":%d.0,"window_minutes":300,"resets_at":1767268800},"secondary":null,"plan_type":"plus"}}}`+"\n", sec/60, sec%60, used)
}

// otherLines is n bytes or more of rollout lines with no token_count.
func otherLines(n int) string {
	line := `{"timestamp":"2026-01-01T09:00:00.000Z","type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 1000) + `"}}` + "\n"
	return strings.Repeat(line, n/len(line)+1)
}

// testUsage is a usage service over one rollout in a temp folder, with the reads it made.
type testUsage struct {
	*usageService
	path string
	mu   sync.Mutex
	logs []string
	at   []time.Time // when each read began
}

func newTestUsage(t *testing.T, debounce, maxWait time.Duration) *testUsage {
	t.Helper()
	tu := &testUsage{path: filepath.Join(t.TempDir(), "rollout.jsonl")}
	tu.usageService = newUsageService(&usageService{
		store: habusage.NewStore(nil, nil), host: "hab-a", codexHome: "/srv/agent/.codex",
		debounce: debounce, maxWait: maxWait,
		find: func(string) (string, error) {
			tu.mu.Lock()
			tu.at = append(tu.at, time.Now())
			tu.mu.Unlock()
			return tu.path, nil
		},
		logf: func(f string, a ...any) {
			tu.mu.Lock()
			tu.logs = append(tu.logs, fmt.Sprintf(f, a...))
			tu.mu.Unlock()
		},
	})
	t.Cleanup(tu.Close)
	return tu
}

func (tu *testUsage) reads() []time.Time {
	tu.mu.Lock()
	defer tu.mu.Unlock()
	return append([]time.Time(nil), tu.at...)
}

// used is the stored 5h percent of the Codex account, or -1 with none.
func (tu *testUsage) used() int {
	for _, a := range tu.store.List().Accounts {
		for _, w := range a.Windows {
			if a.Harness == "codex" && w.Name == "5h" {
				return w.UsedPct
			}
		}
	}
	return -1
}

func (tu *testUsage) hook() {
	tu.Hook(harness.Envelope{Harness: detect.KindCodex, Event: "PostToolUse", Payload: []byte(`{"session_id":"s1"}`)})
}

// TestUsageRolloutReads: the read takes the token_count far from the end of a big rollout, skips
// a file whose size has not changed, and reads one that grew.
func TestUsageRolloutReads(t *testing.T) {
	tu := newTestUsage(t, time.Hour, time.Hour)
	// The token_count is 100 KB before the end, inside the 256 KiB tail.
	body := otherLines(300<<10) + tokenLine(10, 12) + otherLines(100<<10)
	writeFile(t, tu.path, body)
	if !tu.codex("s1") || tu.used() != 12 {
		t.Fatalf("first read: used %d", tu.used())
	}
	// Same size, other numbers: not read again.
	writeFile(t, tu.path, strings.Replace(body, `"used_percent":12.0`, `"used_percent":13.0`, 1))
	if !tu.codex("s1") || tu.used() != 12 {
		t.Errorf("a file of the same size was read again: used %d", tu.used())
	}
	// It grew: read again.
	f, err := os.OpenFile(tu.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(tokenLine(20, 14))
	f.Close()
	if !tu.codex("s1") || tu.used() != 14 {
		t.Errorf("a grown file was not read again: used %d", tu.used())
	}
}

// TestUsageDebounce: a burst of hooks is one read, after the last hook of the burst; a steady
// stream of hooks still reads within the max wait.
func TestUsageDebounce(t *testing.T) {
	d := scaleTimeout(150 * time.Millisecond)
	tu := newTestUsage(t, d, time.Hour)
	writeFile(t, tu.path, tokenLine(10, 12))
	var last time.Time
	for range 4 {
		last = time.Now()
		tu.hook()
		time.Sleep(d / 2)
	}
	time.Sleep(3 * d)
	reads := tu.reads()
	if len(reads) != 1 || reads[0].Sub(last) < d-5*time.Millisecond {
		t.Errorf("a burst of 4 hooks %v apart: reads at %v after the last hook, want one at %v or later", d/2, since(reads, last), d)
	}
	if tu.used() != 12 {
		t.Errorf("used %d after the burst", tu.used())
	}

	tu = newTestUsage(t, d, 2*d)
	writeFile(t, tu.path, tokenLine(10, 12))
	start := time.Now()
	for time.Since(start) < 6*d {
		tu.hook()
		time.Sleep(d / 3)
	}
	if reads := tu.reads(); len(reads) < 2 || reads[0].Sub(start) > 3*d {
		t.Errorf("hooks every %v for %v with a max wait of %v: reads at %v", d/3, 6*d, 2*d, since(reads, start))
	}
}

func since(ts []time.Time, from time.Time) []time.Duration {
	var ds []time.Duration
	for _, t := range ts {
		ds = append(ds, t.Sub(from).Round(time.Millisecond))
	}
	return ds
}

// TestUsageCutLastLine: a read that ends mid-line does not mark the file read, and is tried
// again with no hook, so the token_count still being written is not lost.
func TestUsageCutLastLine(t *testing.T) {
	d := scaleTimeout(100 * time.Millisecond)
	tu := newTestUsage(t, d, time.Hour)
	next := tokenLine(20, 30)
	writeFile(t, tu.path, tokenLine(10, 12)+next[:40])
	tu.hook()
	waitUntil(t, "the first read", func() bool { return tu.used() == 12 })
	tu.mu.Lock()
	_, marked := tu.read["s1"]
	tu.mu.Unlock()
	if marked {
		t.Error("a read that ended mid-line marked the file read")
	}
	f, err := os.OpenFile(tu.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(next[40:])
	f.Close()
	waitUntil(t, "the line, once written whole", func() bool { return tu.used() == 30 })
}

// TestUsageHookEnvelopes: a status-line envelope is consumed, whatever its harness; a Codex
// hook is not; a version outside the tested range is logged once, one inside it is not.
func TestUsageHookEnvelopes(t *testing.T) {
	tu := newTestUsage(t, time.Hour, time.Hour)
	status := func(version string) harness.Envelope {
		return harness.Envelope{Harness: detect.KindClaude, Event: hook.StatusLineEvent, T: 1767261600000,
			Payload: []byte(`{"version":"` + version + `","transcript_path":"/srv/agent/.claude/projects/p/s.jsonl","rate_limits":{"five_hour":{"used_percentage":5,"resets_at":1767268800}}}`)}
	}
	if !tu.Hook(status(habusage.ClaudeTestedMax)) {
		t.Error("a Claude status-line envelope was not consumed")
	}
	if !tu.Hook(harness.Envelope{Harness: detect.KindCodex, Event: hook.StatusLineEvent, Payload: []byte(`{}`)}) {
		t.Error("a status-line envelope of another harness was not consumed")
	}
	if tu.Hook(harness.Envelope{Harness: detect.KindCodex, Event: "Stop", Payload: []byte(`{"session_id":"s1"}`)}) {
		t.Error("a Codex hook was consumed")
	}
	if tu.Hook(harness.Envelope{Harness: detect.KindClaude, Event: "Stop", Payload: []byte(`{}`)}) {
		t.Error("a Claude hook was consumed")
	}
	tu.mu.Lock()
	if len(tu.logs) != 0 {
		t.Errorf("a tested version logged %q", tu.logs)
	}
	tu.mu.Unlock()
	tu.Hook(status("9.0.0"))
	tu.Hook(status("9.0.0"))
	tu.Hook(status(habusage.ClaudeTestedMin))
	tu.mu.Lock()
	if len(tu.logs) != 1 || !strings.Contains(tu.logs[0], "9.0.0 is outside the tested range") {
		t.Errorf("logs %q, want one line for 9.0.0", tu.logs)
	}
	tu.mu.Unlock()
	if l := tu.store.List(); len(l.Accounts) != 1 || l.Accounts[0].Windows[0] != (api.UsageWindow{Name: "5h", UsedPct: 5, ResetsAt: "2026-01-01T12:00:00Z", ReadAt: "2026-01-01T10:00:00Z"}) {
		t.Errorf("store %+v", l)
	}
}

// TestUsageWarnFull: past 256 distinct lines, a new line is dropped, not logged each time.
func TestUsageWarnFull(t *testing.T) {
	tu := newTestUsage(t, time.Hour, time.Hour)
	for i := range 256 {
		tu.warn("line %d", i)
	}
	for range 3 {
		tu.warn("line %d", 999)
		tu.warn("line %d", 0)
	}
	tu.mu.Lock()
	defer tu.mu.Unlock()
	if len(tu.logs) != 256 {
		t.Errorf("%d lines logged, want 256", len(tu.logs))
	}
}
