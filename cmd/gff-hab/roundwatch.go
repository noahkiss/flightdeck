package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// Watchers in gff-hab (rounds.md 4.4): each watch round has one watcher session,
// <round>-watch, whose pane runs `gff-hab watch-pane -- <argv>`. gff-hab starts it when the round
// is created or unpaused and at its own start, restarts it with a backoff after it exits, pauses
// the round after repeated quick exits, hangs it up and starts it again at each wake, and stops
// it on a pause, a delete or a completed round. A gff-hab restart leaves it running.

// Watcher states (api.RoundWatcherState).
const (
	watchStarting api.RoundWatcherState = "starting"
	watchRunning  api.RoundWatcherState = "running"
	watchBackoff  api.RoundWatcherState = "backoff"
	watchStopped  api.RoundWatcherState = "stopped"
)

// watchRetry is how soon a start looks again at a watcher session whose last pane the structure
// has not seen exit yet: gff-hab's own hang-up races the record.
const watchRetry = 250 * time.Millisecond

// Reasons on a stopped watcher.
const (
	watchReasonPaused    = "the round is paused"
	watchReasonCompleted = "the round has completed"
	watchReasonDeleted   = "the round was deleted"
	watchReasonNotWatch  = "the round has no watch trigger"
	watchReasonExits     = "it exited too often soon after its start; unpause the round to start it again"
)

// watcher is one watch round's watcher as gff-hab supervises it.
type watcher struct {
	round    string
	state    api.RoundWatcherState
	reason   string
	session  string
	pane     string // the running pane; empty when none runs
	started  time.Time
	next     time.Time // starting or backoff: the next start
	timer    func() bool
	back     rounds.Backoff
	restarts int
	starts   int
	drops    int
	lastExit string
	// spec is the watch trigger and secret set the running watcher started with: an edit of
	// either restarts it, an edit of any other field does not.
	spec string
	// own says gff-hab is hanging the running watcher up (a wake, an edit, a stop): its exit
	// counts for nothing.
	own bool
}

// watchSup is the watchers' supervisor. One goroutine (loop) starts and stops watchers; the
// rest only mark rounds to look at and kick it.
type watchSup struct {
	s *roundsService

	mu     sync.Mutex
	m      map[string]*watcher // round id
	byPane map[string]string   // running pane to round id
	ready  bool                // the link reached every holder and the start settle passed
	dirty  map[string]bool
	all    bool
	kick   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	// born is when the supervisor was made, read from the scheduler's clock, so the start
	// settle counts on the same clock as now.
	born time.Time
}

func newWatchSup(s *roundsService) *watchSup {
	w := &watchSup{s: s, m: map[string]*watcher{}, byPane: map[string]string{}, dirty: map[string]bool{},
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	w.born = w.now()
	return w
}

func (w *watchSup) now() time.Time {
	if w.s.cfg.clock != nil {
		return w.s.cfg.clock.Now()
	}
	return time.Now()
}

// after runs f after d, on a timer; the returned func stops it.
func (w *watchSup) after(d time.Duration, f func()) func() bool {
	if w.s.cfg.watchAfter != nil {
		return w.s.cfg.watchAfter(d, f)
	}
	return time.AfterFunc(d, f).Stop
}

func (w *watchSup) lim() rounds.WatchLimits { return w.s.watchLimits() }

// look marks a round to look at, or every round with id "", and kicks the loop.
func (w *watchSup) look(id string) {
	w.mu.Lock()
	if id == "" {
		w.all = true
	} else {
		w.dirty[id] = true
	}
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// run is the loop: until every holder is reached and the start settle passed it looks every
// roundsCheckEvery; then it adopts the watchers that survived the last gff-hab and starts the
// rest, and from then on looks at each round it is kicked for.
func (w *watchSup) run() {
	defer close(w.done)
	t := time.NewTicker(roundsCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.checkLost()
		case <-w.kick:
		}
		w.mu.Lock()
		ready := w.ready
		w.mu.Unlock()
		if !ready && !w.becomeReady() {
			continue
		}
		w.mu.Lock()
		var ids []string
		if w.all {
			for _, e := range w.s.store.List() {
				w.dirty[e.Round.ID] = true
			}
			for id := range w.m {
				w.dirty[id] = true
			}
			w.all = false
		}
		for id := range w.dirty {
			ids = append(ids, id)
		}
		w.dirty = map[string]bool{}
		w.mu.Unlock()
		slices.Sort(ids)
		for _, id := range ids {
			w.reconcile(id)
		}
	}
}

// becomeReady adopts the running watchers once the link reached every holder, so a watcher in a
// slow holder is adopted, never started twice (rounds.md 4.4, 8.4). The others start after the
// start settle, as a host reboot does.
func (w *watchSup) becomeReady() bool {
	if !w.s.cfg.complete() {
		return false
	}
	settle := w.s.cfg.settings.WakeSettle
	if settle < 0 {
		settle = 0
	} else if settle == 0 {
		settle = rounds.DefaultWakeSettle
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready {
		return true
	}
	at := w.born.Add(settle)
	for _, e := range w.s.store.List() {
		if e.Round.Trigger.Kind != rounds.TriggerWatch {
			continue
		}
		ss, ok := w.s.cfg.store.SessionOfWatcher(e.Round.ID)
		if !ok || ss.Pane == "" || ss.State == api.SessionStateExited {
			if !e.Round.Paused && !e.State.Completed && w.now().Before(at) {
				wt := w.get(e.Round.ID)
				wt.state, wt.next = watchStarting, at
				wt.timer = w.after(at.Sub(w.now()), func() { w.look(e.Round.ID) })
			}
			continue
		}
		st, ok := w.s.cfg.pane(ss.Pane)
		if !ok || st.Lost || st.Exited {
			continue
		}
		wt := w.get(e.Round.ID)
		wt.state, wt.session, wt.pane, wt.started, wt.spec = watchRunning, ss.ID, ss.Pane, w.now(), watchSpec(e.Round)
		wt.starts = max(wt.starts, 1) // the adopted watcher is this gff-hab's first; the next start is a restart
		w.byPane[ss.Pane] = e.Round.ID
		w.s.cfg.logf("rounds: round %s: its watcher runs on in pane %s", e.Round.Name, ss.Pane)
	}
	w.ready = true
	w.all = true
	return true
}

// get returns the round's watcher, made stopped when there was none. The caller holds w.mu.
func (w *watchSup) get(id string) *watcher {
	wt := w.m[id]
	if wt == nil {
		wt = &watcher{round: id, state: watchStopped}
		w.m[id] = wt
	}
	return wt
}

// watchSpec is the part of a round its watcher depends on: the watch trigger and the secret set.
func watchSpec(r rounds.Round) string {
	var b strings.Builder
	if wt := r.Trigger.Watch; wt != nil {
		b.WriteString(strconv.Quote(strings.Join(wt.Argv, "\x00")))
		b.WriteString(strconv.Quote(wt.Cwd))
		b.WriteString(strconv.Quote(strings.Join(wt.Kinds, ",")))
	}
	b.WriteString(strconv.Quote(r.Secrets))
	return b.String()
}

// wanted reports whether a round's watcher should run, and if not, why.
func wanted(e rounds.Entry, ok bool) (bool, string) {
	switch {
	case !ok:
		return false, watchReasonDeleted
	case e.Round.Trigger.Kind != rounds.TriggerWatch || e.Round.Trigger.Watch == nil:
		return false, watchReasonNotWatch
	case e.Round.Paused:
		if e.State.PausedReason == rounds.ReasonWatcherPause {
			return false, watchReasonExits
		}
		return false, watchReasonPaused
	case e.State.Completed:
		return false, watchReasonCompleted
	}
	return true, ""
}

// reconcile makes a round's watcher match its round. Only the loop calls it.
func (w *watchSup) reconcile(id string) {
	e, ok := w.s.store.Get(id)
	want, why := wanted(e, ok)
	w.mu.Lock()
	wt := w.m[id]
	if !want {
		if wt == nil {
			w.mu.Unlock()
			if !ok {
				w.removeSession(id)
			}
			return
		}
		w.stopTimerLocked(wt)
		if why != watchReasonExits {
			wt.back.Reset()
		}
		wt.state, wt.reason, wt.next = watchStopped, why, time.Time{}
		pane := wt.pane
		if pane != "" && !wt.own {
			wt.own = true
			w.mu.Unlock()
			w.hangUp(id, pane)
			return
		}
		if pane == "" && !ok {
			delete(w.m, id)
			w.mu.Unlock()
			w.removeSession(id)
			return
		}
		w.mu.Unlock()
		return
	}
	if wt == nil {
		wt = w.get(id)
	}
	if wt.state == watchStopped && wt.reason != "" {
		// A round that starts again (an unpause, an edit) starts with a clean backoff.
		wt.back.Reset()
		wt.reason = ""
	}
	if wt.pane != "" {
		pane := wt.pane
		if wt.spec != watchSpec(e.Round) && !wt.own {
			// An edit of watch or secrets: gff-hab's own hang-up, then a start at once.
			wt.own = true
			w.mu.Unlock()
			w.hangUp(id, pane)
			return
		}
		w.mu.Unlock()
		return
	}
	if wt.timer != nil && w.now().Before(wt.next) {
		w.mu.Unlock()
		return // its backoff or the start settle is running
	}
	w.stopTimerLocked(wt)
	if n := w.runningLocked(id); n >= w.lim().MaxWatchers {
		wt.state, wt.reason = watchStopped, fmt.Sprintf("this hab runs its limit of %d watchers (max_watchers)", w.lim().MaxWatchers)
		w.mu.Unlock()
		return
	}
	wt.state, wt.reason = watchStarting, ""
	w.mu.Unlock()
	w.start(e, wt)
}

// runningLocked counts the watchers that run, start or back off, the round's own left out.
func (w *watchSup) runningLocked(except string) int {
	n := 0
	for id, wt := range w.m {
		if id != except && wt.state != watchStopped {
			n++
		}
	}
	return n
}

func (w *watchSup) stopTimerLocked(wt *watcher) {
	if wt.timer != nil {
		wt.timer()
		wt.timer = nil
	}
}

// hangUp ends a watcher's pane as pane.close does. Its exit comes back through exited.
func (w *watchSup) hangUp(id, pane string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.s.cfg.closePane(ctx, pane); err != nil {
		var ce *control.Error
		if errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound {
			w.exited(pane, "pane gone")
			return
		}
		w.s.cfg.logf("rounds: round %s: hang up its watcher's pane %s: %v", id, pane, err)
		// The pane still runs: gff-hab's hang-up is no longer under way, and the loop looks
		// again soon and tries once more if the round still wants it gone or restarted.
		w.mu.Lock()
		if wt := w.m[id]; wt != nil && wt.pane == pane && wt.own {
			wt.own = false
			w.stopTimerLocked(wt)
			wt.timer = w.after(watchHangUpRetry, func() { w.look(id) })
		}
		w.mu.Unlock()
	}
}

// watchHangUpRetry is how soon a failed hang-up of a watcher's pane is tried again.
const watchHangUpRetry = 5 * time.Second

// removeSession removes a deleted round's watcher session once its pane has exited.
func (w *watchSup) removeSession(id string) {
	ss, ok := w.s.cfg.store.SessionOfWatcher(id)
	if !ok || ss.State != api.SessionStateExited {
		return
	}
	if err := w.s.cfg.store.CloseSession(ss.ID); err != nil {
		w.s.cfg.logf("rounds: round %s: remove its watcher session %s: %v", id, ss.ID, err)
	}
}

// start starts a round's watcher in a new pane of its watcher session: the secret set first,
// then the pane. A failure of either counts as an exit and backs off.
func (w *watchSup) start(e rounds.Entry, wt *watcher) {
	r := e.Round
	now := w.now()
	env, err := w.s.watchEnv(r, e.State)
	var session, pane string
	if err == nil {
		session, pane, err = w.spawn(r, env)
	}
	if errors.Is(err, errWatchRetry) {
		w.mu.Lock()
		wt.next = now.Add(watchRetry)
		wt.timer = w.after(watchRetry, func() { w.look(r.ID) })
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	if err != nil {
		wt.started = now
		x := w.exitLocked(r.ID, wt, "did not start: "+err.Error(), now)
		w.mu.Unlock()
		w.s.cfg.logf("rounds: round %s: start its watcher: %v", r.Name, err)
		w.exitDone(x)
		return
	}
	if wt.starts > 0 {
		wt.restarts++
	}
	wt.starts++
	wt.state, wt.reason, wt.session, wt.pane, wt.started, wt.drops = watchRunning, "", session, pane, now, 0
	wt.spec, wt.own = watchSpec(r), false
	w.byPane[pane] = r.ID
	w.mu.Unlock()
	w.s.poke()
	w.s.publishRound(r.ID)
	// A pane that exited before byPane named it is caught here: the link has its exit.
	if st, ok := w.s.cfg.pane(pane); ok && st.Exited {
		w.exited(pane, watchExitText(link.APIExit(st.Exit)))
	}
}

var errWatchRetry = errors.New("the watcher session's last pane has not exited yet")

// spawn makes the watcher's pane: a new session the first time, else a new pane in the session.
func (w *watchSup) spawn(r rounds.Round, env []string) (session, pane string, err error) {
	cwd := r.Trigger.Watch.Cwd
	if cwd == "" {
		cwd = filepath.Join(w.s.cfg.state, "rounds", "watchers", r.ID)
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			return "", "", err
		}
		os.Chmod(cwd, 0o700)
	}
	exe := w.s.cfg.exe
	if exe == "" {
		return "", "", errors.New("gff-hab does not know its own program, which runs the watcher's wrapper")
	}
	lim := w.lim()
	argv := append([]string{exe, "watch-pane", "-round", r.ID,
		"-rate", strconv.Itoa(lim.Rate), "-burst", strconv.Itoa(lim.Burst),
		"-buffer-lines", strconv.Itoa(lim.BufferLines), "-buffer-bytes", strconv.Itoa(lim.BufferBytes), "--"},
		r.Trigger.Watch.Argv...)
	p := api.SessionCreateParams{Argv: argv, Cwd: cwd, Env: env}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ss, ok := w.s.cfg.store.SessionOfWatcher(r.ID)
	if !ok {
		ctx = withRoundStart(ctx, roundStart{ref: api.SessionRound{Round: r.ID, Watcher: true}, name: r.Name + "-watch"})
		made, err := w.s.cfg.create(ctx, p)
		if err != nil {
			return "", "", err
		}
		return made.Session, made.Pane, nil
	}
	if ss.State != api.SessionStateExited {
		if st, ok := w.s.cfg.pane(ss.Pane); ok && !st.Lost && !st.Exited {
			return "", "", fmt.Errorf("its session %s still runs pane %s", ss.Name, ss.Pane)
		}
		return "", "", errWatchRetry
	}
	pane, err = w.s.cfg.resume(ctx, ss.ID, p, cwd)
	return ss.ID, pane, err
}

// exited takes a watcher pane's exit: gff-hab's own hang-up starts it again at once (or not at
// all when the round stopped); any other exit backs off, and enough quick ones pause the round.
func (w *watchSup) exited(pane, how string) {
	w.mu.Lock()
	id, ok := w.byPane[pane]
	if !ok {
		w.mu.Unlock()
		return
	}
	delete(w.byPane, pane)
	wt := w.m[id]
	if wt == nil || wt.pane != pane {
		w.mu.Unlock()
		return
	}
	wt.pane = ""
	now := w.now()
	if wt.own {
		wt.own = false
		wt.lastExit = how + " (hung up by gff-hab)"
		w.mu.Unlock()
		w.s.publishRound(id)
		w.look(id)
		return
	}
	x := w.exitLocked(id, wt, how, now)
	w.mu.Unlock()
	w.exitDone(x)
}

// exitLocked backs off after an exit that was not gff-hab's own, or marks the round for a
// pause, in the same hold of w.mu that cleared the pane: the loop never sees a watcher with no
// pane and no backoff in between. The caller holds w.mu, then calls exitDone without it.
func (w *watchSup) exitLocked(id string, wt *watcher, how string, now time.Time) watchExit {
	wt.lastExit = how
	delay, pause := wt.back.Exit(wt.started, now, false, w.lim())
	w.stopTimerLocked(wt)
	if pause {
		wt.state, wt.reason, wt.next = watchStopped, watchReasonExits, time.Time{}
		return watchExit{id: id, how: how, pause: true, session: wt.session, exits: wt.back.Quick}
	}
	wt.state, wt.next = watchBackoff, now.Add(delay)
	wt.timer = w.after(delay, func() { w.look(id) })
	return watchExit{id: id}
}

// watchExit is what exitDone does once w.mu is let go.
type watchExit struct {
	id, how, session string
	pause            bool
	exits            int
}

func (w *watchSup) exitDone(x watchExit) {
	w.s.publishRound(x.id)
	if !x.pause {
		return
	}
	if err := w.s.sched.PauseWatcher(x.id, x.how, x.session, x.exits); err != nil && !errors.Is(err, rounds.ErrNotFound) {
		w.s.cfg.logf("rounds: round %s: pause after its watcher's exits: %v", x.id, err)
	}
}

// woke hangs up every running watcher at a wake, after its settle, and starts it again: its
// connections died in the sleep (rounds.md 4.4). The hang-up counts for nothing.
func (w *watchSup) woke() {
	w.mu.Lock()
	type hang struct{ id, pane string }
	var hs []hang
	for id, wt := range w.m {
		if wt.pane != "" && !wt.own {
			wt.own = true
			hs = append(hs, hang{id, wt.pane})
		}
	}
	w.mu.Unlock()
	for _, h := range hs {
		w.hangUp(h.id, h.pane)
	}
}

// drop counts lines dropped for a round's running watcher.
func (w *watchSup) drop(id string, n int) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	if wt := w.m[id]; wt != nil {
		wt.drops += n
	}
	w.mu.Unlock()
}

// state is a round's watcher for the API, or nil.
func (w *watchSup) state(id string) *api.RoundWatcher {
	w.mu.Lock()
	defer w.mu.Unlock()
	wt := w.m[id]
	if wt == nil {
		return nil
	}
	out := &api.RoundWatcher{State: wt.state, Session: wt.session, Restarts: wt.restarts, Drops: wt.drops,
		LastExit: wt.lastExit, Reason: wt.reason}
	if wt.state == watchRunning {
		out.StartedAt = wt.started.UTC().Format(time.RFC3339)
	}
	if (wt.state == watchBackoff || wt.state == watchStarting) && !wt.next.IsZero() {
		out.NextStart = wt.next.UTC().Format(time.RFC3339)
	}
	return out
}

// isWatcherPane reports whether pane is a running watcher's pane, and of which round.
func (w *watchSup) roundOfPane(pane string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.byPane[pane]
	return id, ok
}

// checkLost ends a running watcher whose pane the link no longer lists once it reached every
// holder: its holder died, so the host restarted (rounds.md 8.4).
func (w *watchSup) checkLost() {
	if !w.s.cfg.complete() {
		return
	}
	w.mu.Lock()
	var gone []string
	for pane := range w.byPane {
		if st, ok := w.s.cfg.pane(pane); !ok || (st.Lost && !st.Exited) {
			gone = append(gone, pane)
		}
	}
	w.mu.Unlock()
	for _, p := range gone {
		w.exited(p, reasonHostRestarted)
	}
}

func (w *watchSup) close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.mu.Lock()
	for _, wt := range w.m {
		w.stopTimerLocked(wt)
	}
	w.mu.Unlock()
}

// exitText says how a pane's program exited.
func watchExitText(e api.ExitStatus) string {
	switch {
	case e.Code != nil:
		return "exit status " + strconv.Itoa(*e.Code)
	case e.Signal != "":
		return "ended by " + e.Signal
	}
	return "pane gone"
}

// --- The watcher's environment and secret sets ---

// watchEnv is a watcher's environment beside the hab's own: the secret set, then GFF_ROUND,
// GFF_ROUND_LAST_OK and GFF_WATCH_CURSOR (rounds.md 4.4).
func (s *roundsService) watchEnv(r rounds.Round, st rounds.RoundState) ([]string, error) {
	var env []string
	if r.Secrets != "" {
		set, ok := s.cfg.settings.Secrets[r.Secrets]
		if !ok {
			return nil, fmt.Errorf("secret set %s is not in the hab's config file", r.Secrets)
		}
		vals, err := secretValues(set, s.watchLimits().SecretsTimeout)
		if err != nil {
			return nil, fmt.Errorf("secret set %s: %v", r.Secrets, err)
		}
		env = append(env, vals...)
	}
	env = append(env, "GFF_ROUND="+r.ID)
	if st.LastOK != nil {
		env = append(env, "GFF_ROUND_LAST_OK="+st.LastOK.UTC().Format(time.RFC3339))
	}
	env = append(env, "GFF_WATCH_CURSOR="+s.sched.WatchCursor(r.ID))
	return env, nil
}

// secretValues is a secret set's variables: vars, then each NAME=value line its command
// prints. The command runs with stdin /dev/null and its own process group, killed at the
// timeout. Its output and stderr are never logged; an error names only the line number or the
// exit.
func secretValues(set secretSet, timeout time.Duration) ([]string, error) {
	names := make([]string, 0, len(set.Vars))
	for k := range set.Vars {
		names = append(names, k)
	}
	slices.Sort(names)
	var env []string
	for _, k := range names {
		env = append(env, k+"="+set.Vars[k])
	}
	if len(set.Command) == 0 {
		return env, nil
	}
	if timeout <= 0 {
		timeout = rounds.DefaultWatchLimits().SecretsTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, set.Command[0], set.Command[1:]...)
	cmd.Stdin = nil // /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: 1 << 20}
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("its command ran past %s", timeout)
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("its command failed: %s", ee.ProcessState.String())
		}
		return nil, fmt.Errorf("its command did not run: %v", errors.Unwrap(err))
	}
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envNameRE.MatchString(k) || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("its command's output line %d is not NAME=value", n)
		}
		env = append(env, k+"="+v)
	}
	if sc.Err() != nil {
		return nil, errors.New("its command's output has a line over 1 MiB")
	}
	return env, nil
}

// limitWriter keeps the first n bytes written to it and drops the rest.
type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// --- The round side: the cap, the API and the pane route ---

// watchLimits are the hab's watcher limits.
func (s *roundsService) watchLimits() rounds.WatchLimits {
	if s.cfg.settings.Watch == (rounds.WatchLimits{}) {
		return rounds.DefaultWatchLimits()
	}
	return s.cfg.settings.Watch
}

// checkWatchRound refuses a watch round past max_watchers (conflict) and a secret set the config
// lacks (invalid). except is the round being edited or unpaused; it does not count itself.
func (s *roundsService) checkWatchRound(r rounds.Round, except string) error {
	if r.Trigger.Kind != rounds.TriggerWatch {
		return nil
	}
	if r.Secrets != "" {
		if _, ok := s.cfg.settings.Secrets[r.Secrets]; !ok {
			return control.Errorf(api.ErrorKindInvalid, "secrets: the hab's config file has no secret set %s ([rounds.secrets.%s])", r.Secrets, r.Secrets)
		}
	}
	if r.Paused {
		return nil
	}
	n := 0
	for _, e := range s.store.List() {
		if e.Round.ID != except && e.Round.Trigger.Kind == rounds.TriggerWatch && !e.Round.Paused && !e.State.Completed {
			n++
		}
	}
	if max := s.watchLimits().MaxWatchers; n >= max {
		return control.Errorf(api.ErrorKindConflict, "this hab runs its limit of %d watch rounds (max_watchers in [rounds])", max)
	}
	return nil
}

// publishRound sends round.updated for a round whose watcher changed state, with no new
// definition.
func (s *roundsService) publishRound(id string) {
	if e, ok := s.store.Get(id); ok {
		s.publishChanged(e)
	}
}

// WatchEvent answers round.watch_event from a watcher's pane (rounds.md 4.4).
func (s *roundsService) WatchEvent(p api.RoundWatchEventParams) (api.RoundWatchEventResult, error) {
	lines := make([][]byte, len(p.Lines))
	for i, l := range p.Lines {
		lines[i] = []byte(l)
	}
	res, err := s.sched.WatchLines(p.Round, lines)
	switch {
	case errors.Is(err, rounds.ErrNotFound):
		return api.RoundWatchEventResult{}, control.Errorf(api.ErrorKindNotFound, "round %s is gone", p.Round)
	case errors.Is(err, rounds.ErrNotWatch):
		return api.RoundWatchEventResult{}, control.Errorf(api.ErrorKindInvalid, "round %s has no watch trigger", p.Round)
	case err != nil:
		return api.RoundWatchEventResult{}, control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	out := api.RoundWatchEventResult{Lines: make([]api.RoundWatchLine, len(res))}
	dropped := p.Dropped
	for i, r := range res {
		out.Lines[i] = api.RoundWatchLine{Taken: r.Taken, Cursor: r.Cursor, Title: r.Title, Drop: r.Drop, Quiet: r.Quiet}
		if r.Drop != "" && !r.Quiet {
			dropped++
		}
	}
	if s.watchers != nil {
		s.watchers.drop(p.Round, dropped)
	}
	return out, nil
}
