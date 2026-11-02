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

// setAsideFiles returns the files a corrupt state file was set aside as.
func setAsideFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(statePath(dir) + ".bad-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A file that does not parse, or names no version, is set aside as .bad-<unix seconds> and
// the store starts empty.
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
		"null":         `null`,
		"no version":   `{"rounds":{}}`,
		"null version": `{"version":null,"rounds":{}}`,
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
			bad := setAsideFiles(t, dir)
			if len(bad) != 1 {
				t.Fatalf("set-aside files = %v", bad)
			}
			if b, err := os.ReadFile(bad[0]); err != nil || string(b) != content {
				t.Errorf("%s = %q %v", bad[0], b, err)
			}
			if !strings.Contains(logged[0], filepath.Base(bad[0])) {
				t.Errorf("log line names no file: %q", logged[0])
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

// A file a newer build wrote is refused, never set aside, even when its fields do not decode
// into this build's types.
func TestStoreNewerVersionRefused(t *testing.T) {
	for name, content := range map[string]string{
		"plain":       `{"version":99,"rounds":{}}`,
		"new shapes":  `{"version":2,"rounds":{"x":{"round":{"id":"x","timeout":3600}}}}`,
		"runs as map": `{"version":2,"runs":{"x":{"run-1":{}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := statePath(dir)
			os.MkdirAll(filepath.Dir(p), 0o700)
			os.WriteFile(p, []byte(content), 0o600)
			if _, _, err := Open(dir, Options{Zone: "UTC"}); err == nil || !strings.Contains(err.Error(), "newer") {
				t.Errorf("err = %v", err)
			}
			if bad := setAsideFiles(t, dir); len(bad) != 0 {
				t.Errorf("a newer file was set aside: %v", bad)
			}
			if b, _ := os.ReadFile(p); string(b) != content {
				t.Errorf("the newer file changed: %q", b)
			}
		})
	}
}

// A second corrupt file never replaces one set aside before.
func TestStoreSetAsideKeepsEarlier(t *testing.T) {
	dir := t.TempDir()
	p := statePath(dir)
	os.MkdirAll(filepath.Dir(p), 0o700)
	for i, content := range []string{"first", "second", "third"} {
		os.WriteFile(p, []byte(content), 0o600)
		if _, setAside, err := Open(dir, Options{Zone: "UTC"}); err != nil || !setAside {
			t.Fatalf("open %d: %v %v", i, setAside, err)
		}
	}
	got := map[string]bool{}
	for _, f := range setAsideFiles(t, dir) {
		b, _ := os.ReadFile(f)
		got[string(b)] = true
	}
	if len(got) != 3 || !got["first"] || !got["second"] || !got["third"] {
		t.Errorf("set-aside contents = %v", got)
	}

	// Names within one second take a counter.
	at := time.Unix(1700000000, 0)
	base := fmt.Sprintf("%s.bad-%d", p, at.Unix())
	n1, _ := setAsideName(p, at)
	os.WriteFile(n1, nil, 0o600)
	n2, _ := setAsideName(p, at)
	os.WriteFile(n2, nil, 0o600)
	n3, _ := setAsideName(p, at)
	if n1 != base || n2 != base+"-2" || n3 != base+"-3" {
		t.Errorf("names = %s %s %s", n1, n2, n3)
	}
}

// A stored round whose trigger no longer compiles loads, and TriggerErrors reports it.
func TestStoreTriggerErrors(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	good, _ := s.Create(freshRound(), "", testNow)
	r := freshRound()
	r.Name = "elsewhere"
	r.Trigger.Zone = "Europe/Paris"
	bad, _ := s.Create(r, "", testNow)
	if errs := s.TriggerErrors(); len(errs) != 0 {
		t.Errorf("errors before = %v", errs)
	}
	b, _ := os.ReadFile(statePath(dir))
	b = []byte(strings.Replace(string(b), "Europe/Paris", "Mars/Olympus", 1))
	os.WriteFile(statePath(dir), b, 0o600)

	s2 := openStore(t, dir)
	if len(s2.List()) != 2 {
		t.Fatalf("rounds = %d", len(s2.List()))
	}
	errs := s2.TriggerErrors()
	if len(errs) != 1 || errs[bad.Round.ID] == nil || !strings.Contains(errs[bad.Round.ID].Error(), "Mars/Olympus") {
		t.Errorf("errors = %v", errs)
	}
	if _, ok := errs[good.Round.ID]; ok {
		t.Error("a good round reported")
	}
	// The round can still be fixed by an edit.
	e, _ := s2.Get(bad.Round.ID)
	e.Round.Trigger.Zone = "Asia/Tokyo"
	if _, err := s2.Update(e.Round, testNow); err != nil {
		t.Fatal(err)
	}
	if errs := s2.TriggerErrors(); len(errs) != 0 {
		t.Errorf("errors after the fix = %v", errs)
	}
}

// With no zone in Options, a new round takes the host's zone at the moment it is created.
func TestStoreZoneAtCreate(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	s, _, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Create(freshRound(), "", testNow)
	t.Setenv("TZ", "Europe/Paris")
	r := freshRound()
	r.Name = "later"
	b, _ := s.Create(r, "", testNow)
	if a.Round.Trigger.Zone != "Asia/Tokyo" || b.Round.Trigger.Zone != "Europe/Paris" {
		t.Errorf("zones = %q %q", a.Round.Trigger.Zone, b.Round.Trigger.Zone)
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

// An edit that changes only the trigger's zone moves the next slot.
func TestStoreUpdateZoneOnly(t *testing.T) {
	s := openStore(t, t.TempDir())
	e, _ := s.Create(freshRound(), "", testNow) // 09:00 New York
	r := e.Round
	r.Trigger.Zone = "Europe/Paris"
	u, err := s.Update(r, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// 09:00 in Paris on 2026-06-02 (CEST) is 07:00 UTC; 2026-06-01 09:00 has passed at noon UTC.
	if want := utc("2026-06-02T07:00:00Z"); u.State.NextSlot == nil || !u.State.NextSlot.Equal(want) {
		t.Errorf("next slot = %v, want %v", u.State.NextSlot, want)
	}
}

// A changed trigger re-arms a completed round; another edit leaves it completed.
func TestStoreUpdateRearmsCompleted(t *testing.T) {
	s := openStore(t, t.TempDir())
	at := testNow.Add(time.Hour)
	r := freshRound()
	r.Trigger = Trigger{Kind: TriggerOnce, At: &at}
	e, _ := s.Create(r, "", testNow)
	later := testNow.Add(2 * time.Hour)
	if _, err := s.SetState(e.Round.ID, func(st *RoundState) {
		st.NextSlot, st.LastSlot = nil, &at
		st.Completed, st.CompletedReason = true, "once"
	}); err != nil {
		t.Fatal(err)
	}

	r = e.Round
	r.Prompt = "edited"
	u, err := s.Update(r, later)
	if err != nil {
		t.Fatal(err)
	}
	if !u.State.Completed || u.State.NextSlot != nil {
		t.Errorf("an edit that keeps the trigger re-armed the round: %+v", u.State)
	}

	again := testNow.Add(24 * time.Hour)
	r.Trigger.At = &again
	u, err = s.Update(r, later)
	if err != nil {
		t.Fatal(err)
	}
	if u.State.Completed || u.State.CompletedReason != "" || u.State.NextSlot == nil || !u.State.NextSlot.Equal(again) {
		t.Errorf("a new instant did not re-arm the round: %+v", u.State)
	}
	if u.State.LastSlot == nil || !u.State.LastSlot.Equal(at) {
		t.Errorf("the last served slot changed: %v", u.State.LastSlot)
	}
}

// Get, ByName and List return copies: changing one never reaches the store.
func TestStoreReturnsCopies(t *testing.T) {
	s := openStore(t, t.TempDir())
	r := freshRound()
	exp := testNow.Add(time.Hour)
	r.HarnessArgs, r.ExpiresAt = []string{"--a"}, &exp
	e, _ := s.Create(r, "", testNow)
	slot := *e.State.NextSlot

	mutate := func(g Entry) {
		g.Round.HarnessArgs[0] = "--changed"
		*g.Round.ExpiresAt = exp.Add(time.Hour)
		*g.State.NextSlot = slot.Add(time.Hour)
	}
	g, _ := s.Get(e.Round.ID)
	mutate(g)
	n, _ := s.ByName(r.Name)
	mutate(n)
	mutate(s.List()[0])
	mutate(e)

	g, _ = s.Get(e.Round.ID)
	if g.Round.HarnessArgs[0] != "--a" || !g.Round.ExpiresAt.Equal(exp) || !g.State.NextSlot.Equal(slot) {
		t.Errorf("the store shares memory with a caller: %+v %+v", g.Round, g.State)
	}
}

// A run that has ended never changes state, number or creation time again.
func TestRunLogEndedRun(t *testing.T) {
	s := openStore(t, t.TempDir())
	e, _ := s.Create(freshRound(), "", testNow)
	log := s.Runs()
	run := Run{ID: "run-1", Number: 1, RoundID: e.Round.ID, State: RunRunning, Created: testNow}
	if err := log.Append(run); err != nil {
		t.Fatal(err)
	}
	run.State = RunSucceeded
	if err := log.Update(run); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Run){
		"state":   func(r *Run) { r.State = RunRunning },
		"final":   func(r *Run) { r.State = RunFailed },
		"number":  func(r *Run) { r.Number = 9 },
		"created": func(r *Run) { r.Created = testNow.Add(time.Hour) },
	} {
		r := run
		edit(&r)
		if err := log.Update(r); !errors.Is(err, ErrRunEnded) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Other fields of an ended run can still be filled in, such as the session id.
	r := run
	r.SessionID, r.Result = "sess-1", "done"
	if err := log.Update(r); err != nil {
		t.Errorf("filling in an ended run: %v", err)
	}
	if got, _, _ := log.Page(e.Round.ID, 0, 1); got[0].State != RunSucceeded || got[0].Result != "done" {
		t.Errorf("stored = %+v", got[0])
	}
}

// A run id is unique across the whole log, not only within one round.
func TestRunLogIDAcrossRounds(t *testing.T) {
	s := openStore(t, t.TempDir())
	a, _ := s.Create(freshRound(), "", testNow)
	r := freshRound()
	r.Name = "other"
	b, _ := s.Create(r, "", testNow)
	if err := s.Runs().Append(Run{ID: "run-1", Number: 1, RoundID: a.Round.ID, State: RunQueued}); err != nil {
		t.Fatal(err)
	}
	if err := s.Runs().Append(Run{ID: "run-1", Number: 1, RoundID: b.Round.ID, State: RunQueued}); !errors.Is(err, ErrRunExists) {
		t.Errorf("err = %v", err)
	}
}

// Pruned runs stay gone after the store is opened again.
func TestPruneSaved(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	s.write = func(p string, b []byte) error { return os.WriteFile(p, b, 0o600) }
	e, _ := s.Create(freshRound(), "", testNow)
	old := testNow.Add(-2 * KeepFor)
	for i := 1; i <= KeepRuns+5; i++ {
		if err := s.Runs().Append(Run{ID: fmt.Sprintf("run-%d", i), Number: i, RoundID: e.Round.ID, State: RunSucceeded, Created: old}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.Runs().Prune(e.Round.ID, testNow)
	if err != nil || len(removed) != 5 {
		t.Fatalf("removed %d, %v", len(removed), err)
	}
	s2 := openStore(t, dir)
	runs, more, _ := s2.Runs().Page(e.Round.ID, 0, 1000)
	if len(runs) != KeepRuns || more || runs[len(runs)-1].Number != 6 {
		t.Errorf("after reopen: %d runs, oldest %d", len(runs), runs[len(runs)-1].Number)
	}
}

// However recent, a round keeps at most MaxKeepRuns ended runs; live runs are never pruned
// and do not count.
func TestRetentionCap(t *testing.T) {
	now := utc("2026-06-01T00:00:00Z")
	var runs []Run
	n := MaxKeepRuns + 300
	for i := 1; i <= n; i++ {
		st := RunSucceeded
		if i == 1 || i == n {
			st = RunRunning
		}
		// One run a minute: all within 90 days.
		runs = append(runs, Run{Number: i, State: st, Created: now.Add(-time.Duration(n-i) * time.Minute)})
	}
	keep := Keep(runs, now)
	kept, oldestEnded := 0, 0
	for i, k := range keep {
		if !k {
			continue
		}
		kept++
		if runs[i].State.Final() && oldestEnded == 0 {
			oldestEnded = runs[i].Number
		}
	}
	if kept != MaxKeepRuns+2 || !keep[0] || !keep[n-1] {
		t.Errorf("kept %d (first %v, last %v), want %d ended and both live runs", kept, keep[0], keep[n-1], MaxKeepRuns)
	}
	if want := n - 1 - MaxKeepRuns + 1; oldestEnded != want {
		t.Errorf("oldest ended run kept = %d, want %d", oldestEnded, want)
	}
}
