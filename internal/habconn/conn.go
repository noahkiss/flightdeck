package habconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// Defaults from docs/specs/hab-logic-api.md 3.5 and gff-client.md 11.5.
const (
	DefaultPingInterval = 15 * time.Second
	DefaultDeadAfter    = 45 * time.Second
	DefaultMinBackoff   = 500 * time.Millisecond
	DefaultMaxBackoff   = 30 * time.Second
	DefaultOpenTimeout  = 20 * time.Second
)

// Config is what Connect needs. Transport and Client are required.
type Config struct {
	Transport Transport
	Client    api.ClientInfo
	Label     string
	// Scope is the subscription scope. Default: the whole host.
	Scope api.Scope
	// DeviceID, when set, is the pinned device id: a welcome with another id stops the Conn
	// with a *DeviceChangedError. Empty: the first welcome's id is pinned for the Conn's life.
	DeviceID string

	PingInterval time.Duration // hab.ping after this long with nothing sent
	DeadAfter    time.Duration // a connection that delivers nothing for this long is dead
	MinBackoff   time.Duration // the first retry is at once, the second after about this long
	MaxBackoff   time.Duration // retries are at most this far apart, before jitter
	// OpenTimeout bounds one dial plus hello (and subscribe, or data welcome).
	OpenTimeout time.Duration
	Logf        func(format string, args ...any)
}

// UpdateKind says what an Update carries.
type UpdateKind int

const (
	// UpdateOnline: a control connection is up and its snapshot is applied. Welcome,
	// Structure and NewEpoch are set. A client with NewEpoch drops everything not saved
	// (owners it assumed, desired sizes, activity) and redraws from Structure.
	UpdateOnline UpdateKind = iota + 1
	// UpdateEvent: one event that passed the seq and revision rules, in seq order. Changed
	// says whether it changed the structure copy; ephemeral events never do.
	UpdateEvent
	// UpdateOffline: the control connection ended; Err says why. The Conn reconnects.
	UpdateOffline
	// UpdateFailed: the Conn stopped for good; Err says why. The channel closes after it.
	UpdateFailed
)

// Update is one change a Conn reports, in order.
type Update struct {
	Kind      UpdateKind
	Welcome   api.Welcome
	NewEpoch  bool
	Structure api.Structure
	Event     api.Event
	Changed   bool
	Err       error
}

// Conn is one hab: a control connection kept up across drops, the structure copy, and the
// data streams opened through it.
type Conn struct {
	cfg     Config
	updates *queue[Update]
	closed  chan struct{}
	retry   chan struct{}
	once    sync.Once

	mu       sync.Mutex
	cur      *control.Client // nil while offline
	online   chan struct{}   // closed while cur is set
	welcome  api.Welcome
	epoch    string
	token    string
	deviceID string
	state    *state
	err      error // why the Conn stopped
	panes    map[*PaneStream]struct{}
	sessions int // control connections that came online
}

// Connect opens the first control connection, says hello, subscribes and applies the
// snapshot. It fails when that does not work; after it returns, the Conn reconnects on its
// own until Close or a permanent failure.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.Transport == nil {
		return nil, errors.New("habconn: no transport")
	}
	if cfg.Scope.Kind == "" {
		cfg.Scope.Kind = api.ScopeKindHost
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = DefaultPingInterval
	}
	if cfg.DeadAfter <= 0 {
		cfg.DeadAfter = DefaultDeadAfter
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = DefaultOpenTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	c := &Conn{
		cfg:      cfg,
		updates:  newQueue[Update](),
		closed:   make(chan struct{}),
		retry:    make(chan struct{}, 1),
		online:   make(chan struct{}),
		deviceID: cfg.DeviceID,
		state:    newState(),
		panes:    map[*PaneStream]struct{}{},
	}
	s, err := c.open(ctx)
	if err != nil {
		c.updates.close()
		return nil, err
	}
	go c.run(s)
	return c, nil
}

// Updates delivers every update in order, without loss. It closes after UpdateFailed or
// Close.
func (c *Conn) Updates() <-chan Update { return c.updates.out }

// Structure returns a copy of the hab's structure as the client holds it.
func (c *Conn) Structure() api.Structure {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.structure()
}

// Welcome returns the last welcome and whether a control connection is up now.
func (c *Conn) Welcome() (api.Welcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.welcome, c.cur != nil
}

// Err says why the Conn stopped: nil while it runs.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Retry cuts a reconnect's backoff short, for a wake from sleep or a network change
// (gff-client.md 11.5).
func (c *Conn) Retry() {
	select {
	case c.retry <- struct{}{}:
	default:
	}
}

// Close ends the control connection and every data stream.
func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.closed)
		c.mu.Lock()
		cur := c.cur
		panes := make([]*PaneStream, 0, len(c.panes))
		for p := range c.panes {
			panes = append(panes, p)
		}
		if c.err == nil {
			c.err = ErrClosed
		}
		c.mu.Unlock()
		if cur != nil {
			cur.Close()
		}
		for _, p := range panes {
			p.Close()
		}
	})
}

// Call sends one request on the control connection. Offline, it fails with ErrOffline at
// once. An error answer is a *control.Error. Conn implements api.Caller.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	cur := c.cur
	c.mu.Unlock()
	if cur == nil {
		return ErrOffline
	}
	return cur.Call(ctx, method, params, result)
}

// API returns the typed methods over Call.
func (c *Conn) API() api.Client { return api.Client{Caller: c} }

// waitOnline returns the control client once one is up.
func (c *Conn) waitOnline(ctx context.Context) (*control.Client, error) {
	for {
		c.mu.Lock()
		cur, online, err := c.cur, c.online, c.err
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if cur != nil {
			return cur, nil
		}
		select {
		case <-online:
		case <-c.closed:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// session is one control connection that came online.
type session struct {
	cl       *control.Client
	lastRead *atomic.Int64 // unix nanos of the last byte read
	notes    []control.Notification
	floor    uint64 // the subscribe response's seq: events at or below it are in the snapshot
	lastSeq  uint64 // the last event seq seen on this connection
}

// errPermanent marks errors after which reconnecting cannot help.
type errPermanent struct{ error }

func (e errPermanent) Unwrap() error { return e.error }

// open dials, says hello and subscribes. Notifications that arrive before the subscribe
// response are kept for serve, which applies those above the response's seq.
func (c *Conn) open(ctx context.Context) (*session, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.OpenTimeout)
	defer cancel()
	rw, err := c.cfg.Transport.Dial(ctx, KindControl)
	if err != nil {
		return nil, fmt.Errorf("habconn: dial: %w", err)
	}
	s := &session{lastRead: &atomic.Int64{}}
	s.lastRead.Store(time.Now().UnixNano())
	cl := control.NewClient(&stampRW{ReadWriteCloser: rw, last: s.lastRead})
	s.cl = cl
	// Close ends the dial too: the hello and subscribe calls fail when cl closes.
	opened := make(chan struct{})
	defer close(opened)
	go func() {
		select {
		case <-c.closed:
			cl.Close()
		case <-opened:
		}
	}()

	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	w, err := cl.Hello(ctx, c.cfg.Client, c.cfg.Label, token)
	if err != nil {
		cl.Close()
		var ce *control.Error
		if errors.As(err, &ce) && (ce.Data.Kind == api.ErrorKindRevisionUnsupported || ce.Data.Kind == api.ErrorKindInvalid) {
			return nil, errPermanent{fmt.Errorf("habconn: hello refused: %w", err)}
		}
		if se, ok := rw.(stderrer); ok {
			if msg := se.Stderr(); msg != "" {
				return nil, fmt.Errorf("habconn: hello: %w (%s)", err, msg)
			}
		}
		return nil, fmt.Errorf("habconn: hello: %w", err)
	}
	c.mu.Lock()
	pinned := c.deviceID
	c.mu.Unlock()
	if pinned != "" && w.DeviceID != pinned {
		cl.Close()
		return nil, errPermanent{&DeviceChangedError{Pinned: pinned, Got: w.DeviceID}}
	}
	cl.KeepAlive(c.cfg.PingInterval)

	// Subscribe while collecting notifications: the response and the events travel on
	// different paths inside the client, so events above the response's seq may be read
	// before the call returns.
	type result struct {
		r   api.StructureResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := api.Client{Caller: cl}.EventsSubscribe(ctx, api.SubscribeParams{Scope: c.cfg.Scope})
		done <- result{r, err}
	}()
	notes := cl.Notifications()
	for {
		select {
		case n, ok := <-notes:
			if !ok {
				notes = nil
				continue
			}
			s.notes = append(s.notes, n)
		case res := <-done:
			if res.err != nil {
				cl.Close()
				return nil, fmt.Errorf("habconn: subscribe: %w", res.err)
			}
			s.floor = res.r.Seq.Seq
			c.goOnline(s, w, res.r.Structure)
			return s, nil
		}
	}
}

// goOnline applies the snapshot and makes s the current connection.
func (c *Conn) goOnline(s *session, w api.Welcome, st api.Structure) {
	c.mu.Lock()
	newEpoch := c.epoch != "" && c.epoch != w.Epoch
	c.epoch = w.Epoch
	c.token = w.ReconnectToken
	if c.deviceID == "" {
		c.deviceID = w.DeviceID
	}
	c.welcome = w
	// Every object is replaced, in the same epoch too: events from the gap are not replayed
	// (spec 3.7), and a deleted object is simply absent from the snapshot.
	c.state.reset(st)
	snap := c.state.structure()
	c.cur = s.cl
	close(c.online)
	c.sessions++
	again := c.sessions > 1
	panes := make([]*PaneStream, 0, len(c.panes))
	for p := range c.panes {
		panes = append(panes, p)
	}
	c.updates.push(Update{Kind: UpdateOnline, Welcome: w, NewEpoch: newEpoch, Structure: snap})
	c.mu.Unlock()
	if again {
		// The link that dropped the control connection likely took the data streams with
		// it, and a quiet data stream cannot tell. Each resumes from its offset.
		for _, p := range panes {
			p.kick()
		}
	}
}

// run serves sessions and reconnects until Close or a permanent failure.
func (c *Conn) run(s *session) {
	for {
		err := c.serve(s)
		c.mu.Lock()
		c.cur = nil
		c.online = make(chan struct{})
		c.mu.Unlock()
		select {
		case <-c.closed:
			c.updates.close()
			return
		default:
		}
		c.cfg.Logf("habconn: control connection ended: %v", err)
		c.updates.push(Update{Kind: UpdateOffline, Err: err})
		s, err = c.reconnect()
		if err != nil {
			c.mu.Lock()
			if c.err == nil {
				c.err = err
			}
			panes := make([]*PaneStream, 0, len(c.panes))
			for p := range c.panes {
				panes = append(panes, p)
			}
			c.mu.Unlock()
			for _, p := range panes {
				p.kick()
			}
			if !errors.Is(err, ErrClosed) {
				c.updates.push(Update{Kind: UpdateFailed, Err: err})
			}
			c.updates.close()
			return
		}
	}
}

// reconnect retries at once, then with backoff and jitter, until it is online, closed, or
// refused for good.
func (c *Conn) reconnect() (*session, error) {
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(backoff(c.cfg.MinBackoff, c.cfg.MaxBackoff, attempt))
			select {
			case <-c.closed:
				t.Stop()
				return nil, ErrClosed
			case <-c.retry:
				t.Stop()
			case <-t.C:
			}
		}
		select {
		case <-c.closed:
			return nil, ErrClosed
		default:
		}
		s, err := c.open(context.Background())
		if err == nil {
			return s, nil
		}
		var p errPermanent
		if errors.As(err, &p) {
			return nil, p.error
		}
		c.cfg.Logf("habconn: reconnect: %v", err)
	}
}

// backoff is the wait before retry attempt (1 or more): lo doubling up to hi, then a random
// point in its upper half, so many clients do not retry in step.
func backoff(lo, hi time.Duration, attempt int) time.Duration {
	d := lo
	for i := 1; i < attempt && d < hi; i++ {
		d *= 2
	}
	d = min(d, hi)
	return d/2 + rand.N(d/2+1)
}

// errGap: an event seq skipped a number, which the hab never does on a healthy connection.
var errGap = errors.New("habconn: event seq gap")

// errDead: nothing arrived for DeadAfter.
var errDead = errors.New("habconn: no message from the hab; connection presumed dead")

// serve applies notifications until the connection ends, and says why it ended.
func (c *Conn) serve(s *session) error {
	defer s.cl.Close()
	for _, n := range s.notes {
		if err := c.note(s, n); err != nil {
			return err
		}
	}
	s.notes = nil
	tick := time.NewTicker(c.cfg.DeadAfter / 4)
	defer tick.Stop()
	notes := s.cl.Notifications()
	for {
		select {
		case n, ok := <-notes:
			if !ok {
				return s.cl.Err()
			}
			if err := c.note(s, n); err != nil {
				return err
			}
		case <-tick.C:
			if time.Since(time.Unix(0, s.lastRead.Load())) > c.cfg.DeadAfter {
				return errDead
			}
		case <-c.closed:
			return ErrClosed
		}
	}
}

// note handles one notification: the seq rule, the gap check, the revision rule.
func (c *Conn) note(s *session, n control.Notification) error {
	switch n.Method {
	case "event":
		e, err := n.Event()
		if err != nil {
			return fmt.Errorf("habconn: bad event: %w", err)
		}
		if e.Seq != s.lastSeq+1 {
			return fmt.Errorf("%w: %d after %d", errGap, e.Seq, s.lastSeq)
		}
		s.lastSeq = e.Seq
		if e.Seq <= s.floor {
			return nil // already in the snapshot
		}
		c.mu.Lock()
		_, kept := eventKey(e.Type)
		changed := c.state.apply(e)
		if changed || !kept {
			c.updates.push(Update{Kind: UpdateEvent, Event: e, Changed: changed})
		}
		c.mu.Unlock()
	case "heartbeat":
		var hb api.Heartbeat
		if err := json.Unmarshal(n.Params, &hb); err != nil {
			return fmt.Errorf("habconn: bad heartbeat: %w", err)
		}
		if hb.Seq != s.lastSeq {
			return fmt.Errorf("%w: heartbeat at %d after event %d", errGap, hb.Seq, s.lastSeq)
		}
	case "closing":
		// The connection ends next; the client's error carries the reason.
	}
	return nil
}

// stampRW records when the last read returned bytes.
type stampRW struct {
	io.ReadWriteCloser
	last *atomic.Int64
}

// SetWriteDeadline passes a write deadline through when the stream takes one; otherwise the
// control client falls back to closing the stream on a stuck write.
func (s *stampRW) SetWriteDeadline(t time.Time) error {
	if wd, ok := s.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return wd.SetWriteDeadline(t)
	}
	return errors.ErrUnsupported
}

func (s *stampRW) Read(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Read(p)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// queue hands values to a channel in order without ever blocking the sender.
type queue[T any] struct {
	out  chan T
	mu   sync.Mutex
	q    []T
	wake chan struct{}
	done bool
}

func newQueue[T any]() *queue[T] {
	q := &queue[T]{out: make(chan T), wake: make(chan struct{}, 1)}
	go q.pump()
	return q
}

func (q *queue[T]) push(v T) {
	q.mu.Lock()
	if !q.done {
		q.q = append(q.q, v)
	}
	q.mu.Unlock()
	q.signal()
}

func (q *queue[T]) close() {
	q.mu.Lock()
	q.done = true
	q.mu.Unlock()
	q.signal()
}

func (q *queue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue[T]) pump() {
	defer close(q.out)
	for {
		q.mu.Lock()
		batch, done := q.q, q.done
		q.q = nil
		q.mu.Unlock()
		for _, v := range batch {
			q.out <- v
		}
		if len(batch) > 0 {
			continue
		}
		if done {
			return
		}
		<-q.wake
	}
}
