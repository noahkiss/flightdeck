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
//
// The parser is injected (vt.Parser), so this package is pure Go.
package link

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// Config sets up a link. Zero durations and sizes take the defaults below.
type Config struct {
	// HolderDir is the holder registry directory (holder spec 4.2).
	HolderDir string
	// StateDir holds the checkpoint files, one <pane uuid>.ckpt per pane. It is created if
	// missing.
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
	// BufferBytes is the recent output kept per pane for subscribers that resume (spec 5.7).
	// Default 4 MiB.
	BufferBytes int
	// ScrollbackBytes caps each parser's scrollback. Zero keeps the parser's default.
	ScrollbackBytes int
	// Logf receives diagnostics. Nil discards them.
	Logf func(format string, args ...any)
}

// Errors returned by the link.
var (
	ErrNoPane = errors.New("link: no such pane")
	ErrLost   = errors.New("link: pane lost: its holder connection ended")
	ErrGone   = errors.New("link: pane gone")
	ErrClosed = errors.New("link: closed")
	ErrAhead  = errors.New("link: offset is past the pane's output")
	ErrSlow   = errors.New("link: subscriber fell behind")
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

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	rescan chan struct{}
	tasks  chan func() // run on the event loop, in order with holder frames

	mu       sync.Mutex
	panes    map[hp.UUID]*pane
	byHandle map[paneKey]*pane
	holders  map[*holderclient.Holder]bool
	orphans  map[paneKey][]hp.Frame // frames for a pane not attached yet (a spawn in flight)
	watchers map[int]func(Notice)
	nextW    int
	closed   bool
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
	if cfg.BufferBytes <= 0 {
		cfg.BufferBytes = 4 << 20
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	removeTemps(cfg.StateDir)
	encs := cfg.Parser.Encodings()
	if len(encs) == 0 {
		return nil, errors.New("link: parser has no snapshot encoding")
	}
	enc := encs[0]
	for _, e := range encs {
		if e == vt.EncodingGhostty { // keeps the most state, and the whole scrollback
			enc = e
		}
	}
	l := &Link{
		cfg: cfg,
		client: holderclient.New(holderclient.Config{
			Dir: cfg.HolderDir, Build: cfg.Build, Protocol: cfg.Protocol, Logf: cfg.Logf,
		}),
		enc:      enc,
		rescan:   make(chan struct{}, 1),
		tasks:    make(chan func()),
		panes:    map[hp.UUID]*pane{},
		byHandle: map[paneKey]*pane{},
		holders:  map[*holderclient.Holder]bool{},
		orphans:  map[paneKey][]hp.Frame{},
		watchers: map[int]func(Notice){},
	}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.wg.Add(1)
	go l.eventLoop()
	l.scan()
	l.wg.Add(2)
	go l.rescanLoop()
	go l.checkpointLoop()
	return l, nil
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
// holder of this build is connected, and attaches the panes of every new holder.
func (l *Link) scan() {
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	defer cancel()
	if err := l.client.Scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
		l.cfg.Logf("link: scan: %v", err)
	}
	if l.client.Current() != nil {
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
	case *hp.ReplayData:
		p.feed(l, f.Offset, f.Data, false)
	case *hp.ReplayEnd:
		p.replayEnd(l, f)
	case *hp.Exited:
		if n, ok := p.reaped(f.Status); ok {
			notices = append(notices, n)
		}
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
	} else {
		l.cfg.Logf("link: holder %s connection ended: %v; %d panes lost", h.Instance(), err, len(ps))
	}
	var notices []Notice
	for _, p := range ps {
		if n, ok := p.lose(why); ok {
			notices = append(notices, n)
		}
		if why == ErrGone {
			removeCheckpoint(l.cfg.StateDir, p.uuid)
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
		p = newPane(pi.UUID)
		l.panes[pi.UUID] = p
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
	for _, f := range orphans {
		l.apply(p, f)
	}
	if fresh || back {
		l.notify([]Notice{{Kind: PaneAdded, Pane: p.status()}})
	}
}

// Spawn starts a program in a new pane on the current holder and follows its output from the
// first byte.
func (l *Link) Spawn(ctx context.Context, s hp.Spawn) (PaneStatus, error) {
	h := l.client.Current()
	if h == nil {
		return PaneStatus{}, holderclient.ErrNoHolder
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
	var added bool
	ok := l.run(func() {
		l.mu.Lock()
		if p := l.panes[pn.UUID]; p != nil {
			l.mu.Unlock()
			st = p.status() // a retry of a spawn that already ran
			return
		}
		p := newPane(pn.UUID)
		p.spawned(l, h, pi)
		l.panes[pn.UUID] = p
		k := paneKey{h, pn.Handle}
		l.byHandle[k] = p
		orphans := l.orphans[k]
		delete(l.orphans, k)
		l.mu.Unlock()
		for _, f := range orphans {
			l.apply(p, f)
		}
		st, added = p.status(), true
	})
	if !ok {
		return PaneStatus{}, ErrClosed
	}
	if added {
		l.notify([]Notice{{Kind: PaneAdded, Pane: st}})
	}
	return st, nil
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
