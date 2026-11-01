package panesize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeClock runs timers only when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	live := !t.stopped && !t.fired
	t.stopped = true
	return live
}

// pending counts the timers that have neither fired nor stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// advance moves the clock by d and runs every timer due by then, in time order, on this
// goroutine.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t)
		}
	}
	c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool { return t.fired || t.stopped })
	c.mu.Unlock()
	slices.SortStableFunc(due, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
	for _, t := range due {
		t.f()
	}
}

// rig is a Manager over a fake PTY and a fake clock.
type rig struct {
	t     *testing.T
	m     *Manager
	clock *fakeClock

	mu        sync.Mutex
	sizes     map[string]size
	resizes   []string // "pane CxR" in order
	events    []string // "pane owner reason CxR" in order
	observers map[string]bool
	snapshots []string
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, clock: &fakeClock{now: time.Unix(1000, 0)},
		sizes: map[string]size{"p": {80, 24}}, observers: map[string]bool{}}
	r.m = New(Config{
		Size: func(pane string) (int, int, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			s, ok := r.sizes[pane]
			return s.cols, s.rows, ok
		},
		Resize: func(_ context.Context, pane string, cols, rows int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if _, ok := r.sizes[pane]; !ok {
				return errors.New("no pane")
			}
			r.sizes[pane] = size{cols, rows}
			r.resizes = append(r.resizes, fmt.Sprintf("%s %dx%d", pane, cols, rows))
			return nil
		},
		Owner: func(pane string, o api.PaneOwner) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, fmt.Sprintf("%s %s %s %dx%d", pane, o.Owner, o.Reason, o.Cols, o.Rows))
		},
		Observer: func(client, pane string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.observers[client+" "+pane]
		},
		Snapshot: func(client, pane string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.snapshots = append(r.snapshots, client+" "+pane)
		},
		Clock: r.clock,
		Logf:  t.Logf,
	})
	t.Cleanup(r.m.Close)
	return r
}

func waitScale() time.Duration {
	if f, err := strconv.ParseFloat(os.Getenv("GFF_TEST_TIMEOUT_SCALE"), 64); err == nil && f > 0 {
		return time.Duration(f * float64(5*time.Second))
	}
	return 5 * time.Second
}

// waitFor waits until cond holds. The resize worker runs on its own goroutine, so a test waits
// for its result rather than for time to pass.
func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(waitScale())
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s; resizes %q, events %q", what, r.resizeLog(), r.eventLog())
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *rig) resizeLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.resizes)
}

func (r *rig) eventLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *rig) size(pane string) size {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sizes[pane]
}

// waitSize waits for the PTY size, then for the worker to settle.
func (r *rig) waitSize(cols, rows int) {
	r.t.Helper()
	r.waitFor(fmt.Sprintf("size %dx%d", cols, rows), func() bool { return r.size("p") == size{cols, rows} })
	r.idle()
}

// idle waits until no resize worker runs.
func (r *rig) idle() {
	r.t.Helper()
	r.waitFor("the resize worker to stop", func() bool {
		r.m.mu.Lock()
		defer r.m.mu.Unlock()
		for _, st := range r.m.panes {
			if st.busy {
				return false
			}
		}
		return true
	})
}

func (r *rig) wantEvents(want ...string) {
	r.t.Helper()
	if got := r.eventLog(); !slices.Equal(got, want) {
		r.t.Fatalf("owner events:\n got %q\nwant %q", got, want)
	}
}

func (r *rig) wantResizes(want ...string) {
	r.t.Helper()
	if got := r.resizeLog(); !slices.Equal(got, want) {
		r.t.Fatalf("resizes:\n got %q\nwant %q", got, want)
	}
}

// TestTypingMovesOwnership is the stage's first acceptance test at the lowest layer: B types and
// becomes the owner after the debounce, with its desired size; A's next keystroke takes it back
// after the debounce; each change emits exactly one event with its reason.
func TestTypingMovesOwnership(t *testing.T) {
	r := newRig(t)
	r.m.Connected("A")
	r.m.Connected("B")
	r.m.Created("A", "p")
	if ok, err := r.m.SetDesired("B", "p", 100, 30); ok || err != nil {
		t.Fatalf("B's size while A owns the pane: applied %v, %v", ok, err)
	}

	r.m.Typed("B", "p")
	r.clock.advance(DefaultDebounce - time.Millisecond)
	r.m.Typed("B", "p") // steady typing does not restart the debounce
	r.wantEvents("p A create 80x24")
	r.clock.advance(time.Millisecond)
	r.wantEvents("p A create 80x24", "p B typed 100x30")
	r.waitSize(100, 30)

	r.m.Typed("B", "p") // the owner typing changes nothing
	r.clock.advance(time.Second)

	r.m.Typed("A", "p")
	r.clock.advance(DefaultDebounce)
	r.waitSize(80, 24)
	r.clock.advance(time.Second)
	r.wantEvents("p A create 80x24", "p B typed 100x30", "p A typed 80x24")
	r.wantResizes("p 100x30", "p 80x24")
}

// TestDebounceLatestSource: ownership goes to a typist only while it is still the latest source
// of input when the debounce ends.
func TestDebounceLatestSource(t *testing.T) {
	r := newRig(t)
	for _, c := range []string{"A", "B", "C"} {
		r.m.Connected(c)
	}
	r.m.Created("A", "p")

	// The owner types during B's debounce: B never takes it.
	r.m.Typed("B", "p")
	r.clock.advance(100 * time.Millisecond)
	r.m.Typed("A", "p")
	r.clock.advance(time.Second)
	r.wantEvents("p A create 80x24")

	// C types during B's debounce: C's own debounce decides.
	r.m.Typed("B", "p")
	r.clock.advance(200 * time.Millisecond)
	r.m.Typed("C", "p")
	r.clock.advance(200 * time.Millisecond) // B's debounce would have ended
	r.wantEvents("p A create 80x24")
	r.clock.advance(100 * time.Millisecond)
	r.wantEvents("p A create 80x24", "p C typed 80x24")
	r.wantResizes()
}

// TestObserverNeverResizes: a client that follows a pane through observer streams only never
// changes its size, by setting a size, typing, taking or nudging.
func TestObserverNeverResizes(t *testing.T) {
	r := newRig(t)
	r.m.Connected("O")
	r.m.Connected("N")
	r.mu.Lock()
	r.observers["O p"] = true
	r.mu.Unlock()

	if ok, err := r.m.SetDesired("O", "p", 40, 10); ok || err != nil {
		t.Fatalf("observer's size on a pane with no owner: applied %v, %v", ok, err)
	}
	r.m.Typed("O", "p")
	r.clock.advance(time.Second)
	var pe *Error
	if _, err := r.m.Take("O", "p"); !errors.As(err, &pe) || pe.Kind != api.ErrorKindInvalid {
		t.Fatalf("observer take: %v", err)
	}
	if _, err := r.m.Request("O", "p"); !errors.As(err, &pe) || pe.Kind != api.ErrorKindInvalid {
		t.Fatalf("observer request: %v", err)
	}
	if err := r.m.Redraw("O", "p", api.RedrawModeNudge); !errors.As(err, &pe) || pe.Kind != api.ErrorKindInvalid {
		t.Fatalf("observer nudge: %v", err)
	}
	if err := r.m.Redraw("O", "p", api.RedrawModeSnapshot); err != nil {
		t.Fatalf("observer snapshot redraw: %v", err)
	}
	r.idle()
	r.wantEvents()
	r.wantResizes()

	// A client with a full view sets the size of a pane with no owner.
	if ok, err := r.m.SetDesired("N", "p", 90, 30); !ok || err != nil {
		t.Fatalf("size on a pane with no owner: applied %v, %v", ok, err)
	}
	r.waitSize(90, 30)
	r.wantEvents()
}

// TestDisconnectAndRestore: an owner that leaves keeps the pane's size and has it back when it
// reconnects within the grace period, unless another client took it meanwhile.
func TestDisconnectAndRestore(t *testing.T) {
	r := newRig(t)
	r.m.Connected("A")
	r.m.Connected("B")
	r.m.Created("A", "p")
	r.m.SetDesired("A", "p", 100, 30)
	r.waitSize(100, 30)

	// A second connection of A (a reconnect while the old one looks open) closes: no change.
	r.m.Connected("A")
	r.m.Disconnected("A")
	r.wantEvents("p A create 80x24")

	r.m.Disconnected("A")
	r.m.Connected("A")
	r.wantEvents("p A create 80x24", "p  disconnect 100x30", "p A restore 100x30")

	// B takes the pane while A is away: A comes back with no ownership.
	r.m.Disconnected("A")
	r.m.Take("B", "p")
	r.m.Disown("B", "p")
	r.m.Connected("A")
	r.wantEvents("p A create 80x24", "p  disconnect 100x30", "p A restore 100x30",
		"p  disconnect 100x30", "p B take 100x30", "p  disown 100x30")

	// Past the grace period the claim and the sizes are gone.
	r.m.Take("A", "p")
	r.m.Disconnected("A")
	r.clock.advance(DefaultGrace)
	r.m.Connected("A")
	r.m.mu.Lock()
	sizes := len(r.m.panes["p"].sizes)
	r.m.mu.Unlock()
	if sizes != 0 { // B never set one
		t.Fatalf("desired sizes after A's grace period: %d, want 0", sizes)
	}
	r.wantEvents("p A create 80x24", "p  disconnect 100x30", "p A restore 100x30",
		"p  disconnect 100x30", "p B take 100x30", "p  disown 100x30",
		"p A take 100x30", "p  disconnect 100x30")
	r.wantResizes("p 100x30")
}

// TestExplicitControl: take, request and disown, and their refusals.
func TestExplicitControl(t *testing.T) {
	r := newRig(t)
	r.m.Connected("A")
	r.m.Connected("B")
	r.m.Created("A", "p")
	if o, err := r.m.Request("B", "p"); o != "A" || err != nil {
		t.Fatalf("request while A owns: %q, %v", o, err)
	}
	var pe *Error
	if _, err := r.m.Disown("B", "p"); !errors.As(err, &pe) || pe.Kind != api.ErrorKindNotOwner {
		t.Fatalf("disown by a client that does not own: %v", err)
	}
	if o, err := r.m.Disown("A", "p"); o != "" || err != nil {
		t.Fatalf("disown: %q, %v", o, err)
	}
	if o, err := r.m.Request("B", "p"); o != "B" || err != nil {
		t.Fatalf("request with no owner: %q, %v", o, err)
	}
	if o, err := r.m.Take("A", "p"); o != "A" || err != nil {
		t.Fatalf("take: %q, %v", o, err)
	}
	if o, err := r.m.Take("A", "p"); o != "A" || err != nil {
		t.Fatalf("take again: %q, %v", o, err)
	}
	if _, err := r.m.Take("A", "nope"); !errors.As(err, &pe) || pe.Kind != api.ErrorKindNotFound {
		t.Fatalf("take of an unknown pane: %v", err)
	}
	if _, err := r.m.SetDesired("A", "p", 0, 10); !errors.As(err, &pe) || pe.Kind != api.ErrorKindInvalid {
		t.Fatalf("size 0x10: %v", err)
	}
	r.wantEvents("p A create 80x24", "p  disown 80x24", "p B request 80x24", "p A take 80x24")
}

// TestOwnerSizeThrottled: a burst of sizes from the owner reaches the PTY as the first at once
// and the last after the throttle, trailing.
func TestOwnerSizeThrottled(t *testing.T) {
	r := newRig(t)
	r.m.Connected("A")
	r.m.Created("A", "p")
	r.m.SetDesired("A", "p", 81, 24)
	r.waitSize(81, 24)
	for c := 82; c <= 90; c++ {
		r.m.SetDesired("A", "p", c, 24)
	}
	r.waitFor("the throttle timer", func() bool { return r.clock.pending() > 0 })
	r.wantResizes("p 81x24")
	r.clock.advance(DefaultThrottle)
	r.waitSize(90, 24)
	r.wantResizes("p 81x24", "p 90x24")
}

// TestNoOwnerDropsPendingSize: a size the owner wanted that still waits in the throttle never
// reaches the PTY once the pane has no owner: the pane keeps the size it has (spec 6.5), after a
// disconnect and after a disown alike.
func TestNoOwnerDropsPendingSize(t *testing.T) {
	for _, leave := range []string{"disconnect", "disown"} {
		t.Run(leave, func(t *testing.T) {
			r := newRig(t)
			r.m.Connected("A")
			r.m.Created("A", "p")
			r.m.SetDesired("A", "p", 81, 24)
			r.waitSize(81, 24)
			r.m.SetDesired("A", "p", 90, 24) // held by the throttle
			r.waitFor("the throttle timer", func() bool { return r.clock.pending() > 0 })
			if leave == "disconnect" {
				r.m.Disconnected("A")
			} else if _, err := r.m.Disown("A", "p"); err != nil {
				t.Fatal(err)
			}
			r.clock.advance(DefaultThrottle)
			r.idle()
			r.wantResizes("p 81x24")
			r.wantEvents("p A create 80x24", "p  "+leave+" 81x24")
			if got := r.size("p"); got != (size{81, 24}) {
				t.Fatalf("size after %s: %v, want 81x24", leave, got)
			}
		})
	}
}

// TestRedraw: snapshot mode asks for a snapshot and touches no size; nudge mode resizes one
// column narrower and back after the gap.
func TestRedraw(t *testing.T) {
	r := newRig(t)
	r.m.Connected("A")
	if err := r.m.Redraw("A", "p", ""); err != nil {
		t.Fatal(err)
	}
	r.idle()
	r.wantResizes()
	if err := r.m.Redraw("A", "p", api.RedrawModeNudge); err != nil {
		t.Fatal(err)
	}
	r.waitSize(79, 24)
	r.wantResizes("p 79x24")
	r.clock.advance(DefaultNudgeGap)
	r.waitSize(80, 24)
	r.wantResizes("p 79x24", "p 80x24")
	r.mu.Lock()
	snaps := slices.Clone(r.snapshots)
	r.mu.Unlock()
	if !slices.Equal(snaps, []string{"A p", "A p"}) {
		t.Fatalf("snapshots: %q", snaps)
	}
	var pe *Error
	if err := r.m.Redraw("A", "p", "wipe"); !errors.As(err, &pe) || pe.Kind != api.ErrorKindInvalid {
		t.Fatalf("unknown mode: %v", err)
	}
	r.wantEvents()
}
