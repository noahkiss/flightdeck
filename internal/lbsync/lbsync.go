// Package lbsync syncs a hab's Logbook replica over its peer links (logbook.md 7). The hab that
// holds a link drives both directions with logbook.Store.Sync: it pulls the ops it lacks page by
// page, then pushes what the peer lacks. It runs an exchange on every link it dialed when the
// link comes up, every poll period, at once when its own vector grows, and when the peer sends
// mesh.changed with the logbook reason. It serves logbook.pull and logbook.push to the links
// other habs dialed to it, and wakes those habs with mesh.changed when ops wait for them. Ops of
// every origin travel, so habs that never link converge through any hab both reach (7.2). Logbook
// puts nothing in the mesh outbox: the op log and the version vectors are the outbox (7.3).
package lbsync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// Capability is the capability word of a hab with a Logbook replica that syncs.
const Capability = "logbook"

const (
	maxBackoff = time.Minute
	// minPageBytes is the smallest page a slow link shrinks to; one larger op still travels.
	minPageBytes = 16 << 10
)

// Link is one peer link this hab dialed.
type Link struct {
	Label    string
	DeviceID string
	Online   bool
	Caps     []string
	Caller   api.Caller // nil when the link is down
}

// Peers lists the links this hab dialed. gff-hab gives internal/peers.
type Peers interface {
	Links() []Link
}

// Config is what New needs. Self and Store are required.
type Config struct {
	Self  string         // this hab's device id
	Store *logbook.Store // this hab's replica
	// Path is the store's file. A pinned SQLite connection checks data_version every Watch, so
	// ops another process wrote (the CLI while gff-hab was busy, logbook.md 4.3) go out too.
	// Empty: only Changed and peers' ops start a send.
	Path  string
	Peers Peers // nil: no links this hab dialed
	// Wake sends mesh.changed with the logbook reason to every peer link the hab with this
	// device id dialed to this one, and reports whether any link took it (logbook.md 7.1).
	Wake  func(device string) bool
	Poll  time.Duration // the exchange round over every link; default 60 s (logbook.md 7.1)
	Watch time.Duration // the file check; default 500 ms
	// PageTime is how long one pull or push call may take before the exchange gives up on it;
	// default 60 s. A call slower than a quarter of it halves the link's page size, one faster
	// than a sixteenth doubles it, up to logbook.PageBytes (logbook.md 7.6, Slow links).
	PageTime time.Duration
	// MinBackoff is the wait after a first failed exchange, doubled per failure in a row up to
	// 60 s; default 1 s.
	MinBackoff time.Duration
	// StartHold is how long writes to the file wait at start, while the first exchange runs,
	// when the peer list is not empty; default 10 s. A completed exchange ends it sooner.
	StartHold time.Duration
	Now       func() time.Time
	Logf      func(string, ...any)

	// repairGrace, when set, replaces logbook.OrphanGrace in each round (tests).
	repairGrace *time.Duration
	// checked, when set, runs after each look of the change loop (tests).
	checked func()
}

// Service syncs one replica.
type Service struct {
	cfg Config

	mu      sync.Mutex
	last    logbook.Vector            // the vector the change loop last acted on
	have    map[string]logbook.Vector // by device id: the vector of its last pull here
	woken   map[string]bool           // devices told since their last pull
	fails   map[string]*failure       // by link label
	online  map[string]bool           // by link label
	running map[string]bool           // links with an exchange in flight
	again   map[string]bool           // links whose exchange runs once more when it ends
	stats   map[string]Stats          // by link label
	wanted  map[string]bool           // links whose peer sent mesh.changed with logbook
	nudge   map[string]chan struct{}  // by label: ends a backoff wait when a wake-up comes
	page    map[string]int            // by label: the page size in bytes the link asks for
	late    map[string]chan struct{}  // by label: closed when a call given up on has its answer
	held    bool                      // the start hold is on

	kick, changed, wake chan struct{}
	done                chan struct{}
	ctx                 context.Context
	stop                context.CancelFunc
	wg                  sync.WaitGroup
}

type failure struct {
	n    int
	next time.Time
}

// Stats is what the last exchange over one link moved, for logs and tests.
type Stats struct {
	At     time.Time
	Pulled int // ops stored from the peer
	Pushed int // ops sent to the peer
	Err    string
}

// New starts the rounds. The first round runs at once.
func New(cfg Config) (*Service, error) {
	if cfg.Self == "" || cfg.Store == nil {
		return nil, errors.New("lbsync: Self and Store are required")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 60 * time.Second
	}
	if cfg.Watch <= 0 {
		cfg.Watch = 500 * time.Millisecond
	}
	if cfg.PageTime <= 0 {
		cfg.PageTime = 60 * time.Second
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.StartHold <= 0 {
		cfg.StartHold = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	v, err := cfg.Store.Vector(context.Background())
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, last: v,
		have: map[string]logbook.Vector{}, woken: map[string]bool{}, fails: map[string]*failure{},
		online: map[string]bool{}, running: map[string]bool{}, again: map[string]bool{}, stats: map[string]Stats{}, wanted: map[string]bool{},
		nudge: map[string]chan struct{}{}, page: map[string]int{}, late: map[string]chan struct{}{},
		kick: make(chan struct{}, 1), changed: make(chan struct{}, 1), wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.ctx, s.stop = context.WithCancel(context.Background())
	// The start hold (logbook.md 7.6, Restored file): with peers listed, writes wait until the
	// first exchange ends or the hold runs out, so a file restored from an older copy gets its
	// own ops back before it writes. With none but this hab's own entry, a mesh of one, nothing
	// waits.
	if cfg.Peers != nil && slices.ContainsFunc(cfg.Peers.Links(), func(l Link) bool { return l.DeviceID != cfg.Self }) {
		if err := cfg.Store.HoldWrites(context.Background(), cfg.Now().Add(cfg.StartHold)); err != nil {
			return nil, err
		}
		s.held = true
	}
	s.wg.Add(3)
	go s.roundLoop()
	go s.changeLoop()
	go s.wakeLoop()
	return s, nil
}

// Close stops the rounds and waits for exchanges in flight to end.
func (s *Service) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	s.stop()
	s.wg.Wait()
	s.endHold()
}

// endHold ends the start hold once.
func (s *Service) endHold() {
	s.mu.Lock()
	held := s.held
	s.held = false
	s.mu.Unlock()
	if held {
		if err := s.cfg.Store.HoldWrites(context.Background(), time.Time{}); err != nil {
			s.cfg.Logf("logbook: ending the start hold: %v", err)
		}
	}
}

// Kick runs a round now: a peer's row changed, so a link may have come up.
func (s *Service) Kick() { signal(s.kick) }

// Changed tells the service the store gained ops: a write through gff-hab. Peers that lack them
// get them now.
func (s *Service) Changed() { signal(s.changed) }

// Woken takes mesh.changed with the logbook reason from the link labelled label: ops wait there,
// so this hab exchanges over it now, past any backoff. It never blocks: the peer manager calls it
// from its update loop, and the exchange starts from the wake loop.
func (s *Service) Woken(label string) {
	s.mu.Lock()
	s.wanted[label] = true
	s.mu.Unlock()
	signal(s.wake)
}

// wakeLoop starts an exchange over each link whose peer said ops wait.
func (s *Service) wakeLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		s.mu.Lock()
		wanted := maps.Clone(s.wanted)
		clear(s.wanted)
		s.mu.Unlock()
		if s.cfg.Peers == nil {
			continue
		}
		for _, l := range s.cfg.Peers.Links() {
			if wanted[l.Label] && s.up(l) {
				s.start(l, true)
			}
		}
	}
}

// LinkStats returns what the last exchange over each link moved, by label.
func (s *Service) LinkStats() map[string]Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.stats)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// --- Serving the links other habs dialed ---

// Pull answers logbook.pull: one page of the ops the caller lacks.
func (s *Service) Pull(ctx context.Context, from api.PeerHello, p api.LogbookPullParams) (api.LogbookPullResult, error) {
	if err := s.checkLink(from); err != nil {
		return api.LogbookPullResult{}, err
	}
	have := fromSeqs(p.Have)
	a, err := s.cfg.Store.ServePull(ctx, have, p.Skip, logbook.Limits{Ops: p.MaxOps, Bytes: p.MaxBytes})
	if err != nil {
		return api.LogbookPullResult{}, err
	}
	s.mu.Lock()
	// The caller pulls: ops this hab gains from now on wake it again. It holds the page served
	// too, once the answer lands, so those never count as ops it lacks.
	s.have[from.DeviceID] = merged(have, a.Served)
	delete(s.woken, from.DeviceID)
	s.mu.Unlock()
	return api.LogbookPullResult{Ops: a.Ops, Vector: toSeqs(a.Vector), More: a.More, Skip: a.Skip}, nil
}

// Push answers logbook.push: it applies one page and answers with this hab's vector.
func (s *Service) Push(ctx context.Context, from api.PeerHello, p api.LogbookPushParams) (api.LogbookPushResult, error) {
	if err := s.checkLink(from); err != nil {
		return api.LogbookPushResult{}, err
	}
	if !logbook.OnePage(p.Ops) {
		return api.LogbookPushResult{}, control.Errorf(api.ErrorKindInvalid, "%d ops; at most %d ops or %d bytes in one push", len(p.Ops), logbook.PageOps, logbook.PageBytes)
	}
	// The pusher holds what it pushes: those ops never wake it (an echo of its own push). Its
	// have counts them before they are stored, so the change loop never sees them stored while
	// its have lacks them. Ops refused or failed here are still the pusher's own.
	s.mu.Lock()
	if h, ok := s.have[from.DeviceID]; ok {
		s.have[from.DeviceID] = merged(h, logbook.PageVector(p.Ops))
	}
	s.mu.Unlock()
	a, res, err := s.cfg.Store.ServePush(ctx, p.Ops)
	if err != nil {
		return api.LogbookPushResult{}, err
	}
	if res.Stored > 0 {
		s.Changed() // other links may lack them (7.2)
	}
	return api.LogbookPushResult{Vector: toSeqs(a.Vector), Skip: a.Skip}, nil
}

func (s *Service) checkLink(from api.PeerHello) error {
	switch {
	case from.DeviceID == "":
		return control.Errorf(api.ErrorKindInvalid, "Logbook sync needs a peer link: client.kind hab with the hello's peer field")
	case from.DeviceID == s.cfg.Self:
		return control.Errorf(api.ErrorKindInvalid, "the peer link claims this hab's own device id")
	}
	return nil
}

// --- Driving the links this hab dialed ---

// roundLoop exchanges over every link every poll period and at every kick.
func (s *Service) roundLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.Poll)
	defer t.Stop()
	for {
		s.round()
		select {
		case <-s.done:
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// round starts an exchange over each online link with the logbook capability that is not
// waiting out a backoff. A link that comes back online is tried at once.
func (s *Service) round() {
	// An orphan redaction past its grace gets the hab's forget, also on a hab that only answers
	// links (logbook.md 7.6, Redacted ops).
	grace := logbook.OrphanGrace
	if s.cfg.repairGrace != nil {
		grace = *s.cfg.repairGrace
	}
	if err := s.cfg.Store.RepairOrphans(s.ctx, grace); err != nil {
		s.cfg.Logf("logbook: repairing missing forgets: %v", err)
	}
	if s.cfg.Peers == nil {
		return
	}
	now := s.cfg.Now()
	for _, l := range s.cfg.Peers.Links() {
		up := s.up(l)
		s.mu.Lock()
		if up && !s.online[l.Label] {
			delete(s.fails, l.Label)
		}
		s.online[l.Label] = up
		f := s.fails[l.Label]
		backoff := f != nil && now.Before(f.next)
		s.mu.Unlock()
		if up && !backoff {
			s.start(l, false)
		}
	}
}

// up reports whether l is a link this hab can sync over.
func (s *Service) up(l Link) bool {
	return l.Online && l.Caller != nil && l.DeviceID != "" && l.DeviceID != s.cfg.Self && slices.Contains(l.Caps, Capability)
}

// start runs one exchange over l in the background. When one is in flight already, that one
// runs once more after it ends instead, so a link never has two exchanges in flight. urgent (a
// wake-up) runs that next exchange past any backoff, and ends a backoff wait at once.
func (s *Service) start(l Link, urgent bool) {
	s.mu.Lock()
	if s.running[l.Label] {
		s.again[l.Label] = true
		if urgent {
			signal(s.nudge[l.Label])
		}
		s.mu.Unlock()
		return
	}
	s.running[l.Label] = true
	nudge := make(chan struct{}, 1)
	s.nudge[l.Label] = nudge
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			// A wake-up that came before this exchange is served by it.
			select {
			case <-nudge:
			default:
			}
			ok := s.exchange(l)
			s.mu.Lock()
			again := s.again[l.Label] && s.ctx.Err() == nil
			delete(s.again, l.Label)
			if !again {
				delete(s.running, l.Label)
				delete(s.nudge, l.Label)
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			if !ok {
				// Another was asked for while this one failed: run it when the backoff ends, or
				// at once when a wake-up came during the exchange or comes during the wait.
				if !s.sleepBackoff(l.Label, nudge) {
					s.mu.Lock()
					delete(s.running, l.Label)
					delete(s.nudge, l.Label)
					s.mu.Unlock()
					return
				}
			}
		}
	}()
}

// sleepBackoff waits out the link's backoff, or until nudge; false when the service closed.
func (s *Service) sleepBackoff(label string, nudge chan struct{}) bool {
	s.mu.Lock()
	var d time.Duration
	if f := s.fails[label]; f != nil {
		d = f.next.Sub(s.cfg.Now())
	}
	s.mu.Unlock()
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-s.done:
		return false
	case <-nudge:
		return true
	case <-t.C:
		return true
	}
}

// exchange runs Store.Sync over the link. It reports whether it ran to the end.
func (s *Service) exchange(l Link) bool {
	res, err := s.cfg.Store.Sync(s.ctx, &linkRemote{s: s, label: l.Label, c: api.Client{Caller: l.Caller}})
	st := Stats{At: s.cfg.Now(), Pulled: res.Pulled.Stored, Pushed: res.Pushed}
	if err != nil {
		st.Err = err.Error()
	}
	s.mu.Lock()
	s.stats[l.Label] = st
	if err != nil {
		if s.ctx.Err() == nil {
			f := s.fails[l.Label]
			if f == nil {
				f = &failure{}
				s.fails[l.Label] = f
				s.cfg.Logf("logbook: sync with %s: %v", l.Label, err)
			}
			f.n++
			f.next = s.cfg.Now().Add(min(s.cfg.MinBackoff<<min(f.n-1, 16), maxBackoff))
		}
		s.mu.Unlock()
		return false
	}
	if s.fails[l.Label] != nil {
		s.cfg.Logf("logbook: %s answers again", l.Label)
		delete(s.fails, l.Label)
	}
	s.mu.Unlock()
	s.endHold()
	if res.Stalled != "" {
		s.cfg.Logf("logbook: sync with %s: the %s stopped before the end: a page moved nothing, or the page bound; the next exchange goes on", l.Label, res.Stalled)
	}
	if len(res.Passed) > 0 {
		s.cfg.Logf("logbook: sync with %s: passed over origins whose next op was refused: %v", l.Label, res.Passed)
	}
	if res.Pulled.Stored > 0 {
		s.Changed() // ops from this peer go on to the others (7.2)
	}
	return true
}

// --- Sending when the replica gains ops ---

// changeLoop acts when the vector grows: by Changed, by a peer's push, or by a write another
// process made to the file.
func (s *Service) changeLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.Watch)
	defer t.Stop()
	watch, err := openWatcher(s.ctx, s.cfg.Path)
	if err != nil {
		s.cfg.Logf("logbook: change watcher: %v; checking the vector each tick", err)
	}
	if watch != nil {
		defer watch.Close()
	}
	var version int64
	// Check once even if a commit landed while the watcher was opening.
	s.onChange()
	for {
		select {
		case <-s.done:
			return
		case <-s.changed:
		case <-t.C:
			if s.cfg.Path == "" {
				continue
			}
			if watch != nil {
				now, err := watch.Version(s.ctx)
				if err == nil && now == version {
					if s.cfg.checked != nil {
						s.cfg.checked()
					}
					continue
				}
				if err == nil {
					version = now
				}
			}
		}
		s.onChange()
		if s.cfg.checked != nil {
			s.cfg.checked()
		}
	}
}

// onChange pushes over every link this hab dialed, and wakes each hab that pulls from this one
// and lacks some of the ops, once until its next pull. A hab this one pushes to gets no wake-up.
func (s *Service) onChange() {
	v, err := s.cfg.Store.Vector(s.ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			s.cfg.Logf("logbook: vector: %v", err)
		}
		return
	}
	s.mu.Lock()
	grew := ahead(v, s.last)
	s.last = v
	s.mu.Unlock()
	if !grew {
		return
	}
	pushed := map[string]bool{}
	if s.cfg.Peers != nil {
		now := s.cfg.Now()
		for _, l := range s.cfg.Peers.Links() {
			if !s.up(l) {
				continue
			}
			s.mu.Lock()
			f := s.fails[l.Label]
			backoff := f != nil && now.Before(f.next)
			s.mu.Unlock()
			if !backoff {
				pushed[l.DeviceID] = true
				s.start(l, false)
			}
		}
	}
	if s.cfg.Wake == nil {
		return
	}
	s.mu.Lock()
	var devs []string
	for dev, have := range s.have {
		if !s.woken[dev] && !pushed[dev] && ahead(v, have) {
			s.woken[dev] = true
			devs = append(devs, dev)
		}
	}
	s.mu.Unlock()
	for _, dev := range devs {
		if !s.cfg.Wake(dev) {
			// No link from that hab now: it pulls when its link comes up.
			s.mu.Lock()
			delete(s.woken, dev)
			s.mu.Unlock()
		}
	}
}

// ahead reports whether a holds a seq above b's for some origin.
func ahead(a, b logbook.Vector) bool {
	for o, n := range a {
		if n > b[o] {
			return true
		}
	}
	return false
}

// --- The peer link as a logbook.Remote ---

// linkRemote is one link as the exchange sees it. Each call waits until the answer to any
// earlier call over the link that the exchange gave up on has come, or the link ended: the hab
// at the other end writes every answer it makes, so a new call while one is still on the way
// would queue a second page behind it and could pass the control connection's backlog, which
// closes the link (logbook.md 7.6, Slow links).
type linkRemote struct {
	s     *Service
	label string
	c     api.Client
}

func (r *linkRemote) Page() logbook.Limits {
	return logbook.Limits{Bytes: r.s.pageBytes(r.label)}
}

func (r *linkRemote) Pull(ctx context.Context, have logbook.Vector, skip []string, lim logbook.Limits) (logbook.PullAnswer, error) {
	var res api.LogbookPullResult
	err := r.s.call(ctx, r.label, func(ctx context.Context) error {
		var err error
		res, err = r.c.LogbookPull(ctx, api.LogbookPullParams{Have: toSeqs(have), Skip: skip, MaxOps: lim.Ops, MaxBytes: lim.Bytes})
		return err
	})
	if err != nil {
		return logbook.PullAnswer{}, err
	}
	return logbook.PullAnswer{Ops: res.Ops, Vector: fromSeqs(res.Vector), More: res.More, Skip: res.Skip}, nil
}

func (r *linkRemote) Push(ctx context.Context, ops [][]byte) (logbook.PushAnswer, error) {
	var res api.LogbookPushResult
	err := r.s.call(ctx, r.label, func(ctx context.Context) error {
		var err error
		res, err = r.c.LogbookPush(ctx, api.LogbookPushParams{Ops: ops})
		return err
	})
	if err != nil {
		return logbook.PushAnswer{}, err
	}
	return logbook.PushAnswer{Vector: fromSeqs(res.Vector), Skip: res.Skip}, nil
}

// pageBytes is the page size the link labelled label asks for now.
func (s *Service) pageBytes(label string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.page[label]; ok {
		return n
	}
	return logbook.PageBytes
}

// call runs f over the link labelled label: after any answer still on the way, and for at most
// PageTime. Past that it gives up, halves the link's page size, and leaves f running until its
// answer comes, so the next call waits for it. f gets a context that only Close ends.
func (s *Service) call(ctx context.Context, label string, f func(context.Context) error) error {
	s.mu.Lock()
	late := s.late[label]
	s.mu.Unlock()
	if late != nil {
		select {
		case <-late:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	var err error
	start := time.Now()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		err = f(s.ctx)
	}()
	t := time.NewTimer(s.cfg.PageTime)
	defer t.Stop()
	select {
	case <-done:
		s.adapt(label, time.Since(start))
		return err
	case <-t.C:
		s.mu.Lock()
		s.late[label] = done
		s.mu.Unlock()
		n := s.adapt(label, s.cfg.PageTime)
		return fmt.Errorf("no answer within %v; the next pages hold at most %d bytes", s.cfg.PageTime, n)
	case <-ctx.Done():
		s.mu.Lock()
		s.late[label] = done
		s.mu.Unlock()
		return ctx.Err()
	}
}

// adapt sets the link's page size from how long a call took: slower than a quarter of PageTime
// halves it, down to minPageBytes; faster than a sixteenth doubles it, up to the page limit. It
// returns the new size.
func (s *Service) adapt(label string, took time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.page[label]
	if !ok {
		n = logbook.PageBytes
	}
	switch {
	case took > s.cfg.PageTime/4:
		n = max(n/2, minPageBytes)
	case took < s.cfg.PageTime/16:
		n = min(n*2, logbook.PageBytes)
	}
	s.page[label] = n
	return n
}

// merged is a with every seq of b above it.
func merged(a, b logbook.Vector) logbook.Vector {
	out := maps.Clone(a)
	if out == nil {
		out = logbook.Vector{}
	}
	for o, n := range b {
		out[o] = max(out[o], n)
	}
	return out
}

func toSeqs(v logbook.Vector) []api.LogbookSeq {
	out := make([]api.LogbookSeq, 0, len(v))
	for o, n := range v {
		out = append(out, api.LogbookSeq{Origin: o, Seq: n})
	}
	slices.SortFunc(out, func(a, b api.LogbookSeq) int {
		switch {
		case a.Origin < b.Origin:
			return -1
		case a.Origin > b.Origin:
			return 1
		}
		return 0
	})
	return out
}

func fromSeqs(s []api.LogbookSeq) logbook.Vector {
	v := logbook.Vector{}
	for _, e := range s {
		v[e.Origin] = max(v[e.Origin], e.Seq)
	}
	return v
}
