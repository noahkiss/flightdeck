package main

import (
	"context"
	"sync"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/procscope"
)

// escapedSweeps keeps process inspection off the link's event loop. One worker
// deduplicates queued pane releases and each OS scan has a six-second budget, including its two-second grace.
type escapedSweeps struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	pending map[hp.UUID]bool
	kick    chan struct{}
	done    chan struct{}
	tokens  *hook.Tokens
	logf    func(string, ...any)
	sweep   func(context.Context, string) (int, error)
}

func newEscapedSweeps(tokens *hook.Tokens, logf func(string, ...any)) *escapedSweeps {
	ctx, cancel := context.WithCancel(context.Background())
	s := &escapedSweeps{ctx: ctx, cancel: cancel, pending: map[hp.UUID]bool{}, kick: make(chan struct{}, 1), done: make(chan struct{}), tokens: tokens, logf: logf, sweep: procscope.Sweep}
	go s.run()
	return s
}
func (s *escapedSweeps) Close() { s.cancel(); <-s.done }
func (s *escapedSweeps) notice(n link.Notice) {
	if n.Kind != link.PaneGone || !n.Pane.Gone || !n.Released {
		return
	}
	s.mu.Lock()
	s.pending[n.Pane.UUID] = true
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}
func (s *escapedSweeps) run() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		}
		for {
			s.mu.Lock()
			var u hp.UUID
			found := false
			for id := range s.pending {
				u = id
				delete(s.pending, id)
				found = true
				break
			}
			s.mu.Unlock()
			if !found {
				break
			}
			if s.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(s.ctx, 6*time.Second)
			n, err := s.sweep(ctx, s.tokens.Token(u))
			cancel()
			if err != nil {
				s.logf("pane %s: escaped process sweep: %v", u, err)
			} else if n > 0 {
				s.logf("pane %s: signalled %d escaped processes", u, n)
			}
		}
	}
}
