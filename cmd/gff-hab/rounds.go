package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Rounds in gff-hab (rounds.md 3, 7, 8.4, 13): the round.* methods over internal/rounds, the
// round events, and command runs in holder panes. Harness prompts, the lock, notices and
// peers come in later stages.

// Reasons gff-hab writes on the runs it ends.
const (
	reasonHostRestarted = "host restarted"
	reasonSessionGone   = "run session gone"
	reasonPaneGone      = "pane gone before its program exited"
	reasonTyped         = "a client typed into the run session"
	reasonSent          = "a client sent the run session a message"
	reasonClosed        = "a client closed the run's pane"
	reasonNoHarnessRuns = "resume and fork rounds need the session lock, which this hab does not have yet (rounds.md 14, stage 5)"
)

// roundsCheckEvery is how often a run whose pane is on a holder the link has not reached yet is
// looked at again (rounds.md 8.4).
const roundsCheckEvery = 2 * time.Second

// roundsSettings is the [rounds] section of the hab's config file.
type roundsSettings struct {
	MaxRunning int
	// WakeSettle is the settle after a wake or a start; negative: none (rounds.md 8.3).
	WakeSettle time.Duration
}

// loadRoundsSettings reads [rounds] from the hab's config file: max_running, a whole number
// from 1, and wake_settle, a Go duration ("0s" for none). A missing file or section takes the
// defaults.
func loadRoundsSettings(path string) (roundsSettings, error) {
	var out roundsSettings
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var fc struct {
		Rounds struct {
			MaxRunning *int    `toml:"max_running"`
			WakeSettle *string `toml:"wake_settle"`
		} `toml:"rounds"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil {
		return out, fmt.Errorf("%s: %w", path, err)
	}
	if v := fc.Rounds.MaxRunning; v != nil {
		if *v < 1 {
			return out, fmt.Errorf("%s: rounds: max_running must be 1 or more", path)
		}
		out.MaxRunning = *v
	}
	if v := fc.Rounds.WakeSettle; v != nil {
		d, err := time.ParseDuration(*v)
		if err != nil || d < 0 {
			return out, fmt.Errorf("%s: rounds: wake_settle %q is not a duration such as 30s", path, *v)
		}
		out.WakeSettle = d
		if d == 0 {
			out.WakeSettle = -1
		}
	}
	return out, nil
}

// roundStart travels with a run's session.create through its context, so actions files the
// session with Session.round and names it after the round. No client can set it.
type roundStart struct {
	ref  api.SessionRound
	name string
}

type roundStartKey struct{}

func withRoundStart(ctx context.Context, rs roundStart) context.Context {
	return context.WithValue(ctx, roundStartKey{}, rs)
}

func roundStartFrom(ctx context.Context) (roundStart, bool) {
	rs, ok := ctx.Value(roundStartKey{}).(roundStart)
	return rs, ok
}

// runWatch is one live run gff-hab watches: its pane's exit, its timeout, and take over; for a
// harness run also its agent's states (roundharness.go).
type runWatch struct {
	run     rounds.Run
	round   rounds.Round // the values the run started with
	session string
	pane    string
	timer   *time.Timer
	h       *harnessWatch // nil for a command run
}

// stopLocked stops the watch's timers and, for a harness run, a prompt send in flight. The
// caller holds s.mu.
func (w *runWatch) stopLocked() {
	if w.timer != nil {
		w.timer.Stop()
	}
	if w.h != nil {
		w.h.stop()
	}
}

// roundsConfig is what newRounds needs.
type roundsConfig struct {
	state    string
	settings roundsSettings
	store    *structure.Store
	// create runs session.create for the hab itself, with its idempotency key.
	create func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error)
	// pane reads a pane's status from the holder link; complete says the link reached every
	// holder.
	pane     func(id string) (link.PaneStatus, bool)
	complete func() bool
	// closePane hangs up a pane's program and releases the pane (pane.close), as the hab, not as
	// a client: no take over.
	closePane func(ctx context.Context, pane string) error
	// projectFor finds or makes the project of an absolute folder, as session.create does.
	projectFor func(ctx context.Context, dir string) (string, error)
	publish    func(structure.Event)
	// notice raises one Flightdeck notice (a state file set aside).
	notice func(title, body string)
	tick   time.Duration // 0: rounds.TickEvery
	logf   func(string, ...any)

	// Harness runs (roundharness.go). agent reads a pane's agent state; send delivers a prompt
	// as session.send does, but as the hab, not as a client: no take over; interrupt stops a
	// working turn through its adapter; result reads a run's final agent message from its
	// session's conversation (native: the harness's own session id, when known); messages
	// reads that conversation once, for a run adopted after a restart; argv reads a process's
	// arguments; screen reads a pane's visible text; lookHarness finds a harness's program
	// (default exec.LookPath). ready is the readiness limit (default roundReady).
	agent       func(pane string) (api.AgentState, bool)
	send        func(ctx context.Context, session, text string) error
	interrupt   func(ctx context.Context, pane string) error
	result      func(ctx context.Context, session, harness, native string) (string, error)
	messages    func(ctx context.Context, session, harness, native string) ([]api.ChatMessage, error)
	argv        func(ctx context.Context, pid int) ([]string, error)
	screen      func(pane string) (string, bool)
	lookHarness func(name string) (string, error)
	ready       time.Duration
}

// roundsService runs the rounds of one gff-hab.
type roundsService struct {
	cfg   roundsConfig
	store *rounds.Store
	sched *rounds.Scheduler

	editMu sync.Mutex // one edit at a time: a revision check and its update
	liveMu sync.Mutex // one live-state report of a harness run at a time (liveLater)

	mu      sync.Mutex
	revs    map[string]uint64    // round id to its revision in this epoch
	watches map[string]*runWatch // run id to its watch
	byPane  map[string]string    // pane id to run id
	pending map[string]rounds.Run
	// recheck is closed to stop the loop that looks at pending runs again; nil while none runs.
	recheck chan struct{}
	closed  bool

	cancel context.CancelFunc
	done   chan struct{}
	// updateRun is the scheduler's UpdateRun; tests replace it.
	updateRun func(runID string, u rounds.RunUpdate) error
	// bg holds the work handed off the caller's goroutine: run ends, prunes and pane closes.
	// Close waits for it, so nothing writes the state after Close returns.
	bg sync.WaitGroup
}

var _ control.Rounds = (*roundsService)(nil)

// newRounds opens <state>/rounds and the scheduler. A state file that does not parse is set
// aside and one notice is raised; a file from a newer build stops gff-hab (rounds.md 7.7).
func newRounds(cfg roundsConfig) (*roundsService, error) {
	if cfg.lookHarness == nil {
		cfg.lookHarness = exec.LookPath
	}
	if cfg.ready <= 0 {
		cfg.ready = roundReady
	}
	if cfg.agent == nil {
		cfg.agent = func(string) (api.AgentState, bool) { return api.AgentState{}, false }
	}
	if cfg.send == nil {
		cfg.send = func(context.Context, string, string) error {
			return control.Errorf(api.ErrorKindUnavailable, "this hab has no harness adapters")
		}
	}
	if cfg.interrupt == nil {
		cfg.interrupt = func(context.Context, string) error { return nil }
	}
	if cfg.result == nil {
		cfg.result = func(context.Context, string, string, string) (string, error) { return "", nil }
	}
	if cfg.messages == nil {
		cfg.messages = func(context.Context, string, string, string) ([]api.ChatMessage, error) {
			return nil, errors.New("this hab reads no transcripts")
		}
	}
	st, aside, err := rounds.Open(cfg.state, rounds.Options{Logf: cfg.logf})
	if err != nil {
		return nil, err
	}
	if aside && cfg.notice != nil {
		cfg.notice("Rounds state set aside",
			"The rounds state file did not parse. gff-hab kept it beside the new one and started with no rounds.")
	}
	s := &roundsService{cfg: cfg, store: st, revs: map[string]uint64{}, watches: map[string]*runWatch{},
		byPane: map[string]string{}, pending: map[string]rounds.Run{}, done: make(chan struct{})}
	s.attachLostStarts()
	for _, e := range st.List() {
		s.revs[e.Round.ID] = 1
	}
	sc := rounds.SchedulerConfig{Store: st, Starter: s, Sink: s, MaxRunning: cfg.settings.MaxRunning,
		WakeSettle: cfg.settings.WakeSettle, Logf: cfg.logf}
	if cfg.tick > 0 {
		sc.NewTicker = func() (<-chan time.Time, func()) {
			t := time.NewTicker(cfg.tick)
			return t.C, t.Stop
		}
	}
	if s.sched, err = rounds.NewScheduler(sc); err != nil {
		return nil, err
	}
	s.updateRun = s.sched.UpdateRun
	return s, nil
}

// attachLostStarts runs before the scheduler loads: a run saved as started with no session id
// may still have its session, when gff-hab stopped after the spawn but before the scheduler
// saved the id. The session record carries Session.round with the run id, so the run gets
// that session back in the run log and is adopted at start, instead of failing "start lost"
// while its program runs on untracked. A run with no such session fails "start lost" as
// before (rounds.md 8.4).
func (s *roundsService) attachLostStarts() {
	live, err := s.store.Runs().Live()
	if err != nil {
		s.cfg.logf("rounds: live runs: %v", err)
		return
	}
	for _, r := range live {
		if r.State == rounds.RunQueued || r.SessionID != "" {
			continue
		}
		ss, ok := s.cfg.store.SessionOfRun(r.ID)
		if !ok {
			continue
		}
		r.SessionID = ss.ID
		if err := s.store.Runs().Update(r); err != nil {
			s.cfg.logf("rounds: run %s: attach session %s: %v", r.ID, ss.ID, err)
			continue
		}
		s.cfg.logf("rounds: run %s: its start was cut short; found its session %s", r.ID, ss.Name)
	}
}

// start adopts the runs that were live when the last gff-hab stopped, then starts the tick.
// Call it once the control server can create sessions and the link notices reach notice.
func (s *roundsService) start() {
	live, err := s.store.Runs().Live()
	if err != nil {
		s.cfg.logf("rounds: live runs: %v", err)
	}
	for _, r := range live {
		if r.State != rounds.RunQueued && r.SessionID != "" {
			s.adopt(r)
		}
	}
	for _, e := range s.store.List() {
		s.prune(e.Round.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		defer close(s.done)
		s.sched.Run(ctx)
	}()
}

// Close stops the tick and the timers, and waits for the work in flight. Runs stay as they are
// on disk; the next gff-hab adopts them.
func (s *roundsService) Close() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	s.mu.Lock()
	s.closed = true
	for _, w := range s.watches {
		w.stopLocked()
	}
	s.stopRecheckLocked()
	s.mu.Unlock()
	s.bg.Wait()
}

// --- Run sessions ---

// Start starts a run's session (rounds.Starter): a fresh command run, or a fresh harness run, in
// a new pane. It returns at the spawn, with the session id; the end comes back through the
// pane's exit, or the agent's turn.
func (s *roundsService) Start(r rounds.Round, run rounds.Run) (string, error) {
	harnessRun := r.Mode == rounds.ModeFresh && len(r.Command) == 0 && r.Harness != ""
	if !harnessRun && (r.Mode != rounds.ModeFresh || len(r.Command) == 0) {
		return "", errors.New(reasonNoHarnessRuns)
	}
	env := []string{"GFF_ROUND=" + r.ID, "GFF_ROUND_RUN=" + run.ID}
	if run.Slot != nil {
		env = append(env, "GFF_ROUND_SLOT="+run.Slot.UTC().Format(time.RFC3339))
	}
	if e, ok := s.store.Get(r.ID); ok && e.State.LastOK != nil {
		env = append(env, "GFF_ROUND_LAST_OK="+e.State.LastOK.UTC().Format(time.RFC3339))
	}
	if harnessRun {
		return s.startHarness(r, run, env)
	}
	p := api.SessionCreateParams{Argv: r.Command, Project: r.Project, Cwd: r.Cwd, Env: env,
		IdempotencyKey: "round-run-" + run.ID}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = withRoundStart(ctx, roundStart{ref: api.SessionRound{Round: r.ID, Run: run.ID}, name: r.Name})
	made, err := s.cfg.create(ctx, p)
	if err != nil {
		return "", err
	}
	run.SessionID = made.Session
	s.watch(run, r, made.Session, made.Pane)
	return made.Session, nil
}

// watch follows a live run's pane: its exit ends the run, and the timeout hangs it up. A pane
// that exited before the watch began ends the run at once.
func (s *roundsService) watch(run rounds.Run, r rounds.Round, session, pane string) {
	s.watchWith(run, r, session, pane, nil)
}

// watchWith is watch, with h for a harness run. It reports false when the service is closed:
// nothing is watched.
func (s *roundsService) watchWith(run rounds.Run, r rounds.Round, session, pane string, h *harnessWatch) bool {
	run.SessionID = session
	w := &runWatch{run: run, round: r, session: session, pane: pane, h: h}
	started := run.Created
	if run.StartedAt != nil {
		started = *run.StartedAt
	}
	left := time.Until(started.Add(time.Duration(r.Timeout)))
	s.mu.Lock()
	if s.closed {
		if h != nil {
			h.stop()
		}
		s.mu.Unlock()
		return false
	}
	s.watches[run.ID] = w
	s.byPane[pane] = run.ID
	w.timer = time.AfterFunc(max(left, 0), func() { s.goBg(func() { s.timedOut(run.ID) }) })
	s.mu.Unlock()
	if st, ok := s.cfg.pane(pane); ok && st.Exited {
		s.exited(pane, st.Exit)
	}
	return true
}

// finish ends a watched run once: the first end wins, and later ones (an exit after a hang-up)
// find no watch. A harness run whose turn ended keeps that end: finish then does nothing while
// its final message is read (finishTurn). The scheduler hears the end off the caller's
// goroutine, so a link watcher or a client call never waits for the next run's spawn. After
// Close nothing ends: the run stays live on disk for the next gff-hab.
func (s *roundsService) finish(runID string, u rounds.RunUpdate) bool {
	return s.endRun(runID, u, false)
}

// finishTurn is finish for the end of a harness run's turn.
func (s *roundsService) finishTurn(runID string, u rounds.RunUpdate) bool {
	return s.endRun(runID, u, true)
}

func (s *roundsService) endRun(runID string, u rounds.RunUpdate, turn bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.watches[runID]
	if w == nil || (!turn && w.h != nil && w.h.end != nil) {
		return false
	}
	u.SessionID = w.session
	if !s.goLocked(func() { s.update(runID, u) }) {
		return false
	}
	delete(s.watches, runID)
	delete(s.byPane, w.pane)
	w.stopLocked()
	return true
}

// unwatch stops watching a run without ending it.
func (s *roundsService) unwatch(w *runWatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watches[w.run.ID] == w {
		delete(s.watches, w.run.ID)
		delete(s.byPane, w.pane)
		w.stopLocked()
	}
}

func (s *roundsService) update(runID string, u rounds.RunUpdate) {
	if err := s.updateRun(runID, u); err != nil {
		s.cfg.logf("rounds: run %s: %v", runID, err)
	}
}

func (s *roundsService) runOf(pane string) (*runWatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byPane[pane]
	if !ok {
		return nil, false
	}
	return s.watches[id], true
}

// exited ends a run whose program exited: exit 0 succeeded, any other status failed. A harness
// whose program exits before its turn ended fails, whatever its status.
func (s *roundsService) exited(pane string, st hp.ExitStatus) {
	w, ok := s.runOf(pane)
	if !ok {
		return
	}
	s.finish(w.run.ID, s.exitOf(w, link.APIExit(st)))
}

// exitOf is the end of a run whose program exited with e.
func (s *roundsService) exitOf(w *runWatch, e api.ExitStatus) rounds.RunUpdate {
	u := exitUpdate(e)
	if w.h == nil {
		return u
	}
	reason := reasonHarnessExited
	switch {
	case u.State == rounds.RunSucceeded:
		reason += " (exit status 0)"
	case u.Reason != "":
		reason += " (" + u.Reason + ")"
	}
	return rounds.RunUpdate{State: rounds.RunFailed, Reason: reason}
}

func exitUpdate(e api.ExitStatus) rounds.RunUpdate {
	switch {
	case e.Code != nil && *e.Code == 0:
		return rounds.RunUpdate{State: rounds.RunSucceeded}
	case e.Code != nil:
		return rounds.RunUpdate{State: rounds.RunFailed, Reason: "exit status " + strconv.Itoa(*e.Code)}
	case e.Signal != "":
		return rounds.RunUpdate{State: rounds.RunFailed, Reason: "ended by " + e.Signal}
	}
	return rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonPaneGone}
}

// timedOut ends a run past its timeout, then hangs its program up as pane.close does (7.3).
func (s *roundsService) timedOut(runID string) {
	s.mu.Lock()
	w := s.watches[runID]
	s.mu.Unlock()
	if w == nil {
		return
	}
	if w.h != nil {
		s.harnessTimedOut(w)
		return
	}
	if !s.finish(runID, rounds.RunUpdate{State: rounds.RunTimedOut,
		Reason: "ran past its timeout of " + time.Duration(w.round.Timeout).String()}) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.cfg.closePane(ctx, w.pane); err != nil {
		s.cfg.logf("rounds: run %s: hang up pane %s: %v", runID, w.pane, err)
	}
}

// notice takes the holder link's pane notices: a run's pane exited or went, or a pane came
// back that a run adopted at start waits for.
func (s *roundsService) notice(n link.Notice) {
	pane := n.Pane.UUID.String()
	switch n.Kind {
	case link.PaneExited:
		s.exited(pane, n.Pane.Exit)
	case link.PaneGone:
		w, ok := s.runOf(pane)
		if !ok {
			return
		}
		if n.Pane.Lost && !n.Pane.Gone {
			// The holder connection ended. The pane comes back if the holder lives; if it
			// died, the run fails "host restarted" once the link reaches every holder.
			s.unwatch(w)
			s.wait(w.run)
			return
		}
		if n.Pane.Exited {
			s.finish(w.run.ID, s.exitOf(w, link.APIExit(n.Pane.Exit)))
		} else {
			s.finish(w.run.ID, rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonPaneGone})
		}
	case link.PaneAdded:
		s.checkPending()
	}
}

// takeOver ends the run of a pane as taken over (7.5): a client typed into it, sent it a
// message, or closed it. The session is the user's from then on: never closed or timed out by
// the round. Input that answers Claude Code's trust dialog is not a take over, as an
// approval.answer is not; a run whose turn already ended keeps that end.
func (s *roundsService) takeOver(pane, reason string) {
	s.mu.Lock()
	id, ok := s.byPane[pane]
	w := s.watches[id]
	answer := ok && w != nil && reason == reasonTyped && w.trustAnswerLocked()
	s.mu.Unlock()
	if ok && !answer {
		s.finish(id, rounds.RunUpdate{State: rounds.RunTakenOver, Reason: reason})
	}
}

func (s *roundsService) takeOverSession(session, reason string) {
	if ss, ok := s.cfg.store.Session(session); ok && ss.Pane != "" {
		s.takeOver(ss.Pane, reason)
	}
}

// adopt picks up a run that was live when the last gff-hab stopped (rounds.md 8.4): its
// session's pane still runs on a holder, so it is watched again; it exited meanwhile, so the
// run ends by its exit; or the holder no longer has it, so the host restarted.
func (s *roundsService) adopt(run rounds.Run) {
	ss, ok := s.cfg.store.Session(run.SessionID)
	if !ok || ss.Pane == "" {
		s.update(run.ID, rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonSessionGone, SessionID: run.SessionID})
		return
	}
	r := rounds.Round{ID: run.RoundID, Timeout: rounds.Duration(rounds.DefaultTimeout), OnFinish: rounds.OnFinishClose}
	if e, ok := s.store.Get(run.RoundID); ok {
		r = e.Round
	}
	run.SessionID = ss.ID
	var h *harnessWatch
	if r.Mode == rounds.ModeFresh && len(r.Command) == 0 && r.Harness != "" {
		// Whether the prompt went in before the restart is not kept: adoptHarness reads the
		// run's conversation for it.
		h = &harnessWatch{kind: r.Harness, prompt: r.Prompt, phase: phaseTurn, blocked: run.State == rounds.RunBlocked}
	}
	if st, ok := s.cfg.pane(ss.Pane); ok && !st.Lost {
		if h != nil {
			s.adoptHarness(ss.ID, int(st.Pid), h)
			s.watchHarness(run, r, ss.ID, ss.Pane, h)
		} else {
			s.watch(run, r, ss.ID, ss.Pane)
		}
		return
	}
	if ss.Exit != nil && (ss.Exit.Code != nil || ss.Exit.Signal != "") {
		s.update(run.ID, withSession(s.exitOf(&runWatch{h: h}, *ss.Exit), ss.ID))
		return
	}
	if s.cfg.complete() {
		s.update(run.ID, rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonHostRestarted, SessionID: ss.ID})
		return
	}
	// A holder the link has not reached yet may have the pane: look again when a pane comes
	// back, and every few seconds, until every holder is reached.
	s.wait(run)
}

// wait keeps a run whose pane the link does not list now, and looks at it again when a pane
// comes back and every roundsCheckEvery, until the pane is back or every holder is reached.
func (s *roundsService) wait(run rounds.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.pending[run.ID] = run
	if s.recheck != nil {
		return
	}
	stop := make(chan struct{})
	s.recheck = stop
	go func() {
		t := time.NewTicker(roundsCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.checkPending()
			}
		}
	}()
}

// stopRecheckLocked ends the loop of wait.
func (s *roundsService) stopRecheckLocked() {
	if s.recheck != nil {
		close(s.recheck)
		s.recheck = nil
	}
}

func withSession(u rounds.RunUpdate, session string) rounds.RunUpdate {
	u.SessionID = session
	return u
}

// checkPending adopts the waiting runs again; it reports whether any still waits.
func (s *roundsService) checkPending() bool {
	s.mu.Lock()
	runs := make([]rounds.Run, 0, len(s.pending))
	for _, r := range s.pending {
		runs = append(runs, r)
	}
	s.pending = map[string]rounds.Run{}
	s.mu.Unlock()
	for _, r := range runs {
		s.adopt(r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		s.stopRecheckLocked()
		return false
	}
	return true
}

// --- The scheduler's sink: events, and the end of a run ---

func (s *roundsService) RoundChanged(e rounds.Entry) {
	s.mu.Lock()
	s.revs[e.Round.ID]++
	r := s.apiRoundLocked(e)
	s.mu.Unlock()
	s.cfg.publish(structure.Event{Type: control.EventRoundUpdated, ID: r.ID, Revision: r.Revision, Data: api.NonNil(r)})
}

func (s *roundsService) RoundDeleted(e rounds.Entry) {
	s.mu.Lock()
	s.revs[e.Round.ID]++
	r := s.apiRoundLocked(e)
	delete(s.revs, e.Round.ID)
	s.mu.Unlock()
	r.Deleted, r.LastRun = true, nil
	s.cfg.publish(structure.Event{Type: control.EventRoundUpdated, ID: r.ID, Revision: r.Revision, Data: api.NonNil(r)})
}

// RunChanged sends round.run_updated. A run that ended is no longer watched; a succeeded run
// of a round with on_finish close has its pane closed: the session record stays, exited
// (7.6). A run that failed, timed out or was taken over keeps its pane.
func (s *roundsService) RunChanged(r rounds.Run) {
	s.cfg.publish(structure.Event{Type: control.EventRoundRunUpdated, ID: r.ID, Data: api.NonNil(apiRun(r))})
	if !r.State.Final() {
		return
	}
	s.mu.Lock()
	w := s.watches[r.ID]
	if w != nil {
		delete(s.watches, r.ID)
		delete(s.byPane, w.pane)
		w.stopLocked()
	}
	s.mu.Unlock()
	s.bg.Go(func() { s.prune(r.RoundID) })
	if r.State != rounds.RunSucceeded || r.SessionID == "" {
		return
	}
	onFinish := rounds.OnFinishClose
	if w != nil {
		onFinish = w.round.OnFinish
	} else if e, ok := s.store.Get(r.RoundID); ok {
		onFinish = e.Round.OnFinish
	}
	if onFinish != rounds.OnFinishClose {
		return
	}
	ss, ok := s.cfg.store.Session(r.SessionID)
	if !ok || ss.Pane == "" {
		return
	}
	s.bg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.cfg.closePane(ctx, ss.Pane); err != nil {
			s.cfg.logf("rounds: run %s: close pane %s: %v", r.ID, ss.Pane, err)
		}
	})
}

// prune applies the retention rule to a round's runs (rounds.md 7.6) and removes the session
// records of the runs it dropped, when they have exited. A live session stays: it is the
// user's now. Transcripts are not touched.
func (s *roundsService) prune(roundID string) {
	removed, err := s.store.Runs().Prune(roundID, time.Now())
	if err != nil {
		s.cfg.logf("rounds: round %s: prune: %v", roundID, err)
		return
	}
	for _, r := range removed {
		if r.SessionID == "" {
			continue
		}
		if ss, ok := s.cfg.store.Session(r.SessionID); ok && ss.State == api.SessionStateExited {
			if err := s.cfg.store.CloseSession(ss.ID); err != nil {
				s.cfg.logf("rounds: run %s: remove session %s: %v", r.ID, ss.ID, err)
			}
		}
	}
}

// Notice logs a notice the scheduler wants raised; round notifications come in a later stage
// (rounds.md 10.2, stage 6).
func (s *roundsService) Notice(n rounds.Notice) { s.cfg.logf("rounds: %s", n.Text) }

// --- The API ---

func (s *roundsService) apiRoundLocked(e rounds.Entry) api.Round {
	rev := s.revs[e.Round.ID]
	if rev == 0 {
		rev = 1
		s.revs[e.Round.ID] = rev
	}
	r := apiRound(e)
	r.Revision = rev
	if p, ok := s.cfg.store.Project(e.Round.Project); ok {
		r.ProjectRemote = p.Remote
	}
	if _, err := e.Round.Trigger.Schedule(); err != nil {
		r.TriggerError = err.Error()
	}
	if runs, _, err := s.store.Runs().Page(e.Round.ID, 0, 1); err == nil && len(runs) > 0 {
		lr := apiRun(runs[0])
		r.LastRun = &lr
	}
	return r
}

func rfc(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func apiRound(e rounds.Entry) api.Round {
	r, st := e.Round, e.State
	return api.Round{
		ID: r.ID,
		RoundSpec: api.RoundSpec{
			Name: r.Name, Mode: api.RoundMode(r.Mode),
			Trigger: api.RoundTrigger{Kind: api.RoundTriggerKind(r.Trigger.Kind), Cron: r.Trigger.Cron, Zone: r.Trigger.Zone, At: rfc(r.Trigger.At)},
			Prompt:  r.Prompt, Harness: r.Harness, Model: r.Model, HarnessArgs: r.HarnessArgs, Command: r.Command,
			Project: r.Project, Cwd: r.Cwd, Target: r.Target, MaxRuns: r.MaxRuns, ExpiresAt: rfc(r.ExpiresAt),
			TimeoutMS: time.Duration(r.Timeout).Milliseconds(), WaitMS: time.Duration(r.Wait).Milliseconds(),
			CatchUp: api.RoundCatchUp(r.CatchUp), OnFinish: api.RoundOnFinish(r.OnFinish), Notify: api.RoundNotify(r.Notify),
			Paused: r.Paused,
		},
		CreatedBy: r.CreatedBy, CreatedAt: rfc(&r.CreatedAt), UpdatedAt: rfc(&r.UpdatedAt),
		NextSlot: rfc(st.NextSlot), LastSlot: rfc(st.LastSlot), LastOK: rfc(st.LastOK),
		Failures: st.Failures, Skips: st.Skips, PausedReason: st.PausedReason,
		Completed: st.Completed, CompletedReason: st.CompletedReason,
	}
}

func apiRun(r rounds.Run) api.RoundRun {
	return api.RoundRun{
		ID: r.ID, Number: r.Number, Round: r.RoundID, Slot: rfc(r.Slot), Trigger: api.RoundTriggerKind(r.Trigger),
		Missed: r.Missed, State: api.RoundRunState(r.State), Reason: r.Reason, Session: r.SessionID,
		Created: rfc(&r.Created), StartedAt: rfc(r.StartedAt), EndedAt: rfc(r.EndedAt), Result: r.Result,
	}
}

// List returns every round, by name: the snapshot's rounds and round.list.
func (s *roundsService) List() []api.Round {
	entries := s.store.List()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.Round, 0, len(entries))
	for _, e := range entries {
		out = append(out, s.apiRoundLocked(e))
	}
	return out
}

func (s *roundsService) entry(ref api.RoundRef) (rounds.Entry, error) {
	var e rounds.Entry
	var ok bool
	switch {
	case ref.ID != "" && ref.Name != "":
		return e, control.Errorf(api.ErrorKindInvalid, "give a round's id or its name, not both")
	case ref.ID != "":
		e, ok = s.store.Get(ref.ID)
	case ref.Name != "":
		e, ok = s.store.ByName(ref.Name)
	default:
		return e, control.Errorf(api.ErrorKindInvalid, "name a round by id or name")
	}
	if !ok {
		return e, control.Errorf(api.ErrorKindNotFound, "no round %s%s on this hab", ref.ID, ref.Name)
	}
	return e, nil
}

func (s *roundsService) round(id string) (api.Round, error) {
	e, ok := s.store.Get(id)
	if !ok {
		return api.Round{}, control.Errorf(api.ErrorKindNotFound, "round %s is gone", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiRoundLocked(e), nil
}

func (s *roundsService) Get(ref api.RoundRef) (api.Round, error) {
	e, err := s.entry(ref)
	if err != nil {
		return api.Round{}, err
	}
	return s.round(e.Round.ID)
}

// definition turns a spec into a round's definition, finding its project (rounds.md 3).
func (s *roundsService) definition(ctx context.Context, p api.RoundSpec) (rounds.Round, error) {
	bad := func(field, format string, a ...any) error {
		return control.Errorf(api.ErrorKindInvalid, "%s: %s", field, fmt.Sprintf(format, a...))
	}
	for _, w := range []struct{ set, word string }{
		{"RoundMode", string(p.Mode)}, {"RoundTriggerKind", string(p.Trigger.Kind)}, {"RoundCatchUp", string(p.CatchUp)},
		{"RoundOnFinish", string(p.OnFinish)}, {"RoundNotify", string(p.Notify)},
	} {
		if w.word != "" && !wordIn(w.set, w.word) {
			return rounds.Round{}, bad(w.set, "%q is not one of %v", w.word, api.Enums[w.set])
		}
	}
	r := rounds.Round{
		Name: p.Name, Mode: rounds.Mode(p.Mode), Prompt: p.Prompt, Harness: p.Harness, Model: p.Model,
		HarnessArgs: p.HarnessArgs, Command: p.Command, Project: p.Project, Cwd: p.Cwd, Target: p.Target,
		MaxRuns: p.MaxRuns, Timeout: rounds.Duration(time.Duration(p.TimeoutMS) * time.Millisecond),
		Wait: rounds.Duration(time.Duration(p.WaitMS) * time.Millisecond), CatchUp: rounds.CatchUp(p.CatchUp),
		OnFinish: rounds.OnFinish(p.OnFinish), Notify: rounds.Notify(p.Notify), Paused: p.Paused,
		Trigger: rounds.Trigger{Kind: rounds.TriggerKind(p.Trigger.Kind), Cron: p.Trigger.Cron, Zone: p.Trigger.Zone},
	}
	t := p.Trigger
	if t.Every != "" {
		if t.Cron != "" {
			return r, bad("trigger", "give cron or every, not both")
		}
		if t.Kind != "" && t.Kind != api.RoundTriggerKindCron {
			return r, bad("trigger", "every is for a cron trigger")
		}
		c, err := rounds.PresetCron(t.Every)
		if err != nil {
			return r, bad("trigger", "%v", err)
		}
		r.Trigger.Kind, r.Trigger.Cron = rounds.TriggerCron, c
	}
	if t.At != "" {
		at, err := time.Parse(time.RFC3339, t.At)
		if err != nil {
			return r, bad("trigger", "at %q is not an RFC 3339 time", t.At)
		}
		r.Trigger.At = &at
	}
	if p.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, p.ExpiresAt)
		if err != nil {
			return r, bad("expires_at", "%q is not an RFC 3339 time", p.ExpiresAt)
		}
		r.ExpiresAt = &at
	}
	if p.TimeoutMS < 0 || p.WaitMS < 0 {
		return r, bad("timeout_ms", "must not be negative")
	}
	n := 0
	for _, v := range []string{p.Project, p.ProjectDir, p.ProjectRemote} {
		if v != "" {
			n++
		}
	}
	switch {
	case n > 1:
		return r, bad("project", "give one of project, project_dir and project_remote")
	case p.Project != "":
		if _, ok := s.cfg.store.Project(p.Project); !ok {
			return r, control.Errorf(api.ErrorKindNotFound, "project %s does not exist", p.Project)
		}
	case p.ProjectDir != "":
		if !filepath.IsAbs(p.ProjectDir) {
			return r, bad("project_dir", "%q is not an absolute path", p.ProjectDir)
		}
		id, err := s.cfg.projectFor(ctx, p.ProjectDir)
		if err != nil {
			return r, err
		}
		r.Project = id
	case p.ProjectRemote != "":
		pr, ok := s.cfg.store.ProjectByIdentity(p.ProjectRemote, "")
		if !ok {
			return r, control.Errorf(api.ErrorKindNotFound, "no project on this hab has the remote %s", p.ProjectRemote)
		}
		r.Project = pr.ID
	}
	if r.Cwd != "" && !filepath.IsAbs(r.Cwd) {
		return r, bad("cwd", "%q is not an absolute path", r.Cwd)
	}
	if r.Target != "" {
		if _, ok := s.cfg.store.Session(r.Target); !ok {
			return r, control.Errorf(api.ErrorKindNotFound, "target session %s does not exist on this hab", r.Target)
		}
	}
	return r, nil
}

func wordIn(set, w string) bool {
	for _, x := range api.Enums[set] {
		if x == w {
			return true
		}
	}
	return false
}

// roundErr maps a rounds error to its API kind.
func roundErr(err error) error {
	var fe *rounds.FieldError
	var ce *control.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.As(err, &fe):
		return control.Errorf(api.ErrorKindInvalid, "%v", err)
	case errors.Is(err, rounds.ErrNameTaken):
		return control.Errorf(api.ErrorKindConflict, "a round named %s exists", trimErr(err, rounds.ErrNameTaken))
	case errors.Is(err, rounds.ErrNotFound):
		return control.Errorf(api.ErrorKindNotFound, "%v", err)
	case errors.Is(err, rounds.ErrRunLive), errors.Is(err, rounds.ErrRoundPaused), errors.Is(err, rounds.ErrRoundCompleted):
		return control.Errorf(api.ErrorKindConflict, "%v", err)
	}
	return control.Errorf(api.ErrorKindUnavailable, "%v", err)
}

func trimErr(err, kind error) string {
	s := err.Error()
	if p := kind.Error() + ": "; len(s) > len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func (s *roundsService) Create(ctx context.Context, p api.RoundCreateParams, createdBy string) (api.RoundCreated, error) {
	r, err := s.definition(ctx, p.Round)
	if err != nil {
		return api.RoundCreated{}, err
	}
	if err := s.checkHarness(r); err != nil {
		return api.RoundCreated{}, err
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	e, run, err := s.sched.Create(r, createdBy, p.RunNow)
	if err != nil && e.Round.ID == "" {
		return api.RoundCreated{}, roundErr(err)
	}
	if err != nil {
		// The round is stored; only its first run could not be queued. The answer says so.
		return api.RoundCreated{}, control.Errorf(api.ErrorKindUnavailable, "round %s was created, but its run now was not queued: %v", r.Name, err)
	}
	out := api.RoundCreated{}
	if out.Round, err = s.round(e.Round.ID); err != nil {
		return api.RoundCreated{}, err
	}
	if run != nil {
		ar := apiRun(*run)
		if runs, _, err := s.store.Runs().Page(e.Round.ID, run.Number+1, 1); err == nil && len(runs) == 1 && runs[0].ID == run.ID {
			ar = apiRun(runs[0])
		}
		out.Run = &ar
	}
	return out, nil
}

func (s *roundsService) Update(ctx context.Context, p api.RoundUpdateParams) (api.Round, error) {
	if p.ID == "" {
		return api.Round{}, control.Errorf(api.ErrorKindInvalid, "round.update needs the round's id")
	}
	r, err := s.definition(ctx, p.Round)
	if err != nil {
		return api.Round{}, err
	}
	r.ID = p.ID
	s.editMu.Lock()
	defer s.editMu.Unlock()
	old, ok := s.store.Get(p.ID)
	if !ok {
		return api.Round{}, control.Errorf(api.ErrorKindNotFound, "no round %s on this hab", p.ID)
	}
	// A harness is checked when the edit names a new one; a round whose harness went away
	// stays editable (its runs fail "harness not found").
	if old.Round.Harness != r.Harness || old.Round.Mode != r.Mode {
		if err := s.checkHarness(r); err != nil {
			return api.Round{}, err
		}
	}
	s.mu.Lock()
	rev := max(s.revs[p.ID], 1)
	s.mu.Unlock()
	if p.Revision != 0 && p.Revision != rev {
		return api.Round{}, control.Errorf(api.ErrorKindConflict, "round %s is at revision %d, not %d; read it again", r.Name, rev, p.Revision)
	}
	if _, err := s.sched.Update(r); err != nil {
		return api.Round{}, roundErr(err)
	}
	return s.round(p.ID)
}

func (s *roundsService) Delete(ref api.RoundRef) error {
	e, err := s.entry(ref)
	if err != nil {
		return err
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	return roundErr(s.sched.Delete(e.Round.ID))
}

func (s *roundsService) Pause(ref api.RoundRef) (api.Round, error) {
	e, err := s.entry(ref)
	if err != nil {
		return api.Round{}, err
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if err := s.sched.Pause(e.Round.ID); err != nil {
		return api.Round{}, roundErr(err)
	}
	return s.round(e.Round.ID)
}

func (s *roundsService) Unpause(ref api.RoundRef) (api.Round, error) {
	e, err := s.entry(ref)
	if err != nil {
		return api.Round{}, err
	}
	s.editMu.Lock()
	defer s.editMu.Unlock()
	if err := s.sched.Unpause(e.Round.ID); err != nil {
		return api.Round{}, roundErr(err)
	}
	return s.round(e.Round.ID)
}

func (s *roundsService) Run(ref api.RoundRef) (api.RoundRun, error) {
	e, err := s.entry(ref)
	if err != nil {
		return api.RoundRun{}, err
	}
	run, err := s.sched.RunNow(e.Round.ID)
	if err != nil {
		return api.RoundRun{}, roundErr(err)
	}
	if runs, _, err := s.store.Runs().Page(e.Round.ID, run.Number+1, 1); err == nil && len(runs) == 1 && runs[0].ID == run.ID {
		run = runs[0]
	}
	return apiRun(run), nil
}

// Runs pages a round's runs, newest first: 50 by default, at most 200.
func (s *roundsService) Runs(p api.RoundRunsParams) (api.RoundRuns, error) {
	e, err := s.entry(p.RoundRef)
	if err != nil {
		return api.RoundRuns{}, err
	}
	limit := p.Limit
	switch {
	case limit < 0:
		return api.RoundRuns{}, control.Errorf(api.ErrorKindInvalid, "limit must not be negative")
	case limit == 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	runs, more, err := s.store.Runs().Page(e.Round.ID, p.Before, limit)
	if err != nil {
		return api.RoundRuns{}, roundErr(err)
	}
	out := api.RoundRuns{Runs: make([]api.RoundRun, 0, len(runs)), More: more}
	for _, r := range runs {
		out.Runs = append(out.Runs, apiRun(r))
	}
	return out, nil
}

// --- Take over: the client calls that end a run (rounds.md 7.5) ---

// canonicalPane returns the link's form of a pane id, or the id as given.
func canonicalPane(pane string) string {
	if u, ok := parseUUID(pane); ok {
		return u.String()
	}
	return pane
}

// roundInput ends a run as taken over when a client's pane.input reaches its pane. Refused
// input takes nothing over.
type roundInput struct {
	control.Input
	rounds *roundsService
}

func (i roundInput) QueueInput(ctx context.Context, client, pane string, data []byte) func() error {
	wait := i.Input.QueueInput(ctx, client, pane, data)
	return func() error {
		err := wait()
		if err == nil {
			i.rounds.takeOver(canonicalPane(pane), reasonTyped)
		}
		return err
	}
}

// roundAgents ends a run as taken over when a client's session.send delivers a message to its
// session; a refused send takes nothing over. approval.answer is not a take over: it answers the run's own request.
type roundAgents struct {
	control.Agents
	rounds *roundsService
}

func (a roundAgents) Send(ctx context.Context, client string, p api.SessionSendParams) error {
	if err := a.Agents.Send(ctx, client, p); err != nil {
		return err
	}
	a.rounds.takeOverSession(p.Session, reasonSent)
	return nil
}

// SendsContext passes the Capcom wrapper's answer through, so a context-only send to a Capcom
// session still gets past the empty-send check (capcom.md 8.4).
func (a roundAgents) SendsContext(session string, keyed bool) bool {
	cs, ok := a.Agents.(control.ContextSender)
	return ok && cs.SendsContext(session, keyed)
}

var _ control.ContextSender = roundAgents{}

// roundActions ends a run as taken over when a client closes its pane (pane.close), before
// the close: the hang-up's exit must not end the run as failed. The hab's own closes (a
// timeout, on_finish) go to the link directly.
type roundActions struct {
	*actions
	rounds *roundsService
}

func (a roundActions) ClosePane(ctx context.Context, pane string) error {
	a.rounds.takeOver(canonicalPane(pane), reasonClosed)
	return a.actions.ClosePane(ctx, pane)
}

// TargetSelf is the target a round names from inside a pane to mean the pane's own session
// (rounds.md 13). Only the pane route takes it.
const TargetSelf = "self"

// roundHook answers the round methods from a pane (rounds.md 13). The pane's session is the
// creator (created_by). The pane route is narrower than the control socket: an agent, perhaps
// in a harness sandbox, must not gain an unsandboxed scheduled command over a round it did not
// make, nor aim a round at another session. So from a pane:
//   - a round's target is the calling session only: "self" or its own id;
//   - round.update, round.delete, round.pause, round.unpause and round.run act only on a round
//     the calling session created; any other is refused with denied.
//
// round.list, round.get and round.runs read every round. A pane no session files creates as a
// user would, cannot name a target, and edits nothing.
func roundHook(store *structure.Store, svc *roundsService) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		bad := func(err error) error { return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err) }
		ss, filed := store.SessionByPane(pane)
		target := func(spec *api.RoundSpec) error {
			switch {
			case spec.Target == "":
				return nil
			case !filed && spec.Target == TargetSelf:
				return control.Errorf(api.ErrorKindInvalid, "target self needs a pane that is a session")
			case !filed:
				return control.Errorf(api.ErrorKindDenied, "from a pane, a round may target only the calling session, and this pane is not a session")
			case spec.Target == TargetSelf || spec.Target == ss.ID:
				spec.Target = ss.ID
				return nil
			}
			return control.Errorf(api.ErrorKindDenied, "from a pane, a round may target only the calling session (self); %s is another session", spec.Target)
		}
		// own checks that the round exists and that the calling session created it.
		own := func(ref api.RoundRef) error {
			r, err := svc.Get(ref)
			if err != nil {
				return err
			}
			if !filed || r.CreatedBy != ss.ID {
				return control.Errorf(api.ErrorKindDenied, "%s: from a pane, only rounds this session created can be changed or run; round %s was not; use the control socket", method, r.Name)
			}
			return nil
		}
		var ref api.RoundRef
		switch method {
		case "round.list":
			return api.RoundList{Rounds: svc.List()}, nil, nil
		case "round.create":
			var p api.RoundCreateParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			if err := target(&p.Round); err != nil {
				return nil, nil, err
			}
			by := ""
			if filed {
				by = ss.ID
			}
			r, err := svc.Create(ctx, p, by)
			return r, nil, err
		case "round.update":
			var p api.RoundUpdateParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			if err := own(api.RoundRef{ID: p.ID}); err != nil {
				return nil, nil, err
			}
			if err := target(&p.Round); err != nil {
				return nil, nil, err
			}
			r, err := svc.Update(ctx, p)
			return api.RoundResult{Round: r}, nil, err
		case "round.runs":
			var p api.RoundRunsParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			r, err := svc.Runs(p)
			return r, nil, err
		}
		if err := json.Unmarshal(params, &ref); err != nil {
			return nil, nil, bad(err)
		}
		if method == "round.get" {
			r, err := svc.Get(ref)
			return api.RoundResult{Round: r}, nil, err
		}
		if err := own(ref); err != nil {
			return nil, nil, err
		}
		switch method {
		case "round.delete":
			return api.Empty{}, nil, svc.Delete(ref)
		case "round.pause":
			r, err := svc.Pause(ref)
			return api.RoundResult{Round: r}, nil, err
		case "round.unpause":
			r, err := svc.Unpause(ref)
			return api.RoundResult{Round: r}, nil, err
		default: // round.run
			r, err := svc.Run(ref)
			return api.RoundRunResult{Run: r}, nil, err
		}
	}
}
