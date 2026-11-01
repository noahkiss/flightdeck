package harness

import (
	"context"
	"sync"
	"time"
)

// MaxPermissionWait bounds how long a blocking permission hook may wait for an answer. The hook
// config gives the hook more than this, so the hab, not the harness, ends the wait.
const MaxPermissionWait = 120 * time.Second

// DialogSettle is how long after a hook gave up with no decision an answer by keys waits: the
// harness is only now drawing its own dialog, and a key sent before it is up lands in the prompt.
const DialogSettle = time.Second

// Decision is the app's answer to a permission request.
type Decision struct {
	Allow bool
}

// Gate holds the blocking permission hooks that wait for the app's answer (harness-adapters.md,
// Open decisions: Claude blocking PermissionRequest hook). It is the owner's switch: a Gate
// made with a zero wait is off, and no hook ever waits. It is safe for concurrent use.
type Gate struct {
	wait  time.Duration
	now   func() time.Time
	mu    sync.Mutex
	waits map[gateKey]chan delivery
	ended map[gateKey]time.Time // when a wait last ended with no decision
}

type gateKey struct{ pane, request string }

// delivery is a decision on its way to a waiting hook. The hook reports on ack whether it wrote
// the decision to the harness.
type delivery struct {
	d   Decision
	ack chan error
}

// NewGate returns a gate whose hooks wait at most wait, capped at MaxPermissionWait. Zero or
// less turns it off.
func NewGate(wait time.Duration) *Gate {
	return &Gate{wait: min(wait, MaxPermissionWait), now: time.Now, waits: map[gateKey]chan delivery{}, ended: map[gateKey]time.Time{}}
}

// Enabled reports whether hooks may wait.
func (g *Gate) Enabled() bool { return g != nil && g.wait > 0 }

// Wait blocks until Answer delivers a decision for the pane's request, the wait runs out, or
// ctx ends (the hook gave up). ok is false with no decision; the harness then shows its own
// dialog. With a decision, the caller must call written once, with the result of handing it to
// the harness: Answer reports that result. A second wait for the same request replaces the
// first, which ends with no decision.
func (g *Gate) Wait(ctx context.Context, pane, request string) (d Decision, written func(error), ok bool) {
	if !g.Enabled() {
		return Decision{}, nil, false
	}
	k := gateKey{pane, request}
	ch := make(chan delivery, 1)
	g.mu.Lock()
	if old := g.waits[k]; old != nil {
		close(old)
	}
	g.waits[k] = ch
	g.mu.Unlock()
	t := time.NewTimer(g.wait)
	defer t.Stop()
	var dl delivery
	select {
	case dl, ok = <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	if !ok {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.waits[k] != ch {
			// Answer or a newer wait took the entry just now: Answer's decision is already in
			// ch, and a newer wait closed it.
			dl, ok = <-ch
		} else {
			delete(g.waits, k)
		}
		if !ok {
			g.endedLocked(k)
			return Decision{}, nil, false
		}
	}
	return dl.d, func(err error) { dl.ack <- err }, true
}

// endedLocked records that a wait for k ended with no decision. The caller holds g.mu.
func (g *Gate) endedLocked(k gateKey) {
	now := g.now()
	for o, at := range g.ended {
		if now.Sub(at) >= DialogSettle {
			delete(g.ended, o)
		}
	}
	g.ended[k] = now
}

// Answer hands d to the hook waiting on the pane's request. ok is false when no hook waits, so
// the caller falls back to keys. Else written gets one value: nil once the hook wrote the
// decision for the harness, or why it could not (the hook went away). Until then the decision
// may still be lost.
func (g *Gate) Answer(pane, request string, d Decision) (written <-chan error, ok bool) {
	if g == nil {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k := gateKey{pane, request}
	ch := g.waits[k]
	if ch == nil {
		return nil, false
	}
	delete(g.waits, k)
	ack := make(chan error, 1)
	ch <- delivery{d, ack}
	return ack, true
}

// Waiting reports whether a hook waits on the pane's request now.
func (g *Gate) Waiting(pane, request string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waits[gateKey{pane, request}] != nil
}

// Settle returns how long an answer by keys to the pane's request should still wait: the rest
// of DialogSettle after a hook for it gave up with no decision, else 0.
func (g *Gate) Settle(pane, request string) time.Duration {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.ended[gateKey{pane, request}]
	if !ok {
		return 0
	}
	return max(0, DialogSettle-g.now().Sub(at))
}
