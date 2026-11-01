package usage

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

var update = flag.Bool("update", false, "rewrite the expected readings in testdata")

// golden compares got, as indented JSON, with testdata/<name>.usage.json.
func golden(t *testing.T, name string, got any) {
	t.Helper()
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := filepath.Join("testdata", name+".usage.json")
	if *update {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(b, want) {
		t.Errorf("%s:\ngot  %s\nwant %s", name, b, want)
	}
}

// result is what a fixture's expected file holds.
type result struct {
	Account api.UsageAccount `json:"account"`
	Version string           `json:"version,omitempty"`
	Drift   []string         `json:"drift,omitempty"`
	Degrade []string         `json:"degrade,omitempty"`
}

// asAccount is the reading as the expected file holds it. The key is checked against Key, not
// written: a hex hash in a fixture reads to a secret scanner as a leaked key.
func asAccount(t *testing.T, r Reading, dataDir string) api.UsageAccount {
	t.Helper()
	if want := Key("hab-a", r.Harness, dataDir); r.Key != want {
		t.Errorf("key %q, want %q", r.Key, want)
	}
	w := r.Windows
	if w == nil {
		w = []api.UsageWindow{}
	}
	return api.UsageAccount{Label: r.Label, Harness: r.Harness, Windows: w}
}

// TestClaudeFixtures: each hand-written status-line input in the shape of Claude Code 2.1.316
// gives the expected reading: whole percent rounded down, reset times from Unix seconds, a
// spend limit with its period, no windows with no rate_limits, and drift for every window that
// does not parse, which is left out, never guessed.
func TestClaudeFixtures(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	files, _ := filepath.Glob("testdata/claude-*.json")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".usage.json") {
			continue
		}
		n++
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			st, err := ParseClaudeStatus(b, at, "hab-a")
			if err != nil {
				t.Fatal(err)
			}
			dir := "/srv/agent/.claude"
			if name == "claude-spend" {
				dir = "/srv/agent/claude-work"
			}
			golden(t, name, result{Account: asAccount(t, st.Reading, dir), Version: st.Version, Drift: st.Drift, Degrade: st.Degrade})
		})
	}
	if n < 4 {
		t.Fatalf("found %d Claude fixtures, want 4", n)
	}
	if _, err := ParseClaudeStatus([]byte("not json"), at, "hab-a"); err == nil {
		t.Error("a status-line input that is not JSON parsed")
	}
}

// TestCodexFixtures: the end of a hand-written rollout in the shape of Codex 0.166.0 gives the
// latest windows of each limit id, the account's own first, read at the timestamp of the line
// that carried them; a limit id with no windows adds none; an older rollout with no limit id and
// resets_in_seconds still reads; drift names what did not parse.
func TestCodexFixtures(t *testing.T) {
	for _, name := range []string{"codex-0.166.0", "codex-older", "codex-drift"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			u := ParseCodexRollout(b, "hab-a", "/srv/agent/.codex")
			golden(t, name, result{Account: asAccount(t, u.Reading, "/srv/agent/.codex"), Version: u.Version, Drift: u.Drift, Degrade: u.Degrade})
		})
	}
}

// TestCodexCutStart: a read that starts mid-line, or holds no token_count, gives no windows and
// no drift.
func TestCodexCutStart(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-0.166.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{b[len(b)-40:], b[:200], nil} {
		u := ParseCodexRollout(data, "hab-a", "")
		if len(u.Windows) != 0 || len(u.Drift) != 0 {
			t.Errorf("%q: %+v", data, u)
		}
	}
	// Any cut of the fixture parses without a panic and never gives a window the whole file lacks.
	full := ParseCodexRollout(b, "hab-a", "")
	for i := range b {
		for _, w := range ParseCodexRollout(b[i:], "hab-a", "").Windows {
			found := false
			for _, f := range full.Windows {
				found = found || f.Name == w.Name
			}
			if !found {
				t.Fatalf("cut at %d gave window %+v", i, w)
			}
		}
	}
}

// TestKey: the key is stable, differs by hab, harness and data directory, and names none of them.
func TestKey(t *testing.T) {
	a := Key("hab-a", "claude", "/srv/agent/.claude")
	if a != Key("hab-a", "claude", "/srv/agent/.claude") {
		t.Error("the key is not stable")
	}
	for _, other := range []string{Key("hab-b", "claude", "/srv/agent/.claude"), Key("hab-a", "codex", "/srv/agent/.claude"), Key("hab-a", "claude", "/srv/other")} {
		if other == a {
			t.Errorf("two accounts share key %s", a)
		}
	}
	if !strings.HasPrefix(a, "claude-") || strings.Contains(a, "hab-a") || strings.Contains(a, "agent") || len(a) != len("claude-")+12 {
		t.Errorf("key %q", a)
	}
}

// recorder keeps published events.
type recorder struct {
	mu sync.Mutex
	ev []structure.Event
}

func (r *recorder) publish(e structure.Event) {
	r.mu.Lock()
	r.ev = append(r.ev, e)
	r.mu.Unlock()
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ev)
}

func win(name string, used int, resets, read string) api.UsageWindow {
	return api.UsageWindow{Name: name, UsedPct: used, ResetsAt: resets, ReadAt: read}
}

// TestStore: a reading with no windows makes no account; windows merge by name and an earlier
// read never replaces a later one; an unchanged reading is sent again only after Refresh; a
// window the reading lacks stays; the label follows the newest reading; List sorts by key and
// hands out copies.
func TestStore(t *testing.T) {
	var rec recorder
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	s := NewStore(rec.publish, clock)

	if s.Put(Reading{Key: "codex-1", Harness: "codex"}) || len(s.List().Accounts) != 0 {
		t.Fatal("a reading with no windows made an account")
	}
	if s.Put(Reading{Harness: "codex", Windows: []api.UsageWindow{win("5h", 1, "", "2026-01-01T10:00:00Z")}}) {
		t.Fatal("a reading with no key was stored")
	}
	r1 := Reading{Key: "codex-1", Harness: "codex", Label: "Plus", Windows: []api.UsageWindow{
		win("5h", 10, "2026-01-01T12:00:00Z", "2026-01-01T10:00:00Z"),
		win("7d", 3, "2026-01-08T00:00:00Z", "2026-01-01T10:00:00Z"),
	}}
	if !s.Put(r1) || rec.count() != 1 {
		t.Fatalf("first reading: %d events", rec.count())
	}
	e := rec.ev[0]
	if e.Type != api.EventUsageUpdated || e.ID != "codex-1" || !structure.Ephemeral(e.Type) {
		t.Fatalf("event %+v", e)
	}
	// The same numbers read later: no event until Refresh has passed.
	r2 := r1
	r2.Windows = []api.UsageWindow{win("5h", 10, "2026-01-01T12:00:00Z", "2026-01-01T10:01:00Z")}
	if s.Put(r2) {
		t.Fatal("an unchanged reading was sent at once")
	}
	if got := s.List().Accounts[0].Windows[0].ReadAt; got != "2026-01-01T10:01:00Z" {
		t.Fatalf("read_at not updated: %s", got)
	}
	advance(Refresh)
	if !s.Put(r2) {
		t.Fatal("an unchanged reading was not sent again after Refresh")
	}
	// An earlier reading (an older rollout) does not undo a newer one.
	old := Reading{Key: "codex-1", Harness: "codex", Label: "Pro", Windows: []api.UsageWindow{win("5h", 90, "2026-01-01T12:00:00Z", "2026-01-01T09:00:00Z")}}
	if s.Put(old) {
		t.Fatal("an older reading was sent")
	}
	a := s.List().Accounts[0]
	if a.Windows[0].UsedPct != 10 || a.Label != "Plus" || len(a.Windows) != 2 || a.Windows[1].Name != "7d" {
		t.Fatalf("after an older reading: %+v", a)
	}
	// A change is sent at once; the 7d window the reading lacks stays.
	r3 := Reading{Key: "codex-1", Harness: "codex", Label: "Pro", Windows: []api.UsageWindow{win("5h", 11, "2026-01-01T12:00:00Z", "2026-01-01T10:02:00Z")}}
	if !s.Put(r3) {
		t.Fatal("a change was not sent")
	}
	a = s.List().Accounts[0]
	if a.Windows[0].UsedPct != 11 || a.Label != "Pro" || len(a.Windows) != 2 {
		t.Fatalf("after a change: %+v", a)
	}
	// A second account sorts by key; List hands out copies.
	s.Put(Reading{Key: "claude-1", Harness: "claude", Windows: []api.UsageWindow{win("5h", 1, "", "2026-01-01T10:00:00Z")}})
	l := s.List()
	if len(l.Accounts) != 2 || l.Accounts[0].Key != "claude-1" {
		t.Fatalf("list: %+v", l)
	}
	l.Accounts[1].Windows[0].UsedPct = 77
	if s.List().Accounts[1].Windows[0].UsedPct == 77 {
		t.Fatal("List shares the store's windows")
	}
}

// TestStoreBounds: the store keeps at most maxAccounts accounts and maxWindows windows each.
func TestStoreBounds(t *testing.T) {
	s := NewStore(nil, nil)
	var ws []api.UsageWindow
	for i := range maxWindows + 5 {
		ws = append(ws, win(fmt.Sprintf("w%d", i), i, "", "2026-01-01T10:00:00Z"))
	}
	for i := range maxAccounts + 5 {
		s.Put(Reading{Key: fmt.Sprintf("k%03d", i), Harness: "codex", Windows: ws})
	}
	l := s.List()
	if len(l.Accounts) != maxAccounts || len(l.Accounts[0].Windows) != maxWindows {
		t.Fatalf("%d accounts, %d windows", len(l.Accounts), len(l.Accounts[0].Windows))
	}
}

// TestStoreConcurrent: Put and List from many goroutines (run with -race).
func TestStoreConcurrent(t *testing.T) {
	var rec recorder
	s := NewStore(rec.publish, nil)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				read := time.Date(2026, 1, 1, 10, 0, i, 0, time.UTC).Format(time.RFC3339)
				s.Put(Reading{Key: fmt.Sprintf("k%d", g%2), Harness: "codex", Windows: []api.UsageWindow{win("5h", i%100, "", read)}})
				s.List()
			}
		}()
	}
	wg.Wait()
	for _, a := range s.List().Accounts {
		if a.Windows[0].ReadAt != time.Date(2026, 1, 1, 10, 3, 19, 0, time.UTC).Format(time.RFC3339) {
			t.Errorf("%s: newest read lost: %+v", a.Key, a.Windows[0])
		}
	}
}

// TestClaudeWindowName: a known key keeps its name; another key of lower-case words is a window
// named by its words with a leading length shortened; any other key is refused. A spend period
// that is not short lower-case words is left off the name.
func TestClaudeWindowName(t *testing.T) {
	const day = 24 * time.Hour
	for k, want := range map[string]struct {
		name   string
		length time.Duration
	}{
		"five_hour": {"5h", 5 * time.Hour}, "seven_day": {"7d", 7 * day}, "spend_limit": {"spend", 0},
		"seven_day_opus": {"7d opus", 7 * day}, "five_hour_sonnet": {"5h sonnet", 5 * time.Hour},
		"30_day": {"30d", 30 * day}, "two_weeks": {"2w", 14 * day}, "one_week_opus": {"1w opus", 7 * day}, "ten_minutes": {"10m", 10 * time.Minute},
		"monthly": {"monthly", 0}, "opus_weekly": {"opus weekly", 0}, "day_opus": {"day opus", 0},
		"99999_day": {"99999d", 0}, "Opus": {}, "seven__day": {}, "_x": {}, "x_": {}, "seven-day": {}, "": {},
		strings.Repeat("a", 41): {}, "été": {},
	} {
		got, length, ok := claudeWindowName(k)
		if got != want.name || length != want.length || ok != (want.name != "") {
			t.Errorf("%q: %q, %v, %v; want %q, %v", k, got, length, ok, want.name, want.length)
		}
	}
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for period, want := range map[string]string{"monthly": "spend monthly", "per day": "spend per day",
		"": "spend", "Monthly": "spend", strings.Repeat("m", 25): "spend", "a\nb": "spend"} {
		b, _ := json.Marshal(map[string]any{"rate_limits": map[string]any{"spend_limit": map[string]any{"used_percentage": 1, "period": period}}})
		st, err := ParseClaudeStatus(b, at, "hab-a")
		if err != nil || len(st.Windows) != 1 || st.Windows[0].Name != want {
			t.Errorf("period %q: %+v %v, want %q", period, st.Windows, err, want)
		}
	}
}

// TestCodexOrder: when lines come out of time order, the newest timestamp of each limit id and
// of the plan wins, not the last line.
func TestCodexOrder(t *testing.T) {
	line := func(ts, limit string, used int, plan string) string {
		return fmt.Sprintf(`{"timestamp":"2026-01-01T10:00:%s.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":%q,"primary":{"used_percent":%d,"window_minutes":300,"resets_at":1767268800},"plan_type":%q}}}`+"\n", ts, limit, used, plan)
	}
	data := line("20", "codex", 20, "pro") + line("10", "codex", 10, "plus") +
		line("30", "other", 30, "") + line("05", "other", 5, "")
	u := ParseCodexRollout([]byte(data), "hab-a", "")
	if u.Label != "Pro" || len(u.Windows) != 2 || u.Windows[0].UsedPct != 20 || u.Windows[1].UsedPct != 30 ||
		u.Windows[0].ReadAt != "2026-01-01T10:00:20Z" {
		t.Errorf("out of order: label %q, windows %+v", u.Label, u.Windows)
	}
}

// TestPlanLabel: the first letter is upper-cased by rune; an empty or long plan has no label.
func TestPlanLabel(t *testing.T) {
	for p, want := range map[string]string{"plus": "Plus", "pro": "Pro", "": "", "élite": "Élite",
		strings.Repeat("p", 33): ""} {
		if got := planLabel(p); got != want {
			t.Errorf("planLabel(%q) = %q, want %q", p, got, want)
		}
	}
}

// unix is midnight UTC on a date, in Unix seconds.
func unix(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() }

// TestWindowSpan: a window's length and start come only from what the harness's data defines.
// A Claude spend limit's period gives the calendar period that ends at the reset time; a period
// with no fixed length, or no reset time, gives none. A Codex window with no reset time has a
// length and no start, and an absurd window_minutes gives neither.
func TestWindowSpan(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	const reset = 1772323200 // 2026-03-01T00:00:00Z
	for _, c := range []struct {
		period string
		resets any
		length int64
		start  string
	}{
		{"monthly", reset, 28 * 86400, "2026-02-01T00:00:00Z"},
		{"weekly", reset, 7 * 86400, "2026-02-22T00:00:00Z"},
		{"daily", reset, 86400, "2026-02-28T00:00:00Z"},
		{"per day", reset, 0, ""},
		{"", reset, 0, ""},
		{"monthly", nil, 0, ""},
		// A reset on day 29 to 31 starts on the same day of the month before, clamped to its end.
		{"monthly", unix(2026, 3, 31), 31 * 86400, "2026-02-28T00:00:00Z"},
		{"monthly", unix(2026, 3, 30), 30 * 86400, "2026-02-28T00:00:00Z"},
		{"monthly", unix(2026, 5, 31), 31 * 86400, "2026-04-30T00:00:00Z"},
		{"monthly", unix(2028, 3, 31), 31 * 86400, "2028-02-29T00:00:00Z"},
		{"monthly", unix(2027, 3, 29), 29 * 86400, "2027-02-28T00:00:00Z"},
		{"monthly", unix(2027, 1, 31), 31 * 86400, "2026-12-31T00:00:00Z"},
	} {
		lim := map[string]any{"used_percentage": 1, "period": c.period}
		if c.resets != nil {
			lim["resets_at"] = c.resets
		}
		b, _ := json.Marshal(map[string]any{"rate_limits": map[string]any{"spend_limit": lim}})
		st, err := ParseClaudeStatus(b, at, "hab-a")
		if err != nil || len(st.Windows) != 1 {
			t.Fatalf("period %q: %+v %v", c.period, st.Windows, err)
		}
		if w := st.Windows[0]; w.LengthS != c.length || w.StartsAt != c.start {
			t.Errorf("period %q, resets %v: length %d start %q, want %d %q", c.period, c.resets, w.LengthS, w.StartsAt, c.length, c.start)
		}
	}
	// A period on a key other than spend_limit gives no calendar period: the key's length stays.
	b, _ := json.Marshal(map[string]any{"rate_limits": map[string]any{
		"five_hour":   map[string]any{"used_percentage": 1, "period": "monthly", "resets_at": reset},
		"weekly_opus": map[string]any{"used_percentage": 1, "period": "monthly", "resets_at": reset}}})
	if st, _ := ParseClaudeStatus(b, at, "hab-a"); len(st.Windows) != 2 || st.Windows[0].LengthS != 18000 ||
		st.Windows[0].StartsAt != "2026-02-28T19:00:00Z" || st.Windows[1].LengthS != 0 || st.Windows[1].StartsAt != "" {
		t.Errorf("a period on another key: %+v", st.Windows)
	}
	b, _ = json.Marshal(map[string]any{"rate_limits": map[string]any{"five_hour": map[string]any{"used_percentage": 1}}})
	if st, _ := ParseClaudeStatus(b, at, "hab-a"); len(st.Windows) != 1 || st.Windows[0].LengthS != 18000 || st.Windows[0].StartsAt != "" {
		t.Errorf("5h with no reset: %+v", st.Windows)
	}
	for minutes, want := range map[int64]int64{300: 18000, codexMaxMinutes: codexMaxMinutes * 60, codexMaxMinutes + 1: 0, 1 << 60: 0} {
		w, problem := codexUsageWindow(&codexWindow{Used: new(1.0), Minutes: &minutes}, at)
		if problem != "" || w.LengthS != want || w.StartsAt != "" {
			t.Errorf("window_minutes %d: %+v %q, want length %d and no start", minutes, w, problem, want)
		}
	}
}

// TestClaudeRateLimitsShape: a rate_limits that is not an object is drift on the account the
// input names, not an error, so the store can mark that account degraded.
func TestClaudeRateLimitsShape(t *testing.T) {
	in := `{"transcript_path":"/srv/agent/.claude/projects/-w/s.jsonl","rate_limits":[{"used_percentage":5}]}`
	st, err := ParseClaudeStatus([]byte(in), time.Now(), "hab-a")
	if err != nil || st.Key != Key("hab-a", "claude", "/srv/agent/.claude") || len(st.Windows) != 0 ||
		len(st.Drift) != 1 || st.Drift[0] != "rate_limits is not an object" {
		t.Fatalf("%+v %v", st, err)
	}
}

// TestStoreDegraded: a reading with Degrade marks its account degraded, creating it when it has
// no windows, and publishes the change; a reading with no windows and no Degrade leaves the
// mark; a reading with windows and no Degrade clears it. Drift alone does nothing. The mark
// follows report time: an older reading neither clears nor sets it, and one with no time counts.
func TestStoreDegraded(t *testing.T) {
	var rec recorder
	s := NewStore(rec.publish, nil)
	const k = "claude-1"
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	mark := func() string { return s.List().Accounts[0].Degraded }
	if s.Put(Reading{Key: "claude-2", Harness: "claude", Drift: []string{"logged only"}, At: t0}) || len(s.List().Accounts) != 0 {
		t.Fatal("drift that does not degrade made an account")
	}
	if !s.Put(Reading{Key: k, Harness: "claude", Drift: []string{"x"}, Degrade: []string{"rate_limits is not an object"}, At: t0}) {
		t.Fatal("a degraded reading with no windows was not sent")
	}
	l := s.List()
	if len(l.Accounts) != 1 || l.Accounts[0].Degraded != "rate_limits is not an object" || len(l.Accounts[0].Windows) != 0 {
		t.Fatalf("after drift: %+v", l)
	}
	good := func(at time.Time) Reading {
		return Reading{Key: k, Harness: "claude", At: at, Drift: []string{"an unknown key"},
			Windows: []api.UsageWindow{win("5h", 1, "", at.Format(time.RFC3339))}}
	}
	bad := func(at time.Time) Reading {
		r := good(at)
		r.Degrade = []string{"a", "b", "c"}
		return r
	}
	if !s.Put(good(t0.Add(time.Minute))) || mark() != "" {
		t.Fatalf("a good reading did not clear the mark: %+v", s.List())
	}
	if !s.Put(bad(t0.Add(2*time.Minute))) || mark() != "a (and 2 more)" {
		t.Fatalf("drift with windows: %+v", s.List())
	}
	if s.Put(Reading{Key: k, Harness: "claude", At: t0.Add(3 * time.Minute)}) || mark() == "" {
		t.Fatalf("an empty clean reading changed the mark: %+v", s.List())
	}
	if s.Put(good(t0.Add(time.Minute+30*time.Second))) || mark() == "" {
		t.Fatalf("an older good reading cleared the mark: %+v", s.List())
	}
	if !s.Put(good(t0.Add(4*time.Minute))) || mark() != "" {
		t.Fatalf("a newer good reading did not clear the mark: %+v", s.List())
	}
	if s.Put(bad(t0.Add(3*time.Minute))) || mark() != "" {
		t.Fatalf("an older degraded reading set the mark: %+v", s.List())
	}
	if r := bad(t0.Add(5 * time.Minute)); !s.Put(Reading{Key: k, Harness: "claude", Degrade: r.Degrade}) || mark() == "" {
		t.Fatalf("a degraded reading with no time did not set the mark: %+v", s.List())
	}
	if rec.count() != 5 {
		t.Fatalf("%d events, want 5", rec.count())
	}
	if e := rec.ev[2].Data.(api.UsageAccount); e.Degraded != "a (and 2 more)" {
		t.Fatalf("the degraded event carries %+v", e)
	}
}

// TestDegradeRules: which drift marks an account degraded. Claude Code: drift in a known key
// does, drift in an unknown key does not while a window reads, and all drift does when no window
// reads, unless the input names no data directory. Codex: drift in a line older than the newest
// line that gave a window does not; drift in or after it does, and so does a bad window.
func TestDegradeRules(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	const tp = `"transcript_path":"/srv/agent/.claude/projects/-w/s.jsonl",`
	const good = `"five_hour":{"used_percentage":5}`
	for name, c := range map[string]struct {
		in      string
		degrade int
	}{
		"unknown key, a window reads": {`{` + tp + `"rate_limits":{` + good + `,"overage_enabled":false}}`, 0},
		"unknown key, no window":      {`{` + tp + `"rate_limits":{"overage_enabled":false}}`, 1},
		"known key":                   {`{` + tp + `"rate_limits":{` + good + `,"seven_day":{"used_percentage":"x"}}}`, 1},
		"no data directory":           {`{"rate_limits":[1]}`, 0},
		"no data directory, a window": {`{"rate_limits":{` + good + `,"seven_day":false}}`, 1},
	} {
		st, err := ParseClaudeStatus([]byte(c.in), at, "hab-a")
		if err != nil || len(st.Degrade) != c.degrade || len(st.Drift) == 0 {
			t.Errorf("%s: degrade %q, drift %q, %v", name, st.Degrade, st.Drift, err)
		}
		if !st.At.Equal(at) {
			t.Errorf("%s: at %v", name, st.At)
		}
	}
	line := func(sec int, limits string) string {
		return fmt.Sprintf(`{"timestamp":"2026-01-01T10:00:%02d.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":%s}}`+"\n", sec, limits)
	}
	ok := `{"limit_id":"codex","primary":{"used_percent":1,"window_minutes":300,"resets_at":1767268800}}`
	oldBad := ParseCodexRollout([]byte(line(1, "[1]")+line(2, ok)), "hab-a", "/c")
	if len(oldBad.Windows) != 1 || len(oldBad.Drift) != 1 || len(oldBad.Degrade) != 0 || !oldBad.At.Equal(at.Add(2*time.Second)) {
		t.Errorf("an old bad line: %+v", oldBad)
	}
	newBad := ParseCodexRollout([]byte(line(1, ok)+line(2, "[1]")), "hab-a", "/c")
	if len(newBad.Windows) != 1 || len(newBad.Degrade) != 1 {
		t.Errorf("a new bad line: %+v", newBad)
	}
	badWin := `{"limit_id":"model_y","primary":{"used_percent":1}}`
	oldWin := ParseCodexRollout([]byte(line(1, badWin)+line(2, ok)), "hab-a", "/c")
	if len(oldWin.Windows) != 1 || len(oldWin.Degrade) != 1 || oldWin.Degrade[0] != "a rate-limit window has no window_minutes" {
		t.Errorf("a bad window of another limit: %+v", oldWin)
	}
}
