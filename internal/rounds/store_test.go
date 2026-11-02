package rounds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, setAside, err := Open(dir, Options{Zone: "America/New_York"})
	if err != nil {
		t.Fatal(err)
	}
	if setAside {
		t.Fatal("state set aside")
	}
	return s
}

func statePath(dir string) string { return filepath.Join(dir, "rounds", "state.json") }

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	e, err := s.Create(freshRound(), "sess-agent", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.Round.ID, "round-") || e.Round.CreatedBy != "sess-agent" || !e.Round.CreatedAt.Equal(testNow) {
		t.Errorf("created: %+v", e.Round)
	}
	// 09:00 in New York on 2026-06-01 (EDT) is 13:00 UTC, after testNow.
	if e.State.NextSlot == nil || !e.State.NextSlot.Equal(utc("2026-06-01T13:00:00Z")) {
		t.Errorf("next slot = %v", e.State.NextSlot)
	}
	once := utc("2026-07-01T00:00:00Z")
	r2 := Round{Name: "later", Mode: ModeResume, Trigger: Trigger{Kind: TriggerOnce, At: &once},
		Prompt: "look again", Target: "sess-2", HarnessArgs: []string{"--x"}}
	e2, err := s.Create(r2, "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState(e2.Round.ID, func(st *RoundState) { st.Failures = 2; st.LastRun = 7 }); err != nil {
		t.Fatal(err)
	}
	slot := utc("2026-06-01T13:00:00Z")
	start := slot.Add(time.Second)
	run := Run{ID: NewID("run"), Number: 1, RoundID: e.Round.ID, Slot: &slot, Trigger: TriggerCron,
		Missed: 3, State: RunRunning, SessionID: "sess-9", Created: slot, StartedAt: &start}
	if err := s.Runs().Append(run); err != nil {
		t.Fatal(err)
	}
	run.State, run.Result = RunSucceeded, "all clear"
	if err := s.Runs().Update(run); err != nil {
		t.Fatal(err)
	}

	s2 := openStore(t, dir)
	if !reflect.DeepEqual(s.List(), s2.List()) {
		t.Errorf("reloaded rounds differ:\n%+v\n%+v", s.List(), s2.List())
	}
	got, _, err := s2.Runs().Page(e.Round.ID, 0, 10)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], run) {
		t.Errorf("reloaded runs = %+v %v", got, err)
	}
	if g, ok := s2.ByName("later"); !ok || g.State.Failures != 2 || g.Round.Wait != Duration(DefaultWait) {
		t.Errorf("ByName = %+v %v", g, ok)
	}
	if fi, err := os.Stat(statePath(dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi, err)
	}
}

func TestStoreNames(t *testing.T) {
	s := openStore(t, t.TempDir())
	a, _ := s.Create(freshRound(), "", testNow)
	if _, err := s.Create(freshRound(), "", testNow); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate name: %v", err)
	}
	b := freshRound()
	b.Name = "other"
	eb, err := s.Create(b, "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	eb.Round.Name = a.Round.Name
	if _, err := s.Update(eb.Round, testNow); !errors.Is(err, ErrNameTaken) {
		t.Errorf("rename onto a taken name: %v", err)
	}
	if _, err := s.Update(Round{ID: "round-none"}, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
	if names := []string{s.List()[0].Round.Name, s.List()[1].Round.Name}; names[0] != "other" || names[1] != "triage" {
		t.Errorf("List order = %v", names)
	}
}

func TestStoreUpdate(t *testing.T) {
	s := openStore(t, t.TempDir())
	e, _ := s.Create(freshRound(), "sess-agent", testNow)
	later := testNow.Add(time.Hour)

	// An edit that keeps the trigger keeps the next slot.
	r := e.Round
	r.Prompt = "new"
	r.CreatedBy = "forged"
	u, err := s.Update(r, later)
	if err != nil {
		t.Fatal(err)
	}
	if !u.State.NextSlot.Equal(*e.State.NextSlot) || u.Round.CreatedBy != "sess-agent" ||
		!u.Round.CreatedAt.Equal(testNow) || !u.Round.UpdatedAt.Equal(later) || u.Round.Prompt != "new" {
		t.Errorf("edit: %+v %+v", u.Round, u.State)
	}

	// A changed trigger sets the next slot from now.
	r.Trigger = cronTrigger("30 * * * *")
	u, err = s.Update(r, later)
	if err != nil {
		t.Fatal(err)
	}
	if want := utc("2026-06-01T13:30:00Z"); !u.State.NextSlot.Equal(want) {
		t.Errorf("next slot after trigger change = %v, want %v", u.State.NextSlot, want)
	}
	// A refused edit changes nothing.
	r.Mode = "bad"
	if _, err := s.Update(r, later); err == nil {
		t.Error("bad edit accepted")
	}
	if g, _ := s.Get(e.Round.ID); g.Round.Mode != ModeFresh {
		t.Errorf("refused edit stored: %+v", g.Round)
	}
}

// A file that does not parse is set aside as .bad and the store starts empty.
func TestStoreCorruptFile(t *testing.T) {
	for name, content := range map[string]string{
		"truncated":    "",
		"half":         `{"version":1,"rounds":{"round-1":{"round":{"id":"round-1","na`,
		"not json":     "hello",
		"entry id":     `{"version":1,"rounds":{"round-1":{"round":{"id":"round-2"}}}}`,
		"null entry":   `{"version":1,"rounds":{"round-1":null}}`,
		"version 0":    `{"version":0,"rounds":{}}`,
		"wrong shape":  `{"version":1,"rounds":[]}`,
		"string state": `"state"`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := statePath(dir)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if name == "truncated" {
				// A real file cut short.
				s := openStore(t, dir)
				s.Create(freshRound(), "", testNow)
				b, _ := os.ReadFile(p)
				content = string(b[:len(b)/2])
			}
			os.WriteFile(p, []byte(content), 0o600)
			var logged []string
			s, setAside, err := Open(dir, Options{Zone: "UTC", Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
			if err != nil {
				t.Fatal(err)
			}
			if !setAside || len(s.List()) != 0 || len(logged) != 1 {
				t.Errorf("setAside=%v rounds=%d logged=%v", setAside, len(s.List()), logged)
			}
			if b, err := os.ReadFile(p + ".bad"); err != nil || string(b) != content {
				t.Errorf(".bad = %q %v", b, err)
			}
			// The store works and saves again.
			if _, err := s.Create(freshRound(), "", testNow); err != nil {
				t.Fatal(err)
			}
			if s2 := openStore(t, dir); len(s2.List()) != 1 {
				t.Error("store did not save after a set-aside")
			}
		})
	}
}

func TestStoreNewerVersionRefused(t *testing.T) {
	dir := t.TempDir()
	p := statePath(dir)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"version":99,"rounds":{}}`), 0o600)
	if _, _, err := Open(dir, Options{Zone: "UTC"}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(p + ".bad"); err == nil {
		t.Error("a newer file was set aside")
	}
}

// A crash between the temporary file and the rename leaves the old state, and the next open
// removes the temporary file.
func TestStoreCrashBeforeRename(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	e, _ := s.Create(freshRound(), "", testNow)

	s.write = func(p string, b []byte) error {
		// The temporary file is written and synced, then the process dies before the rename.
		os.WriteFile(p+".tmp", b, 0o600)
		return errors.New("crash")
	}
	r := e.Round
	r.Prompt = "lost edit"
	if _, err := s.Update(r, testNow); err == nil {
		t.Fatal("update reported success")
	}
	if g, _ := s.Get(e.Round.ID); g.Round.Prompt != "triage new issues" {
		t.Errorf("memory kept the failed edit: %q", g.Round.Prompt)
	}
	if _, err := os.Stat(statePath(dir) + ".tmp"); err != nil {
		t.Fatal("no temp file")
	}
	s2 := openStore(t, dir)
	if g, ok := s2.Get(e.Round.ID); !ok || g.Round.Prompt != "triage new issues" {
		t.Errorf("reloaded = %+v", g.Round)
	}
	if _, err := os.Stat(statePath(dir) + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left: %v", err)
	}
}

func TestStoreFailedSaveRollsBack(t *testing.T) {
	s := openStore(t, t.TempDir())
	e, _ := s.Create(freshRound(), "", testNow)
	s.write = func(string, []byte) error { return errors.New("disk full") }
	if err := s.Runs().Append(Run{ID: "run-1", Number: 1, RoundID: e.Round.ID, State: RunQueued}); err == nil {
		t.Fatal("append reported success")
	}
	if _, err := s.Create(Round{Name: "x", Mode: ModeFresh, Trigger: Trigger{Kind: TriggerManual}, Command: []string{"true"}, Project: "p"}, "", testNow); err == nil {
		t.Fatal("create reported success")
	}
	if err := s.Delete(e.Round.ID); err == nil {
		t.Fatal("delete reported success")
	}
	if runs, _, _ := s.Runs().Page(e.Round.ID, 0, 10); len(runs) != 0 || len(s.List()) != 1 {
		t.Errorf("failed saves left changes: runs=%v rounds=%d", runs, len(s.List()))
	}
}

func TestRunLog(t *testing.T) {
	s := openStore(t, t.TempDir())
	e, _ := s.Create(freshRound(), "", testNow)
	log := s.Runs()
	for n := 1; n <= 5; n++ {
		if err := log.Append(Run{ID: fmt.Sprintf("run-%d", n), Number: n, RoundID: e.Round.ID, State: RunSucceeded, Created: testNow}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Append(Run{ID: "run-1", Number: 9, RoundID: e.Round.ID}); !errors.Is(err, ErrRunExists) {
		t.Errorf("duplicate id: %v", err)
	}
	if err := log.Append(Run{ID: "run-x"}); err == nil {
		t.Error("a run with no round id was accepted")
	}
	if err := log.Update(Run{ID: "run-none", RoundID: e.Round.ID}); !errors.Is(err, ErrNoRun) {
		t.Errorf("update unknown: %v", err)
	}
	numbers := func(runs []Run) []int {
		var out []int
		for _, r := range runs {
			out = append(out, r.Number)
		}
		return out
	}
	p1, more, _ := log.Page(e.Round.ID, 0, 2)
	p2, more2, _ := log.Page(e.Round.ID, p1[len(p1)-1].Number, 2)
	p3, more3, _ := log.Page(e.Round.ID, p2[len(p2)-1].Number, 2)
	if !reflect.DeepEqual(numbers(p1), []int{5, 4}) || !more ||
		!reflect.DeepEqual(numbers(p2), []int{3, 2}) || !more2 ||
		!reflect.DeepEqual(numbers(p3), []int{1}) || more3 {
		t.Errorf("pages %v %v %v (%v %v %v)", numbers(p1), numbers(p2), numbers(p3), more, more2, more3)
	}
	if _, _, err := log.Page(e.Round.ID, 0, 0); err == nil {
		t.Error("a zero limit was accepted")
	}

	// Deleting a round drops its ended runs and keeps a live one until it ends.
	log.Append(Run{ID: "run-6", Number: 6, RoundID: e.Round.ID, State: RunRunning, Created: testNow})
	if err := s.Delete(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	if runs, _, _ := log.Page(e.Round.ID, 0, 10); !reflect.DeepEqual(numbers(runs), []int{6}) {
		t.Errorf("after delete: %v", numbers(runs))
	}
	log.Update(Run{ID: "run-6", Number: 6, RoundID: e.Round.ID, State: RunSucceeded, Created: testNow})
	removed, err := log.Prune(e.Round.ID, testNow)
	if err != nil || !reflect.DeepEqual(numbers(removed), []int{6}) {
		t.Errorf("prune after delete: %v %v", numbers(removed), err)
	}
	if err := s.Delete(e.Round.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

// Retention keeps a run while it is among the round's last 200 or newer than 90 days.
func TestRetention(t *testing.T) {
	day := 24 * time.Hour
	now := utc("2026-06-01T00:00:00Z")
	tests := []struct {
		name string
		n    int
		// age returns how old run number i (1 is the oldest) is at now.
		age      func(i int) time.Duration
		state    func(i int) RunState
		wantKept int
		// wantOldestKept is the lowest run number kept.
		wantOldestKept int
	}{
		{"250 runs in 10 days keeps all", 250,
			func(i int) time.Duration { return time.Duration(250-i) * 10 * day / 250 }, nil, 250, 1},
		{"300 runs over 400 days keeps the last 200 and any newer than 90 days", 300,
			// Run i is (300-i)*400/300 days old: runs 233 and up are within 90 days.
			func(i int) time.Duration { return time.Duration((300-i)*400*24/300) * time.Hour }, nil, 200, 101},
		{"300 runs over 100 days keeps those within 90 days beyond the last 200", 300,
			// Run i is (300-i)/3 days old: runs 31 and up are newer than 90 days.
			func(i int) time.Duration { return time.Duration(300-i) * day / 3 }, nil, 270, 31},
		{"exactly 200 old runs are all kept", 200,
			func(i int) time.Duration { return 365 * day }, nil, 200, 1},
		{"201 old runs drop the oldest", 201,
			func(i int) time.Duration { return 365 * day }, nil, 200, 2},
		{"a live run is never dropped", 201,
			func(i int) time.Duration { return 365 * day },
			func(i int) RunState {
				if i == 1 {
					return RunBlocked
				}
				return RunFailed
			}, 201, 1},
		{"exactly 90 days old is dropped", 201,
			func(i int) time.Duration { return KeepFor }, nil, 200, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t, t.TempDir())
			// Hundreds of synced saves are slow; this test is about the rule, not the disk.
			s.write = func(p string, b []byte) error { return os.WriteFile(p, b, 0o600) }
			e, _ := s.Create(freshRound(), "", now)
			log := s.Runs()
			for i := 1; i <= tt.n; i++ {
				st := RunSucceeded
				if tt.state != nil {
					st = tt.state(i)
				}
				if err := log.Append(Run{ID: fmt.Sprintf("run-%d", i), Number: i, RoundID: e.Round.ID, State: st, Created: now.Add(-tt.age(i))}); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := log.Prune(e.Round.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			kept, _, _ := log.Page(e.Round.ID, 0, 1000)
			if len(kept) != tt.wantKept || len(kept)+len(removed) != tt.n {
				t.Errorf("kept %d removed %d, want kept %d", len(kept), len(removed), tt.wantKept)
			}
			if len(kept) > 0 && kept[len(kept)-1].Number != tt.wantOldestKept {
				t.Errorf("oldest kept = %d, want %d", kept[len(kept)-1].Number, tt.wantOldestKept)
			}
			// A second prune removes nothing.
			if again, err := log.Prune(e.Round.ID, now); len(again) != 0 || err != nil {
				t.Errorf("second prune removed %d (%v)", len(again), err)
			}
		})
	}
}
