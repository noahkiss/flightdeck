package main

// The watchers' supervisor (rounds.md 4.4, 4.7, 14.1 stage 11) over fake panes, a fake clock
// and fake timers: a wake hangs each watcher up once and starts it again after the settle,
// counting nothing toward its backoff; the backoff doubles up to watch_backoff_max; quick exits
// pause the round at watch_pause_after.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// watchClock is a scheduler clock the test moves: wall and monotonic apart for a sleep.
type watchClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (c *watchClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *watchClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *watchClock) move(wall, mono time.Duration) {
	c.mu.Lock()
	c.wall, c.mono = c.wall.Add(wall), c.mono+mono
	c.mu.Unlock()
}

// watchFakes are the supervisor's fake panes and timers.
type watchFakes struct {
	mu      sync.Mutex
	n       int
	exited  map[string]bool
	starts  []string // panes in start order
	hangUps []string
	timers  []*watchTimer
	svc     *roundsService
}

type watchTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (f *watchFakes) after(d time.Duration, fn func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	tm := &watchTimer{d: d, f: fn}
	f.timers = append(f.timers, tm)
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		was := !tm.stopped
		tm.stopped = true
		return was
	}
}

// fire moves the clock past the live timers, as real time would, runs them and returns their
// delays.
func (f *watchFakes) fire(clk *watchClock) []time.Duration {
	f.mu.Lock()
	var run []*watchTimer
	for _, tm := range f.timers {
		if !tm.stopped {
			tm.stopped = true
			run = append(run, tm)
		}
	}
	f.timers = nil
	f.mu.Unlock()
	var ds []time.Duration
	var most time.Duration
	for _, tm := range run {
		ds = append(ds, tm.d)
		most = max(most, tm.d)
	}
	clk.move(most, most)
	for _, tm := range run {
		tm.f()
	}
	return ds
}

func (f *watchFakes) counts() (starts, hangUps int, last string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) > 0 {
		last = f.starts[len(f.starts)-1]
	}
	return len(f.starts), len(f.hangUps), last
}

func newWatchSupTest(t *testing.T, lim rounds.WatchLimits, more ...func(*roundsConfig)) (*roundsService, *watchFakes, *watchClock, string) {
	t.Helper()
	f := &watchFakes{exited: map[string]bool{}}
	clk := &watchClock{wall: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	svc, _ := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.settings.Watch = lim
		c.clock = clk
		c.watchAfter = f.after
		c.exe = "/bin/gff-hab-test"
		c.create = func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.n++
			pane := fmt.Sprintf("p%d", f.n)
			f.starts = append(f.starts, pane)
			return api.SessionCreated{Session: "s-watch", Pane: pane}, nil
		}
		c.pane = func(id string) (link.PaneStatus, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return link.PaneStatus{Exited: f.exited[id]}, true
		}
		c.closePane = func(_ context.Context, pane string) error {
			f.mu.Lock()
			f.hangUps = append(f.hangUps, pane)
			f.exited[pane] = true
			svc := f.svc
			f.mu.Unlock()
			svc.watchers.exited(pane, "ended by SIGHUP")
			return nil
		}
		for _, m := range more {
			m(c)
		}
	})
	f.mu.Lock()
	f.svc = svc
	f.mu.Unlock()
	p, err := svc.cfg.store.CreateProject("app", "https://example.com/team/app.git", "")
	if err != nil {
		t.Fatal(err)
	}
	return svc, f, clk, p.ID
}

func createWatchRound(t *testing.T, svc *roundsService, project, name string) string {
	t.Helper()
	made, err := svc.Create(context.Background(), api.RoundCreateParams{Round: api.RoundSpec{
		Name: name, Mode: api.RoundModeFresh, Project: project, Command: []string{"true"},
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindWatch, Watch: &api.RoundWatch{Argv: []string{"/bin/sleep", "60"}}},
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	return made.Round.ID
}

// crash ends a running watcher's pane on its own, as the link's notice would.
func (f *watchFakes) crash(pane string) {
	f.mu.Lock()
	f.exited[pane] = true
	svc := f.svc
	f.mu.Unlock()
	svc.watchers.exited(pane, "exit status 3")
}

func TestWatchSupWakeAndBackoff(t *testing.T) {
	lim := rounds.DefaultWatchLimits()
	lim.BackoffMax, lim.PauseAfter, lim.QuickExit = 10*time.Second, 0, time.Minute
	svc, f, clk, project := newWatchSupTest(t, lim)
	svc.start()
	id := createWatchRound(t, svc, project, "w")
	waitUntil(t, "the first start", func() bool { n, _, _ := f.counts(); return n == 1 })
	svc.sched.Tick() // the first reading after start is no wake

	// A sleep: the wall clock moves an hour, the monotonic clock does not. The scheduler's tick
	// sees the wake; past the settle (0 here) the supervisor hangs the watcher up once and starts
	// it again at once.
	clk.move(time.Hour, 0)
	svc.sched.Tick()
	waitUntil(t, "the start after the wake", func() bool { n, h, _ := f.counts(); return n == 2 && h == 1 })
	svc.sched.Tick()
	time.Sleep(100 * time.Millisecond)
	if n, h, _ := f.counts(); n != 2 || h != 1 {
		t.Fatalf("after one wake: %d starts, %d hang-ups", n, h)
	}
	w := svc.watchers.state(id)
	if w.State != watchRunning || w.Restarts != 1 || w.LastExit != "ended by SIGHUP (hung up by gff-hab)" {
		t.Fatalf("after the wake %+v", w)
	}
	svc.watchers.mu.Lock()
	back := svc.watchers.m[id].back
	svc.watchers.mu.Unlock()
	if back != (rounds.Backoff{}) {
		t.Fatalf("gff-hab's own hang-up counted toward the backoff: %+v", back)
	}

	// Crashes back off 1 s, 2 s, 4 s, 8 s, then the cap of 10 s, and never pause (pause_after 0).
	var got []time.Duration
	for i := 0; i < 6; i++ {
		_, _, pane := f.counts()
		clk.move(2*time.Minute, 2*time.Minute) // not a quick exit
		f.crash(pane)
		if w := svc.watchers.state(id); w.State != watchBackoff {
			t.Fatalf("after crash %d: %+v", i, w)
		}
		ds := f.fire(clk)
		if len(ds) != 1 {
			t.Fatalf("crash %d: timers %v", i, ds)
		}
		got = append(got, ds[0])
		waitUntil(t, "the start after the backoff", func() bool { n, _, _ := f.counts(); return n == 3+i })
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("backoff %v, want %v", got, want)
	}
	if e, _ := svc.store.Get(id); e.Round.Paused {
		t.Fatal("pause_after 0 paused the round")
	}
}

func TestWatchSupPauseAfter(t *testing.T) {
	lim := rounds.DefaultWatchLimits()
	lim.PauseAfter = 2
	svc, f, clk, project := newWatchSupTest(t, lim)
	svc.start()
	id := createWatchRound(t, svc, project, "w")
	waitUntil(t, "the first start", func() bool { n, _, _ := f.counts(); return n == 1 })
	_, _, pane := f.counts()
	f.crash(pane) // quick exit 1: backs off
	if w := svc.watchers.state(id); w.State != watchBackoff || w.LastExit != "exit status 3" {
		t.Fatalf("after one quick exit %+v", w)
	}
	f.fire(clk)
	waitUntil(t, "the restart", func() bool { n, _, _ := f.counts(); return n == 2 })
	_, _, pane = f.counts()
	f.crash(pane) // quick exit 2: pauses
	waitUntil(t, "the pause", func() bool {
		e, _ := svc.store.Get(id)
		return e.Round.Paused && e.State.PausedReason == rounds.ReasonWatcherPause
	})
	w := svc.watchers.state(id)
	if w.State != watchStopped || w.Reason != watchReasonExits {
		t.Fatalf("after the pause %+v", w)
	}
	if ds := f.fire(clk); len(ds) != 0 {
		t.Fatalf("a paused round's watcher still has a timer: %v", ds)
	}
	time.Sleep(100 * time.Millisecond)
	if n, _, _ := f.counts(); n != 2 {
		t.Fatalf("%d starts after the pause, want 2", n)
	}
}

// A look at the round between the quick exit that pauses it and the scheduler's pause (here
// the round.updated that exitDone publishes first) starts nothing: the watcher waits for the
// pause, then stays stopped with the crash as its last exit.
func TestWatchSupPauseInFlight(t *testing.T) {
	lim := rounds.DefaultWatchLimits()
	lim.PauseAfter = 1
	var svcp atomic.Pointer[roundsService]
	var looked atomic.Bool
	svc, f, _, project := newWatchSupTest(t, lim, func(c *roundsConfig) {
		c.publish = func(ev structure.Event) {
			svc := svcp.Load()
			if svc == nil || ev.Type != control.EventRoundUpdated {
				return
			}
			w := svc.watchers.state(ev.ID)
			if e, _ := svc.store.Get(ev.ID); w == nil || w.Reason != watchReasonExits || e.Round.Paused || looked.Swap(true) {
				return
			}
			// The loop gets the look and has time to act on it before the pause lands.
			svc.watchers.look(ev.ID)
			time.Sleep(300 * time.Millisecond)
		}
	})
	svcp.Store(svc)
	svc.start()
	id := createWatchRound(t, svc, project, "w")
	waitUntil(t, "the first start", func() bool { n, _, _ := f.counts(); return n == 1 })
	_, _, pane := f.counts()
	f.crash(pane) // quick exit 1: pauses
	if !looked.Load() {
		t.Fatal("the test's look between the exit and the pause never ran")
	}
	waitUntil(t, "the pause", func() bool {
		e, _ := svc.store.Get(id)
		return e.Round.Paused && e.State.PausedReason == rounds.ReasonWatcherPause
	})
	time.Sleep(200 * time.Millisecond)
	if n, h, _ := f.counts(); n != 1 || h != 0 {
		t.Fatalf("%d starts and %d hang-ups around the pause, want 1 and 0", n, h)
	}
	if w := svc.watchers.state(id); w.State != watchStopped || w.Reason != watchReasonExits || w.LastExit != "exit status 3" || w.Restarts != 0 {
		t.Fatalf("after the pause %+v", w)
	}
}

// The watcher keys of rounds.md 4.7 and the secret sets of 4.4: good values are taken, a bad
// one is logged once by its key and the default runs, and a bad set is left out with a log
// line that holds no value.
func TestWatchSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	writeFile(t, p, "[rounds]\nmax_watchers = 3\nwatch_backoff_max = \"30s\"\nwatch_backoff_reset = \"5m\"\nwatch_pause_after = 2\n"+
		"watch_quick_exit = \"10s\"\nwatch_buffer_lines = 50\nwatch_buffer_bytes = 65536\nwatch_rate = 10\nwatch_burst = 4\n"+
		"event_title_max = 80\nevent_key_window = \"2h\"\nevent_keys_max = 9\nevent_data_max = 100\ncursor_max = 128\nsecrets_timeout = \"5s\"\n\n"+
		"[rounds.secrets.mail]\nvars = { IMAP_USER = \"user-value-1b2c\" }\ncommand = [\"/bin/cat\", \"/nowhere\"]\n\n"+
		"[rounds.secrets.\"bad name!\"]\nvars = { A = \"name-value-77aa\" }\n\n"+
		"[rounds.secrets.badvar]\nvars = { \"1X\" = \"var-value-55cd\" }\n\n"+
		"[rounds.secrets.badcmd]\ncommand = \"/bin/cat\"\n\n"+
		"[rounds.secrets.badkey]\nvalue = \"key-value-ee01\"\n")
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	s, err := loadRoundsSettings(p, logf)
	if err != nil {
		t.Fatal(err)
	}
	want := rounds.WatchLimits{MaxWatchers: 3, BackoffMax: 30 * time.Second, BackoffReset: 5 * time.Minute, PauseAfter: 2,
		QuickExit: 10 * time.Second, BufferLines: 50, BufferBytes: 65536, Rate: 10, Burst: 4, TitleMax: 80,
		KeyWindow: 2 * time.Hour, KeysMax: 9, DataMax: 100, CursorMax: 128, SecretsTimeout: 5 * time.Second}
	if s.Watch != want {
		t.Fatalf("watch limits %+v, want %+v", s.Watch, want)
	}
	if len(s.Secrets) != 1 || s.Secrets["mail"].Vars["IMAP_USER"] != "user-value-1b2c" || fmt.Sprint(s.Secrets["mail"].Command) != "[/bin/cat /nowhere]" {
		t.Fatalf("secret sets %+v", s.Secrets)
	}
	if len(logs) != 4 {
		t.Fatalf("%d log lines, want one per bad set: %v", len(logs), logs)
	}
	for _, l := range logs {
		for _, v := range []string{"user-value-1b2c", "name-value-77aa", "var-value-55cd", "key-value-ee01"} {
			if strings.Contains(l, v) {
				t.Errorf("a log line holds a secret value: %s", l)
			}
		}
	}

	writeFile(t, p, "[rounds]\nwatch_backoff_max = \"1s\"\nwatch_pause_after = 0\nwatch_rate = \"fast\"\nmax_watchers = 1000\n")
	logs = nil
	s, err = loadRoundsSettings(p, logf)
	if err != nil || len(logs) != 4 {
		t.Fatalf("%v: %d lines: %v", err, len(logs), logs)
	}
	if s.Watch != rounds.DefaultWatchLimits() {
		t.Fatalf("defaults did not run: %+v", s.Watch)
	}
}

// secretValues: vars in name order, then the command's NAME=value lines (comments and blank
// lines skipped); a failure names a line number or the exit, never a value.
func TestSecretValues(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "good")
	writeFile(t, good, "# comment\n\nB_TOKEN=cmd-value-4f4f\r\nC=with=equals\n")
	env, err := secretValues(secretSet{Vars: map[string]string{"Z": "z-value", "A": "a-value"}, Command: []string{"/bin/cat", good}}, time.Second)
	if err != nil || fmt.Sprint(env) != "[A=a-value Z=z-value B_TOKEN=cmd-value-4f4f C=with=equals]" {
		t.Fatalf("%v %v", env, err)
	}
	bad := filepath.Join(d, "bad")
	writeFile(t, bad, "OK=1\nnot-a-line-value-9e9e\n")
	for _, c := range []struct {
		set  secretSet
		want string
	}{
		{secretSet{Command: []string{"/bin/cat", bad}}, "line 2 is not NAME=value"},
		{secretSet{Command: []string{"/bin/sh", "-c", "echo printed-value-3c3c >&2; exit 4"}}, "exit status 4"},
		{secretSet{Command: []string{"/bin/sleep", "5"}}, "ran past"},
		{secretSet{Command: []string{filepath.Join(d, "none")}}, "did not run"},
	} {
		_, err := secretValues(c.set, 300*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "value-") {
			t.Errorf("%v: %v, want %q", c.set.Command, err, c.want)
		}
	}
}
