package rounds

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Scheduler rules and defaults (rounds.md 7.2, 8, 10.1).
const (
	// TickEvery is how often the scheduler compares wall-clock time with each round's next slot.
	TickEvery = 30 * time.Second
	// WakeGap: a wall-clock gap between ticks this much longer than the monotonic gap means the
	// host slept, or its clock jumped forward.
	WakeGap = 60 * time.Second
	// DefaultMaxRunning is the host limit, rounds.max_running.
	DefaultMaxRunning = 2
	// DefaultWakeSettle is how long due runs wait after a wake, rounds.wake_settle.
	DefaultWakeSettle = 30 * time.Second
	// RetryWindow and RetryDelay: a run that fails with a network or sign-in error within
	// RetryWindow of a wake is tried once more RetryDelay later.
	RetryWindow = 2 * time.Minute
	RetryDelay  = 2 * time.Minute
	// PauseAfterFailures failed or timed-out runs in a row pause the round.
	PauseAfterFailures = 5
	// SkipsNotice skipped runs in a row raise one notice.
	SkipsNotice = 5
	// MaxMissedCount bounds how many missed slots a catch-up run counts, so one tick never
	// walks a year of 1-minute slots (525,600) under the lock. Past the bound, missed says
	// MaxMissedCount and the run still serves the latest slot.
	MaxMissedCount = 10000
)

// Reasons the scheduler writes on runs and rounds.
const (
	ReasonPaused       = "paused"
	ReasonExpired      = "round expired"
	ReasonDeleted      = "round deleted"
	ReasonCompleted    = "round completed"
	ReasonMaxRuns      = "max_runs reached"
	ReasonOnceDone     = "once trigger served"
	ReasonOnceMissed   = "once trigger missed, catch_up skip"
	ReasonFailurePause = "5 failed runs in a row"
)

// Clock is the scheduler's time source. Now is the wall clock. Mono is a monotonic reading
// that stops while the host sleeps, as Go's monotonic clock does on Linux and macOS
// (rounds.md 8.1); only differences between two readings matter.
type Clock interface {
	Now() time.Time
	Mono() time.Duration
}

type systemClock struct{ start time.Time }

// SystemClock returns the host's clock: the wall clock, and Go's monotonic reading.
func SystemClock() Clock { return systemClock{start: time.Now()} }

func (c systemClock) Now() time.Time      { return time.Now().Round(0) }
func (c systemClock) Mono() time.Duration { return time.Since(c.start) }

// Starter starts a run's session. The scheduler has already marked the run running; Start
// returns an error when the run cannot start (spawn failed), and the run then fails with it.
// Later changes (waiting, blocked, the end) come back through Scheduler.UpdateRun. Start is
// called with no scheduler lock held.
type Starter interface {
	Start(round Round, run Run) error
}

// NoticeKind names a notice the scheduler wants raised (rounds.md 10.2).
type NoticeKind string

const (
	NoticeRunFailed    NoticeKind = "run_failed"    // a run failed or timed out
	NoticeRunSucceeded NoticeKind = "run_succeeded" // notify = always only
	NoticePaused       NoticeKind = "paused"        // the round paused itself
	NoticeSkips        NoticeKind = "skips"         // SkipsNotice skips in a row
)

// Notice is a notice the scheduler wants raised. The round's notify setting is already
// applied: notify = never leaves only NoticePaused.
type Notice struct {
	Kind  NoticeKind
	Round Entry
	Run   *Run
	Text  string
}

// Sink receives the scheduler's events in order. Its methods are called with no scheduler
// lock held, but one at a time; a sink must not call back into the scheduler from them.
type Sink interface {
	RoundChanged(e Entry)
	RunChanged(r Run)
	Notice(n Notice)
}

// RunUpdate reports a run's new state to the scheduler. A final state ends the run.
type RunUpdate struct {
	State     RunState
	Reason    string
	SessionID string
	// Result is the final agent message; the scheduler keeps its first MaxResultBytes.
	Result string
	// Network is set when the harness reported a network or sign-in error (rounds.md 8.3).
	Network bool
}

// SchedulerConfig configures NewScheduler.
type SchedulerConfig struct {
	Store   *Store
	Starter Starter
	Sink    Sink
	// Clock defaults to SystemClock().
	Clock Clock
	// MaxRunning defaults to DefaultMaxRunning; WakeSettle to DefaultWakeSettle (a negative
	// value means none).
	MaxRunning int
	WakeSettle time.Duration
	// NewTicker makes the tick channel Run reads. Default: a time.Ticker of TickEvery.
	NewTicker func() (ticks <-chan time.Time, stop func())
	// Logf receives one line per store error and per trigger that no longer compiles.
	Logf func(format string, a ...any)
}

type compiled struct {
	trig  Trigger
	sched *Schedule
	err   error
}

type retryPlan struct {
	at     time.Time
	slot   time.Time
	missed int
}

type event struct {
	round  *Entry
	run    *Run
	notice *Notice
}

// Scheduler is the rounds scheduler (rounds.md 7.2, 8, 10.1). It is safe for concurrent use.
type Scheduler struct {
	mu     sync.Mutex
	emitMu sync.Mutex
	cfg    SchedulerConfig
	store  *Store
	runs   RunLog

	scheds map[string]compiled
	live   map[string]*Run // run id to a run that has not ended
	// owed marks a round whose slot came due on time while its run was live: the slot runs
	// after that run ends even with catch_up = skip.
	owed    map[string]bool
	retry   map[string]retryPlan
	retried map[string]bool // run ids that are a retry after wake

	ticked      bool
	lastWall    time.Time
	lastMono    time.Duration
	lastWake    time.Time
	settleUntil time.Time

	events []event
	starts []Run
}

// NewScheduler loads the rounds and their live runs from the store, and recomputes each
// future next slot, so a zone database change takes effect at start (rounds.md 8.5). A next
// slot already past stays: the first tick catches it up, as after a sleep (8.4).
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Store == nil || cfg.Starter == nil || cfg.Sink == nil {
		return nil, errors.New("rounds: a scheduler needs a store, a starter and a sink")
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock()
	}
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = DefaultMaxRunning
	}
	if cfg.WakeSettle == 0 {
		cfg.WakeSettle = DefaultWakeSettle
	} else if cfg.WakeSettle < 0 {
		cfg.WakeSettle = 0
	}
	s := &Scheduler{
		cfg:     cfg,
		store:   cfg.Store,
		runs:    cfg.Store.Runs(),
		scheds:  map[string]compiled{},
		live:    map[string]*Run{},
		owed:    map[string]bool{},
		retry:   map[string]retryPlan{},
		retried: map[string]bool{},
	}
	now := cfg.Clock.Now()
	for _, e := range s.store.List() {
		id := e.Round.ID
		if err := s.loadLive(id); err != nil {
			return nil, err
		}
		c := s.compile(e.Round)
		if c.err != nil || e.State.NextSlot == nil || !e.State.NextSlot.After(now) {
			continue
		}
		next := nextAfter(c.sched, now, e.State.LastSlot)
		if !sameSlot(next, e.State.NextSlot) {
			s.setState(id, func(st *RoundState) { st.NextSlot = next })
		}
	}
	// Live runs of deleted rounds: the store keeps them until they end.
	return s, nil
}

func (s *Scheduler) loadLive(roundID string) error {
	before := 0
	for {
		page, more, err := s.runs.Page(roundID, before, 500)
		if err != nil {
			return err
		}
		for i := range page {
			if !page[i].State.Final() {
				r := page[i]
				s.live[r.ID] = &r
			}
			before = page[i].Number
		}
		if !more {
			return nil
		}
	}
}

func (s *Scheduler) logf(format string, a ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, a...)
	}
}

// compile returns the round's schedule, compiled once per trigger.
func (s *Scheduler) compile(r Round) compiled {
	if c, ok := s.scheds[r.ID]; ok && c.trig.Equal(r.Trigger) {
		return c
	}
	sched, err := r.Trigger.Schedule()
	c := compiled{trig: r.Trigger, sched: sched, err: err}
	if err != nil {
		s.logf("rounds: round %s: trigger: %v; not scheduled", r.Name, err)
	}
	s.scheds[r.ID] = c
	return c
}

// TriggerErrors returns, by round id, the rounds left unscheduled because their trigger does
// not compile (rounds.md 7.7).
func (s *Scheduler) TriggerErrors() map[string]error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]error{}
	for id, c := range s.scheds {
		if c.err != nil {
			out[id] = c.err
		}
	}
	return out
}

// Run ticks until ctx ends: once at once, then on every tick.
func (s *Scheduler) Run(ctx context.Context) {
	newTicker := s.cfg.NewTicker
	if newTicker == nil {
		newTicker = func() (<-chan time.Time, func()) {
			t := time.NewTicker(TickEvery)
			return t.C, t.Stop
		}
	}
	ticks, stop := newTicker()
	defer stop()
	s.Tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			s.Tick()
		}
	}
}

// Tick runs one pass: wake detection, then each round's due slots, then the starts the host
// limit and the wake settle allow. The first tick after start counts as a wake: a restart is
// treated like a short sleep (rounds.md 8.4).
func (s *Scheduler) Tick() {
	now, mono := s.cfg.Clock.Now(), s.cfg.Clock.Mono()
	s.mu.Lock()
	wake := !s.ticked || now.Sub(s.lastWall)-(mono-s.lastMono) > WakeGap
	if wake {
		s.lastWake = now
		s.settleUntil = now.Add(s.cfg.WakeSettle)
	}
	prev := s.lastWall
	if !s.ticked {
		prev = now
	}
	s.ticked, s.lastWall, s.lastMono = true, now, mono
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
}

// UpdateRun reports a run's new state: waiting, running or blocked while it lives, or a final
// state when it ends. An ended run updates the round's counts, may pause or complete the
// round, and lets a coalesced slot or a queued run start.
func (s *Scheduler) UpdateRun(runID string, u RunUpdate) error {
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	run := s.live[runID]
	if run == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s is not live", ErrNoRun, runID)
	}
	if u.SessionID != "" {
		run.SessionID = u.SessionID
	}
	if !u.State.Final() {
		switch u.State {
		case RunWaiting, RunRunning, RunBlocked:
		default:
			s.mu.Unlock()
			return fmt.Errorf("rounds: a run cannot move to %q", u.State)
		}
		run.State = u.State
		if run.StartedAt == nil {
			run.StartedAt = &now
		}
		s.saveRun(run)
		s.unlockAndFlush()
		return nil
	}
	s.endLocked(run, u, now)
	s.evaluateLocked(now, s.lastWall, false)
	s.startLocked(now)
	s.unlockAndFlush()
	return nil
}

// Pause pauses a round by the user: no new runs; a queued or waiting run ends skipped,
// "paused" (rounds.md 11). A run already running goes on.
func (s *Scheduler) Pause(roundID string) error {
	return s.pause(roundID, "")
}

// Unpause resumes a paused round and clears its failure and skip counts (10.1). Slots that
// passed while it was paused do not run; the next slot is the first after now.
func (s *Scheduler) Unpause(roundID string) error {
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	e, ok := s.store.Get(roundID)
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if e.Round.Paused {
		r := e.Round
		r.Paused = false
		if _, err := s.store.Update(r, now); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	c := s.compile(e.Round)
	s.setState(roundID, func(st *RoundState) {
		st.Failures, st.Skips, st.PausedReason = 0, 0, ""
		if c.err == nil && !st.Completed {
			st.NextSlot = nextAfter(c.sched, now, st.LastSlot)
		}
	})
	s.startLocked(now)
	s.unlockAndFlush()
	return nil
}

func (s *Scheduler) pause(roundID, reason string) error {
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	err := s.pauseLocked(roundID, reason, now)
	s.unlockAndFlush()
	return err
}

func (s *Scheduler) pauseLocked(roundID, reason string, now time.Time) error {
	e, ok := s.store.Get(roundID)
	if !ok {
		return ErrNotFound
	}
	if !e.Round.Paused {
		r := e.Round
		r.Paused = true
		if _, err := s.store.Update(r, now); err != nil {
			return err
		}
	}
	s.setState(roundID, func(st *RoundState) { st.PausedReason = reason })
	delete(s.owed, roundID)
	delete(s.retry, roundID)
	if run := s.liveFor(roundID); run != nil && (run.State == RunQueued || run.State == RunWaiting) {
		s.endLocked(run, RunUpdate{State: RunSkipped, Reason: ReasonPaused}, now)
	}
	return nil
}

// unlockAndFlush releases the lock and hands the gathered events to the sink in order, then
// starts the runs marked running. emitMu keeps the order across callers.
func (s *Scheduler) unlockAndFlush() {
	evs, starts := s.events, s.starts
	s.events, s.starts = nil, nil
	s.emitMu.Lock()
	s.mu.Unlock()
	for _, ev := range evs {
		switch {
		case ev.round != nil:
			s.cfg.Sink.RoundChanged(*ev.round)
		case ev.run != nil:
			s.cfg.Sink.RunChanged(*ev.run)
		case ev.notice != nil:
			s.cfg.Sink.Notice(*ev.notice)
		}
	}
	s.emitMu.Unlock()
	for _, run := range starts {
		e, ok := s.store.Get(run.RoundID)
		if !ok {
			s.UpdateRun(run.ID, RunUpdate{State: RunSkipped, Reason: ReasonDeleted})
			continue
		}
		if err := s.cfg.Starter.Start(e.Round, run); err != nil {
			s.UpdateRun(run.ID, RunUpdate{State: RunFailed, Reason: "start failed: " + err.Error()})
		}
	}
}

func (s *Scheduler) liveFor(roundID string) *Run {
	for _, r := range s.live {
		if r.RoundID == roundID {
			return r
		}
	}
	return nil
}

func (s *Scheduler) setState(id string, fn func(*RoundState)) (Entry, bool) {
	e, err := s.store.SetState(id, fn)
	if err != nil {
		s.logf("rounds: round %s: save: %v", id, err)
		return Entry{}, false
	}
	s.events = append(s.events, event{round: &e})
	return e, true
}

func (s *Scheduler) saveRun(run *Run) {
	if err := s.runs.Update(*run); err != nil {
		s.logf("rounds: run %s: save: %v", run.ID, err)
	}
	c := *run
	s.events = append(s.events, event{run: &c})
}

func (s *Scheduler) notice(n Notice) {
	s.events = append(s.events, event{notice: &n})
}

// nextAfter is the first slot after now and after the last served slot: a clock moved back
// never brings a served slot round again (rounds.md 8.5).
func nextAfter(sched *Schedule, now time.Time, last *time.Time) *time.Time {
	from := now
	if last != nil && last.After(from) {
		from = *last
	}
	return nextSlot(sched, from)
}

func sameSlot(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// dueSlots counts the slots from first through now that lie after last, and returns the
// latest. Past MaxMissedCount it stops counting and finds the latest slot from near now.
func dueSlots(sched *Schedule, first time.Time, last *time.Time, now time.Time) (int, time.Time) {
	n := 0
	var latest time.Time
	for cur, ok := first, true; ok && !cur.After(now); cur, ok = sched.Next(cur) {
		if last != nil && !cur.After(*last) {
			continue
		}
		n++
		latest = cur
		if n == MaxMissedCount {
			if l, ok := latestSlot(sched, now); ok {
				latest = l
			}
			break
		}
	}
	return n, latest
}

// latestSlot finds the latest slot at or before now, looking back over growing spans.
func latestSlot(sched *Schedule, now time.Time) (time.Time, bool) {
	for _, d := range []time.Duration{time.Hour, 24 * time.Hour, 32 * 24 * time.Hour, 367 * 24 * time.Hour, 5 * 367 * 24 * time.Hour} {
		cur, ok := sched.Next(now.Add(-d))
		if !ok || cur.After(now) {
			continue
		}
		for {
			nx, ok := sched.Next(cur)
			if !ok || nx.After(now) {
				return cur, true
			}
			cur = nx
		}
	}
	return time.Time{}, false
}

// evaluateLocked looks at every round at now. prev is the wall time of the tick before; a slot
// after it came due on time. wake says the host woke (or the clock jumped forward, or the hab
// started) since then: every due slot is a missed one.
func (s *Scheduler) evaluateLocked(now, prev time.Time, wake bool) {
	entries := s.store.List()
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Round.ID] = true
		s.evaluateRound(e, now, prev, wake)
	}
	for _, run := range s.live {
		if !have[run.RoundID] && run.State == RunQueued {
			s.endLocked(run, RunUpdate{State: RunSkipped, Reason: ReasonDeleted}, now)
		}
	}
	for id := range s.scheds {
		if !have[id] {
			delete(s.scheds, id)
			delete(s.owed, id)
			delete(s.retry, id)
		}
	}
}

func (s *Scheduler) evaluateRound(e Entry, now, prev time.Time, wake bool) {
	id := e.Round.ID
	c := s.compile(e.Round)
	if c.err != nil {
		return
	}
	st := e.State
	live := s.liveFor(id)
	waitingRun := live != nil && (live.State == RunQueued || live.State == RunWaiting)

	if st.Completed {
		if live != nil && live.State == RunQueued {
			s.endLocked(live, RunUpdate{State: RunSkipped, Reason: ReasonCompleted}, now)
		}
		return
	}
	if e.Round.Paused {
		delete(s.owed, id)
		delete(s.retry, id)
		if waitingRun {
			s.endLocked(live, RunUpdate{State: RunSkipped, Reason: ReasonPaused}, now)
		}
		if st.NextSlot != nil && !st.NextSlot.After(now) {
			s.setState(id, func(st *RoundState) { st.NextSlot = nextAfter(c.sched, now, st.LastSlot) })
		}
		return
	}
	if e.Round.ExpiresAt != nil && !now.Before(*e.Round.ExpiresAt) {
		s.complete(id, ReasonExpired)
		if live != nil && live.State == RunQueued {
			s.endLocked(live, RunUpdate{State: RunSkipped, Reason: ReasonExpired}, now)
		}
		return
	}
	// A next slot at or before the last served one (an edit or a zone change while the clock
	// was set back) moves on with no run.
	if st.NextSlot != nil && st.LastSlot != nil && !st.NextSlot.After(*st.LastSlot) {
		next := nextSlot(c.sched, *st.LastSlot)
		if e2, ok := s.setState(id, func(rs *RoundState) { rs.NextSlot = next }); ok {
			st = e2.State
		}
	}

	if st.NextSlot == nil || st.NextSlot.After(now) {
		if p, ok := s.retry[id]; ok && live == nil && !now.Before(p.at) {
			delete(s.retry, id)
			if run := s.newRun(e, p.slot, p.missed, now, nil, true); run != nil {
				s.retried[run.ID] = true
			}
		}
		return
	}

	n, latest := dueSlots(c.sched, *st.NextSlot, st.LastSlot, now)
	next := nextAfter(c.sched, now, st.LastSlot)
	if n == 0 {
		s.setState(id, func(rs *RoundState) { rs.NextSlot = next })
		return
	}
	onTime := !wake && latest.After(prev)
	switch {
	case waitingRun:
		// Coalesce into the run that has not started its work yet (7.2, 6.3).
		live.Slot = &latest
		live.Missed += n
		s.saveRun(live)
		s.setState(id, func(rs *RoundState) { rs.LastSlot, rs.NextSlot = &latest, next })
	case live != nil:
		// One run per round: the slot stays due and runs once after the live run ends.
		if onTime {
			s.owed[id] = true
		}
	case e.Round.CatchUp == CatchUpSkip && !onTime && !s.owed[id]:
		delete(s.retry, id)
		s.setState(id, func(rs *RoundState) { rs.LastSlot, rs.NextSlot = &latest, next })
		if next == nil && e.Round.Trigger.Kind == TriggerOnce {
			s.complete(id, ReasonOnceMissed)
		}
	default:
		delete(s.owed, id)
		delete(s.retry, id) // a new slot's run replaces the retry
		s.newRun(e, latest, n, now, next, false)
	}
}

// newRun queues a run for slot, numbered from the round's state, and saves the round's last
// served slot and next slot first. A retry serves a slot already served and leaves both alone.
func (s *Scheduler) newRun(e Entry, slot time.Time, missed int, now time.Time, next *time.Time, retry bool) *Run {
	var num int
	_, ok := s.setState(e.Round.ID, func(st *RoundState) {
		st.LastRun++
		num = st.LastRun
		if !retry {
			st.LastSlot, st.NextSlot = &slot, next
		}
	})
	if !ok {
		return nil
	}
	run := Run{
		ID:      NewID("run"),
		Number:  num,
		RoundID: e.Round.ID,
		Slot:    &slot,
		Trigger: e.Round.Trigger.Kind,
		Missed:  missed,
		State:   RunQueued,
		Created: now,
	}
	if err := s.runs.Append(run); err != nil {
		s.logf("rounds: run %s: save: %v", run.ID, err)
		return nil
	}
	s.live[run.ID] = &run
	c := run
	s.events = append(s.events, event{run: &c})
	return &run
}

func (s *Scheduler) complete(id, reason string) {
	s.setState(id, func(st *RoundState) {
		st.Completed, st.CompletedReason, st.NextSlot = true, reason, nil
	})
	delete(s.owed, id)
	delete(s.retry, id)
}

// startLocked starts queued runs, oldest slot first, while the host limit allows and the wake
// settle has passed (rounds.md 7.2, 8.3). It marks them running; unlockAndFlush calls the
// starter.
func (s *Scheduler) startLocked(now time.Time) {
	if now.Before(s.settleUntil) {
		return
	}
	running := 0
	var queued []*Run
	for _, r := range s.live {
		if r.State == RunQueued {
			queued = append(queued, r)
		} else {
			running++
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		a, b := runOrder(queued[i]), runOrder(queued[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		if !queued[i].Created.Equal(queued[j].Created) {
			return queued[i].Created.Before(queued[j].Created)
		}
		return queued[i].ID < queued[j].ID
	})
	for _, r := range queued {
		if running >= s.cfg.MaxRunning {
			return
		}
		t := now
		r.State, r.StartedAt = RunRunning, &t
		s.saveRun(r)
		s.starts = append(s.starts, *r)
		running++
	}
}

func runOrder(r *Run) time.Time {
	if r.Slot != nil {
		return *r.Slot
	}
	return r.Created
}

// endLocked ends a live run and applies the round's rules: counts, the failure pause, the
// skips notice, max_runs, a spent once trigger, the retry after wake (rounds.md 8.3, 10.1).
func (s *Scheduler) endLocked(run *Run, u RunUpdate, now time.Time) {
	delete(s.live, run.ID)
	isRetry := s.retried[run.ID]
	delete(s.retried, run.ID)
	run.State = u.State
	run.Reason = u.Reason
	if u.SessionID != "" {
		run.SessionID = u.SessionID
	}
	run.Result = TruncateResult(u.Result)
	t := now
	run.EndedAt = &t
	s.saveRun(run)

	e, ok := s.store.Get(run.RoundID)
	if !ok {
		return
	}
	internal := u.State == RunSkipped &&
		(u.Reason == ReasonPaused || u.Reason == ReasonExpired || u.Reason == ReasonDeleted || u.Reason == ReasonCompleted)
	if internal {
		return
	}

	failed := u.State == RunFailed || u.State == RunTimedOut
	if failed && u.Network && !isRetry && run.Slot != nil && !s.lastWake.IsZero() &&
		!now.After(s.lastWake.Add(RetryWindow)) {
		s.retry[run.RoundID] = retryPlan{at: now.Add(RetryDelay), slot: *run.Slot, missed: run.Missed}
	}
	_, retrying := s.retry[run.RoundID]

	ne, ok := s.setState(run.RoundID, func(st *RoundState) {
		switch u.State {
		case RunSucceeded:
			st.Failures, st.Skips = 0, 0
			st.LastOK = run.StartedAt
			if st.LastOK == nil {
				st.LastOK = &run.Created
			}
			st.Finished++
		case RunFailed, RunTimedOut:
			st.Failures++
			st.Skips = 0
			st.Finished++
		case RunTakenOver:
			st.Skips = 0
			st.Finished++
		case RunSkipped:
			st.Skips++
		}
		switch {
		case e.Round.MaxRuns > 0 && st.Finished >= e.Round.MaxRuns:
			st.Completed, st.CompletedReason, st.NextSlot = true, ReasonMaxRuns, nil
		case e.Round.Trigger.Kind == TriggerOnce && st.NextSlot == nil && !retrying:
			st.Completed, st.CompletedReason = true, ReasonOnceDone
		}
	})
	if !ok {
		return
	}
	if ne.State.Completed {
		delete(s.retry, run.RoundID)
		delete(s.owed, run.RoundID)
	}

	never := e.Round.Notify == NotifyNever
	r := *run
	switch {
	case failed && !never:
		s.notice(Notice{Kind: NoticeRunFailed, Round: ne, Run: &r,
			Text: fmt.Sprintf("round %s: run %d %s: %s", e.Round.Name, run.Number, run.State, run.Reason)})
	case u.State == RunSucceeded && e.Round.Notify == NotifyAlways:
		s.notice(Notice{Kind: NoticeRunSucceeded, Round: ne, Run: &r,
			Text: fmt.Sprintf("round %s: run %d succeeded", e.Round.Name, run.Number)})
	case u.State == RunSkipped && ne.State.Skips == SkipsNotice && !never:
		since := e.Round.CreatedAt
		if ne.State.LastOK != nil {
			since = *ne.State.LastOK
		}
		s.notice(Notice{Kind: NoticeSkips, Round: ne, Run: &r,
			Text: fmt.Sprintf("round %s has not run since %s: %s", e.Round.Name, since.Format(time.RFC3339), run.Reason)})
	}
	if failed && ne.State.Failures >= PauseAfterFailures && !e.Round.Paused && !ne.State.Completed {
		if err := s.pauseLocked(run.RoundID, ReasonFailurePause, now); err != nil {
			s.logf("rounds: round %s: pause: %v", e.Round.Name, err)
			return
		}
		pe, _ := s.store.Get(run.RoundID)
		s.notice(Notice{Kind: NoticePaused, Round: pe, Run: &r,
			Text: fmt.Sprintf("round %s paused: %s", e.Round.Name, ReasonFailurePause)})
	}
}
