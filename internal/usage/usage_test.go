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
			golden(t, name, result{Account: asAccount(t, st.Reading, dir), Version: st.Version, Drift: st.Drift})
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
			golden(t, name, result{Account: asAccount(t, u.Reading, "/srv/agent/.codex"), Version: u.Version, Drift: u.Drift})
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
