// Package stall is gff-hab's stall watchdog (docs/specs/diagnostics.md 4.7). Every few seconds
// it sends a no-op through each long-lived serialisation point (the structure store's lock, the
// holder link's event loop) and waits for it. A probe still unanswered after a threshold is a
// stall: the watchdog reports it once, and once more when the probe returns.
//
// A probe is work that waits, so "silent while work waits" needs no code on the loops
// themselves: nothing on a hot path changes.
package stall

import (
	"sync"
	"time"
)

// Defaults of 4.7: a check every 5 seconds, a stall after 10 seconds, at most one report per
// loop an hour.
const (
	DefaultEvery = 5 * time.Second
	DefaultAfter = 10 * time.Second
	DefaultQuiet = time.Hour
)

// Probe is one loop the watchdog watches. Run sends a no-op through the loop and returns once
// the loop ran it. It may block for as long as the loop is stuck.
type Probe struct {
	Name string
	Run  func()
}

// Clock reads the wall time and the monotonic time. Across a sleep of the machine the wall time
// moves on and the monotonic time does not (on macOS and Linux), which is how the watchdog sees
// a sleep.
type Clock func() (wall time.Time, mono time.Duration)

// Config sets up a watchdog.
type Config struct {
	Probes []Probe
	Every  time.Duration // between checks; DefaultEvery when zero
	After  time.Duration // a probe this long unanswered is a stall; DefaultAfter when zero
	Quiet  time.Duration // a loop reports at most one stall per Quiet; DefaultQuiet when zero
	// Stalled is called once per reported stall, with the loop's name and how long its probe
	// has waited. It runs on the watchdog's goroutine.
	Stalled func(name string, wait time.Duration)
	// Recovered is called when the probe of a reported stall returns, with the total wait.
	Recovered func(name string, wait time.Duration)
	Clock     Clock // nil: the system clocks
}

type loop struct {
	p        Probe
	inFlight bool
	sent     time.Duration // monotonic time the probe in flight was sent, or its wait restarted
	reported bool          // the probe in flight was reported stalled
	lastRep  time.Duration // monotonic time of the last report; -1: none
	done     chan struct{}
}

// Watchdog watches its probes until Close.
type Watchdog struct {
	cfg   Config
	loops []*loop

	mu       sync.Mutex
	lastWall time.Time
	lastMono time.Duration
	started  bool

	stop chan struct{}
	wg   sync.WaitGroup
}

// New makes a watchdog. Start runs it; tests call Check by hand instead.
func New(cfg Config) *Watchdog {
	if cfg.Every <= 0 {
		cfg.Every = DefaultEvery
	}
	if cfg.After <= 0 {
		cfg.After = DefaultAfter
	}
	if cfg.Quiet <= 0 {
		cfg.Quiet = DefaultQuiet
	}
	if cfg.Stalled == nil {
		cfg.Stalled = func(string, time.Duration) {}
	}
	if cfg.Recovered == nil {
		cfg.Recovered = func(string, time.Duration) {}
	}
	if cfg.Clock == nil {
		base := time.Now()
		cfg.Clock = func() (time.Time, time.Duration) {
			now := time.Now()
			return now.Round(0), now.Sub(base)
		}
	}
	w := &Watchdog{cfg: cfg, stop: make(chan struct{})}
	for _, p := range cfg.Probes {
		w.loops = append(w.loops, &loop{p: p, lastRep: -1})
	}
	return w
}

// Start checks every Every until Close.
func (w *Watchdog) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.Every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.Check()
			case <-w.stop:
				return
			}
		}
	}()
}

// Close stops the checks. A probe still blocked in its loop stays blocked; its goroutine ends
// when the loop answers or closes.
func (w *Watchdog) Close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wg.Wait()
}

// sleepSlack is how far the wall time may run ahead of the monotonic time between two checks
// before the gap counts as a sleep.
const sleepSlack = 2 * time.Second

// Check runs one round: it reports a probe in flight past After, notes a probe that returned,
// and sends a new probe through each idle loop. A round whose gap since the last spans a sleep
// reports nothing and restarts the wait of each probe in flight: a loop cannot answer while the
// machine sleeps (12).
func (w *Watchdog) Check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	wall, mono := w.cfg.Clock()
	slept := w.started && wall.Sub(w.lastWall)-(mono-w.lastMono) > sleepSlack
	w.lastWall, w.lastMono, w.started = wall, mono, true
	for _, l := range w.loops {
		if l.inFlight {
			select {
			case <-l.done:
				l.inFlight = false
				if l.reported {
					w.cfg.Recovered(l.p.Name, mono-l.sent)
				}
			default:
			}
		}
		if l.inFlight {
			if slept {
				l.sent = mono
				continue
			}
			wait := mono - l.sent
			if !l.reported && wait >= w.cfg.After && (l.lastRep < 0 || mono-l.lastRep >= w.cfg.Quiet) {
				l.reported, l.lastRep = true, mono
				w.cfg.Stalled(l.p.Name, wait)
			}
			continue
		}
		l.inFlight, l.reported, l.sent = true, false, mono
		done := make(chan struct{})
		l.done = done
		go func(run func()) {
			run()
			close(done)
		}(l.p.Run)
	}
}
