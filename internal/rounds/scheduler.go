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
	// WakeGap: a wall-clock gap between two readings this much longer than the monotonic gap
	// means the host slept, or its clock jumped forward.
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
	ReasonStartLost    = "start lost"
	ReasonMaxRuns      = "max_runs reached"
	ReasonOnceDone     = "once trigger served"
	ReasonOncePassed   = "once trigger passed while paused"
	ReasonOnceMissed   = "once trigger missed, catch_up skip"
	ReasonFailurePause = "5 failed runs in a row"
	ReasonTargetGone   = "target session gone"
)

// ErrTargetGone is the start error of a resume or fork run whose target session's record was
// removed (rounds.md 10.1): the run ends skipped and the round completes, with one notice.
var ErrTargetGone = errors.New("the target session is gone")

// Clock is the scheduler's time source. Now is the wall clock. Mono is a monotonic reading
// that stops while the host sleeps, as Go's monotonic clock does on Linux and macOS
// (rounds.md 8.1); only differences between two readings matter.
type Clock interface {
	Now() time.Time
	Mono() time.Duration
}

type systemClock struct{ start time.Time }

// SystemClock returns the host's clock: the wall clock, and Go's monotonic reading. Now
// strips the monotonic reading, so a difference of two Now values is wall-clock time; with
// it kept, time.Time.Sub would use monotonic time and never see a sleep.
func SystemClock() Clock { return systemClock{start: time.Now()} }

func (c systemClock) Now() time.Time      { return time.Now().Round(0) }
func (c systemClock) Mono() time.Duration { return time.Since(c.start) }

// Starter starts a run's session.
//
// The scheduler has already saved the run as running when it calls Start, with no scheduler
// lock held, on the goroutine of the tick or update that started the run. Start returns once
// the spawn is done: the session exists, or it could not be made. It returns the session id,
// which the scheduler saves on the run, or an error, and the run then fails with it. Start
// must not wait for the harness to reach idle or for the turn: readiness, waiting, blocked
// and the end come back later through Scheduler.UpdateRun. A run that is running with no
// session id when gff-hab starts lost its start, and fails as "start lost".
type Starter interface {
	Start(round Round, run Run) (sessionID string, err error)
}

// NoticeKind names a notice the scheduler wants raised (rounds.md 10.2).
type NoticeKind string

const (
	NoticeRunFailed    NoticeKind = "run_failed"    // a run failed or timed out
	NoticeRunSucceeded NoticeKind = "run_succeeded" // notify = always only
	NoticePaused       NoticeKind = "paused"        // the round paused itself
	NoticeSkips        NoticeKind = "skips"         // SkipsNotice skips in a row
	NoticeTargetGone   NoticeKind = "target_gone"   // the round completed: its target is gone
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
	// RoundDeleted reports a round Delete removed; e is the round as it was.
	RoundDeleted(e Entry)
	RunChanged(r Run)
	Notice(n Notice)
}

// RunUpdate reports a run's new state to the scheduler. A final state ends the run.
type RunUpdate struct {
	State RunState
	// Reason says why the run ended, or, on a live state, why it is in it (a blocked run's
	// dialog).
	Reason    string
	SessionID string
	// Result is the final agent message; the scheduler keeps its first MaxResultBytes.
	Result string
	// Network is set when the harness reported a network or sign-in error (rounds.md 8.3).
	Network bool
	// TargetGone ends a resume or fork run whose target session's record was removed: the
	// round completes with ReasonTargetGone and one notice; the run counts for nothing. A run
	// whose session is not the round's target now (an edit re-targeted it) completes nothing.
	TargetGone bool
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
	round   *Entry
	deleted *Entry
	run     *Run
	notice  *Notice
}

// Scheduler is the rounds scheduler (rounds.md 7.2, 8, 10.1). It is safe for concurrent use.
type Scheduler struct {
	mu     sync.Mutex
	emitMu sync.Mutex
	cfg    SchedulerConfig
	store  *Store
	runs   RunLog

	scheds map[string]compiled
	live   map[string]*Run // run id to a run that has not ended, deleted rounds included
	// owed marks a round whose slot came due on time while its run was live: the slot runs
	// after that run ends even with catch_up = skip.
	owed    map[string]bool
	retry   map[string]retryPlan
	retried map[string]bool // run ids that are a retry after wake

	observed    bool
	lastWall    time.Time
	lastMono    time.Duration
	lastWake    time.Time
	settleUntil time.Time

	events []event
	starts []Run
}

// NewScheduler loads the rounds and every live run from the store, and recomputes each future
// next slot, so a zone database change takes effect at start (rounds.md 8.5). A next slot
// already past stays: the first tick catches it up, as after a sleep (8.4). A run saved as
// started that has no session id lost its start in a crash, and fails as "start lost".
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
	live, err := s.runs.Live()
	if err != nil {
		return nil, err
	}
	for i := range live {
		r := live[i]
		s.live[r.ID] = &r
	}
	s.mu.Lock()
	now := cfg.Clock.Now()
	for _, e := range s.store.List() {
		c := s.compile(e.Round)
		if c.err != nil || e.State.NextSlot == nil || !e.State.NextSlot.After(now) {
			continue
		}
		next := nextAfter(c.sched, now, e.State.LastSlot)
		if !sameSlot(next, e.State.NextSlot) {
			s.setState(e.Round.ID, func(st *RoundState) { st.NextSlot = next })
		}
	}
	for _, id := range sortedIDs(s.live) {
		r := s.live[id]
		if r.State != RunQueued && r.SessionID == "" {
			s.endLocked(r, RunUpdate{State: RunFailed, Reason: ReasonStartLost}, now, false)
		}
	}
	s.unlockAndFlush()
	return s, nil
}

func sortedIDs(m map[string]*Run) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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

// observeLocked reads the clock under the lock and detects a wake: a wall-clock gap since the
// last reading more than WakeGap longer than the monotonic gap. Every entry point calls it
// first, so a run that ends at wake before any tick still sees the wake. The first reading
// after start counts as a wake: a restart is treated like a short sleep (rounds.md 8.4). prev
// is the wall time of the reading before; a slot after it came due on time.
func (s *Scheduler) observeLocked() (now, prev time.Time, wake bool) {
	now, mono := s.cfg.Clock.Now(), s.cfg.Clock.Mono()
	wake = !s.observed || now.Sub(s.lastWall)-(mono-s.lastMono) > WakeGap
	prev = s.lastWall
	if !s.observed {
		prev = now
	}
	if wake {
		s.lastWake = now
		s.settleUntil = now.Add(s.cfg.WakeSettle)
	}
	s.observed, s.lastWall, s.lastMono = true, now, mono
	return now, prev, wake
}

// Tick runs one pass: wake detection, then each round's due slots, then the starts the host
// limit and the wake settle allow.
func (s *Scheduler) Tick() {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
}

// UpdateRun reports a run's new state: waiting, running or blocked while it lives, or a final
// state when it ends. An ended run updates the round's counts, may pause or complete the
// round, and lets a coalesced slot or a queued run start. A queued run belongs to the
// scheduler, which starts it: an update to a queued run, or into queued, is refused.
func (s *Scheduler) UpdateRun(runID string, u RunUpdate) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	run := s.live[runID]
	if err := checkUpdate(run, runID, u); err != nil {
		// The reading still counts: a wake it saw is acted on now.
		s.evaluateLocked(now, prev, wake)
		s.startLocked(now)
		s.unlockAndFlush()
		return err
	}
	if !u.State.Final() {
		if u.SessionID != "" {
			run.SessionID = u.SessionID
		}
		// A live run's reason says why it is in its state (a blocked run's dialog); the next
		// live update replaces it, and an empty one clears it.
		run.State, run.Reason = u.State, u.Reason
		s.saveRun(run)
	} else {
		s.endLocked(run, u, now, false)
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return nil
}

func checkUpdate(run *Run, runID string, u RunUpdate) error {
	switch {
	case run == nil:
		return fmt.Errorf("%w: %s is not live", ErrNoRun, runID)
	case run.State == RunQueued:
		return fmt.Errorf("rounds: run %s is queued; the scheduler starts it", runID)
	case u.State.Final():
		return nil
	case u.State == RunWaiting, u.State == RunRunning, u.State == RunBlocked:
		return nil
	}
	return fmt.Errorf("rounds: a run cannot move to %q", u.State)
}

// Pause pauses a round by the user: no new runs; a queued or waiting run ends skipped,
// "paused" (rounds.md 11), in the evaluation that follows. A run already running goes on.
func (s *Scheduler) Pause(roundID string) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	err := s.pauseLocked(roundID, "", now)
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return err
}

// Unpause resumes a paused round and clears its failure and skip counts (10.1). Slots that
// passed while it was paused do not run; the next slot is the first after now and after the
// last served slot. On a round that is not paused it does nothing.
func (s *Scheduler) Unpause(roundID string) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	err := s.unpauseLocked(roundID, now)
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return err
}

func (s *Scheduler) unpauseLocked(roundID string, now time.Time) error {
	e, ok := s.store.Get(roundID)
	if !ok {
		return ErrNotFound
	}
	if !e.Round.Paused {
		return nil
	}
	r := e.Round
	r.Paused = false
	if _, err := s.store.Update(r, now); err != nil {
		return err
	}
	c := s.compile(e.Round)
	s.setState(roundID, func(st *RoundState) {
		st.Failures, st.Skips, st.PausedReason = 0, 0, ""
		if c.err == nil && !st.Completed {
			st.NextSlot = nextAfter(c.sched, now, st.LastSlot)
		}
	})
	return nil
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
	// The evaluation that follows every pause ends a run that has not started and drops the
	// round's plans (evaluateRound, the paused branch).
	return nil
}

// Errors of the round edits.
var (
	// ErrRunLive refuses a run by hand while the round has a run that has not ended (rounds.md
	// 7.2, 13: round.run answers conflict).
	ErrRunLive = errors.New("the round has a live run")
	// ErrRoundPaused and ErrRoundCompleted refuse a run by hand of a round that starts no runs.
	ErrRoundPaused    = errors.New("the round is paused")
	ErrRoundCompleted = errors.New("the round has completed")
)

// Create checks and stores a new round (Store.Create) and reports it to the sink. With runNow
// it also queues one run by hand, which does not move the next slot (rounds.md 4.1, Run on
// create); the run is returned. A failed queue leaves the round created and returns its error
// beside the entry.
func (s *Scheduler) Create(r Round, createdBy string, runNow bool) (Entry, *Run, error) {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	e, err := s.store.Create(r, createdBy, now)
	if err != nil {
		s.unlockAndFlush()
		return Entry{}, nil, err
	}
	c := e
	s.events = append(s.events, event{round: &c})
	var run *Run
	if runNow {
		run, err = s.manualLocked(e.Round.ID, now)
		if ne, ok := s.store.Get(e.Round.ID); ok {
			e = ne
		}
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	if run != nil {
		// A copy: the live run changes under s.mu once it starts.
		c := *run
		run = &c
	}
	s.unlockAndFlush()
	return e, run, err
}

// Update replaces a round's definition (Store.Update) and reports it to the sink. The pause
// mark is not an edit: it stays as stored, and Pause and Unpause change it. A run in flight
// keeps the values it started with (rounds.md 3, Edits).
func (s *Scheduler) Update(r Round) (Entry, error) {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	old, ok := s.store.Get(r.ID)
	if !ok {
		s.unlockAndFlush()
		return Entry{}, ErrNotFound
	}
	r.Paused = old.Round.Paused
	e, err := s.store.Update(r, now)
	if err == nil {
		c := e
		s.events = append(s.events, event{round: &c})
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return e, err
}

// Delete removes a round and its ended runs, and reports it to the sink. A live run goes on to
// its end; a run that has not started ends skipped, "round deleted" (rounds.md 11).
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	old, ok := s.store.Get(id)
	if !ok {
		s.unlockAndFlush()
		return ErrNotFound
	}
	err := s.store.Delete(id)
	if err == nil {
		s.events = append(s.events, event{deleted: &old})
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return err
}

// RunNow queues one run by hand, outside the trigger: no slot, trigger kind manual (rounds.md
// 4, 13). A round with a live run is refused with ErrRunLive, a paused one with ErrRoundPaused
// and a completed one with ErrRoundCompleted. The run starts under the host limit and the wake
// settle, as any queued run.
func (s *Scheduler) RunNow(id string) (Run, error) {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	run, err := s.manualLocked(id, now)
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	var out Run
	if run != nil {
		out = *run
		if r := s.live[run.ID]; r != nil {
			out = *r
		}
	}
	s.unlockAndFlush()
	return out, err
}

func (s *Scheduler) manualLocked(id string, now time.Time) (*Run, error) {
	e, ok := s.store.Get(id)
	switch {
	case !ok:
		return nil, ErrNotFound
	case s.liveFor(id) != nil:
		return nil, ErrRunLive
	case e.Round.Paused:
		return nil, ErrRoundPaused
	case e.State.Completed:
		return nil, ErrRoundCompleted
	}
	run := Run{
		ID:      NewID("run"),
		Number:  e.State.LastRun + 1,
		RoundID: id,
		Trigger: TriggerManual,
		State:   RunQueued,
		Created: now,
	}
	ne, err := s.store.QueueRun(run, func(st *RoundState) { st.LastRun = run.Number })
	if err != nil {
		return nil, err
	}
	s.live[run.ID] = &run
	c := run
	s.events = append(s.events, event{round: &ne}, event{run: &c})
	return &run, nil
}

// notStarted reports whether a run has not begun its work: queued for a start, or waiting for
// the lock. The scheduler ends such a run itself when its round pauses, expires, completes or
// goes.
func notStarted(r *Run) bool {
	return r != nil && (r.State == RunQueued || r.State == RunWaiting)
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
		case ev.deleted != nil:
			s.cfg.Sink.RoundDeleted(*ev.deleted)
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
		sid, err := s.cfg.Starter.Start(e.Round, run)
		if errors.Is(err, ErrTargetGone) {
			s.UpdateRun(run.ID, RunUpdate{State: RunSkipped, Reason: err.Error(), TargetGone: true})
			continue
		}
		if err != nil {
			s.UpdateRun(run.ID, RunUpdate{State: RunFailed, Reason: "start failed: " + err.Error()})
			continue
		}
		s.setSession(run.ID, sid)
	}
}

// setSession saves the session id Start returned, unless the run has ended or already
// reported one.
func (s *Scheduler) setSession(runID, sid string) {
	if sid == "" {
		return
	}
	s.mu.Lock()
	if r := s.live[runID]; r != nil && r.SessionID == "" {
		r.SessionID = sid
		s.saveRun(r)
	}
	s.unlockAndFlush()
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

// evaluateLocked looks at every round at now. prev is the wall time of the reading before; a
// slot after it came due on time. wake says the host woke (or the clock jumped forward, or the
// hab started) since then: every due slot is a missed one.
func (s *Scheduler) evaluateLocked(now, prev time.Time, wake bool) {
	entries := s.store.List()
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Round.ID] = true
		s.evaluateRound(e, now, prev, wake)
	}
	for _, id := range sortedIDs(s.live) {
		if run := s.live[id]; !have[run.RoundID] && notStarted(run) {
			s.endLocked(run, RunUpdate{State: RunSkipped, Reason: ReasonDeleted}, now, true)
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

	if st.Completed {
		delete(s.owed, id)
		delete(s.retry, id)
		if notStarted(live) {
			s.endLocked(live, RunUpdate{State: RunSkipped, Reason: ReasonCompleted}, now, true)
		}
		return
	}
	if e.Round.Paused {
		delete(s.owed, id)
		delete(s.retry, id)
		if notStarted(live) {
			s.endLocked(live, RunUpdate{State: RunSkipped, Reason: ReasonPaused}, now, true)
		}
		if st.NextSlot != nil && !st.NextSlot.After(now) {
			s.setState(id, func(st *RoundState) { st.NextSlot = nextAfter(c.sched, now, st.LastSlot) })
		}
		return
	}
	if e.Round.ExpiresAt != nil && !now.Before(*e.Round.ExpiresAt) {
		s.complete(id, ReasonExpired)
		if notStarted(live) {
			s.endLocked(live, RunUpdate{State: RunSkipped, Reason: ReasonExpired}, now, true)
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
		p, planned := s.retry[id]
		switch {
		case planned && live == nil && !now.Before(p.at):
			delete(s.retry, id)
			if run := s.newRun(e, p.slot, p.missed, now, nil, true); run != nil {
				s.retried[run.ID] = true
			}
		case st.NextSlot == nil && e.Round.Trigger.Kind == TriggerOnce && live == nil && !planned:
			// A once trigger with nothing left to do: its run ended without completing the
			// round (a pause ended it, or a restart dropped its retry), or its instant passed
			// while the round was paused.
			reason := ReasonOnceDone
			if st.LastSlot == nil {
				reason = ReasonOncePassed
			}
			s.complete(id, reason)
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
	case notStarted(live):
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

// newRun queues a run for slot, numbered from the round's state. The run and the round's
// state (its number, and for a new slot the last served slot and the next slot) are saved in
// one write, so a failed save leaves the slot due. A retry serves a slot already served and
// leaves both slots alone.
func (s *Scheduler) newRun(e Entry, slot time.Time, missed int, now time.Time, next *time.Time, retry bool) *Run {
	run := Run{
		ID:      NewID("run"),
		Number:  e.State.LastRun + 1,
		RoundID: e.Round.ID,
		Slot:    &slot,
		Trigger: e.Round.Trigger.Kind,
		Missed:  missed,
		State:   RunQueued,
		Created: now,
	}
	ne, err := s.store.QueueRun(run, func(st *RoundState) {
		st.LastRun = run.Number
		if !retry {
			st.LastSlot, st.NextSlot = &slot, next
		}
	})
	if err != nil {
		s.logf("rounds: round %s: queue run: %v", e.Round.Name, err)
		return nil
	}
	s.live[run.ID] = &run
	c := run
	s.events = append(s.events, event{round: &ne}, event{run: &c})
	return &run
}

// complete marks a round completed. The completed branch of evaluateRound drops its plans.
func (s *Scheduler) complete(id, reason string) {
	s.setState(id, func(st *RoundState) {
		st.Completed, st.CompletedReason, st.NextSlot = true, reason, nil
	})
}

// startLocked starts queued runs, oldest slot first, while the host limit allows and the wake
// settle has passed (rounds.md 7.2, 8.3). Every live run that is not queued holds a place,
// waiting and blocked runs included. It marks the runs running; unlockAndFlush calls the
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
	sortQueued(queued)
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

// sortQueued orders queued runs oldest slot first (a run with no slot by when it was made),
// then by when it was made, then by id.
func sortQueued(queued []*Run) {
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
}

func runOrder(r *Run) time.Time {
	if r.Slot != nil {
		return *r.Slot
	}
	return r.Created
}

// endLocked ends a live run and applies the round's rules: counts, the failure pause, the
// skips notice, max_runs, the retry after wake (rounds.md 8.3, 10.1).
// internal is set when the scheduler ends a run that never started its work, because its
// round paused, expired, completed or went: such a run counts for nothing.
func (s *Scheduler) endLocked(run *Run, u RunUpdate, now time.Time, internal bool) {
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
	if !ok || internal {
		return
	}
	if u.TargetGone && (run.SessionID == "" || run.SessionID == e.Round.Target) {
		ne, ok := s.setState(run.RoundID, func(st *RoundState) {
			st.Completed, st.CompletedReason, st.NextSlot = true, ReasonTargetGone, nil
		})
		if ok && e.Round.Notify != NotifyNever && !e.State.Completed {
			r := *run
			s.notice(Notice{Kind: NoticeTargetGone, Round: ne, Run: &r,
				Text: fmt.Sprintf("round %s completed: its target session is gone", e.Round.Name)})
		}
		return
	}

	failed := u.State == RunFailed || u.State == RunTimedOut
	if failed && u.Network && !isRetry && run.Slot != nil && !s.lastWake.IsZero() &&
		!now.After(s.lastWake.Add(RetryWindow)) {
		s.retry[run.RoundID] = retryPlan{at: now.Add(RetryDelay), slot: *run.Slot, missed: run.Missed}
	}

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
		// A spent once trigger completes in the evaluation that follows (evaluateRound).
		if e.Round.MaxRuns > 0 && st.Finished >= e.Round.MaxRuns {
			st.Completed, st.CompletedReason, st.NextSlot = true, ReasonMaxRuns, nil
		}
	})
	if !ok {
		return
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
