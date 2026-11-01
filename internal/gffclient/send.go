package gffclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habconn"
)

// callTimeout bounds one control call the client makes on its own.
const callTimeout = 10 * time.Second

// maxPendingInput bounds the input waiting for one pane. Past it, new input is dropped: the hab
// is not taking input, and typing that lands much later could do something the owner no
// longer wants (gff-client.md 11.6).
const maxPendingInput = 1 << 20

// inputSender sends one pane's input in order, as pane.input calls (hab-logic-api.md 3.8).
// Input that comes while a call is out waits and goes as the next call, so a burst costs one
// call per round trip and a paste goes in as few calls as it can.
type inputSender struct {
	conn *habconn.Conn
	pane string
	note func(string)
	mu   sync.Mutex
	buf  []byte
	wake chan struct{}
}

func newInputSender(conn *habconn.Conn, pane string, note func(string)) *inputSender {
	return &inputSender{conn: conn, pane: pane, note: note, wake: make(chan struct{}, 1)}
}

// send queues p; it never blocks.
func (s *inputSender) send(p []byte) {
	s.mu.Lock()
	if len(s.buf)+len(p) > maxPendingInput {
		s.mu.Unlock()
		s.note("input dropped: the hab is not taking it")
		return
	}
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *inputSender) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		s.mu.Lock()
		data := s.buf
		s.buf = nil
		s.mu.Unlock()
		if len(data) == 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		_, err := s.conn.API().PaneInput(cctx, api.InputParams{Pane: s.pane, Data: data})
		cancel()
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, habconn.ErrOffline):
			// Refused, never queued: a late keystroke could approve something (11.6).
			s.note("offline: input dropped")
		default:
			s.note("input: " + err.Error())
		}
	}
}

// opKind names a control call the client makes for a pane.
type opKind int

const (
	opSize opKind = iota + 1
	opTake
	opRedraw
)

type op struct {
	kind       opKind
	pane       string
	cols, rows int
	mode       api.RedrawMode
}

// opQueue makes the size, take and redraw calls in the order they were asked, one at a time,
// off the client's loop. A size still waiting is replaced by a newer size for the same pane,
// so only the last one goes out.
type opQueue struct {
	conn *habconn.Conn
	note func(pane, msg string)
	mu   sync.Mutex
	q    []op
	wake chan struct{}
}

func newOpQueue(conn *habconn.Conn, note func(pane, msg string)) *opQueue {
	return &opQueue{conn: conn, note: note, wake: make(chan struct{}, 1)}
}

func (o *opQueue) push(p op) {
	o.mu.Lock()
	replaced := false
	if p.kind == opSize {
		for i := range o.q {
			if o.q[i].kind == opSize && o.q[i].pane == p.pane {
				o.q[i], replaced = p, true
				break
			}
		}
	}
	if !replaced {
		o.q = append(o.q, p)
	}
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *opQueue) run(ctx context.Context) {
	for {
		o.mu.Lock()
		if len(o.q) == 0 {
			o.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-o.wake:
			}
			continue
		}
		p := o.q[0]
		o.q = o.q[1:]
		o.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		var err error
		c := o.conn.API()
		switch p.kind {
		case opSize:
			_, err = c.PaneSetDesiredSize(cctx, api.DesiredSizeParams{Pane: p.pane, Cols: p.cols, Rows: p.rows})
		case opTake:
			_, err = c.PaneTake(cctx, api.PaneRef{Pane: p.pane})
		case opRedraw:
			_, err = c.PaneRedraw(cctx, api.RedrawParams{Pane: p.pane, Mode: p.mode})
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			o.note(p.pane, err.Error())
		}
	}
}
