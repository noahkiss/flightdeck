// Package panesize keeps each pane's size owner (docs/specs/hab-logic-api.md 6): one owner per
// pane, typing takes ownership after a debounce, explicit take, request and disown, each
// client's last desired size, the owner's size applied to the PTY at most once per throttle
// period, the owner restored when it reconnects within the grace period, and the redraw nudge.
//
// The Manager does not know the holder link, the structure store or the data streams. gff-hab
// wires them in through Config. Time comes from Config.Clock, so tests drive the debounce, the
// throttle and the grace period without sleeping.
package panesize

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Defaults from spec 6.2, 6.6, 6.7 and Q8.
const (
	DefaultDebounce = 300 * time.Millisecond
	DefaultThrottle = 50 * time.Millisecond
	DefaultNudgeGap = 100 * time.Millisecond
	DefaultGrace    = 60 * time.Second
	// resizeTimeout bounds one resize of a PTY.
	resizeTimeout = 10 * time.Second
	maxSize       = 0xffff
)

// Clock is the time source. Timer callbacks may run on any goroutine.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc.
type Timer interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Error is a refused call, with the API error kind to answer.
type Error struct {
	Kind api.ErrorKind
	Hint string
}

func (e *Error) Error() string { return e.Hint }

func refuse(kind api.ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Hint: fmt.Sprintf(format, args...)}
}

// Config wires a Manager. Size and Resize are required.
type Config struct {
	// Size returns a pane's PTY size; ok is false for a pane that does not exist.
	Size func(pane string) (cols, rows int, ok bool)
	// Resize sets a pane's PTY size.
	Resize func(ctx context.Context, pane string, cols, rows int) error
	// Owner is called once for every owner change, in the order of the changes, with the new
	// owner (empty for none), the reason and the size the pane gets. It runs under the
	// Manager's lock and must not call the Manager.
	Owner func(pane string, o api.PaneOwner)
	// Observer reports whether client follows pane only through observer data streams
	// (spec 6.4). It runs under the Manager's lock and must not call the Manager. Nil: never.
	Observer func(client, pane string) bool
	// Snapshot sends a fresh snapshot on client's data streams of pane (spec 6.7). Nil: none.
	Snapshot func(client, pane string)

	Debounce time.Duration // typing takes ownership after this; default 300 ms
	Throttle time.Duration // the owner's size reaches the PTY at most once per this; default 50 ms
	NudgeGap time.Duration // a nudge returns to the size after this; default 100 ms
	Grace    time.Duration // a disconnected client's sizes and ownership are kept this long; default 60 s
	Clock    Clock
	Logf     func(format string, args ...any)
}

type size struct{ cols, rows int }

type desired struct {
	client string
	size
}

// paneState is one pane's ownership and its pending resizes.
type paneState struct {
	id     string
	owner  string
	reason api.OwnerReason
	// latest is the latest source of input. cand is the client the debounce runs for; debGen
	// tells a stale debounce timer from the current one.
	latest string
	cand   string
	deb    Timer
	debGen uint64
	sizes  []desired // each client's last desired size, newest first
	// restore is the owner that left by disconnect. Any later owner change clears it.
	restore string

	// The resize worker's state. want is the size to apply next. nudge asks for a nudge; back
	// is the size a nudge returns to. No resize goes out before hold; wake is the timer that
	// runs the worker at hold. busy is set while a worker runs.
	want  *size
	nudge bool
	back  *size
	hold  time.Time
	wake  Timer
	busy  bool
	gone  bool
}

type clientState struct {
	conns  int // open control connections
	forget Timer
	gen    uint64
}

// Manager keeps the size owner of every pane.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	panes   map[string]*paneState
	clients map[string]*clientState
	closed  bool
	wg      sync.WaitGroup
}

// New returns a Manager.
func New(cfg Config) *Manager {
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.Throttle <= 0 {
		cfg.Throttle = DefaultThrottle
	}
	if cfg.NudgeGap <= 0 {
		cfg.NudgeGap = DefaultNudgeGap
	}
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Owner == nil {
		cfg.Owner = func(string, api.PaneOwner) {}
	}
	if cfg.Observer == nil {
		cfg.Observer = func(string, string) bool { return false }
	}
	if cfg.Snapshot == nil {
		cfg.Snapshot = func(string, string) {}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Manager{cfg: cfg, panes: map[string]*paneState{}, clients: map[string]*clientState{}}
}

// Close stops every timer and waits for running resizes.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, st := range m.panes {
		st.stopTimers()
	}
	for _, cs := range m.clients {
		if cs.forget != nil {
			cs.forget.Stop()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// paneLocked returns the state of pane, made on first use. ok is false for a pane that does not
// exist. The caller holds m.mu.
func (m *Manager) paneLocked(pane string) (*paneState, bool) {
	if st := m.panes[pane]; st != nil {
		return st, true
	}
	if m.closed {
		return nil, false
	}
	if _, _, ok := m.cfg.Size(pane); !ok {
		return nil, false
	}
	st := &paneState{id: pane, reason: api.OwnerReasonDisown}
	m.panes[pane] = st
	return st, true
}

func (st *paneState) stopTimers() {
	st.stopDebounce()
	if st.wake != nil {
		st.wake.Stop()
		st.wake = nil
	}
}

func (st *paneState) stopDebounce() {
	if st.deb != nil {
		st.deb.Stop()
		st.deb = nil
	}
	st.cand = ""
	st.debGen++
}

func (st *paneState) desiredOf(client string) *size {
	for _, d := range st.sizes {
		if d.client == client {
			s := d.size
			return &s
		}
	}
	return nil
}

func (st *paneState) setDesired(client string, s size) {
	st.sizes = slices.DeleteFunc(st.sizes, func(d desired) bool { return d.client == client })
	st.sizes = slices.Insert(st.sizes, 0, desired{client: client, size: s})
}

// setOwnerLocked makes owner (empty for none) the pane's owner for reason, sends the one owner
// event, and applies the new owner's desired size, or with no owner drops any size waiting to
// be applied. The caller holds m.mu.
func (m *Manager) setOwnerLocked(st *paneState, owner string, reason api.OwnerReason) {
	st.owner, st.reason, st.restore = owner, reason, ""
	if st.cand == owner && owner != "" {
		st.stopDebounce()
	}
	if owner != "" {
		if d := st.desiredOf(owner); d != nil {
			st.want = d
		}
	} else {
		// No owner: the pane keeps the size it has (spec 6.5). A size the old owner wanted and
		// the throttle still holds is dropped, so it never lands after a disconnect or disown.
		st.want = nil
	}
	target := st.want
	if target == nil {
		c, r, _ := m.cfg.Size(st.id)
		target = &size{c, r}
	}
	m.cfg.Owner(st.id, api.PaneOwner{Owner: owner, Reason: reason, Cols: target.cols, Rows: target.rows})
	m.kickLocked(st)
}

// --- Clients ---

// Connected records an open control connection of client. A client that comes back while it
// has no other connection gets back each pane it owned when it left by disconnect, unless
// another client took the pane meanwhile (reason restore).
func (m *Manager) Connected(client string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.clients[client]
	if cs == nil {
		cs = &clientState{}
		m.clients[client] = cs
	}
	cs.conns++
	if cs.forget != nil {
		cs.forget.Stop()
		cs.forget = nil
	}
	cs.gen++
	if cs.conns != 1 {
		return
	}
	for _, st := range m.sortedPanes() {
		if st.restore == client && st.owner == "" {
			m.setOwnerLocked(st, client, api.OwnerReasonRestore)
		}
	}
}

// Disconnected records that a control connection of client closed. When it was the client's
// last, every pane it owned keeps its size and has no owner (reason disconnect). After the grace
// period the client's desired sizes are forgotten.
func (m *Manager) Disconnected(client string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.clients[client]
	if cs == nil || cs.conns == 0 {
		return
	}
	cs.conns--
	if cs.conns > 0 {
		return
	}
	for _, st := range m.sortedPanes() {
		if st.cand == client {
			st.stopDebounce()
		}
		if st.latest == client {
			st.latest = ""
		}
		if st.owner == client {
			m.setOwnerLocked(st, "", api.OwnerReasonDisconnect)
			st.restore = client
		}
	}
	if m.closed {
		return
	}
	cs.gen++
	gen := cs.gen
	cs.forget = m.cfg.Clock.AfterFunc(m.cfg.Grace, func() { m.forgetClient(client, gen) })
}

// forgetClient drops a client whose grace period ended: its desired sizes and its claim to
// restore (spec 7.2).
func (m *Manager) forgetClient(client string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.clients[client]
	if cs == nil || cs.gen != gen || cs.conns > 0 {
		return
	}
	delete(m.clients, client)
	for _, st := range m.panes {
		st.sizes = slices.DeleteFunc(st.sizes, func(d desired) bool { return d.client == client })
		if st.restore == client {
			st.restore = ""
		}
	}
}

// sortedPanes returns the panes in id order, so events for several panes go out in a stable
// order. The caller holds m.mu.
func (m *Manager) sortedPanes() []*paneState {
	out := make([]*paneState, 0, len(m.panes))
	for _, st := range m.panes {
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b *paneState) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	return out
}

// Forget drops a pane that is gone.
func (m *Manager) Forget(pane string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.panes[pane]; st != nil {
		st.gone = true
		st.stopTimers()
		delete(m.panes, pane)
	}
}

// --- Calls ---

// Typed records input from client to pane (spec 6.2). Input from a client that is not the owner
// starts the debounce; if the client is still the latest source of input when it ends, it
// becomes the owner (reason typed). Input from the owner changes nothing. An observer's input
// never counts.
func (m *Manager) Typed(client, pane string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.paneLocked(pane)
	if !ok || m.cfg.Observer(client, pane) {
		return
	}
	st.latest = client
	if client == st.owner {
		st.stopDebounce()
		return
	}
	if st.deb != nil && st.cand == client {
		return // the debounce already runs for this client
	}
	st.stopDebounce()
	st.cand = client
	gen := st.debGen
	st.deb = m.cfg.Clock.AfterFunc(m.cfg.Debounce, func() { m.debounced(pane, client, gen) })
}

func (m *Manager) debounced(pane, client string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.panes[pane]
	if st == nil || st.debGen != gen || m.closed {
		return
	}
	st.deb, st.cand = nil, ""
	st.debGen++
	if st.latest != client || st.owner == client || m.cfg.Observer(client, pane) {
		return
	}
	m.setOwnerLocked(st, client, api.OwnerReasonTyped)
}

// Created makes client, which created pane, its owner (reason create), with the pane's size as
// the client's desired size.
func (m *Manager) Created(client, pane string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.paneLocked(pane)
	if !ok {
		return
	}
	if c, r, ok := m.cfg.Size(pane); ok {
		st.setDesired(client, size{c, r})
	}
	if st.owner != client {
		m.setOwnerLocked(st, client, api.OwnerReasonCreate)
	}
}

func (m *Manager) checkLocked(client, pane, method string) (*paneState, error) {
	st, ok := m.paneLocked(pane)
	if !ok {
		return nil, refuse(api.ErrorKindNotFound, "no pane %s", pane)
	}
	if m.cfg.Observer(client, pane) {
		return nil, refuse(api.ErrorKindInvalid, "%s: this client follows pane %s through observer streams only, and an observer never takes ownership or resizes", method, pane)
	}
	return st, nil
}

// Take makes client the owner now (reason take). It returns the owner after the call.
func (m *Manager) Take(client, pane string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.checkLocked(client, pane, "pane.take")
	if err != nil {
		return "", err
	}
	if st.owner != client {
		m.setOwnerLocked(st, client, api.OwnerReasonTake)
	}
	return st.owner, nil
}

// Request makes client the owner only if the pane has none (reason request). It returns the
// owner after the call.
func (m *Manager) Request(client, pane string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.checkLocked(client, pane, "pane.request")
	if err != nil {
		return "", err
	}
	if st.owner == "" {
		m.setOwnerLocked(st, client, api.OwnerReasonRequest)
	}
	return st.owner, nil
}

// Disown gives up client's ownership: the pane keeps its size and has no owner (reason disown).
// A client that is not the owner is refused with not_owner.
func (m *Manager) Disown(client, pane string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.paneLocked(pane)
	if !ok {
		return "", refuse(api.ErrorKindNotFound, "no pane %s", pane)
	}
	if st.owner != client {
		return st.owner, refuse(api.ErrorKindNotOwner, "this client is not the size owner of pane %s", pane)
	}
	m.setOwnerLocked(st, "", api.OwnerReasonDisown)
	return "", nil
}

// SetDesired stores client's wanted size for pane. It is applied when client is the owner or the
// pane has no owner, and applied reports that. An observer's size is neither stored nor applied.
func (m *Manager) SetDesired(client, pane string, cols, rows int) (applied bool, err error) {
	if cols < 1 || rows < 1 || cols > maxSize || rows > maxSize {
		return false, refuse(api.ErrorKindInvalid, "size %dx%d: cols and rows must be 1 to %d", cols, rows, maxSize)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.paneLocked(pane)
	if !ok {
		return false, refuse(api.ErrorKindNotFound, "no pane %s", pane)
	}
	if m.cfg.Observer(client, pane) {
		return false, nil
	}
	s := size{cols, rows}
	st.setDesired(client, s)
	if st.owner != client && st.owner != "" {
		return false, nil
	}
	st.want = &s
	m.kickLocked(st)
	return true, nil
}

// Redraw sends a fresh snapshot on client's data streams of pane (mode snapshot, the default).
// Mode nudge then resizes the PTY one column narrower and back after the nudge gap, so the
// program repaints. Neither writes to the program's input.
func (m *Manager) Redraw(client, pane string, mode api.RedrawMode) error {
	if mode == "" {
		mode = api.RedrawModeSnapshot
	}
	if mode != api.RedrawModeSnapshot && mode != api.RedrawModeNudge {
		return refuse(api.ErrorKindInvalid, "redraw mode %q is not one of %v", mode, api.Enums["RedrawMode"])
	}
	m.mu.Lock()
	st, ok := m.paneLocked(pane)
	if !ok {
		m.mu.Unlock()
		return refuse(api.ErrorKindNotFound, "no pane %s", pane)
	}
	if mode == api.RedrawModeNudge && m.cfg.Observer(client, pane) {
		m.mu.Unlock()
		return refuse(api.ErrorKindInvalid, "pane.redraw nudge: this client follows pane %s through observer streams only, and an observer never resizes", pane)
	}
	m.mu.Unlock()
	m.cfg.Snapshot(client, pane)
	if mode == api.RedrawModeNudge {
		m.mu.Lock()
		if !st.gone {
			st.nudge = true
			m.kickLocked(st)
		}
		m.mu.Unlock()
	}
	return nil
}

// --- Resizes ---

// kickLocked starts the pane's resize worker unless one runs or waits for its hold to end. The
// caller holds m.mu.
func (m *Manager) kickLocked(st *paneState) {
	if st.busy || st.wake != nil || st.gone || m.closed {
		return
	}
	if st.want == nil && !st.nudge && st.back == nil {
		return
	}
	st.busy = true
	m.wg.Add(1)
	go m.work(st)
}

// work applies the pane's pending resizes in order: a nudge one column narrower, then the size
// it returns to (or a newer wanted size), then any wanted size. After each resize no other goes
// out before the hold ends: the nudge gap after a nudge, the throttle after the rest. A size
// wanted during the hold is applied when it ends, so the PTY gets the latest size, trailing.
func (m *Manager) work(st *paneState) {
	defer m.wg.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if st.gone || m.closed || (st.want == nil && !st.nudge && st.back == nil) {
			st.busy = false
			return
		}
		if now := m.cfg.Clock.Now(); now.Before(st.hold) {
			st.busy = false
			st.wake = m.cfg.Clock.AfterFunc(st.hold.Sub(now), func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				st.wake = nil
				m.kickLocked(st)
			})
			return
		}
		cols, rows, ok := m.cfg.Size(st.id)
		if !ok {
			st.busy = false
			return
		}
		var to size
		gap := m.cfg.Throttle
		switch {
		case st.back != nil:
			to = *st.back
			if st.want != nil {
				to = *st.want
			}
			st.back, st.want = nil, nil
		case st.nudge:
			st.nudge = false
			to = size{cols - 1, rows}
			if cols <= 1 {
				to.cols = cols + 1
			}
			st.back = &size{cols, rows}
			gap = m.cfg.NudgeGap
		default:
			to = *st.want
			st.want = nil
		}
		if to == (size{cols, rows}) {
			continue
		}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), resizeTimeout)
		err := m.cfg.Resize(ctx, st.id, to.cols, to.rows)
		cancel()
		m.mu.Lock()
		if err != nil {
			m.cfg.Logf("pane %s: resize to %dx%d: %v", st.id, to.cols, to.rows, err)
		}
		st.hold = m.cfg.Clock.Now().Add(gap)
	}
}
