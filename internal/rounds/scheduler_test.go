package rounds

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock has a wall clock and a monotonic reading that tests move apart: Advance moves
// both (awake time), Sleep and Set move the wall clock only.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *fakeClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *fakeClock) move(wall, mono time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(wall)
	c.mono += mono
	c.mu.Unlock()
}

type fakeStarter struct {
	mu      sync.Mutex
	started []Run
	err     error
}

func (f *fakeStarter) Start(_ Round, run Run) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, run)
	if f.err != nil {
		return "", f.err
	}
	return "sess-" + run.ID, nil
}

func (f *fakeStarter) list() []Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Run(nil), f.started...)
}

type fakeSink struct {
	mu      sync.Mutex
	rounds  []Entry
	runs    []Run
	notices []Notice
}

func (f *fakeSink) RoundChanged(e Entry) { f.mu.Lock(); f.rounds = append(f.rounds, e); f.mu.Unlock() }
func (f *fakeSink) RunChanged(r Run)     { f.mu.Lock(); f.runs = append(f.runs, r); f.mu.Unlock() }
func (f *fakeSink) Notice(n Notice)      { f.mu.Lock(); f.notices = append(f.notices, n); f.mu.Unlock() }

func (f *fakeSink) noticesOf(k NoticeKind) []Notice {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Notice
	for _, n := range f.notices {
		if n.Kind == k {
			out = append(out, n)
		}
	}
	return out
}

type harness struct {
	t     *testing.T
	dir   string
	clock *fakeClock
	store *Store
	start *fakeStarter
	sink  *fakeSink
	s     *Scheduler
	max   int
	// settle is the wake settle; 0 takes the default.
	settle time.Duration
}

func newHarness(t *testing.T, at string) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), clock: &fakeClock{wall: utc(at), mono: time.Hour}}
	h.reopen()
	return h
}

// reopen loads the store from disk and builds a new scheduler, as a gff-hab restart does.
func (h *harness) reopen() {
	h.t.Helper()
	st, _, err := Open(h.dir, Options{Zone: "UTC"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = st
	h.start = &fakeStarter{}
	h.sink = &fakeSink{}
	h.s, err = NewScheduler(SchedulerConfig{Store: st, Starter: h.start, Sink: h.sink, Clock: h.clock, MaxRunning: h.max, WakeSettle: h.settle})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) round(name, cron string, edit func(*Round)) Entry {
	h.t.Helper()
	r := Round{Name: name, Mode: ModeFresh, Project: "proj", Command: []string{"true"},
		Trigger: Trigger{Kind: TriggerCron, Cron: cron, Zone: "UTC"}}
	if edit != nil {
		edit(&r)
	}
	e, err := h.store.Create(r, "", h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// awake ticks every 30 s of awake time until the wall clock reaches at.
func (h *harness) awake(at string) {
	end := utc(at)
	for h.clock.Now().Before(end) {
		step := TickEvery
		if d := end.Sub(h.clock.Now()); d < step {
			step = d
		}
		h.clock.move(step, step)
		h.s.Tick()
	}
}

// sleep moves the wall clock to at with 30 s of monotonic time, then ticks: a host that slept.
func (h *harness) sleep(at string) {
	h.clock.move(utc(at).Sub(h.clock.Now()), TickEvery)
	h.s.Tick()
}

func (h *harness) runs(id string) []Run {
	h.t.Helper()
	runs, _, err := h.store.Runs().Page(id, 0, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	return runs // newest first
}

func (h *harness) entry(id string) Entry {
	e, ok := h.store.Get(id)
	if !ok {
		h.t.Fatalf("round %s gone", id)
	}
	return e
}

func (h *harness) end(run Run, state RunState, reason string, network bool) {
	h.t.Helper()
	if err := h.s.UpdateRun(run.ID, RunUpdate{State: state, Reason: reason, Network: network}); err != nil {
		h.t.Fatal(err)
	}
}

// endLatest ends the round's newest run, which must be running.
func (h *harness) endLatest(id string, state RunState) Run {
	h.t.Helper()
	runs := h.runs(id)
	if len(runs) == 0 || runs[0].State != RunRunning {
		h.t.Fatalf("no running run: %+v", runs)
	}
	h.end(runs[0], state, string(state), false)
	return runs[0]
}

func TestSchedulerSleepCatchUpOnce(t *testing.T) {
	for _, skip := range []bool{false, true} {
		h := newHarness(t, "2026-03-02T09:59:00Z")
		e := h.round("hourly", "0 * * * *", func(r *Round) {
			if skip {
				r.CatchUp = CatchUpSkip
			}
		})
		h.s.Tick()
		h.awake("2026-03-02T10:00:30Z")
		runs := h.runs(e.Round.ID)
		if len(runs) != 1 || runs[0].Missed != 1 || !runs[0].Slot.Equal(utc("2026-03-02T10:00:00Z")) || runs[0].State != RunRunning {
			t.Fatalf("on-time run: %+v", runs)
		}
		h.endLatest(e.Round.ID, RunSucceeded)

		// Asleep from 10:00:30 to 20:00:20: ten slots, 11:00 to 20:00.
		h.sleep("2026-03-02T20:00:20Z")
		h.awake("2026-03-02T20:01:00Z")
		runs = h.runs(e.Round.ID)
		if skip {
			if len(runs) != 1 {
				t.Fatalf("skip: runs = %+v", runs)
			}
			if n := h.entry(e.Round.ID).State.NextSlot; n == nil || !n.Equal(utc("2026-03-02T21:00:00Z")) {
				t.Errorf("skip: next slot = %v", n)
			}
			continue
		}
		if len(runs) != 2 {
			t.Fatalf("runs = %+v", runs)
		}
		r := runs[0]
		if r.Missed != 10 || !r.Slot.Equal(utc("2026-03-02T20:00:00Z")) || r.Number != 2 {
			t.Errorf("catch-up run: %+v", r)
		}
		if st := h.entry(e.Round.ID).State; !st.NextSlot.Equal(utc("2026-03-02T21:00:00Z")) || !st.LastSlot.Equal(utc("2026-03-02T20:00:00Z")) {
			t.Errorf("state: next %v last %v", st.NextSlot, st.LastSlot)
		}
	}
}

func TestSchedulerDailyWakeAfterSettle(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].State != RunQueued || !runs[0].Created.Equal(utc("2026-03-02T22:00:00Z")) {
		t.Fatalf("at wake: %+v", runs)
	}
	if len(h.start.list()) != 0 {
		t.Fatal("started inside the settle")
	}
	h.awake("2026-03-02T22:00:30Z")
	got := h.start.list()
	if len(got) != 1 || !got[0].StartedAt.Equal(utc("2026-03-02T22:00:30Z")) || got[0].Missed != 1 {
		t.Fatalf("starts: %+v", got)
	}
	h.awake("2026-03-02T23:00:00Z")
	if len(h.runs(e.Round.ID)) != 1 {
		t.Error("more than one run")
	}
}

func TestSchedulerClockBack(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)

	// The clock is set back an hour, with no monotonic gap.
	h.clock.move(-time.Hour, TickEvery)
	h.s.Tick()
	// An edit that sets the next slot from the moved-back now lands on the served 10:00 slot.
	cur := h.entry(e.Round.ID)
	cur.Round.Trigger.Cron = "0 */1 * * *"
	if _, err := h.store.Update(cur.Round, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.awake("2026-03-02T09:01:30Z")
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T11:00:00Z")) {
		t.Errorf("next slot after the edit = %v, want the slot after the last served", n)
	}
	h.awake("2026-03-02T10:30:00Z")
	if runs := h.runs(e.Round.ID); len(runs) != 1 {
		t.Fatalf("10:00 served twice: %+v", runs)
	}
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T11:00:00Z")) {
		t.Errorf("next slot = %v", n)
	}
	h.awake("2026-03-02T11:00:30Z")
	if runs := h.runs(e.Round.ID); len(runs) != 2 || !runs[0].Slot.Equal(utc("2026-03-02T11:00:00Z")) {
		t.Errorf("11:00: %+v", runs)
	}
}

func TestSchedulerClockJumpForward(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)

	// The wall clock jumps 2 h while the monotonic clock moves one tick: seen as a wake.
	h.clock.move(2*time.Hour, TickEvery)
	h.s.Tick()
	runs := h.runs(e.Round.ID)
	if len(runs) != 2 || runs[0].State != RunQueued || runs[0].Missed != 2 {
		t.Fatalf("after the jump: %+v", runs)
	}
	h.awake("2026-03-02T12:01:30Z")
	if runs := h.runs(e.Round.ID); len(runs) != 2 || runs[0].State != RunRunning {
		t.Fatalf("after the settle: %+v", runs)
	}
	// A small forward step is no wake: a slot found on an ordinary tick starts at once.
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T13:00:30Z")
	if runs := h.runs(e.Round.ID); len(runs) != 3 || runs[0].State != RunRunning {
		t.Fatalf("13:00: %+v", runs)
	}
}

func TestSchedulerWakeDetection(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:00:00Z")
	h.s.Tick()
	first := h.s.lastWake
	if !first.Equal(utc("2026-03-02T09:00:00Z")) {
		t.Fatalf("the first tick is no wake: %v", first)
	}
	cases := []struct {
		wall, mono time.Duration
		wake       bool
	}{
		{TickEvery, TickEvery, false},
		{TickEvery + WakeGap, TickEvery, false},              // exactly the gap: no wake
		{TickEvery + WakeGap + time.Second, TickEvery, true}, // one second over
		{-time.Hour, TickEvery, false},                       // the clock set back
		{3 * time.Minute, TickEvery, true},
	}
	for i, c := range cases {
		before := h.s.lastWake
		h.clock.move(c.wall, c.mono)
		h.s.Tick()
		if got := !h.s.lastWake.Equal(before); got != c.wake {
			t.Errorf("case %d: wake = %v, want %v", i, got, c.wake)
		}
	}
}

func TestSchedulerRestart(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:50:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T09:55:00Z")
	// gff-hab stops at 09:55 and starts again at 10:02.
	h.clock.move(7*time.Minute, 7*time.Minute)
	h.reopen()
	h.s.Tick()
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].State != RunQueued || runs[0].Missed != 1 {
		t.Fatalf("after restart: %+v", runs)
	}
	h.awake("2026-03-02T10:03:00Z")
	if got := h.start.list(); len(got) != 1 {
		t.Fatalf("starts: %+v", got)
	}

	// A restart right after a slot queues its run; a second restart finds the queued run on
	// disk and starts it, with no second run.
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T10:59:50Z")
	h.reopen()
	h.clock.move(30*time.Second, 30*time.Second)
	h.s.Tick()
	if runs := h.runs(e.Round.ID); len(runs) != 2 || runs[0].State != RunQueued {
		t.Fatalf("after the second restart: %+v", runs)
	}
	h.reopen()
	h.awake("2026-03-02T11:30:00Z")
	if got := h.start.list(); len(got) != 1 || got[0].Missed != 1 {
		t.Fatalf("starts after the third start: %+v", got)
	}
	if runs := h.runs(e.Round.ID); len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestSchedulerRestartRecomputesNextSlot(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:50:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	// A next slot that no longer matches the trigger (as after a zone database change).
	odd := utc("2026-03-02T10:17:00Z")
	if _, err := h.store.SetState(e.Round.ID, func(st *RoundState) { st.NextSlot = &odd }); err != nil {
		t.Fatal(err)
	}
	h.reopen()
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T10:00:00Z")) {
		t.Errorf("next slot = %v", n)
	}
}

func TestSchedulerCoalesce(t *testing.T) {
	for _, skip := range []bool{false, true} {
		h := newHarness(t, "2026-03-02T09:59:00Z")
		e := h.round("hourly", "0 * * * *", func(r *Round) {
			if skip {
				r.CatchUp = CatchUpSkip
			}
		})
		h.s.Tick()
		h.awake("2026-03-02T12:30:00Z") // the 10:00 run is still running at 11:00 and 12:00
		runs := h.runs(e.Round.ID)
		if len(runs) != 1 {
			t.Fatalf("skip=%v: a second run while one is live: %+v", skip, runs)
		}
		h.end(runs[0], RunSucceeded, "", false)
		runs = h.runs(e.Round.ID)
		if len(runs) != 2 || runs[0].State != RunRunning || runs[0].Missed != 2 || !runs[0].Slot.Equal(utc("2026-03-02T12:00:00Z")) {
			t.Fatalf("skip=%v: coalesced run: %+v", skip, runs)
		}
		h.end(runs[0], RunSucceeded, "", false)
		h.awake("2026-03-02T12:59:00Z")
		if runs := h.runs(e.Round.ID); len(runs) != 2 {
			t.Fatalf("skip=%v: runs = %d", skip, len(runs))
		}
	}
}

func TestSchedulerHostLimit(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	a := h.round("a", "0 10 * * *", nil)
	b := h.round("b", "30 9 * * *", nil)
	c := h.round("c", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T10:05:00Z")
	h.awake("2026-03-02T10:05:30Z")
	got := h.start.list()
	if len(got) != 2 || got[0].RoundID != c.Round.ID || got[1].RoundID != b.Round.ID {
		t.Fatalf("starts: %+v", got)
	}
	if r := h.runs(a.Round.ID); len(r) != 1 || r[0].State != RunQueued {
		t.Fatalf("a: %+v", r)
	}
	h.awake("2026-03-02T10:10:00Z")
	if len(h.start.list()) != 2 {
		t.Fatal("a third run started over the limit")
	}
	h.endLatest(c.Round.ID, RunSucceeded)
	if got := h.start.list(); len(got) != 3 || got[2].RoundID != a.Round.ID {
		t.Fatalf("after an end: %+v", got)
	}
}

func TestSchedulerFailurePauseAndUnpause(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("minutely", "* * * * *", nil)
	h.s.Tick()
	for i := 0; i < PauseAfterFailures; i++ {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		h.endLatest(e.Round.ID, RunFailed)
	}
	en := h.entry(e.Round.ID)
	if !en.Round.Paused || en.State.PausedReason != ReasonFailurePause || en.State.Failures != PauseAfterFailures {
		t.Fatalf("not paused: %+v %+v", en.Round.Paused, en.State)
	}
	if n := h.sink.noticesOf(NoticePaused); len(n) != 1 {
		t.Errorf("pause notices = %d", len(n))
	}
	if n := h.sink.noticesOf(NoticeRunFailed); len(n) != PauseAfterFailures {
		t.Errorf("failure notices = %d", len(n))
	}
	h.awake(h.clock.Now().Add(5 * time.Minute).Format(time.RFC3339))
	if runs := h.runs(e.Round.ID); len(runs) != PauseAfterFailures {
		t.Fatalf("ran while paused: %d", len(runs))
	}
	if err := h.s.Unpause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	en = h.entry(e.Round.ID)
	if en.Round.Paused || en.State.Failures != 0 || en.State.Skips != 0 || en.State.PausedReason != "" {
		t.Fatalf("after unpause: %+v %+v", en.Round.Paused, en.State)
	}
	h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
	if runs := h.runs(e.Round.ID); len(runs) != PauseAfterFailures+1 || runs[0].Missed != 1 {
		t.Fatalf("after unpause: %+v", runs[0])
	}
	// A success between failures resets the count.
	h.endLatest(e.Round.ID, RunSucceeded)
	if f := h.entry(e.Round.ID).State.Failures; f != 0 {
		t.Errorf("failures after success = %d", f)
	}
}

func TestSchedulerSkips(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("minutely", "* * * * *", nil)
	h.s.Tick()
	for i := 0; i < SkipsNotice+2; i++ {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		runs := h.runs(e.Round.ID)
		h.end(runs[0], RunSkipped, "in use", false)
	}
	if n := h.sink.noticesOf(NoticeSkips); len(n) != 1 || !strings.Contains(n[0].Text, "in use") {
		t.Fatalf("skip notices = %+v", n)
	}
	en := h.entry(e.Round.ID)
	if en.Round.Paused || en.State.Skips != SkipsNotice+2 {
		t.Fatalf("state: %+v", en.State)
	}
	if err := h.s.Pause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Unpause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	if s := h.entry(e.Round.ID).State.Skips; s != 0 {
		t.Errorf("skips after unpause = %d", s)
	}
}

func TestSchedulerNotifyNever(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("quiet", "* * * * *", func(r *Round) { r.Notify = NotifyNever })
	h.s.Tick()
	for i := 0; i < PauseAfterFailures; i++ {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		h.endLatest(e.Round.ID, RunTimedOut)
	}
	if len(h.sink.noticesOf(NoticeRunFailed)) != 0 || len(h.sink.noticesOf(NoticePaused)) != 1 {
		t.Errorf("notices = %+v", h.sink.notices)
	}
}

func TestSchedulerMaxRunsAndExpiry(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	m := h.round("twice", "* * * * *", func(r *Round) { r.MaxRuns = 2 })
	exp := utc("2026-03-02T10:02:30Z")
	x := h.round("brief", "* * * * *", func(r *Round) { r.ExpiresAt = &exp })
	h.s.Tick()
	for i := 0; i < 4; i++ {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		for _, id := range []string{m.Round.ID, x.Round.ID} {
			if runs := h.runs(id); len(runs) > 0 && runs[0].State == RunRunning {
				h.end(runs[0], RunSucceeded, "", false)
			}
		}
	}
	if runs := h.runs(m.Round.ID); len(runs) != 2 {
		t.Errorf("max_runs: runs = %d", len(runs))
	}
	if st := h.entry(m.Round.ID).State; !st.Completed || st.CompletedReason != ReasonMaxRuns || st.NextSlot != nil {
		t.Errorf("max_runs state: %+v", st)
	}
	if runs := h.runs(x.Round.ID); len(runs) != 3 {
		t.Errorf("expiry: runs = %d", len(runs))
	}
	if st := h.entry(x.Round.ID).State; !st.Completed || st.CompletedReason != ReasonExpired {
		t.Errorf("expiry state: %+v", st)
	}
	if len(h.sink.notices) != 0 {
		t.Errorf("completion raised notices: %+v", h.sink.notices)
	}
}

func TestSchedulerOnceMissed(t *testing.T) {
	for _, skip := range []bool{false, true} {
		h := newHarness(t, "2026-03-02T08:00:00Z")
		at := utc("2026-03-02T09:00:00Z")
		e := h.round("one", "", func(r *Round) {
			r.Trigger = Trigger{Kind: TriggerOnce, At: &at, Zone: "UTC"}
			if skip {
				r.CatchUp = CatchUpSkip
			}
		})
		h.s.Tick()
		h.awake("2026-03-02T08:01:00Z")
		h.sleep("2026-03-02T12:00:00Z")
		h.awake("2026-03-02T12:00:30Z")
		runs := h.runs(e.Round.ID)
		if skip {
			st := h.entry(e.Round.ID).State
			if len(runs) != 0 || !st.Completed || st.CompletedReason != ReasonOnceMissed {
				t.Fatalf("skip: runs %+v state %+v", runs, st)
			}
			continue
		}
		if len(runs) != 1 || runs[0].State != RunRunning {
			t.Fatalf("once: %+v", runs)
		}
		h.end(runs[0], RunSucceeded, "", false)
		if st := h.entry(e.Round.ID).State; !st.Completed || st.CompletedReason != ReasonOnceDone {
			t.Errorf("once state: %+v", st)
		}
	}
}

func TestSchedulerNetworkRetryAfterWake(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("daily", "0 9 * * *", nil)
	other := h.round("other", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	h.awake("2026-03-02T22:01:00Z")
	// Both fail inside the window; only the network failure is retried, 2 minutes later.
	h.endLatest(e.Round.ID, RunFailed)
	runs := h.runs(other.Round.ID)
	h.end(runs[0], RunFailed, "exit 1", false)
	h.awake("2026-03-02T22:06:00Z")
	if len(h.runs(e.Round.ID)) != 1 || len(h.runs(other.Round.ID)) != 1 {
		t.Fatal("not network: no retry expected")
	}

	h2 := newHarness(t, "2026-03-02T08:00:00Z")
	e2 := h2.round("daily", "0 9 * * *", nil)
	h2.s.Tick()
	h2.awake("2026-03-02T08:01:00Z")
	h2.sleep("2026-03-02T22:00:00Z")
	h2.awake("2026-03-02T22:01:00Z")
	runs = h2.runs(e2.Round.ID)
	h2.end(runs[0], RunFailed, "network unreachable", true)
	h2.awake("2026-03-02T22:02:30Z")
	if len(h2.runs(e2.Round.ID)) != 1 {
		t.Fatal("retried before 2 minutes")
	}
	h2.awake("2026-03-02T22:03:30Z")
	runs = h2.runs(e2.Round.ID)
	if len(runs) != 2 || runs[0].State != RunRunning || !runs[0].Slot.Equal(utc("2026-03-02T09:00:00Z")) {
		t.Fatalf("retry: %+v", runs)
	}
	// The retry fails the same way: no second retry.
	h2.end(runs[0], RunFailed, "network unreachable", true)
	h2.awake("2026-03-02T22:10:00Z")
	if len(h2.runs(e2.Round.ID)) != 2 {
		t.Fatal("a second retry")
	}

	// A network failure long after the wake is not retried.
	h2.awake("2026-03-03T09:00:30Z")
	runs = h2.runs(e2.Round.ID)
	h2.end(runs[0], RunFailed, "network unreachable", true)
	h2.awake("2026-03-03T09:10:00Z")
	if len(h2.runs(e2.Round.ID)) != 3 {
		t.Fatal("retried outside the window")
	}
}

func TestSchedulerPauseEndsQueuedRun(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T10:00:00Z")
	if err := h.s.Pause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].State != RunSkipped || runs[0].Reason != ReasonPaused {
		t.Fatalf("runs: %+v", runs)
	}
	h.awake("2026-03-02T10:05:00Z")
	if len(h.start.list()) != 0 {
		t.Error("a paused round started a run")
	}
	if s := h.entry(e.Round.ID).State.Skips; s != 0 {
		t.Errorf("a pause counted as a skip: %d", s)
	}
}

func TestSchedulerStartError(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	h.start.err = errors.New("no such program")
	e := h.round("bad", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].State != RunFailed || !strings.Contains(runs[0].Reason, "no such program") {
		t.Fatalf("runs: %+v", runs)
	}
	if f := h.entry(e.Round.ID).State.Failures; f != 1 {
		t.Errorf("failures = %d", f)
	}
}

func TestSchedulerTriggerError(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("elsewhere", "0 * * * *", func(r *Round) { r.Trigger.Zone = "Europe/Paris" })
	b, _ := os.ReadFile(statePath(h.dir))
	os.WriteFile(statePath(h.dir), []byte(strings.Replace(string(b), "Europe/Paris", "Mars/Olympus", 1)), 0o600)
	h.reopen()
	h.awake("2026-03-02T11:00:30Z")
	if errs := h.s.TriggerErrors(); errs[e.Round.ID] == nil {
		t.Errorf("errors = %v", errs)
	}
	if len(h.runs(e.Round.ID)) != 0 {
		t.Error("an unscheduled round ran")
	}
}

func TestSchedulerRunLoop(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	ticks := make(chan time.Time)
	h.s.cfg.NewTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	e := h.round("hourly", "0 * * * *", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		h.clock.move(TickEvery, TickEvery)
		ticks <- time.Time{}
	}
	ticks <- time.Time{} // the third tick has finished once this one is taken
	cancel()
	<-done
	if runs := h.runs(e.Round.ID); len(runs) != 1 || runs[0].State != RunRunning {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestSystemClock(t *testing.T) {
	c := SystemClock()
	a := c.Mono()
	if d := c.Now().Sub(time.Now()); d > time.Second || d < -time.Second {
		t.Errorf("wall clock off by %v", d)
	}
	if c.Mono() < a {
		t.Error("monotonic reading went back")
	}
	// Now carries no monotonic reading, so Sub of two Now values is wall-clock time.
	if s := c.Now().String(); strings.Contains(s, "m=") {
		t.Errorf("Now keeps the monotonic reading: %s", s)
	}
}

func TestSchedulerLongSleepBound(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("minutely", "* * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	// A year asleep: 525,600 slots, counted up to the bound; the run serves the latest.
	h.sleep("2027-03-02T10:00:40Z")
	runs := h.runs(e.Round.ID)
	if len(runs) != 2 || runs[0].Missed != MaxMissedCount || !runs[0].Slot.Equal(utc("2027-03-02T10:00:00Z")) {
		t.Fatalf("runs: %+v", runs[0])
	}
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2027-03-02T10:01:00Z")) {
		t.Errorf("next slot = %v", n)
	}
}

// A run that ends at wake, before any tick, sees the wake: missed slots stay missed, due runs
// wait the settle, and a network failure is retried.
func TestSchedulerRunEndsAtWakeBeforeAnyTick(t *testing.T) {
	h := newHarness(t, "2026-03-02T07:59:00Z")
	l := h.round("long", "0 8 * * *", nil)
	sk := h.round("skipper", "0 9 * * *", func(r *Round) { r.CatchUp = CatchUpSkip })
	o := h.round("once", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	lr := h.runs(l.Round.ID)
	if len(lr) != 1 || lr[0].State != RunRunning {
		t.Fatalf("setup: %+v", lr)
	}
	// Asleep to 22:00 with no tick; the long run fails at wake with a network error.
	h.clock.move(utc("2026-03-02T22:00:00Z").Sub(h.clock.Now()), time.Second)
	h.end(lr[0], RunFailed, "network", true)
	if r := h.runs(sk.Round.ID); len(r) != 0 {
		t.Errorf("a skip round ran a slot missed in sleep: %+v", r)
	}
	if r := h.runs(o.Round.ID); len(r) != 1 || r[0].State != RunQueued {
		t.Errorf("catch-up run not queued for the settle: %+v", r)
	}
	h.awake("2026-03-02T22:05:00Z")
	if r := h.runs(l.Round.ID); len(r) != 2 || !r[0].Slot.Equal(utc("2026-03-02T08:00:00Z")) {
		t.Errorf("network failure at wake not retried: %+v", r)
	}
	if r := h.runs(sk.Round.ID); len(r) != 0 {
		t.Errorf("skip round ran later: %+v", r)
	}
}

func TestSchedulerDeletedRoundLiveRun(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	h.max = 1
	h.reopen()
	e := h.round("hourly", "0 * * * *", nil)
	other := h.round("other", "30 10 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	run := h.runs(e.Round.ID)[0]
	if err := h.store.Delete(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	h.reopen()
	// The deleted round's run still holds the host's one place.
	h.awake("2026-03-02T10:31:00Z")
	if r := h.runs(other.Round.ID); len(r) != 1 || r[0].State != RunQueued {
		t.Fatalf("other: %+v", r)
	}
	if err := h.s.UpdateRun(run.ID, RunUpdate{State: RunSucceeded}); err != nil {
		t.Fatalf("a deleted round's live run unknown after restart: %v", err)
	}
	if r := h.runs(other.Round.ID); len(r) != 1 || r[0].State != RunRunning {
		t.Fatalf("other after the end: %+v", r)
	}
	if r := h.runs(e.Round.ID); len(r) != 1 || r[0].State != RunSucceeded {
		t.Errorf("deleted round's run: %+v", r)
	}
}

func TestSchedulerDeleteEndsQueuedAndWaitingRuns(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		h := newHarness(t, "2026-03-02T08:00:00Z")
		e := h.round("daily", "0 9 * * *", nil)
		h.s.Tick()
		h.awake("2026-03-02T08:01:00Z")
		h.sleep("2026-03-02T10:00:00Z")
		if waiting {
			h.awake("2026-03-02T10:00:30Z")
			run := h.runs(e.Round.ID)[0]
			if err := h.s.UpdateRun(run.ID, RunUpdate{State: RunWaiting}); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.store.Delete(e.Round.ID); err != nil {
			t.Fatal(err)
		}
		h.awake("2026-03-02T10:02:00Z")
		r := h.runs(e.Round.ID)
		if len(r) != 1 || r[0].State != RunSkipped || r[0].Reason != ReasonDeleted {
			t.Fatalf("waiting=%v: %+v", waiting, r)
		}
		if !waiting && len(h.start.list()) != 0 {
			t.Error("a deleted round's queued run started")
		}
	}
}

func TestSchedulerOnceNeverLeftHanging(t *testing.T) {
	at := utc("2026-03-02T09:00:00Z")
	once := func(r *Round) { r.Trigger = Trigger{Kind: TriggerOnce, At: &at, Zone: "UTC"} }

	// Its queued run ended by a pause, then unpaused: complete, served.
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("one", "", once)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T10:00:00Z")
	h.s.Pause(e.Round.ID)
	h.s.Unpause(e.Round.ID)
	if st := h.entry(e.Round.ID).State; !st.Completed || st.CompletedReason != ReasonOnceDone {
		t.Errorf("paused run: %+v", st)
	}

	// A retry planned, then dropped by a restart: complete, served.
	h = newHarness(t, "2026-03-02T08:00:00Z")
	e = h.round("one", "", once)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	h.awake("2026-03-02T22:01:00Z")
	h.end(h.runs(e.Round.ID)[0], RunFailed, "net", true)
	if st := h.entry(e.Round.ID).State; st.Completed {
		t.Fatalf("completed with a retry planned: %+v", st)
	}
	h.reopen()
	h.awake("2026-03-02T22:10:00Z")
	if st := h.entry(e.Round.ID).State; !st.Completed || st.CompletedReason != ReasonOnceDone {
		t.Errorf("retry dropped: %+v", st)
	}
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("runs = %d", n)
	}

	// Paused across its instant, then unpaused: complete, never served.
	h = newHarness(t, "2026-03-02T08:00:00Z")
	e = h.round("one", "", once)
	h.s.Tick()
	h.s.Pause(e.Round.ID)
	h.awake("2026-03-02T09:30:00Z")
	h.s.Unpause(e.Round.ID)
	if st := h.entry(e.Round.ID).State; !st.Completed || st.CompletedReason != ReasonOncePassed {
		t.Errorf("passed while paused: %+v", st)
	}
	if n := len(h.runs(e.Round.ID)); n != 0 {
		t.Errorf("runs = %d", n)
	}
}

func TestSchedulerOnceWaitsForItsRetry(t *testing.T) {
	at := utc("2026-03-02T09:00:00Z")
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("one", "", func(r *Round) { r.Trigger = Trigger{Kind: TriggerOnce, At: &at, Zone: "UTC"} })
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	h.awake("2026-03-02T22:01:00Z")
	h.end(h.runs(e.Round.ID)[0], RunFailed, "net", true)
	h.awake("2026-03-02T22:03:30Z")
	if st := h.entry(e.Round.ID).State; st.Completed {
		t.Fatalf("completed before its retry ended: %+v", st)
	}
	h.endLatest(e.Round.ID, RunSucceeded)
	if st := h.entry(e.Round.ID).State; !st.Completed || st.CompletedReason != ReasonOnceDone {
		t.Errorf("after the retry: %+v", st)
	}
}

func TestSchedulerQueueRunIsOneWrite(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T09:59:59Z")
	orig := h.store.write
	writes := 0
	h.store.write = func(string, []byte) error { writes++; return errors.New("disk full") }
	h.awake("2026-03-02T10:00:20Z")
	h.store.write = orig
	if writes != 1 {
		t.Errorf("writes = %d, want the run and the state in one", writes)
	}
	st := h.entry(e.Round.ID).State
	if st.LastSlot != nil || st.LastRun != 0 || len(h.runs(e.Round.ID)) != 0 {
		t.Fatalf("a failed save left a trace: last %v run %d runs %d", st.LastSlot, st.LastRun, len(h.runs(e.Round.ID)))
	}
	h.awake("2026-03-02T10:01:00Z")
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].Number != 1 || !runs[0].Slot.Equal(utc("2026-03-02T10:00:00Z")) {
		t.Fatalf("the slot after the disk recovered: %+v", runs)
	}
}

func TestSchedulerStartLost(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	// The run is saved as running, and gff-hab stops before Start reports a session.
	h.start.mu.Lock()
	h.start.err = nil
	h.start.mu.Unlock()
	h.s.cfg.Starter = startFunc(func(Round, Run) (string, error) { return "", nil })
	h.awake("2026-03-02T10:00:30Z")
	h.reopen()
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].State != RunFailed || runs[0].Reason != ReasonStartLost {
		t.Fatalf("after restart: %+v", runs)
	}
	if st := h.entry(e.Round.ID).State; st.Failures != 1 {
		t.Errorf("failures = %d", st.Failures)
	}
	h.awake("2026-03-02T11:00:30Z")
	if runs := h.runs(e.Round.ID); len(runs) != 2 || runs[0].State != RunRunning {
		t.Fatalf("11:00: %+v", runs)
	}
	// A run with its session survives a restart and can be ended after it.
	if id := h.runs(e.Round.ID)[0].SessionID; !strings.HasPrefix(id, "sess-") {
		t.Fatalf("session id not saved: %q", id)
	}
	h.reopen()
	runs = h.runs(e.Round.ID)
	if runs[0].State != RunRunning {
		t.Fatalf("a run with a session failed at restart: %+v", runs[0])
	}
	h.end(runs[0], RunSucceeded, "", false)
}

type startFunc func(Round, Run) (string, error)

func (f startFunc) Start(r Round, run Run) (string, error) { return f(r, run) }

func TestSchedulerUnpauseNotPaused(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	if _, err := h.store.SetState(e.Round.ID, func(st *RoundState) { st.Failures = 2 }); err != nil {
		t.Fatal(err)
	}
	h.clock.move(utc("2026-03-02T09:00:10Z").Sub(h.clock.Now()), utc("2026-03-02T09:00:10Z").Sub(h.clock.Now()))
	h.s.Unpause(e.Round.ID)
	h.awake("2026-03-02T09:30:00Z")
	if len(h.runs(e.Round.ID)) != 1 {
		t.Error("unpause of a round that is not paused dropped the due 09:00 slot")
	}
	if f := h.entry(e.Round.ID).State.Failures; f != 2 {
		t.Errorf("unpause of a round that is not paused cleared failures: %d", f)
	}
}

func TestSchedulerExpiryAndCompletionEndWaitingRuns(t *testing.T) {
	for _, how := range []string{"expired", "completed"} {
		h := newHarness(t, "2026-03-02T09:59:50Z")
		exp := utc("2026-03-02T10:05:00Z")
		e := h.round("hourly", "0 * * * *", func(r *Round) { r.ExpiresAt = &exp })
		h.s.Tick()
		h.awake("2026-03-02T10:00:30Z")
		run := h.runs(e.Round.ID)[0]
		if err := h.s.UpdateRun(run.ID, RunUpdate{State: RunWaiting}); err != nil {
			t.Fatal(err)
		}
		want := ReasonExpired
		if how == "completed" {
			want = ReasonCompleted
			h.store.SetState(e.Round.ID, func(st *RoundState) { st.Completed, st.CompletedReason = true, "test" })
		}
		h.awake("2026-03-02T10:06:00Z")
		r := h.runs(e.Round.ID)
		if r[0].State != RunSkipped || r[0].Reason != want {
			t.Errorf("%s: %+v", how, r[0])
		}
		if s := h.entry(e.Round.ID).State.Skips; s != 0 {
			t.Errorf("%s: the scheduler's own skip counted: %d", how, s)
		}
	}
}

func TestSchedulerOutsideSkipCounts(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	// A skip reported from outside counts, whatever its reason text says.
	h.end(h.runs(e.Round.ID)[0], RunSkipped, ReasonPaused, false)
	if s := h.entry(e.Round.ID).State.Skips; s != 1 {
		t.Errorf("skips = %d", s)
	}
}

func TestSchedulerUpdateRunRefusesQueued(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T10:00:00Z")
	run := h.runs(e.Round.ID)[0]
	for _, st := range []RunState{RunRunning, RunSucceeded} {
		if err := h.s.UpdateRun(run.ID, RunUpdate{State: st}); err == nil {
			t.Errorf("a queued run moved to %s from outside", st)
		}
	}
	h.awake("2026-03-02T10:00:30Z")
	run = h.runs(e.Round.ID)[0]
	if err := h.s.UpdateRun(run.ID, RunUpdate{State: RunQueued}); err == nil {
		t.Error("a running run moved back to queued")
	}
	if err := h.s.UpdateRun("run-none", RunUpdate{State: RunSucceeded}); !errors.Is(err, ErrNoRun) {
		t.Errorf("unknown run: %v", err)
	}
	if got := h.start.list(); len(got) != 1 {
		t.Errorf("starts = %d", len(got))
	}
}

func TestSchedulerBlockedHoldsAPlace(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	a := h.round("a", "0 10 * * *", nil)
	b := h.round("b", "0 10 * * *", nil)
	c := h.round("c", "1 10 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.s.UpdateRun(h.runs(a.Round.ID)[0].ID, RunUpdate{State: RunBlocked})
	h.s.UpdateRun(h.runs(b.Round.ID)[0].ID, RunUpdate{State: RunWaiting})
	h.awake("2026-03-02T10:02:00Z")
	if r := h.runs(c.Round.ID); len(r) != 1 || r[0].State != RunQueued {
		t.Fatalf("c: %+v", r)
	}
}

func TestDueSlots(t *testing.T) {
	sched, err := Trigger{Kind: TriggerCron, Cron: "0 * * * *", Zone: "UTC"}.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	ten, now := utc("2026-03-02T10:00:00Z"), utc("2026-03-02T12:30:00Z")
	if n, l := dueSlots(sched, ten, nil, now); n != 3 || !l.Equal(utc("2026-03-02T12:00:00Z")) {
		t.Errorf("no last: %d %v", n, l)
	}
	if n, l := dueSlots(sched, ten, &ten, now); n != 2 || !l.Equal(utc("2026-03-02T12:00:00Z")) {
		t.Errorf("first is last: %d %v", n, l)
	}
	twelve := utc("2026-03-02T12:00:00Z")
	if n, _ := dueSlots(sched, ten, &twelve, now); n != 0 {
		t.Errorf("all served: %d", n)
	}
	if n, l := dueSlots(sched, twelve, nil, twelve); n != 1 || !l.Equal(twelve) {
		t.Errorf("due right now: %d %v", n, l)
	}
}

func TestSchedulerUnpauseAfterClockBack(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	h.clock.move(-time.Hour, TickEvery)
	h.s.Pause(e.Round.ID)
	h.s.Unpause(e.Round.ID)
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T11:00:00Z")) {
		t.Errorf("next slot = %v, want after the served 10:00", n)
	}
}

func TestSchedulerCoalesceIntoQueued(t *testing.T) {
	// The first tick is a wake; with a 5-minute settle, each slot that comes due meanwhile
	// joins the queued run.
	h := newHarness(t, "2026-03-02T09:59:50Z")
	h.settle = 5 * time.Minute
	h.reopen()
	e := h.round("minutely", "* * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:20Z")
	r := h.runs(e.Round.ID)[0]
	if r.State != RunQueued || r.Missed != 1 {
		t.Fatalf("first slot: %+v", r)
	}
	h.awake("2026-03-02T10:02:30Z")
	r = h.runs(e.Round.ID)[0]
	if r.State != RunQueued || r.Missed != 3 || !r.Slot.Equal(utc("2026-03-02T10:02:00Z")) {
		t.Fatalf("three slots in the settle: %+v", r)
	}
	// The 10:02:30 step moved the tick grid: the settle ends at the tick at 10:05:00.
	h.awake("2026-03-02T10:05:00Z")
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].State != RunRunning || runs[0].Missed != 6 || !runs[0].Slot.Equal(utc("2026-03-02T10:05:00Z")) {
		t.Fatalf("after the settle: %+v", runs)
	}
	if st := h.entry(e.Round.ID).State; !st.LastSlot.Equal(utc("2026-03-02T10:05:00Z")) {
		t.Errorf("last slot = %v", st.LastSlot)
	}
}

func TestSchedulerRetryReplacedByNewSlot(t *testing.T) {
	h := newHarness(t, "2026-03-02T20:59:00Z")
	e := h.round("even", "*/2 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T21:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	h.sleep("2026-03-02T22:00:00Z")
	h.awake("2026-03-02T22:00:30Z")
	h.end(h.runs(e.Round.ID)[0], RunFailed, "net", true) // retry planned for 22:02:30
	h.awake("2026-03-02T22:02:00Z")                      // the 22:02 slot runs instead
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T22:03:30Z")
	if runs := h.runs(e.Round.ID); len(runs) != 3 {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestSchedulerRetryOnlyOnce(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	h.settle = -1 // no settle: the retry fails inside the window too
	h.reopen()
	e := h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	h.end(h.runs(e.Round.ID)[0], RunFailed, "net", true)
	h.awake("2026-03-02T22:02:00Z")
	runs := h.runs(e.Round.ID)
	if len(runs) != 2 || runs[0].State != RunRunning {
		t.Fatalf("retry: %+v", runs)
	}
	h.end(runs[0], RunFailed, "net", true)
	h.awake("2026-03-02T22:10:00Z")
	if n := len(h.runs(e.Round.ID)); n != 2 {
		t.Errorf("a retry was retried: %d runs", n)
	}
}

func TestSchedulerPausePaths(t *testing.T) {
	// A pause through a definition edit, as the API does: the queued run ends at the next tick,
	// and the next slot moves on past now while paused.
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T10:00:00Z")
	cur := h.entry(e.Round.ID)
	cur.Round.Paused = true
	if _, err := h.store.Update(cur.Round, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.awake("2026-03-02T11:00:30Z")
	r := h.runs(e.Round.ID)
	if len(r) != 1 || r[0].State != RunSkipped || r[0].Reason != ReasonPaused {
		t.Fatalf("runs: %+v", r)
	}
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T12:00:00Z")) {
		t.Errorf("next slot while paused = %v", n)
	}
	// A long pause: unpause sets the next slot to the first after now.
	h.awake("2026-03-02T15:20:00Z")
	if err := h.s.Unpause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T16:00:00Z")) {
		t.Errorf("next slot after unpause = %v", n)
	}
}

func TestSchedulerExpiryOnATick(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	exp := utc("2026-03-02T10:01:20Z") // exactly a tick
	e := h.round("minutely", "* * * * *", func(r *Round) { r.ExpiresAt = &exp })
	h.s.Tick()
	h.awake("2026-03-02T10:00:50Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T10:01:20Z")
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("a slot ran at the expiry instant: %d runs", n)
	}
	if st := h.entry(e.Round.ID).State; !st.Completed {
		t.Errorf("not completed: %+v", st)
	}
}

func TestSchedulerMaxRunsWithOwedSlot(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("hourly", "0 * * * *", func(r *Round) { r.MaxRuns = 1; r.CatchUp = CatchUpSkip })
	h.s.Tick()
	h.awake("2026-03-02T11:00:30Z") // the 11:00 slot comes due while 10:00 runs
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T13:00:30Z")
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("runs = %d", n)
	}
	// A trigger edit re-arms the round; the old owed slot must not run with skip.
	cur := h.entry(e.Round.ID)
	cur.Round.Trigger.Cron = "0 */1 * * *"
	cur.Round.MaxRuns = 0
	h.store.Update(cur.Round, h.clock.Now())
	h.sleep("2026-03-02T15:30:00Z")
	h.awake("2026-03-02T15:31:00Z")
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("an owed slot survived completion: %d runs", n)
	}
}

func TestSchedulerCounts(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("minutely", "* * * * *", func(r *Round) { r.Notify = NotifyNever })
	h.s.Tick()
	steps := []struct {
		state            RunState
		fail, skip, done int
	}{
		{RunFailed, 1, 0, 1},
		{RunSkipped, 1, 1, 1},
		{RunFailed, 2, 0, 2},
		{RunSucceeded, 0, 0, 3},
		{RunSkipped, 0, 1, 3},
		{RunTakenOver, 0, 0, 4},
		{RunSkipped, 0, 1, 4},
		{RunTimedOut, 1, 0, 5},
		{RunTakenOver, 1, 0, 6},
		{RunSkipped, 1, 1, 6},
		{RunSucceeded, 0, 0, 7},
	}
	for i, s := range steps {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		h.endLatest(e.Round.ID, s.state)
		st := h.entry(e.Round.ID).State
		if st.Failures != s.fail || st.Skips != s.skip || st.Finished != s.done {
			t.Fatalf("step %d (%s): failures %d skips %d finished %d, want %d %d %d",
				i, s.state, st.Failures, st.Skips, st.Finished, s.fail, s.skip, s.done)
		}
	}
}

func TestSchedulerSkipsDoNotFinish(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("minutely", "* * * * *", func(r *Round) { r.MaxRuns = 1 })
	h.s.Tick()
	for i := 0; i < 3; i++ {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		h.endLatest(e.Round.ID, RunSkipped)
	}
	if st := h.entry(e.Round.ID).State; st.Completed {
		t.Errorf("skips completed a max_runs round: %+v", st)
	}
}

func TestSchedulerSkipNoticeAtFive(t *testing.T) {
	for _, never := range []bool{false, true} {
		h := newHarness(t, "2026-03-02T09:59:50Z")
		e := h.round("minutely", "* * * * *", func(r *Round) {
			if never {
				r.Notify = NotifyNever
			}
		})
		h.s.Tick()
		for i := 1; i <= SkipsNotice; i++ {
			h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
			h.endLatest(e.Round.ID, RunSkipped)
			want := 0
			if i == SkipsNotice && !never {
				want = 1
			}
			if n := len(h.sink.noticesOf(NoticeSkips)); n != want {
				t.Fatalf("never=%v after %d skips: %d notices", never, i, n)
			}
		}
	}
}

func TestSchedulerCompletionBeatsPause(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("five", "* * * * *", func(r *Round) { r.MaxRuns = PauseAfterFailures })
	h.s.Tick()
	for i := 0; i < PauseAfterFailures; i++ {
		h.awake(h.clock.Now().Add(time.Minute).Format(time.RFC3339))
		h.endLatest(e.Round.ID, RunFailed)
	}
	en := h.entry(e.Round.ID)
	if !en.State.Completed || en.Round.Paused || len(h.sink.noticesOf(NoticePaused)) != 0 {
		t.Errorf("completed %v paused %v notices %d", en.State.Completed, en.Round.Paused, len(h.sink.noticesOf(NoticePaused)))
	}
}

func TestSchedulerLastOK(t *testing.T) {
	h := newHarness(t, "2026-03-02T08:00:00Z")
	e := h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	h.awake("2026-03-02T22:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	if ok := h.entry(e.Round.ID).State.LastOK; ok == nil || !ok.Equal(utc("2026-03-02T22:00:30Z")) {
		t.Errorf("last ok = %v, want the start", ok)
	}
}

func TestSortQueued(t *testing.T) {
	slot := utc("2026-03-02T09:00:00Z")
	early, late := utc("2026-03-02T22:00:00Z"), utc("2026-03-02T22:03:00Z")
	earlier := utc("2026-03-02T08:30:00Z")
	runs := []*Run{
		{ID: "run-e", Slot: &earlier, Created: utc("2026-03-02T23:30:00Z")}, // by its slot
		{ID: "run-d", Created: utc("2026-03-02T08:00:00Z")},                 // no slot: by when it was made
		{ID: "run-c", Slot: &slot, Created: late},
		{ID: "run-b", Slot: &slot, Created: early},
		{ID: "run-a", Slot: &slot, Created: early},
	}
	sortQueued(runs)
	var got []string
	for _, r := range runs {
		got = append(got, r.ID)
	}
	if strings.Join(got, ",") != "run-d,run-e,run-a,run-b,run-c" {
		t.Errorf("order = %v", got)
	}
}

func TestSchedulerRestartAfterClockBack(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	h.clock.move(-time.Hour, TickEvery)
	h.reopen() // the start's recompute must not land on the served 10:00 slot
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T11:00:00Z")) {
		t.Errorf("next slot = %v", n)
	}
}

func TestSchedulerUnpauseBeforeAnyTick(t *testing.T) {
	h := newHarness(t, "2026-03-02T09:59:00Z")
	e := h.round("hourly", "0 * * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T10:00:30Z")
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T10:30:00Z")
	h.s.Pause(e.Round.ID)
	// Five hours awake with no tick, then an unpause: the slots that passed never run.
	h.clock.move(5*time.Hour, 5*time.Hour)
	h.s.Unpause(e.Round.ID)
	h.awake("2026-03-02T15:31:00Z")
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("runs = %d", n)
	}
	if n := h.entry(e.Round.ID).State.NextSlot; !n.Equal(utc("2026-03-02T16:00:00Z")) {
		t.Errorf("next slot = %v", n)
	}
}

func TestSchedulerPauseAndUnpauseSeeAWake(t *testing.T) {
	for _, op := range []string{"pause", "unpause"} {
		h := newHarness(t, "2026-03-02T08:00:00Z")
		x := h.round("x", "0 9 * * *", nil)
		sk := h.round("skipper", "0 9 * * *", func(r *Round) { r.CatchUp = CatchUpSkip })
		h.s.Tick()
		h.awake("2026-03-02T08:01:00Z")
		h.clock.move(utc("2026-03-02T22:00:00Z").Sub(h.clock.Now()), time.Second)
		if op == "pause" {
			h.s.Pause(x.Round.ID)
		} else {
			h.s.Unpause(x.Round.ID)
		}
		if r := h.runs(sk.Round.ID); len(r) != 0 {
			t.Errorf("%s at wake: a skip round ran a missed slot: %+v", op, r)
		}
	}
}

func TestSchedulerPauseDropsPlans(t *testing.T) {
	// An on-time slot owed behind a live run is dropped by a pause: after unpause, a sleep's
	// missed slots stay skipped.
	h := newHarness(t, "2026-03-02T09:59:50Z")
	e := h.round("hourly", "0 * * * *", func(r *Round) { r.CatchUp = CatchUpSkip })
	h.s.Tick()
	h.awake("2026-03-02T11:00:30Z") // 11:00 owed behind the 10:00 run
	h.s.Pause(e.Round.ID)
	h.endLatest(e.Round.ID, RunSucceeded)
	h.awake("2026-03-02T11:30:00Z")
	h.s.Unpause(e.Round.ID)
	h.sleep("2026-03-02T15:00:20Z")
	h.awake("2026-03-02T15:01:00Z")
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("owed slot survived a pause: %d runs", n)
	}

	// A planned retry is dropped by a pause.
	h = newHarness(t, "2026-03-02T08:00:00Z")
	e = h.round("daily", "0 9 * * *", nil)
	h.s.Tick()
	h.awake("2026-03-02T08:01:00Z")
	h.sleep("2026-03-02T22:00:00Z")
	h.awake("2026-03-02T22:01:00Z")
	h.end(h.runs(e.Round.ID)[0], RunFailed, "net", true)
	h.s.Pause(e.Round.ID)
	h.s.Unpause(e.Round.ID)
	h.awake("2026-03-02T22:06:00Z")
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Errorf("retry survived a pause: %d runs", n)
	}
}
