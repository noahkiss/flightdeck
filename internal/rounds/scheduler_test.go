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

func (f *fakeStarter) Start(_ Round, run Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, run)
	return f.err
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
	h.s, err = NewScheduler(SchedulerConfig{Store: st, Starter: h.start, Sink: h.sink, Clock: h.clock, MaxRunning: h.max})
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
	h.awake("2026-03-02T22:02:30Z")
	if len(h.runs(e.Round.ID)) != 1 {
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
