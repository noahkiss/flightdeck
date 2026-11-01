// Package link is gff-hab's holder link (docs/specs/hab-logic-api.md 2.3): it connects to every
// live holder on the host, keeps one parsed screen per pane, checkpoints each screen to a state
// directory, and rebuilds every screen after a planned restart or a crash.
//
//   - One parser per pane. Output and replayed bytes enter it in offset order, from one event
//     loop that reads every holder's frames in the order the holder sent them.
//   - A checkpoint per pane (a parser snapshot plus the offset it covers) is written atomically
//     when the pane goes idle and at a fixed interval while it keeps printing.
//   - Close is the planned stop: fresh checkpoints, then handover to every holder.
//   - Open loads each pane's checkpoint and asks its holder for an open replay from the saved
//     offset. If the ring no longer reaches that far, or there is no usable checkpoint and the
//     ring has lost the pane's first bytes, the screen is rebuilt from the ring alone and marked
//     suspect (spec 6.7).
//   - Replayed bytes get no answer to terminal queries (spec 5.6). Chunks carry Live, which is
//     false for every byte that did not arrive as live output, so the stage that answers queries
//     can answer live bytes only.
//   - When a holder connection ends, its panes are marked lost at once and their subscribers
//     are told; a rescan picks up a holder that comes back.
//   - After a scan that reached every holder in the registry, panes no holder lists are dropped
//     (lost ones after KeepLost) and their checkpoints removed.
//   - On a draining holder, a pane whose program ended is released once the link holds its last
//     output, so the holder can exit after its last release (releaseEnded).
//
// The parser is injected (vt.Parser), so this package is pure Go.
package link

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/termq"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// Config sets up a link. Zero durations and sizes take the defaults below.
type Config struct {
	// HolderDir is the holder registry directory (holder spec 4.2).
	HolderDir string
	// StateDir holds the checkpoint files, one <pane uuid>.ckpt per pane, and retired.json, the
	// retired pane uuids. It is created if missing.
	StateDir string
	// Build is this gff-hab's build id. Holders of other builds are drained once a holder of
	// this build is connected.
	Build string
	// Parser makes the per-pane terminals.
	Parser vt.Parser
	// Protocol is the holder protocol this gff-hab speaks. Nil means holderproto.V1().
	Protocol *hp.Registry
	// CheckpointEvery is the longest a printing pane goes without a checkpoint. Default 10 s.
	// Negative turns timed checkpoints off.
	CheckpointEvery time.Duration
	// IdleAfter is how long a pane must print nothing before its idle checkpoint. Default 1 s.
	// Negative turns idle checkpoints off.
	IdleAfter time.Duration
	// Rescan is how often the link lists the registry for new holders. Default 2 s. A holder
	// connection that ends triggers a rescan at once.
	Rescan time.Duration
	// KeepLost is how long a lost or released pane stays listed before a clean scan drops it.
	// Default 30 s.
	KeepLost time.Duration
	// BufferBytes is the recent output kept per pane for subscribers that resume (spec 5.7).
	// Default 4 MiB.
	BufferBytes int
	// ScrollbackBytes caps each parser's scrollback. Zero keeps the parser's default.
	ScrollbackBytes int
	// ScrollbackBudget caps the scrollback of all parsers together. When the panes' equal
	// shares fall below ScrollbackBytes, each parser gets its share instead, never less than
	// MinScrollbackShare; shrinking a cap drops the oldest scrollback at once. Zero or less means
	// no budget.
	ScrollbackBudget int
	// RetiredTTL is how long a retired pane uuid stays refused, across restarts. Default
	// DefaultRetiredTTL.
	RetiredTTL time.Duration
	// SnapshotScrollback is the most scrollback rows a subscription's snapshot, a jump snapshot
	// or Screen's VT carries. Scrollback reads older rows. Default DefaultSnapshotScrollback;
	// negative means all.
	SnapshotScrollback int
	// Colors are reported to programs that ask for their colours (spec 5.6). Nil means
	// termq.DefaultColors.
	Colors *termq.Colors
	// Logf receives diagnostics. Nil discards them.
	Logf func(format string, args ...any)
	// Start asks the init system for a holder of this build (holder spec 5.4). With it set, the
	// link starts one whenever none of this build is connected: at a scan (after a failed start,
	// not again for StartBackoff) and before a spawn. A new build then drains the older holders
	// (spec 4.3). Nil means holders are started by hand or by a test.
	Start func(ctx context.Context) error
	// Retired is called on its own goroutine when a holder says bye: it exited cleanly, after its
	// drain or a shutdown, so its job can go (spec 5.4). pid is the holder's process id.
	Retired func(instance string, pid int)

	// saveDelay delays each write of the retired file, so tests can stand in for a slow fsync.
	saveDelay time.Duration
}

// StartBackoff is how long a scan waits after a failed holder start before it tries again.
const StartBackoff = 30 * time.Second

// DefaultSnapshotScrollback is the default of Config.SnapshotScrollback. 1000 rows of 120
// columns are about 0.14 MiB of snapshot-vt (spec 2.3).
const DefaultSnapshotScrollback = 1000

// MinScrollbackShare is the smallest scrollback cap the budget gives a parser.
const MinScrollbackShare = 1 << 20

// DefaultRetiredTTL is the default of Config.RetiredTTL.
const DefaultRetiredTTL = 30 * 24 * time.Hour

// MaxScrollbackPage is the most rows one Scrollback call returns.
const MaxScrollbackPage = 10000

// Errors returned by the link.
var (
	ErrNoPane = errors.New("link: no such pane")
	ErrLost   = errors.New("link: pane lost: its holder connection ended")
	ErrGone   = errors.New("link: pane gone")
	ErrClosed = errors.New("link: closed")
	ErrAhead  = errors.New("link: offset is past the pane's output")
	ErrSlow   = errors.New("link: subscriber fell behind")
	// ErrUUIDUsed refuses a spawn whose uuid names a pane the link follows, follows no longer,
	// or once followed. A pane's uuid is its hook identity: a new program under an old uuid
	// would take the old pane's token, and a leftover process of the old pane could post as it.
	ErrUUIDUsed = errors.New("link: uuid belongs to another pane; a new pane needs a new uuid")
	// ErrReleasing refuses a close while a release of the same pane is in flight.
	ErrReleasing = errors.New("link: a release of this pane is in flight")
)

type paneKey struct {
	h  *holderclient.Holder
	hd hp.Handle
}

// Link holds the parsed screens of every pane on every holder of the host.
type Link struct {
	cfg    Config
	client *holderclient.Client
	enc    vt.Encoding // checkpoint encoding
	colors termq.Colors

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	rescan chan struct{}
	tasks  chan func() // run on the event loop, in order with holder frames

	// scanMu keeps scans one at a time: two scans could connect to one holder twice, and the
	// second hello would take the first connection over.
	scanMu      sync.Mutex
	startFailed time.Time // the last failed Start; guarded by scanMu

	mu       sync.Mutex
	panes    map[hp.UUID]*pane
	byHandle map[paneKey]*pane
	holders  map[*holderclient.Holder]bool
	orphans  map[paneKey][]hp.Frame // frames for a pane not attached yet (a spawn in flight)

	// retired holds the uuids of panes that went gone or that the link dropped, with the time
	// each was retired. Spawn refuses them for RetiredTTL. retired.json in the state directory
	// keeps them across restarts.
	retired map[hp.UUID]time.Time
	// saveRetiredC asks retiredWriter to write the retired file. Buffered: requests made while a
	// write runs coalesce into one more write.
	saveRetiredC chan struct{}
	// retiredSaved gets a value after each write of the retired file; tests wait on it.
	retiredSaved chan struct{}
	// afterSpawnCheck runs in Spawn after the uuid check, before the holder spawns. Tests use it
	// to land a pane with the same uuid inside that window.
	afterSpawnCheck func()

	watchers map[int]func(Notice)
	nextW    int
	closed   bool

	pruned chan struct{} // gets a value after each prune; tests wait on it

	// sbCap is the scrollback cap every parser has now: ScrollbackBytes, or its share of
	// ScrollbackBudget. New and restored parsers get it too. Read without l.mu.
	sbCap atomic.Int64
}

// Open connects to every live holder, loads the checkpoints and starts a replay for every pane.
// It returns without waiting for the replays; use Ready.
func Open(cfg Config) (*Link, error) {
	if cfg.Parser == nil {
		return nil, errors.New("link: no parser")
	}
	if cfg.StateDir == "" || cfg.HolderDir == "" {
		return nil, errors.New("link: StateDir and HolderDir are required")
	}
	if cfg.CheckpointEvery == 0 {
		cfg.CheckpointEvery = 10 * time.Second
	}
	if cfg.IdleAfter == 0 {
		cfg.IdleAfter = time.Second
	}
	if cfg.Rescan <= 0 {
		cfg.Rescan = 2 * time.Second
	}
	if cfg.KeepLost <= 0 {
		cfg.KeepLost = 30 * time.Second
	}
	if cfg.BufferBytes <= 0 {
		cfg.BufferBytes = 4 << 20
	}
	if cfg.RetiredTTL <= 0 {
		cfg.RetiredTTL = DefaultRetiredTTL
	}
	if cfg.SnapshotScrollback == 0 {
		cfg.SnapshotScrollback = DefaultSnapshotScrollback
	} else if cfg.SnapshotScrollback < 0 {
		cfg.SnapshotScrollback = vt.AllScrollback
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	removeTemps(cfg.StateDir)
	now := time.Now()
	retired, pruned, err := loadRetired(cfg.StateDir, cfg.RetiredTTL, now)
	if err != nil {
		cfg.Logf("link: retired uuids: %v; starting with none", err)
	}
	if pruned {
		if err := saveRetired(cfg.StateDir, retired, cfg.RetiredTTL, now); err != nil {
			cfg.Logf("link: retired uuids: %v", err)
		}
	}
	encs := cfg.Parser.Encodings()
	if len(encs) == 0 {
		return nil, errors.New("link: parser has no snapshot encoding")
	}
	l := &Link{
		cfg: cfg,
		client: holderclient.New(holderclient.Config{
			Dir: cfg.HolderDir, Build: cfg.Build, Protocol: cfg.Protocol, Start: cfg.Start, Logf: cfg.Logf,
		}),
		enc:          checkpointEncoding(encs),
		colors:       termq.DefaultColors(),
		rescan:       make(chan struct{}, 1),
		tasks:        make(chan func()),
		panes:        map[hp.UUID]*pane{},
		byHandle:     map[paneKey]*pane{},
		holders:      map[*holderclient.Holder]bool{},
		orphans:      map[paneKey][]hp.Frame{},
		retired:      retired,
		saveRetiredC: make(chan struct{}, 1),
		retiredSaved: make(chan struct{}, 1),
		watchers:     map[int]func(Notice){},
	}
	if cfg.Colors != nil {
		l.colors = *cfg.Colors
	}
	l.sbCap.Store(int64(l.scrollbackShare(0)))
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.pruned = make(chan struct{}, 1)
	l.wg.Add(1)
	go l.eventLoop()
	l.scan()
	l.wg.Add(3)
	go l.rescanLoop()
	go l.checkpointLoop()
	go l.retiredWriter()
	return l, nil
}

// checkpointEncoding picks snapshot-ghostty when the parser has it: it keeps the most state, and
// the whole scrollback.
func checkpointEncoding(encs []vt.Encoding) vt.Encoding {
	if slices.Contains(encs, vt.EncodingGhostty) {
		return vt.EncodingGhostty
	}
	return encs[0]
}

// Ready waits until every pane known now has finished its replay.
func (l *Link) Ready(ctx context.Context) error {
	l.mu.Lock()
	ps := make([]*pane, 0, len(l.panes))
	for _, p := range l.panes {
		ps = append(ps, p)
	}
	l.mu.Unlock()
	for _, p := range ps {
		select {
		case <-p.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Previous reports how the last logic process left each connected holder, by instance id.
func (l *Link) Previous() map[string]hp.Previous {
	out := map[string]hp.Previous{}
	for _, h := range l.client.Holders() {
		out[h.Instance()] = h.Welcome().Previous
	}
	return out
}

// scan lists the registry, connects to new holders, drains holders of other builds once a
// holder of this build is connected, and attaches the panes of every new holder. With
// Config.Start set, it first starts a holder of this build when none is connected.
func (l *Link) scan() { l.scanWith(false) }

// scanWith is scan; force skips the StartBackoff after a failed start.
func (l *Link) scanWith(force bool) {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	defer cancel()
	err := l.client.Scan(ctx)
	// With Start set, a missing directory is expected before the first holder: Upgrade makes it.
	quiet := l.cfg.Start != nil && errors.Is(err, os.ErrNotExist)
	if err != nil && !errors.Is(err, context.Canceled) && !quiet {
		l.cfg.Logf("link: scan: %v", err)
	}
	if l.cfg.Start != nil && l.client.Current() == nil && (force || time.Since(l.startFailed) >= StartBackoff) {
		// Upgrade starts a holder, waits for it, and drains the rest (spec 4.3).
		if _, err := l.client.Upgrade(ctx); err != nil {
			l.cfg.Logf("link: start holder: %v", err)
		}
		if l.client.Current() == nil {
			l.startFailed = time.Now()
		} else {
			l.startFailed = time.Time{}
		}
	} else if l.client.Current() != nil {
		if _, err := l.client.Upgrade(ctx); err != nil {
			l.cfg.Logf("link: drain: %v", err)
		}
	}
	for _, h := range l.client.Holders() {
		l.mu.Lock()
		known := l.holders[h] || l.closed
		l.mu.Unlock()
		if known {
			continue
		}
		l.run(func() {
			// On the event loop: a holder whose end event was already handled is not attached;
			// one that ends later has its end event handled after this.
			l.mu.Lock()
			known := l.holders[h] || l.closed
			l.mu.Unlock()
			if known || h.Closed() {
				return
			}
			l.mu.Lock()
			l.holders[h] = true
			l.mu.Unlock()
			w := h.Welcome()
			l.cfg.Logf("link: holder %s (build %s, version %d): %d panes, previous %s",
				h.Instance(), h.Build(), h.Version(), len(w.Panes), previousWord(w.Previous))
			for _, pi := range w.Panes {
				l.attach(h, pi)
			}
		})
	}
	l.run(func() {
		// A holder drained since the last scan may hold ended panes already.
		for _, p := range l.allPanes() {
			l.releaseEnded(p)
		}
	})
	if l.complete() {
		l.run(l.prune)
	}
}

func (l *Link) allPanes() []*pane {
	l.mu.Lock()
	defer l.mu.Unlock()
	ps := make([]*pane, 0, len(l.panes))
	for _, p := range l.panes {
		ps = append(ps, p)
	}
	return ps
}

// releaseEnded releases pane p when it has ended on a draining holder, so the holder can exit
// after its last release (holder spec 4.3). It waits until the link holds the pane's last output:
// the exit is recorded, the replay after attach has ended, and the parser holds every byte before
// the exit's final count. Subscribers have those bytes then too, since the link queues each byte
// to every subscriber as it parses it. After the release the pane is gone: its subscriptions
// end, its checkpoint is removed and watchers get PaneGone. It runs on the event loop; the release
// itself runs on its own goroutine, because the holder's answer arrives through the event loop.
func (l *Link) releaseEnded(p *pane) {
	p.mu.Lock()
	h, hd := p.holder, p.handle
	ok := h != nil && p.lost == nil && p.exited && p.isReady && !p.releasing && p.off >= p.final && h.Draining()
	if ok {
		p.releasing = true
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	l.mu.Lock()
	ok = l.holders[h] && !l.closed
	l.mu.Unlock()
	if !ok {
		p.mu.Lock()
		p.releasing = false
		p.mu.Unlock()
		return
	}
	l.wg.Add(1) // on the event loop, which the wait group counts, so never after Close's Wait
	go func() {
		defer l.wg.Done()
		ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
		err := h.Release(ctx, hd)
		cancel()
		if err != nil {
			l.cfg.Logf("link: pane %s: release on draining holder %s: %v", p.uuid, h.Instance(), err)
			p.mu.Lock()
			p.releasing = false // the next scan tries again
			p.mu.Unlock()
			return
		}
		l.cfg.Logf("link: pane %s: ended on draining holder %s: released", p.uuid, h.Instance())
		l.run(func() {
			l.mu.Lock()
			if l.byHandle[paneKey{h, hd}] == p {
				delete(l.byHandle, paneKey{h, hd})
			}
			l.mu.Unlock()
			p.mu.Lock()
			mine := p.holder == h && p.handle == hd
			p.mu.Unlock()
			if !mine {
				return
			}
			if n, ok := p.lose(ErrGone); ok {
				removeCheckpoint(l.cfg.StateDir, p.uuid)
				l.retire(p.uuid)
				l.notify([]Notice{n})
			}
		})
	}()
}

// complete reports whether the client is connected to every holder in the registry, so the panes
// the link follows are every pane on the host.
func (l *Link) complete() bool {
	if len(l.client.Unreachable()) > 0 {
		return false
	}
	entries, errs := holderreg.List(l.cfg.HolderDir)
	if len(errs) > 0 {
		return false
	}
	live := map[string]bool{}
	for _, h := range l.client.Holders() {
		if !h.Closed() {
			live[h.Instance()] = true
		}
	}
	for _, e := range entries {
		if !live[e.Instance] {
			return false
		}
	}
	return true
}

// prune drops the panes no connected holder lists, once lost for KeepLost, frees their parsers,
// and removes every checkpoint of a pane the link does not follow. It runs on the event loop,
// after a scan that reached every holder.
func (l *Link) prune() {
	now := time.Now()
	keep := map[string]bool{}
	var drop []*pane
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	for u, p := range l.panes {
		p.mu.Lock()
		live := p.lost == nil && p.holder != nil && l.holders[p.holder]
		old := p.lost != nil && now.Sub(p.lostAt) >= l.cfg.KeepLost
		p.mu.Unlock()
		switch {
		case live:
			keep[u.String()] = true
		case old:
			drop = append(drop, p)
			delete(l.panes, u)
			l.retired[u] = now
			for k, q := range l.byHandle {
				if q == p {
					delete(l.byHandle, k)
				}
			}
		default:
			keep[u.String()] = true // lost lately: listed a while longer
		}
	}
	l.mu.Unlock()
	if len(drop) > 0 {
		l.requestRetiredSave()
	}
	for _, p := range drop {
		p.close()
		l.cfg.Logf("link: pane %s: dropped: no holder lists it", p.uuid)
	}
	if len(drop) > 0 {
		l.rebalance()
	}
	for _, u := range removeCheckpointsExcept(l.cfg.StateDir, keep) {
		l.cfg.Logf("link: removed the checkpoint of pane %s: no holder lists it", u)
	}
	select {
	case l.pruned <- struct{}{}:
	default:
	}
}

// run runs fn on the event loop and waits for it. It returns false when the link closed first.
func (l *Link) run(fn func()) bool {
	done := make(chan struct{})
	select {
	case l.tasks <- func() { fn(); close(done) }:
	case <-l.ctx.Done():
		return false
	}
	select {
	case <-done:
		return true
	case <-l.ctx.Done():
		return false
	}
}

func previousWord(p hp.Previous) string {
	switch p {
	case hp.PreviousNone:
		return "none"
	case hp.PreviousHandover:
		return "handover"
	case hp.PreviousCrash:
		return "crash"
	}
	return fmt.Sprintf("unknown(%d)", p)
}

func (l *Link) rescanLoop() {
	defer l.wg.Done()
	t := time.NewTicker(l.cfg.Rescan)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		case <-l.rescan:
		}
		l.scan()
	}
}

func (l *Link) eventLoop() {
	defer l.wg.Done()
	for {
		select {
		case <-l.ctx.Done():
			return
		case ev := <-l.client.Events():
			l.handle(ev)
		case fn := <-l.tasks:
			fn()
		}
	}
}

// handle applies one holder event. It runs on the event loop only, so frames for a pane are
// applied in the order the holder sent them.
func (l *Link) handle(ev holderclient.Event) {
	if ev.Frame == nil {
		l.holderEnded(ev.Holder, ev.Err)
		return
	}
	hdr := ev.Frame.Header()
	l.mu.Lock()
	p := l.byHandle[paneKey{ev.Holder, hdr.Pane}]
	if p == nil {
		if hdr.Pane != 0 && !l.closed {
			k := paneKey{ev.Holder, hdr.Pane}
			if len(l.orphans[k]) < 4096 {
				l.orphans[k] = append(l.orphans[k], ev.Frame)
			}
		}
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	l.apply(p, ev.Frame)
}

// apply feeds one frame to its pane.
func (l *Link) apply(p *pane, f hp.Frame) {
	var notices []Notice
	switch f := f.(type) {
	case *hp.Output:
		p.feed(l, f.Offset, f.Data, true)
		notices = p.commandNotice(notices)
	case *hp.ReplayData:
		p.feed(l, f.Offset, f.Data, false)
		notices = p.commandNotice(notices)
	case *hp.ReplayEnd:
		p.replayEnd(l, f)
		defer l.releaseEnded(p)
	case *hp.Exited:
		if n, ok := p.reaped(f.Status, f.Final); ok {
			notices = append(notices, n)
		}
		defer l.releaseEnded(p)
	case *hp.InputReady:
		p.inputReady(l)
	case *hp.Error:
		if f.Ref == hp.KindReplay {
			l.cfg.Logf("link: pane %s: replay refused: code %d: %s", p.uuid, f.Code, f.Message)
			if n, ok := p.lose(ErrGone); ok {
				notices = append(notices, n)
			}
		}
	}
	l.notify(notices)
}

// holderEnded marks every pane of h lost (or gone, after a clean bye) and asks for a rescan, so a
// holder that comes back on the same path is found again.
func (l *Link) holderEnded(h *holderclient.Holder, err error) {
	l.mu.Lock()
	delete(l.holders, h)
	var ps []*pane
	for k, p := range l.byHandle {
		if k.h == h {
			ps = append(ps, p)
			delete(l.byHandle, k)
		}
	}
	for k := range l.orphans {
		if k.h == h {
			delete(l.orphans, k)
		}
	}
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return
	}
	why := ErrLost
	if err == nil {
		why = ErrGone
		l.cfg.Logf("link: holder %s said bye", h.Instance())
		if l.cfg.Retired != nil {
			go l.cfg.Retired(h.Instance(), h.Entry().Pid)
		}
	} else {
		l.cfg.Logf("link: holder %s connection ended: %v; %d panes lost", h.Instance(), err, len(ps))
	}
	var notices []Notice
	for _, p := range ps {
		p.mu.Lock()
		mine := p.holder == h
		p.mu.Unlock()
		if !mine {
			continue // the pane moved to a newer connection before this end event arrived
		}
		if n, ok := p.lose(why); ok {
			notices = append(notices, n)
		}
		if why == ErrGone {
			removeCheckpoint(l.cfg.StateDir, p.uuid)
			l.retire(p.uuid)
		}
	}
	l.notify(notices)
	select {
	case l.rescan <- struct{}{}:
	default:
	}
}

// attach starts following pane pi on holder h: it restores the screen from the pane's
// checkpoint, or starts an empty one, and sends an open replay from the offset that screen
// covers. It runs on the event loop.
func (l *Link) attach(h *holderclient.Holder, pi hp.PaneInfo) {
	l.mu.Lock()
	p := l.panes[pi.UUID]
	fresh := p == nil
	if fresh {
		p = newPane(pi.UUID, l.cfg.SnapshotScrollback)
		l.panes[pi.UUID] = p
	}
	for k, q := range l.byHandle {
		if q == p {
			delete(l.byHandle, k) // frames from an older connection no longer reach the pane
		}
	}
	k := paneKey{h, pi.Handle}
	l.byHandle[k] = p
	orphans := l.orphans[k]
	delete(l.orphans, k)
	l.mu.Unlock()

	from, back := p.prepare(l, h, pi)
	tok := h.Token()
	p.mu.Lock()
	p.replayTok = tok
	p.mu.Unlock()
	if err := h.Send(&hp.Replay{Handle: pi.Handle, Token: tok, From: from}); err != nil {
		l.cfg.Logf("link: pane %s: replay: %v", pi.UUID, err)
		if n, ok := p.lose(ErrLost); ok {
			l.notify([]Notice{n})
		}
		return
	}
	if fresh {
		l.rebalance()
	}
	if fresh || back {
		l.notify([]Notice{{Kind: PaneAdded, Pane: p.status()}})
	}
	for _, f := range orphans {
		l.apply(p, f)
	}
}

// Spawn starts a program in a new pane on the current holder and follows its output from the
// first byte. s.UUID must be new: Spawn refuses, with ErrUUIDUsed, a uuid the link knows or once
// knew, unless it names a live pane on the current holder, which makes the call a retry of a
// spawn that already ran (the holder starts nothing then).
func (l *Link) Spawn(ctx context.Context, s hp.Spawn) (PaneStatus, error) {
	h := l.client.Current()
	if h == nil && l.cfg.Start != nil {
		l.scanWith(true) // starts a holder and attaches it
		h = l.client.Current()
	}
	if h == nil {
		return PaneStatus{}, holderclient.ErrNoHolder
	}
	if !l.spawnable(s.UUID, h) {
		return PaneStatus{}, ErrUUIDUsed
	}
	if l.afterSpawnCheck != nil {
		l.afterSpawnCheck()
	}
	pn, err := h.Spawn(ctx, s)
	if err != nil {
		return PaneStatus{}, err
	}
	pi := hp.PaneInfo{UUID: pn.UUID, Handle: pn.Handle, Cols: s.Cols, Rows: s.Rows}
	for _, st := range l.client.Panes() {
		if st.UUID == pn.UUID {
			pi.Pid = st.Pid
		}
	}
	var st PaneStatus
	ok := l.run(func() {
		var replaced []Notice
		l.mu.Lock()
		if old := l.panes[pn.UUID]; old != nil {
			old.mu.Lock()
			retry := old.holder == h && old.handle == pn.Handle && old.lost == nil
			live := old.lost == nil
			old.mu.Unlock()
			if retry {
				l.mu.Unlock()
				st = old.status() // a retry of a spawn that already ran
				return
			}
			// A pane with the same uuid that became lost, or showed up on another holder, after
			// the check in spawnable: the new one replaces it. Watchers hear the old one go
			// first, so state kept for it (agent state among it) is dropped, not inherited. A
			// lost pane's watchers heard it go when it was lost.
			for k, q := range l.byHandle {
				if q == old {
					delete(l.byHandle, k)
				}
			}
			if live {
				gone := old.status()
				gone.Gone = true
				replaced = []Notice{{Kind: PaneGone, Pane: gone}}
			}
			defer old.close()
		}
		removeCheckpoint(l.cfg.StateDir, pn.UUID) // a fresh pane: an older checkpoint is stale
		p := newPane(pn.UUID, l.cfg.SnapshotScrollback)
		p.spawned(l, h, pi)
		l.panes[pn.UUID] = p
		k := paneKey{h, pn.Handle}
		l.byHandle[k] = p
		orphans := l.orphans[k]
		delete(l.orphans, k)
		l.mu.Unlock()
		// Notify the add on the event loop, before the orphan frames and before any later
		// frame: a child that exits at once must not be reported exited before it was added.
		l.rebalance()
		st = p.status()
		l.notify(replaced)
		l.notify([]Notice{{Kind: PaneAdded, Pane: st}})
		for _, f := range orphans {
			l.apply(p, f)
		}
	})
	if !ok {
		return PaneStatus{}, ErrClosed
	}
	return st, nil
}

// ClosePane releases pane u on its holder, at a client's request (pane.close, spec 3.9). On a
// running program the holder hangs up its session (SIGHUP) and closes the terminal; the program
// is reaped whenever it ends, and the holder does not escalate (holder spec 2.7, 6.1). An ended
// pane is released at once, on any holder, so its record and ring are freed. After the release
// the pane is gone, as after releaseEnded: its subscriptions end, its checkpoint is removed,
// watchers get PaneGone, and its parsed screen stays readable until the prune drops it. A pane
// already gone is a success. A lost pane cannot be released: its holder is not connected.
func (l *Link) ClosePane(ctx context.Context, u hp.UUID) error {
	p := l.pane(u)
	if p == nil {
		return ErrNoPane
	}
	p.mu.Lock()
	h, hd, lost, busy := p.holder, p.handle, p.lost, p.releasing
	if lost == nil && !busy && h != nil {
		p.releasing = true
	}
	p.mu.Unlock()
	switch {
	case errors.Is(lost, ErrGone):
		return nil
	case lost != nil:
		return lost
	case busy:
		return ErrReleasing
	case h == nil:
		return ErrNoPane
	}
	err := h.Release(ctx, hd)
	if err != nil {
		p.mu.Lock()
		p.releasing = false
		p.mu.Unlock()
		return err
	}
	l.cfg.Logf("link: pane %s: closed by a client: released on holder %s", u, h.Instance())
	// The pane keeps its handle in byHandle until the prune, so output the holder sent before
	// the release lands on the gone pane, which ignores it, rather than in the orphans.
	if !l.run(func() {
		p.mu.Lock()
		mine := p.holder == h && p.handle == hd
		p.mu.Unlock()
		if !mine {
			return
		}
		if n, ok := p.lose(ErrGone); ok {
			removeCheckpoint(l.cfg.StateDir, p.uuid)
			l.retire(p.uuid)
			l.notify([]Notice{n})
		}
	}) {
		return ErrClosed
	}
	return nil
}

// Input writes data to pane u's program as one write (spec 3.8) and returns its result. It is
// QueueInput followed by its wait.
func (l *Link) Input(ctx context.Context, u hp.UUID, data []byte) error {
	return l.QueueInput(ctx, u, data)()
}

// QueueInput queues data for pane u's program as one write (spec 3.8) and returns without
// waiting for it; wait returns the write's result. Writes to one pane, client input and the
// link's own answers to terminal queries alike, reach the holder in the order they were queued,
// and none waits for the holder's answer to the one before (see inputWriter). So a caller that
// queues writes one after another, without waiting, gets them written in that order. When ctx
// ends while the write still waits in the queue, it is never written and wait returns ctx's
// error; when it is already being written, wait returns that write's result, within
// inputTimeout of its first send.
func (l *Link) QueueInput(ctx context.Context, u hp.UUID, data []byte) (wait func() error) {
	p := l.pane(u)
	if p == nil {
		return func() error { return ErrNoPane }
	}
	r := &inputReq{data: data, done: make(chan error, 1)}
	p.mu.Lock()
	if p.lost != nil {
		err := p.lost
		p.mu.Unlock()
		return func() error { return err }
	}
	p.queueInputLocked(l, r)
	p.mu.Unlock()
	return func() error {
		select {
		case err := <-r.done:
			return err
		case <-ctx.Done():
		}
		p.mu.Lock()
		if !r.taken {
			// Still queued: it is never written, so a client that tries again types it once.
			r.cancelled = true
			p.mu.Unlock()
			return ctx.Err()
		}
		p.mu.Unlock()
		return <-r.done // being written: its result, within inputTimeout
	}
}

// Resize sets pane u's size: the holder resizes the PTY, then the parser takes the size and every
// subscriber gets a ChunkResize at the offset the parser has reached.
func (l *Link) Resize(ctx context.Context, u hp.UUID, cols, rows int) error {
	p := l.pane(u)
	if p == nil {
		return ErrNoPane
	}
	if cols <= 0 || rows <= 0 || cols > 0xffff || rows > 0xffff {
		return fmt.Errorf("link: bad size %dx%d", cols, rows)
	}
	p.mu.Lock()
	h, hd, lost := p.holder, p.handle, p.lost
	p.mu.Unlock()
	if lost != nil {
		return lost
	}
	if h == nil {
		return ErrNoPane
	}
	if err := h.Resize(ctx, hd, uint16(cols), uint16(rows)); err != nil {
		return err
	}
	changed := false
	if !l.run(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.holder == h && p.handle == hd {
			changed = p.resizeLocked(cols, rows)
		}
	}) {
		return ErrClosed
	}
	if changed {
		l.notify([]Notice{{Kind: PaneResized, Pane: p.status()}})
	}
	return nil
}

// SetOwner records pane u's size owner and why (spec 6.5); owner is empty for none. Every
// subscriber gets a ChunkResize at the current size that names the new owner, and every later
// ChunkResize and snapshot carries it. It never resizes.
func (l *Link) SetOwner(u hp.UUID, owner string, reason api.OwnerReason) error {
	p := l.pane(u)
	if p == nil {
		return ErrNoPane
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost != nil {
		return p.lost
	}
	if p.owner == owner && p.reason == reason {
		return nil
	}
	p.owner, p.reason = owner, reason
	if p.term != nil {
		p.sendSizeLocked()
	}
	return nil
}

// retire refuses uuid u to later spawns from now on, here and after a restart. A pane that went
// gone never comes back under its uuid, so it is retired at once; a lost one only when the prune
// drops it. The refusal holds at once; retiredWriter writes the file soon after, off the event
// loop, so a slow fsync never delays a release or a prune.
func (l *Link) retire(u hp.UUID) {
	l.mu.Lock()
	l.retired[u] = time.Now()
	l.mu.Unlock()
	l.requestRetiredSave()
}

func (l *Link) requestRetiredSave() {
	select {
	case l.saveRetiredC <- struct{}{}:
	default: // a write is already asked for; it will carry this change
	}
}

// retiredWriter writes the retired file whenever asked, until the link closes. Close writes it
// once more after this returns.
func (l *Link) retiredWriter() {
	defer l.wg.Done()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-l.saveRetiredC:
		}
		l.saveRetired()
	}
}

// saveRetired writes the retired uuids to the state directory, without the expired ones, which
// it forgets. One write at a time: retiredWriter, then Close after the writer stopped.
func (l *Link) saveRetired() {
	if l.cfg.saveDelay > 0 {
		time.Sleep(l.cfg.saveDelay)
	}
	now := time.Now()
	l.mu.Lock()
	set := make(map[hp.UUID]time.Time, len(l.retired))
	for u, at := range l.retired {
		if now.Sub(at) >= l.cfg.RetiredTTL {
			delete(l.retired, u)
			continue
		}
		set[u] = at
	}
	l.mu.Unlock()
	if err := saveRetired(l.cfg.StateDir, set, l.cfg.RetiredTTL, now); err != nil {
		l.cfg.Logf("link: retired uuids: %v", err)
	}
	select {
	case l.retiredSaved <- struct{}{}:
	default:
	}
}

// spawnable reports whether a spawn of uuid u on holder h may go ahead: u is unknown and was
// never retired, or it names a live pane on h (a retry).
func (l *Link) spawnable(u hp.UUID, h *holderclient.Holder) bool {
	l.mu.Lock()
	old := l.panes[u]
	at, retired := l.retired[u]
	l.mu.Unlock()
	if retired && time.Since(at) < l.cfg.RetiredTTL {
		return false
	}
	if old == nil {
		return true
	}
	old.mu.Lock()
	defer old.mu.Unlock()
	return old.holder == h && old.lost == nil
}

// Pane reports one pane as the link sees it.
func (l *Link) Pane(u hp.UUID) (PaneStatus, bool) {
	p := l.pane(u)
	if p == nil {
		return PaneStatus{}, false
	}
	return p.status(), true
}

func (l *Link) pane(u hp.UUID) *pane {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.panes[u]
}

// Panes reports every pane the link knows, lost ones included.
func (l *Link) Panes() []PaneStatus {
	l.mu.Lock()
	ps := make([]*pane, 0, len(l.panes))
	for _, p := range l.panes {
		ps = append(ps, p)
	}
	l.mu.Unlock()
	out := make([]PaneStatus, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.status())
	}
	sortStatus(out)
	return out
}

// Screen returns pane u's parsed screen.
func (l *Link) Screen(u hp.UUID) (Screen, error) {
	p := l.pane(u)
	if p == nil {
		return Screen{}, ErrNoPane
	}
	return p.screen()
}

// ScrollbackPage is a run of pane scrollback rows older than a snapshot carries; see Scrollback.
type ScrollbackPage struct {
	vt.ScrollbackPage
	// Offset is the pane offset the rows were read at. Skip counts rows back from the newest
	// scrollback row at this offset.
	Offset uint64
}

// Scrollback reads up to n of pane u's scrollback rows, oldest first, that end skip rows above
// the newest scrollback row (vt.Terminal.Scrollback). A client that applied a snapshot and the
// bytes after it up to the returned Offset holds the newest rows of the same scrollback; it
// passes the number of scrollback rows it holds as skip to fetch the rows before them. When the
// client's offset differs from Offset, output scrolled rows in meanwhile and the count is off by
// that much; it asks again once caught up. n is capped at MaxScrollbackPage.
func (l *Link) Scrollback(u hp.UUID, skip, n int) (ScrollbackPage, error) {
	p := l.pane(u)
	if p == nil {
		return ScrollbackPage{}, ErrNoPane
	}
	return p.scrollback(max(skip, 0), min(max(n, 0), MaxScrollbackPage))
}

// scrollbackShare returns the scrollback cap for each of n parsers.
func (l *Link) scrollbackShare(n int) int {
	c := l.cfg.ScrollbackBytes
	if b := l.cfg.ScrollbackBudget; b > 0 {
		share := max(b/max(n, 1), MinScrollbackShare)
		if c <= 0 || share < c {
			c = share
		}
	}
	return c
}

// rebalance sets every parser's scrollback cap to its share of the budget for the panes the link
// follows now. It runs on the event loop, after the set of panes changed, without l.mu held.
func (l *Link) rebalance() {
	l.mu.Lock()
	n := len(l.panes)
	ps := make([]*pane, 0, n)
	for _, p := range l.panes {
		ps = append(ps, p)
	}
	l.mu.Unlock()
	c := l.scrollbackShare(n)
	if old := l.sbCap.Swap(int64(c)); old == int64(c) {
		return
	}
	l.cfg.Logf("link: %d panes: scrollback cap %.1f MiB per pane", n, float64(c)/(1<<20))
	for _, p := range ps {
		p.setScrollback(c)
	}
}

// Checkpoint writes a checkpoint now for every ready pane whose screen changed since its last
// one.
func (l *Link) Checkpoint() error {
	l.mu.Lock()
	ps := make([]*pane, 0, len(l.panes))
	for _, p := range l.panes {
		ps = append(ps, p)
	}
	l.mu.Unlock()
	var errs []error
	for _, p := range ps {
		if err := p.checkpoint(l, true); err != nil {
			errs = append(errs, fmt.Errorf("pane %s: %w", p.uuid, err))
		}
	}
	return errors.Join(errs...)
}

func (l *Link) checkpointLoop() {
	defer l.wg.Done()
	tick := 250 * time.Millisecond
	for _, d := range []time.Duration{l.cfg.IdleAfter / 2, l.cfg.CheckpointEvery / 4} {
		if d > 0 && d < tick {
			tick = d
		}
	}
	tick = max(tick, 10*time.Millisecond)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
		l.mu.Lock()
		ps := make([]*pane, 0, len(l.panes))
		for _, p := range l.panes {
			ps = append(ps, p)
		}
		l.mu.Unlock()
		for _, p := range ps {
			if err := p.checkpoint(l, false); err != nil {
				l.cfg.Logf("link: pane %s: checkpoint: %v", p.uuid, err)
			}
		}
	}
}

// Close is the planned stop (protocol section 9): it stops reading holder output, writes a fresh
// checkpoint for every pane, sends handover to every holder and closes the connections. Every
// subscription ends with ErrClosed.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
	select {
	case <-l.saveRetiredC: // a write asked for and not yet made
		l.saveRetired()
	default:
	}
	err := l.Checkpoint()
	l.client.Close()
	l.mu.Lock()
	ps := make([]*pane, 0, len(l.panes))
	for _, p := range l.panes {
		ps = append(ps, p)
	}
	l.mu.Unlock()
	for _, p := range ps {
		p.close()
	}
	return err
}

// Watch adds a watcher for pane changes; see Notice. fn runs on the link's goroutines, never
// while the link holds a lock. It returns a cancel function.
func (l *Link) Watch(fn func(Notice)) (cancel func()) {
	l.mu.Lock()
	id := l.nextW
	l.nextW++
	l.watchers[id] = fn
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.watchers, id)
		l.mu.Unlock()
	}
}

func (l *Link) notify(ns []Notice) {
	if len(ns) == 0 {
		return
	}
	l.mu.Lock()
	fns := make([]func(Notice), 0, len(l.watchers))
	for _, fn := range l.watchers {
		fns = append(fns, fn)
	}
	l.mu.Unlock()
	for _, n := range ns {
		for _, fn := range fns {
			fn(n)
		}
	}
}
