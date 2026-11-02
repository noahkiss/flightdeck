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
	"maps"
	"os"
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
	minBackoff = time.Second
	maxBackoff = time.Minute
	// pageTime bounds one pull or push call over a link.
	pageTime = 60 * time.Second
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
	// Path is the store's file. Its size and time, and its WAL's, are checked every Watch, so
	// ops another process wrote (the CLI while gff-hab was busy, logbook.md 4.3) go out too.
	// Empty: only Changed and peers' ops start a send.
	Path  string
	Peers Peers // nil: no links this hab dialed
	// Wake sends mesh.changed with the logbook reason to every peer link the hab with this
	// device id dialed to this one, and reports whether any link took it (logbook.md 7.1).
	Wake  func(device string) bool
	Poll  time.Duration // the exchange round over every link; default 60 s (logbook.md 7.1)
	Watch time.Duration // the file check; default 500 ms
	Now   func() time.Time
	Logf  func(string, ...any)
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
		kick: make(chan struct{}, 1), changed: make(chan struct{}, 1), wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.ctx, s.stop = context.WithCancel(context.Background())
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
				s.start(l)
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
	a, err := s.cfg.Store.ServePull(ctx, have, p.Skip)
	if err != nil {
		return api.LogbookPullResult{}, err
	}
	s.mu.Lock()
	// The caller pulls: ops this hab gains from now on wake it again.
	s.have[from.DeviceID] = have
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
			s.start(l)
		}
	}
}

// up reports whether l is a link this hab can sync over.
func (s *Service) up(l Link) bool {
	return l.Online && l.Caller != nil && l.DeviceID != "" && l.DeviceID != s.cfg.Self && slices.Contains(l.Caps, Capability)
}

// start runs one exchange over l in the background. When one is in flight already, that one
// runs once more after it ends instead, so a link never has two exchanges in flight.
func (s *Service) start(l Link) {
	s.mu.Lock()
	if s.running[l.Label] {
		s.again[l.Label] = true
		s.mu.Unlock()
		return
	}
	s.running[l.Label] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			ok := s.exchange(l)
			s.mu.Lock()
			again := s.again[l.Label] && s.ctx.Err() == nil
			delete(s.again, l.Label)
			if !again {
				delete(s.running, l.Label)
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			if !ok {
				// A wake-up or a change asked for another while this one failed: run it when the
				// backoff ends.
				if !s.sleepBackoff(l.Label) {
					s.mu.Lock()
					delete(s.running, l.Label)
					s.mu.Unlock()
					return
				}
			}
		}
	}()
}

// sleepBackoff waits out the link's backoff; false when the service closed.
func (s *Service) sleepBackoff(label string) bool {
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
	case <-t.C:
		return true
	}
}

// exchange runs Store.Sync over the link. It reports whether it ran to the end.
func (s *Service) exchange(l Link) bool {
	res, err := s.cfg.Store.Sync(s.ctx, linkRemote{c: api.Client{Caller: l.Caller}})
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
			f.next = s.cfg.Now().Add(min(minBackoff<<min(f.n-1, 16), maxBackoff))
		}
		s.mu.Unlock()
		return false
	}
	if s.fails[l.Label] != nil {
		s.cfg.Logf("logbook: %s answers again", l.Label)
		delete(s.fails, l.Label)
	}
	s.mu.Unlock()
	if res.Stalled != "" {
		s.cfg.Logf("logbook: sync with %s: the %s stopped: a page moved no vector", l.Label, res.Stalled)
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
	sig := s.fileSig()
	recheck := false
	for {
		select {
		case <-s.done:
			return
		case <-s.changed:
		case <-t.C:
			now := s.fileSig()
			if now == sig && !recheck {
				continue
			}
			// A change seen while a commit was still being written may not show in the vector
			// yet, and the commit's last write need not move the size or the coarse file time
			// again. So a change is looked at once more on the next check.
			recheck = now != sig
			sig = now
		}
		s.onChange()
	}
}

// fileSig is the size and time of the store's file and its WAL: a commit changes it.
func (s *Service) fileSig() [4]int64 {
	var sig [4]int64
	if s.cfg.Path == "" {
		return sig
	}
	for i, p := range []string{s.cfg.Path, s.cfg.Path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			sig[2*i], sig[2*i+1] = fi.Size(), fi.ModTime().UnixNano()
		}
	}
	return sig
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
				s.start(l)
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

type linkRemote struct{ c api.Client }

func (r linkRemote) Pull(ctx context.Context, have logbook.Vector, skip []string) (logbook.PullAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, pageTime)
	defer cancel()
	res, err := r.c.LogbookPull(ctx, api.LogbookPullParams{Have: toSeqs(have), Skip: skip})
	if err != nil {
		return logbook.PullAnswer{}, err
	}
	return logbook.PullAnswer{Ops: res.Ops, Vector: fromSeqs(res.Vector), More: res.More, Skip: res.Skip}, nil
}

func (r linkRemote) Push(ctx context.Context, ops [][]byte) (logbook.PushAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, pageTime)
	defer cancel()
	res, err := r.c.LogbookPush(ctx, api.LogbookPushParams{Ops: ops})
	if err != nil {
		return logbook.PushAnswer{}, err
	}
	return logbook.PushAnswer{Vector: fromSeqs(res.Vector), Skip: res.Skip}, nil
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
