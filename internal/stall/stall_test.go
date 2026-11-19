package stall

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock moves its wall and monotonic times by hand.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (c *fakeClock) read() (time.Time, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall, c.mono
}

// tick moves both clocks by d, as awake time does.
func (c *fakeClock) tick(d time.Duration) {
	c.mu.Lock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
	c.mu.Unlock()
}

// sleep moves the wall clock only, as a machine asleep does.
func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.mu.Unlock()
}

// gate is a loop a test blocks and frees.
type gate struct {
	mu   sync.Mutex
	held chan struct{}
	runs int
}

func (g *gate) run() {
	g.mu.Lock()
	g.runs++
	h := g.held
	g.mu.Unlock()
	if h != nil {
		<-h
	}
}

func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs
}

func (g *gate) block() {
	g.mu.Lock()
	g.held = make(chan struct{})
	g.mu.Unlock()
}

func (g *gate) free() {
	g.mu.Lock()
	if g.held != nil {
		close(g.held)
		g.held = nil
	}
	g.mu.Unlock()
}

type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(f string, a ...any) {
	e.mu.Lock()
	e.log = append(e.log, fmt.Sprintf(f, a...))
	e.mu.Unlock()
}

func (e *events) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

func newTest(g *gate) (*Watchdog, *fakeClock, *events) {
	c := &fakeClock{wall: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	ev := &events{}
	w := New(Config{
		Probes:    []Probe{{Name: "structure", Run: g.run}},
		Clock:     c.read,
		Stalled:   func(n string, d time.Duration) { ev.add("stalled %s %s", n, d) },
		Recovered: func(n string, d time.Duration) { ev.add("recovered %s %s", n, d) },
	})
	return w, c, ev
}

// settle waits until the probe in flight on loop i, which a test freed or never blocked, has
// returned, so the next check sees it answered.
func settle(t *testing.T, w *Watchdog, i int) {
	t.Helper()
	w.mu.Lock()
	l := w.loops[i]
	done, in := l.done, l.inFlight
	w.mu.Unlock()
	if !in {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe did not return")
	}
}

// A blocked loop is reported once, however long it stays blocked, and once more when it
// answers. A second stall within the hour is not reported (diagnostics.md 4.7).
func TestStallReportedOnce(t *testing.T) {
	g := &gate{}
	w, c, ev := newTest(g)
	w.Check() // sends the first probe, answered at once
	settle(t, w, 0)
	g.block()
	c.tick(5 * time.Second)
	w.Check() // notes the answer, sends a probe that blocks
	for range 10 {
		c.tick(5 * time.Second)
		w.Check()
	}
	got := ev.get()
	if len(got) != 1 || got[0] != "stalled structure 10s" {
		t.Fatalf("after 50 s blocked: %q", got)
	}
	g.free()
	settle(t, w, 0)
	c.tick(5 * time.Second)
	w.Check()
	if got := ev.get(); len(got) != 2 || got[1] != "recovered structure 55s" {
		t.Fatalf("after the loop answered: %q", got)
	}
	// A second stall within the hour: silent.
	settle(t, w, 0)
	g.block()
	for range 5 {
		c.tick(5 * time.Second)
		w.Check()
	}
	g.free()
	settle(t, w, 0)
	c.tick(5 * time.Second)
	w.Check()
	settle(t, w, 0)
	if got := ev.get(); len(got) != 2 {
		t.Fatalf("a second stall within the hour was reported: %q", got)
	}
	// After the hour, a stall is reported again.
	c.tick(time.Hour)
	w.Check()
	settle(t, w, 0)
	g.block()
	c.tick(5 * time.Second)
	w.Check()
	for range 3 {
		c.tick(5 * time.Second)
		w.Check()
	}
	g.free()
	if got := ev.get(); len(got) != 3 || got[2] != "stalled structure 10s" {
		t.Fatalf("a stall after the hour: %q", got)
	}
}

// A busy loop that answers each probe within the threshold is never reported, and a long
// operation off the loop (a slow git read, a long hook) is not seen at all: the probe goes
// through the loop, not through the work (4.7, Q8).
func TestBusyLoopNotReported(t *testing.T) {
	g := &gate{}
	w, c, ev := newTest(g)
	off := make(chan struct{})
	go func() { <-off }() // the long operation off the loop
	defer close(off)
	for range 30 {
		w.Check()
		settle(t, w, 0)
		c.tick(9 * time.Second) // each probe answered in under 10 s
	}
	if got := ev.get(); len(got) != 0 {
		t.Fatalf("a loop that answers was reported: %q", got)
	}
	if n := g.count(); n != 30 {
		t.Fatalf("%d probes for 30 checks", n)
	}
}

// A check whose gap spans a sleep of the machine reports nothing, and the wait restarts at the
// wake (12).
func TestSleepSkipped(t *testing.T) {
	g := &gate{}
	w, c, ev := newTest(g)
	g.block()
	defer g.free()
	w.Check() // the probe blocks
	c.tick(5 * time.Second)
	w.Check()
	c.sleep(8 * time.Hour)
	c.tick(5 * time.Second) // 10 s of monotonic time in all, across a sleep
	w.Check()
	if got := ev.get(); len(got) != 0 {
		t.Fatalf("a check across a sleep reported: %q", got)
	}
	c.tick(5 * time.Second)
	w.Check()
	if got := ev.get(); len(got) != 0 {
		t.Fatalf("reported 5 s after the wake: %q", got)
	}
	c.tick(5 * time.Second)
	w.Check()
	if got := ev.get(); len(got) != 1 || got[0] != "stalled structure 10s" {
		t.Fatalf("10 s after the wake: %q", got)
	}
}

// Each loop is probed on its own: one blocked loop does not hold up the other's probes.
func TestLoopsIndependent(t *testing.T) {
	a, b := &gate{}, &gate{}
	c := &fakeClock{}
	ev := &events{}
	w := New(Config{Probes: []Probe{{"structure", a.run}, {"link", b.run}}, Clock: c.read,
		Stalled: func(n string, d time.Duration) { ev.add("stalled %s", n) }})
	a.block()
	defer a.free()
	for range 5 {
		w.Check()
		settle(t, w, 1)
		c.tick(5 * time.Second)
	}
	if got := ev.get(); len(got) != 1 || got[0] != "stalled structure" {
		t.Fatalf("events: %q", got)
	}
	if n := b.count(); n < 4 {
		t.Fatalf("the free loop was probed %d times", n)
	}
}

// Start and Close run the checks on a real ticker.
func TestStartClose(t *testing.T) {
	g := &gate{}
	g.block()
	defer g.free()
	stalled := make(chan string, 1)
	w := New(Config{Probes: []Probe{{"link", g.run}}, Every: 10 * time.Millisecond, After: 50 * time.Millisecond,
		Stalled: func(n string, _ time.Duration) {
			select {
			case stalled <- n:
			default:
			}
		}})
	w.Start()
	defer w.Close()
	select {
	case n := <-stalled:
		if n != "link" {
			t.Fatalf("stalled %q", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no stall reported")
	}
}
