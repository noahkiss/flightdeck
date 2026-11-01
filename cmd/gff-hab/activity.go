package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// activityEvery is the most often one pane sends pane.output, and pane.bell (gff-client.md
// 15.7, hab-logic-api.md 3.9).
const activityEvery = time.Second

// paneOutputs is what activityEvents reads from the link: the panes, and a subscription to one.
type paneOutputs interface {
	Panes() []link.PaneStatus
	Subscribe(ctx context.Context, u hp.UUID, from uint64) (*link.Subscription, error)
	Watch(fn func(link.Notice)) (cancel func())
}

// activityEvents sends pane.output and pane.bell, at most once a second per pane, so a client
// can mark a hidden tab that printed or rang (gff-client.md 5.1). It follows each pane through
// its own link subscription from now on; the byte path itself is untouched. A subscriber that
// falls behind is dropped by the link and starts again from now, and the drop counts as output.
type activityEvents struct {
	src     paneOutputs
	publish func(structure.Event)
	every   time.Duration

	mu     sync.Mutex
	panes  map[hp.UUID]*paneActivity
	closed bool
	stop   chan struct{}
	done   chan struct{}
	wg     sync.WaitGroup
	cancel func()
}

type paneActivity struct {
	cancel context.CancelFunc
	offset uint64
	output bool
	bells  int
	scan   bellScanner
}

func newActivityEvents(src paneOutputs, publish func(structure.Event), every time.Duration) *activityEvents {
	e := &activityEvents{src: src, publish: publish, every: every, panes: map[hp.UUID]*paneActivity{},
		stop: make(chan struct{}), done: make(chan struct{})}
	e.cancel = src.Watch(e.notice)
	for _, p := range src.Panes() {
		e.follow(p.UUID)
	}
	go e.run()
	return e
}

func (e *activityEvents) notice(n link.Notice) {
	switch n.Kind {
	case link.PaneAdded:
		e.follow(n.Pane.UUID)
	case link.PaneGone:
		e.mu.Lock()
		if p := e.panes[n.Pane.UUID]; p != nil {
			p.cancel()
			delete(e.panes, n.Pane.UUID)
		}
		e.mu.Unlock()
	}
}

// follow starts following a pane, once.
func (e *activityEvents) follow(u hp.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.panes[u] != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &paneActivity{cancel: cancel}
	e.panes[u] = p
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.read(ctx, u, p)
		e.mu.Lock()
		if e.panes[u] == p {
			delete(e.panes, u) // a later PaneAdded follows it again
		}
		e.mu.Unlock()
	}()
}

// read follows one pane's output until ctx ends or the pane is gone.
func (e *activityEvents) read(ctx context.Context, u hp.UUID, p *paneActivity) {
	for ctx.Err() == nil {
		sub, err := e.src.Subscribe(ctx, u, link.FromNow)
		if err != nil {
			return // gone, lost or closed: a PaneAdded starts it again
		}
		stop := context.AfterFunc(ctx, sub.Close)
		for ch := range sub.C {
			if ch.Kind != link.ChunkBytes && ch.Kind != link.ChunkSnapshot {
				continue
			}
			e.mu.Lock()
			if ch.Kind == link.ChunkBytes {
				p.offset = ch.Offset + uint64(len(ch.Data))
				p.output = true
				p.bells += p.scan.count(ch.Data)
			} else if ch.Offset > p.offset {
				p.offset = ch.Offset
			}
			e.mu.Unlock()
		}
		stop()
		if !errors.Is(sub.Err(), link.ErrSlow) {
			return
		}
		e.mu.Lock()
		p.output = true // bytes went by unread; they were output
		p.scan = bellScanner{}
		e.mu.Unlock()
	}
}

func (e *activityEvents) run() {
	defer close(e.done)
	t := time.NewTicker(e.every)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
		}
		e.flush()
	}
}

func (e *activityEvents) flush() {
	type out struct {
		id     string
		offset uint64
		output bool
		bells  int
	}
	var outs []out
	e.mu.Lock()
	for u, p := range e.panes {
		if p.output || p.bells > 0 {
			outs = append(outs, out{u.String(), p.offset, p.output, p.bells})
			p.output, p.bells = false, 0
		}
	}
	e.mu.Unlock()
	for _, o := range outs {
		if o.bells > 0 {
			e.publish(structure.Event{Type: api.EventPaneBell, ID: o.id, Data: api.PaneBell{Count: o.bells}})
		}
		if o.output {
			e.publish(structure.Event{Type: api.EventPaneOutput, ID: o.id, Data: api.PaneOutput{Offset: o.offset}})
		}
	}
}

// Close stops following every pane.
func (e *activityEvents) Close() {
	e.cancel()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	for _, p := range e.panes {
		p.cancel()
	}
	e.mu.Unlock()
	close(e.stop)
	<-e.done
	e.wg.Wait()
}

// bellScanner counts BEL bytes that ring: those outside control strings. In an OSC, a BEL ends
// the string and does not ring; DCS, SOS, PM and APC strings end only with ST. It keeps its state
// across reads, so a split sequence counts right.
type bellScanner struct {
	state int
}

const (
	bsGround = iota
	bsEsc
	bsOSC    // in an OSC string: BEL or ST ends it
	bsString // in a DCS, SOS, PM or APC string: ST ends it
	bsStrEsc // ESC inside a string: \ ends it
)

func (b *bellScanner) count(p []byte) int {
	n := 0
	for _, c := range p {
		switch b.state {
		case bsGround:
			switch c {
			case 0x07:
				n++
			case 0x1b:
				b.state = bsEsc
			}
		case bsEsc:
			switch c {
			case ']':
				b.state = bsOSC
			case 'P', 'X', '^', '_':
				b.state = bsString
			case 0x1b:
			default:
				b.state = bsGround
			}
		case bsOSC:
			switch c {
			case 0x07:
				b.state = bsGround
			case 0x1b:
				b.state = bsStrEsc
			}
		case bsString:
			if c == 0x1b {
				b.state = bsStrEsc
			}
		case bsStrEsc:
			if c == '\\' {
				b.state = bsGround
			} else if c != 0x1b {
				b.state = bsString // an OSC that saw ESC without \ ends at ST alone too
			}
		}
	}
	return n
}
