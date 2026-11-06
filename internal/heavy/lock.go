// Package heavy is the host's heavy-run lock and its disk watch (docs/specs/groups.md 8). Build
// and test runs take the lock through `gff heavy -- <command>`: first come, first served,
// heavy.slots at once. The lock lives in memory; a run's ticket is held by the control
// connection that last took it, and goes when that connection drops or the pane it named
// closes. After a gff-hab restart a CLI comes back with its ticket and keeps its place. The disk
// watch reads the free space of the state and worktree volumes; below heavy.disk_min_free the
// lock grants nothing until free space is back above it by the resume margin. Pure Go.
package heavy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hoststats"
)

// Capability is the capability string of the heavy-run lock.
const Capability = "heavy"

// The defaults of groups.md 15.3.
const (
	GiB                 = uint64(1) << 30
	DefaultSlots        = 1
	DefaultMinFree      = 10 * GiB
	DefaultLongHold     = 60 * time.Minute
	DefaultRestartHold  = 10 * time.Second
	DefaultResumeMargin = 2 * GiB
	// DefaultDiskEvery is how often the disk watch reads the volumes.
	DefaultDiskEvery = 30 * time.Second
	// DefaultEvery is how often the lock checks its timers: the start hold, long holds and the
	// disk watch.
	DefaultEvery = time.Second
	// publishStep is how far a volume's free space moves before the catalog section changes,
	// so the catalog's generation does not move with every write on the disk.
	publishStep = GiB
	// maxCommand bounds the command text kept for heavy.status.
	maxCommand = 200
	// MaxWait bounds one heavy.take's wait.
	MaxWait = 600 * time.Second
)

// Config is what New needs. Every field is optional.
type Config struct {
	Host string // this hab's host id, for messages
	// Slots, MinFree and LongHold read the hab-side settings heavy.slots, heavy.disk_min_free
	// and heavy.long_hold at each use, so a change applies at once. Nil: the defaults.
	Slots    func() int
	MinFree  func() uint64
	LongHold func() time.Duration
	// RestartHold is [heavy] restart_hold: after New, nothing is granted for this long, so runs
	// that held or waited before a gff-hab restart come back first. 0: none.
	RestartHold time.Duration
	// ResumeMargin is [heavy] disk_resume_margin: once low, grants start again when every
	// volume has MinFree plus this much free.
	ResumeMargin uint64
	// Volumes are the folders whose volumes the disk watch reads: the state folder and the
	// worktree root.
	Volumes []string
	// Free reads one volume. Default hoststats.Volume.
	Free      func(path string) (free, total uint64, ok bool)
	DiskEvery time.Duration // default DefaultDiskEvery
	Every     time.Duration // default DefaultEvery; negative: no loop (tests call Check)
	// PaneOf resolves a pane token from heavy.take to its pane id and the pane's session name.
	// Nil: a token is refused.
	PaneOf func(token string) (pane, session string, ok bool)
	// Coordinators returns the coordinators to tell of a low-disk episode: the sessions on this
	// hab that coordinate a group with a member on this host (groups.md 8.2). Groups come in a
	// later stage; nil or none: one notice instead.
	Coordinators func() []string
	// Message sends Flightdeck's own message to a session on this hab.
	Message func(session, text string) error
	// Notice raises a Flightdeck notice. Nil: none.
	Notice func(title, body string)
	// Changed hears that the catalog section changed. It runs outside the lock. Nil: none.
	Changed func()
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

// run is one ticket: a run that holds or waits.
type run struct {
	ticket  string
	seq     uint64 // order among runs with one since
	holding bool
	since   time.Time
	granted time.Time
	pane    string
	session string
	command string
	owner   uint64 // the connection that holds it; 0: none
	stop    func() bool
	noticed bool // the long-hold notice went out for this hold
}

// Lock is the host's heavy-run lock and disk watch.
type Lock struct {
	cfg Config

	mu        sync.Mutex
	runs      map[string]*run
	seq       uint64
	owners    uint64
	holdUntil time.Time // zero once the start hold ended
	low       bool
	volumes   []api.HeavyVolume // the last reading
	published []api.HeavyVolume // what the catalog section shows
	lastDisk  time.Time
	section   api.CatalogHeavy // the last section Changed was told of
	wake      chan struct{}    // closed and replaced on every change

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// New starts the lock. Close stops its loop.
func New(cfg Config) *Lock {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Free == nil {
		cfg.Free = hoststats.Volume
	}
	if cfg.DiskEvery <= 0 {
		cfg.DiskEvery = DefaultDiskEvery
	}
	if cfg.Every == 0 {
		cfg.Every = DefaultEvery
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	l := &Lock{cfg: cfg, runs: map[string]*run{}, wake: make(chan struct{}), done: make(chan struct{})}
	if cfg.RestartHold > 0 {
		l.holdUntil = cfg.Now().Add(cfg.RestartHold)
	}
	l.Check()
	if cfg.Every > 0 {
		l.wg.Add(1)
		go l.loop()
	}
	return l
}

// Close stops the loop. Waiting calls end with their state as it is.
func (l *Lock) Close() {
	l.once.Do(func() {
		close(l.done)
		l.wg.Wait()
	})
}

func (l *Lock) loop() {
	defer l.wg.Done()
	t := time.NewTicker(l.cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			l.Check()
		}
	}
}

func (l *Lock) slots() int {
	if l.cfg.Slots != nil {
		if n := l.cfg.Slots(); n > 0 {
			return n
		}
	}
	return DefaultSlots
}

func (l *Lock) minFree() uint64 {
	if l.cfg.MinFree != nil {
		return l.cfg.MinFree()
	}
	return DefaultMinFree
}

func (l *Lock) longHold() time.Duration {
	if l.cfg.LongHold != nil {
		if d := l.cfg.LongHold(); d > 0 {
			return d
		}
	}
	return DefaultLongHold
}

func newTicket() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validTicket(t string) bool {
	if len(t) != 32 {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func cut(s string) string {
	if len(s) <= maxCommand {
		return s
	}
	s = s[:maxCommand]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Take asks for the lock, or takes over a ticket for the connection whose context is owner:
// when owner ends, the ticket is released unless another call took it over meanwhile. A ticket
// the lock does not know is one from before a gff-hab restart: it holds again at once when
// holding is set, else it waits in the place its since gives it. Take answers at once.
func (l *Lock) Take(owner context.Context, p api.HeavyTakeParams) (api.HeavyTaken, error) {
	if p.Ticket != "" && !validTicket(p.Ticket) {
		return api.HeavyTaken{}, control.Errorf(api.ErrorKindInvalid, "ticket %q is not one this hab gives: 32 hex digits", p.Ticket)
	}
	if p.WaitMS < 0 || time.Duration(p.WaitMS)*time.Millisecond > MaxWait {
		return api.HeavyTaken{}, control.Errorf(api.ErrorKindInvalid, "wait_ms must be from 0 to %d", MaxWait.Milliseconds())
	}
	var pane, session string
	if p.PaneToken != "" {
		ok := false
		if l.cfg.PaneOf != nil {
			pane, session, ok = l.cfg.PaneOf(p.PaneToken)
		}
		if !ok {
			return api.HeavyTaken{}, control.Errorf(api.ErrorKindNotFound, "pane_token names no pane on this hab")
		}
	}
	now := l.cfg.Now()
	l.mu.Lock()
	r := l.runs[p.Ticket]
	if r == nil {
		r = &run{ticket: p.Ticket, since: now}
		if r.ticket == "" {
			r.ticket = newTicket()
		} else {
			// A ticket from before a restart: its old place.
			if t, err := time.Parse(time.RFC3339Nano, p.Since); err == nil && t.Before(now) {
				r.since = t
			}
			if p.Holding {
				r.holding, r.granted = true, now
				if t, err := time.Parse(time.RFC3339Nano, p.Granted); err == nil && !t.After(now) {
					r.granted = t
				}
			}
		}
		l.seq++
		r.seq = l.seq
		l.runs[r.ticket] = r
	}
	if pane != "" {
		r.pane, r.session = pane, session
	}
	if p.Command != "" {
		r.command = cut(p.Command)
	}
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	r.owner = 0
	if owner != nil {
		l.owners++
		id, ticket := l.owners, r.ticket
		r.owner = id
		r.stop = context.AfterFunc(owner, func() { l.drop(ticket, id) })
	}
	l.grantLocked(now)
	res := l.answerLocked(r, now)
	l.mu.Unlock()
	l.changed()
	return res, nil
}

// Wait waits until the ticket holds the lock or ctx ends, and answers its state then. An
// unknown ticket is not_found.
func (l *Lock) Wait(ctx context.Context, ticket string) (api.HeavyTaken, error) {
	for {
		l.mu.Lock()
		r := l.runs[ticket]
		if r == nil {
			l.mu.Unlock()
			return api.HeavyTaken{}, control.Errorf(api.ErrorKindNotFound, "no heavy-run ticket %s on this hab: it was released", ticket)
		}
		if r.holding {
			res := l.answerLocked(r, l.cfg.Now())
			l.mu.Unlock()
			return res, nil
		}
		wake := l.wake
		l.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			l.mu.Lock()
			defer l.mu.Unlock()
			if r := l.runs[ticket]; r != nil {
				return l.answerLocked(r, l.cfg.Now()), nil
			}
			return api.HeavyTaken{}, control.Errorf(api.ErrorKindNotFound, "no heavy-run ticket %s on this hab: it was released", ticket)
		case <-l.done:
			return api.HeavyTaken{}, control.Errorf(api.ErrorKindUnavailable, "the heavy-run lock is stopping")
		}
	}
}

// Release gives back a ticket. An unknown one is not an error.
func (l *Lock) Release(p api.HeavyReleaseParams) error {
	l.mu.Lock()
	l.removeLocked(p.Ticket)
	l.mu.Unlock()
	l.changed()
	return nil
}

// ReleasePane gives back every ticket taken from the pane: the pane closed or its program
// exited (groups.md 8.1).
func (l *Lock) ReleasePane(pane string) {
	if pane == "" {
		return
	}
	l.mu.Lock()
	for t, r := range l.runs {
		if r.pane == pane {
			l.removeLocked(t)
		}
	}
	l.mu.Unlock()
	l.changed()
}

// drop releases a ticket when the connection that held it ended, unless another took it over.
func (l *Lock) drop(ticket string, owner uint64) {
	l.mu.Lock()
	if r := l.runs[ticket]; r != nil && r.owner == owner {
		l.removeLocked(ticket)
	}
	l.mu.Unlock()
	l.changed()
}

func (l *Lock) removeLocked(ticket string) {
	r := l.runs[ticket]
	if r == nil {
		return
	}
	if r.stop != nil {
		r.stop()
	}
	delete(l.runs, ticket)
	l.grantLocked(l.cfg.Now())
	l.broadcastLocked()
}

func (l *Lock) broadcastLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}

// orderLocked is the holders in grant order and the waiters in the order they will be granted.
func (l *Lock) orderLocked() (holders, waiters []*run) {
	for _, r := range l.runs {
		if r.holding {
			holders = append(holders, r)
		} else {
			waiters = append(waiters, r)
		}
	}
	slices.SortFunc(holders, func(a, b *run) int {
		if c := a.granted.Compare(b.granted); c != 0 {
			return c
		}
		return cmpRun(a, b)
	})
	slices.SortFunc(waiters, cmpRun)
	return holders, waiters
}

func cmpRun(a, b *run) int {
	if c := a.since.Compare(b.since); c != 0 {
		return c
	}
	switch {
	case a.seq < b.seq:
		return -1
	case a.seq > b.seq:
		return 1
	}
	return 0
}

// grantLocked grants free slots to the first waiters, unless the start hold or a low disk stops
// every grant.
func (l *Lock) grantLocked(now time.Time) {
	if l.blockedLocked(now) != "" {
		return
	}
	holders, waiters := l.orderLocked()
	free := l.slots() - len(holders)
	changed := false
	for _, r := range waiters {
		if free <= 0 {
			break
		}
		r.holding, r.granted, r.noticed = true, now, false
		free--
		changed = true
	}
	if changed {
		l.broadcastLocked()
	}
}

// blockedLocked says why nothing is granted now, or "".
func (l *Lock) blockedLocked(now time.Time) string {
	if l.low {
		return fmt.Sprintf("disk low: less than %s free", gib(l.minFree()))
	}
	if !l.holdUntil.IsZero() && now.Before(l.holdUntil) {
		return "gff-hab started; earlier runs come back first until " + stamp(l.holdUntil)
	}
	return ""
}

func (l *Lock) answerLocked(r *run, now time.Time) api.HeavyTaken {
	res := api.HeavyTaken{Ticket: r.ticket, Since: stamp(r.since)}
	if r.holding {
		res.State, res.Granted = "holding", stamp(r.granted)
		return res
	}
	res.State = "waiting"
	holders, waiters := l.orderLocked()
	res.Ahead = len(holders)
	for _, w := range waiters {
		if w == r {
			break
		}
		res.Ahead++
	}
	res.Reason = l.blockedLocked(now)
	return res
}

// Status answers heavy.status.
func (l *Lock) Status() api.HeavyStatus {
	now := l.cfg.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	holders, waiters := l.orderLocked()
	out := api.HeavyStatus{Slots: l.slots(), DiskLow: l.low, MinFreeBytes: l.minFree(),
		Volumes: slices.Clone(l.volumes), Holders: []api.HeavyRun{}, Waiters: []api.HeavyRun{}}
	if !l.holdUntil.IsZero() && now.Before(l.holdUntil) {
		out.HoldUntil = stamp(l.holdUntil)
	}
	for _, r := range holders {
		out.Holders = append(out.Holders, l.runLocked(r))
	}
	for _, r := range waiters {
		out.Waiters = append(out.Waiters, l.runLocked(r))
	}
	return api.NonNil(out)
}

func (l *Lock) runLocked(r *run) api.HeavyRun {
	h := api.HeavyRun{Ticket: r.ticket, State: "waiting", Command: r.command, Session: r.session, Pane: r.pane, Since: stamp(r.since)}
	if r.holding {
		h.State, h.Granted = "holding", stamp(r.granted)
	}
	return h
}

// Section is the catalog's heavy section (groups.md 8.2).
func (l *Lock) Section() *api.CatalogHeavy {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sectionLocked()
	return &s
}

func (l *Lock) sectionLocked() api.CatalogHeavy {
	s := api.CatalogHeavy{Slots: l.slots(), DiskLow: l.low, Volumes: slices.Clone(l.published)}
	for _, r := range l.runs {
		if r.holding {
			s.Holding++
		} else {
			s.Waiting++
		}
	}
	return api.NonNil(s)
}

// changed tells Changed when the catalog section differs from the last one it heard of.
func (l *Lock) changed() {
	if l.cfg.Changed == nil {
		return
	}
	l.mu.Lock()
	s := l.sectionLocked()
	same := s.Slots == l.section.Slots && s.Holding == l.section.Holding && s.Waiting == l.section.Waiting &&
		s.DiskLow == l.section.DiskLow && slices.Equal(s.Volumes, l.section.Volumes)
	l.section = s
	l.mu.Unlock()
	if !same {
		l.cfg.Changed()
	}
}

// Check runs the timers once: the end of the start hold, the disk watch when a reading is due,
// long holds, and grants a changed heavy.slots allows. The loop runs it every Every.
func (l *Lock) Check() {
	now := l.cfg.Now()
	l.mu.Lock()
	reason := l.blockedLocked(now)
	if !l.holdUntil.IsZero() && !now.Before(l.holdUntil) {
		l.holdUntil = time.Time{}
	}
	due := l.lastDisk.IsZero() || now.Sub(l.lastDisk) >= l.cfg.DiskEvery
	l.mu.Unlock()
	var episode string
	if due {
		episode = l.readDisk(now)
	}
	l.mu.Lock()
	var longs []*run
	limit := l.longHold()
	for _, r := range l.runs {
		if r.holding && !r.noticed && now.Sub(r.granted) >= limit {
			r.noticed = true
			c := *r
			longs = append(longs, &c)
		}
	}
	before := l.wake
	l.grantLocked(now)
	if l.wake == before && l.blockedLocked(now) != reason {
		// The hold ended, or the disk went low or came back: waiters read their reason again.
		l.broadcastLocked()
	}
	l.mu.Unlock()
	if episode != "" {
		l.tellLowDisk(episode)
	}
	slices.SortFunc(longs, func(a, b *run) int { return a.granted.Compare(b.granted) })
	for _, r := range longs {
		l.noticeLongHold(r, limit, now)
	}
	l.changed()
}

// readDisk reads every volume, moves the low state with its hysteresis, and returns the text of
// a new low-disk episode, or "".
func (l *Lock) readDisk(now time.Time) string {
	var vols []api.HeavyVolume
	for _, p := range l.cfg.Volumes {
		free, total, ok := l.cfg.Free(p)
		if !ok {
			continue
		}
		vols = append(vols, api.HeavyVolume{Path: p, FreeBytes: free, TotalBytes: total})
	}
	minFree := l.minFree()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastDisk = now
	l.volumes = vols
	wasLow := l.low
	var lowest *api.HeavyVolume
	for i := range vols {
		if lowest == nil || vols[i].FreeBytes < lowest.FreeBytes {
			lowest = &vols[i]
		}
	}
	switch {
	case lowest == nil:
		// No reading: keep the state.
	case !l.low && lowest.FreeBytes < minFree:
		l.low = true
	case l.low && lowest.FreeBytes >= minFree+l.cfg.ResumeMargin:
		l.low = false
		l.cfg.Logf("heavy: free space is back above %s; heavy runs are granted again", gib(minFree+l.cfg.ResumeMargin))
	}
	if l.publishLocked(vols) || wasLow != l.low {
		l.published = slices.Clone(vols)
	}
	if l.low && !wasLow {
		l.cfg.Logf("heavy: %s free on %s, below %s; heavy runs wait", gib(lowest.FreeBytes), lowest.Path, gib(minFree))
		return fmt.Sprintf("Disk low on %s: %s free on the volume of %s, below the %s threshold (heavy.disk_min_free). Heavy runs on this host wait until %s is free; runs that hold the lock go on.",
			l.hostName(), gib(lowest.FreeBytes), lowest.Path, gib(minFree), gib(minFree+l.cfg.ResumeMargin))
	}
	return ""
}

// publishLocked says whether a reading differs enough from the published one to change the
// catalog section: another set of volumes, or free space that moved by publishStep or more.
func (l *Lock) publishLocked(vols []api.HeavyVolume) bool {
	if len(vols) != len(l.published) {
		return true
	}
	for i, v := range vols {
		p := l.published[i]
		if v.Path != p.Path || v.TotalBytes != p.TotalBytes {
			return true
		}
		d := v.FreeBytes - p.FreeBytes
		if p.FreeBytes > v.FreeBytes {
			d = p.FreeBytes - v.FreeBytes
		}
		if d >= publishStep {
			return true
		}
	}
	return false
}

func (l *Lock) hostName() string {
	if l.cfg.Host != "" {
		return l.cfg.Host
	}
	return "this host"
}

// tellLowDisk sends the episode's one message to each coordinator, or raises one notice when
// there is none or none could be told (groups.md 8.2).
func (l *Lock) tellLowDisk(text string) {
	told := 0
	if l.cfg.Coordinators != nil && l.cfg.Message != nil {
		for _, s := range l.cfg.Coordinators() {
			if err := l.cfg.Message(s, text); err != nil {
				l.cfg.Logf("heavy: low-disk message to %s: %v", s, err)
				continue
			}
			told++
		}
	}
	if told == 0 && l.cfg.Notice != nil {
		l.cfg.Notice("Disk low on "+l.hostName(), text)
	}
}

func (l *Lock) noticeLongHold(r *run, limit time.Duration, now time.Time) {
	what := "A heavy run"
	if r.command != "" {
		what = fmt.Sprintf("The heavy run %q", r.command)
	}
	if r.session != "" {
		what += " of session " + r.session
	}
	body := fmt.Sprintf("%s has held the heavy-run lock on %s for %s (heavy.long_hold is %s). Runs behind it wait; Flightdeck does not stop it.",
		what, l.hostName(), now.Sub(r.granted).Round(time.Minute), limit)
	l.cfg.Logf("heavy: %s", body)
	if l.cfg.Notice != nil {
		l.cfg.Notice("Long heavy run on "+l.hostName(), body)
	}
}

// gib prints bytes in GiB with one decimal.
func gib(b uint64) string {
	s := fmt.Sprintf("%.1f", float64(b)/float64(GiB))
	return strings.TrimSuffix(s, ".0") + " GiB"
}
