package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

// Defaults of modules.md 8.3.
const (
	// DefaultPauseAfter is how many failures in a row pause a module.
	DefaultPauseAfter = 3
	// DefaultLogLines is how many lines the log tail keeps per module.
	DefaultLogLines = 200
)

// ErrPaused is what a PausedError wraps: the module failed too often in a row and runs no call
// until Retry (modules.md 8.3).
var ErrPaused = errors.New("paused after errors")

// PausedError is a call refused because the module is paused. Last is the failure that paused
// it.
type PausedError struct {
	Failures int
	Last     error
}

func (e *PausedError) Error() string {
	if e.Last == nil {
		return "Paused after errors"
	}
	return "Paused after errors: " + e.Last.Error()
}

func (e *PausedError) Unwrap() error { return ErrPaused }

// Loader makes the module's Runner: Host.New or New with the owner's bundle and options. log is
// the Managed's log tail; the owner passes it as Options.Log, so fd.log lines land in the tail.
// A Managed calls it at its first call, and again after the module was stopped or Reload.
type Loader func(ctx context.Context, log func(line string)) (Runner, error)

// ManagedOptions configure a Managed. Zero values take the defaults of modules.md 8.3.
type ManagedOptions struct {
	PauseAfter int // 3
	LogLines   int // 200
	// Log, when set, also receives each line of the tail as it is added.
	Log func(line string)
	// Changed is called after the status or a tree changed, outside the Managed's locks.
	Changed func()
	Now     func() time.Time // time.Now
}

// Tree is a module's last good tree for one place and vocabulary.
type Tree struct {
	JSON []byte
	// Made is when render returned it, by this host's clock: stale_after counts from it.
	Made time.Time
	// Revision rises with each good tree of the module, over every place and vocabulary, so a
	// client keeps the tree with the higher revision.
	Revision uint64
}

// Status is a module's health (modules.md 8.3).
type Status struct {
	// Failures is the count of failures in a row; a success sets it to zero.
	Failures int
	Paused   bool
	// Err is the last failure; nil after a success, or before any call.
	Err error
}

// Managed runs one module with the failure rules of modules.md 8.3: it counts failures in a
// row, pauses the module after three, keeps the last good tree of each place and vocabulary,
// and keeps the module's log tail across reloads. It loads the module again after a call
// stopped it (ErrStopped). Its calls run one at a time, in arrival order; its other methods
// never wait for a call.
//
// What counts as a failure:
//   - refresh and render: any error, except ErrHeapPressure (another module holds the memory),
//     ClientTooOld (the client's), a HostError that is no culprit (the host was down for
//     another reason), ErrHostClosed and the caller's own cancel.
//   - act: only an error that stops the module (MemoryError, InternalError, a stuck
//     TimeoutError), a culprit HostError, and a failed load. An act's own exception or
//     timeout goes back to the tap ("Did not finish") and counts nothing; the refresh after it
//     counts. Nor does a good act reset the count.
type Managed struct {
	load Loader
	o    ManagedOptions

	run sync.Mutex // one call at a time
	r   Runner     // under run; nil before the first load and after a stop

	mu       sync.Mutex
	failures int
	paused   bool
	last     error
	trees    map[treeKey]Tree
	rev      uint64
	logs     []string // a ring of o.LogLines
	logAt    int      // the next slot once the ring is full
	closed   bool
	reload   bool // drop the runner before the next call
}

type treeKey struct {
	place string
	vocab int
}

// NewManaged returns a Managed that loads its module with load at its first call.
func NewManaged(load Loader, o ManagedOptions) *Managed {
	if o.PauseAfter <= 0 {
		o.PauseAfter = DefaultPauseAfter
	}
	if o.LogLines <= 0 {
		o.LogLines = DefaultLogLines
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Managed{load: load, o: o, trees: map[treeKey]Tree{}}
}

// Log adds a line to the module's log tail. It is the log function the Loader gets.
func (m *Managed) Log(line string) {
	m.mu.Lock()
	if len(m.logs) < m.o.LogLines {
		m.logs = append(m.logs, line)
	} else {
		m.logs[m.logAt] = line
		m.logAt = (m.logAt + 1) % m.o.LogLines
	}
	m.mu.Unlock()
	if m.o.Log != nil {
		m.o.Log(line)
	}
}

// Logs returns the log tail, oldest first: the module's fd.log lines and the lines the Managed
// adds for each failure, pause and retry.
func (m *Managed) Logs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.logs))
	out = append(out, m.logs[m.logAt:]...)
	return append(out, m.logs[:m.logAt]...)
}

// Status returns the module's health.
func (m *Managed) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{Failures: m.failures, Paused: m.paused, Err: m.last}
}

// Tree returns the last good tree for the call's place and vocabulary.
func (m *Managed) Tree(c Call) (Tree, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.trees[keyOf(c)]
	return t, ok
}

// Retry ends a pause and sets the count to zero; the next call runs (modules.md 8.3's Retry
// button). A module that is not paused is left as it is.
func (m *Managed) Retry() {
	m.mu.Lock()
	if !m.paused {
		m.mu.Unlock()
		return
	}
	m.paused, m.failures = false, 0
	m.mu.Unlock()
	m.Log("gff-hab: retry after the pause")
	m.changed()
}

// Reload drops the loaded module, so the next call loads it again: a new version, or changed
// settings in the Loader's options. It ends a pause and sets the count to zero; the trees and
// the log tail stay.
func (m *Managed) Reload() {
	m.mu.Lock()
	m.reload = true
	changed := m.paused || m.failures > 0
	m.paused, m.failures = false, 0
	m.mu.Unlock()
	if changed {
		m.changed()
	}
}

// Close unloads the module. Every later call returns ErrHostClosed.
func (m *Managed) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.run.Lock()
	defer m.run.Unlock()
	m.drop()
}

// Refresh runs the module's refresh.
func (m *Managed) Refresh(ctx context.Context, c Call) error {
	return m.call(ctx, "refresh", func(r Runner) error { return r.Refresh(ctx, c) })
}

// Render runs render and keeps the tree. On a failure it returns the last good tree for the
// call's place and vocabulary, if there is one, beside the error: the client shows it with the
// error mark (modules.md 8.3).
func (m *Managed) Render(ctx context.Context, c Call) (Tree, error) {
	var tree []byte
	err := m.call(ctx, "render", func(r Runner) error {
		var err error
		tree, err = r.Render(ctx, c)
		return err
	})
	m.mu.Lock()
	k := keyOf(c)
	if err == nil {
		m.rev++
		m.trees[k] = Tree{JSON: tree, Made: m.o.Now(), Revision: m.rev}
	}
	t := m.trees[k]
	m.mu.Unlock()
	if err == nil {
		m.changed()
	}
	return t, err
}

// Act runs act(name, args, ctx).
func (m *Managed) Act(ctx context.Context, name string, args json.RawMessage, c Call) error {
	return m.call(ctx, "act", func(r Runner) error { return r.Act(ctx, name, args, c) })
}

func (m *Managed) call(ctx context.Context, fn string, f func(Runner) error) error {
	m.run.Lock()
	defer m.run.Unlock()
	m.mu.Lock()
	closed, paused, n, last, reload := m.closed, m.paused, m.failures, m.last, m.reload
	m.reload = false
	m.mu.Unlock()
	if closed {
		return ErrHostClosed
	}
	if paused {
		return &PausedError{Failures: n, Last: last}
	}
	if reload {
		m.drop()
	}
	if m.r == nil {
		r, err := m.load(ctx, m.Log)
		if err != nil {
			m.fail(ctx, fn, err, true)
			return err
		}
		m.r = r
	}
	err := f(m.r)
	if stops(err) {
		m.drop()
	}
	if err == nil {
		if fn != "act" {
			m.succeed()
		}
		return nil
	}
	m.fail(ctx, fn, err, false)
	return err
}

// drop closes the loaded module, so its id is free for the next load. m.run is held.
func (m *Managed) drop() {
	if c, ok := m.r.(interface{ Close() }); ok {
		c.Close()
	}
	m.r = nil
}

func (m *Managed) succeed() {
	m.mu.Lock()
	changed := m.failures > 0 || m.last != nil
	m.failures, m.last = 0, nil
	m.mu.Unlock()
	if changed {
		m.changed()
	}
}

// fail counts err when it is the module's failure. load is a failure of the Loader.
func (m *Managed) fail(ctx context.Context, fn string, err error, load bool) {
	if !counts(ctx, fn, err, load) {
		return
	}
	m.mu.Lock()
	m.failures++
	m.last = err
	n := m.failures
	pause := n >= m.o.PauseAfter && !m.paused
	if pause {
		m.paused = true
	}
	m.mu.Unlock()
	m.Log(fmt.Sprintf("gff-hab: %s failed (%d in a row): %v", fn, n, err))
	if pause {
		m.Log(fmt.Sprintf("gff-hab: paused after %d failures in a row", n))
	}
	m.changed()
}

func (m *Managed) changed() {
	if m.o.Changed != nil {
		m.o.Changed()
	}
}

func keyOf(c Call) treeKey {
	v := c.Vocabulary
	if v <= 0 || v > modules.Version {
		v = modules.Version
	}
	return treeKey{place: c.Place, vocab: v}
}

// stops reports whether err left the module stopped, so it must be loaded again.
func stops(err error) bool {
	var te *TimeoutError
	var me *MemoryError
	var ie *InternalError
	return errors.Is(err, ErrStopped) || errors.As(err, &me) || errors.As(err, &ie) ||
		(errors.As(err, &te) && te.Stuck)
}

// counts reports whether err is a failure of the module (the rules on Managed).
func counts(ctx context.Context, fn string, err error, load bool) bool {
	var he *HostError
	var tooOld *ClientTooOld
	var te *TimeoutError
	switch {
	case err == nil, errors.Is(err, ErrHostClosed), errors.Is(err, ErrHeapPressure), errors.As(err, &tooOld):
		return false
	case errors.As(err, &he):
		return he.Culprit
	case ctx.Err() != nil && !errors.As(err, &te):
		return false // the caller's own cancel
	case load:
		return true
	case fn == "act":
		return stops(err)
	}
	return true
}
