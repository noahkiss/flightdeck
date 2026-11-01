package habconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/datastream"
)

// creditBatch is how much credit a stream holds back before it returns it. The hab's initial
// credit is 256 KiB, so holding this little back never stalls a stream.
const creditBatch = 16 << 10

// PaneOptions set up a pane stream.
type PaneOptions struct {
	// Observe opens observer streams: they never take ownership (spec 6.4).
	Observe bool
	// Offset, when set, is the holder offset after the last byte the client applied from an
	// earlier stream of this pane. The first stream resumes from it; nil starts from a
	// snapshot.
	Offset *uint64
}

// Frame is one frame a pane stream delivers: a snapshot, bytes, a resize or the exit.
type Frame struct {
	Kind api.FrameKind
	// Offset is the offset a snapshot covers, or the offset of the first byte of Data.
	Offset   uint64
	Encoding string // a snapshot's format
	Data     []byte // a snapshot's screen bytes, or program output
	Resize   *api.ResizeFrame
	Exited   *api.ExitedFrame
}

// PaneStream follows one pane over data connections: a snapshot, then the program's bytes.
// When a data connection drops it opens another with a fresh ticket and the offset it holds,
// so the hab continues with no gap and no repeat, or sends a fresh snapshot when it no longer
// has the bytes. One goroutine calls Next; Close may come from any.
type PaneStream struct {
	c       *Conn
	pane    string
	observe bool

	mu      sync.Mutex
	cur     *datastream.Client // nil between data connections
	welcome api.DataWelcome
	offset  uint64 // the offset after the last frame handed out
	have    bool   // offset is known
	owed    int    // credit for frames handed out and not yet returned
	exited  bool
	closed  bool
	opens   int // data connections opened
}

// OpenPane opens the first data connection to pane and reads its welcome.
func (c *Conn) OpenPane(ctx context.Context, pane string, opts PaneOptions) (*PaneStream, error) {
	p := &PaneStream{c: c, pane: pane, observe: opts.Observe}
	if opts.Offset != nil {
		p.offset, p.have = *opts.Offset, true
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.panes[p] = struct{}{}
	c.mu.Unlock()
	if err := p.open(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Pane returns the pane id.
func (p *PaneStream) Pane() string { return p.pane }

// Offset returns the offset after the last frame Next returned, and whether one is known.
// A client keeps it to resume the pane later (gff-client.md 11.3).
func (p *PaneStream) Offset() (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.offset, p.have
}

// Welcome returns the welcome of the current data connection.
func (p *PaneStream) Welcome() api.DataWelcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.welcome
}

// Opens counts the data connections opened so far: one, plus one per resume.
func (p *PaneStream) Opens() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opens
}

// Close ends the stream.
func (p *PaneStream) Close() {
	p.mu.Lock()
	p.closed = true
	cur := p.cur
	p.cur = nil
	p.mu.Unlock()
	if cur != nil {
		cur.Close()
	}
	p.c.mu.Lock()
	delete(p.c.panes, p)
	p.c.mu.Unlock()
}

// kick closes the current data connection; the next read resumes on a new one.
func (p *PaneStream) kick() {
	p.mu.Lock()
	cur := p.cur
	p.mu.Unlock()
	if cur != nil {
		cur.Close()
	}
}

// Next returns the next frame. Calling Next again tells the stream that the previous frame
// was applied, which returns its credit. After an exited frame Next returns io.EOF. A pane
// the hab no longer has ends with ErrPaneGone. When ctx ends Next returns its error; the
// stream stays usable and resumes from its offset on the next call.
func (p *PaneStream) Next(ctx context.Context) (Frame, error) {
	failures := 0
	for {
		p.mu.Lock()
		closed, exited, cur := p.closed, p.exited, p.cur
		p.mu.Unlock()
		if closed {
			return Frame{}, ErrClosed
		}
		if exited {
			return Frame{}, io.EOF
		}
		if err := ctx.Err(); err != nil {
			return Frame{}, err
		}
		if cur == nil {
			if failures > 0 {
				if err := p.wait(ctx, failures); err != nil {
					return Frame{}, err
				}
			}
			err := p.open(ctx)
			if err != nil {
				if errors.Is(err, ErrPaneGone) || errors.Is(err, ErrClosed) || p.c.Err() != nil {
					return Frame{}, err
				}
				failures++
				p.c.cfg.Logf("habconn: pane %s: reopen: %v", p.pane, err)
				continue
			}
			continue
		}
		p.payCredit(cur)
		m, err := readCtx(ctx, cur)
		if err != nil {
			var rej *datastream.RejectError
			if errors.As(err, &rej) && rej.Kind == api.ErrorKindNotFound {
				p.drop(cur)
				return Frame{}, fmt.Errorf("%w: %s", ErrPaneGone, rej.Hint)
			}
			// A dropped connection, a reject such as unavailable, or ctx: the next call
			// resumes from the offset.
			p.drop(cur)
			if ctx.Err() != nil {
				return Frame{}, ctx.Err()
			}
			p.c.cfg.Logf("habconn: pane %s: data connection ended: %v", p.pane, err)
			continue
		}
		failures = 0
		if f, ok := p.take(m); ok {
			return f, nil
		}
	}
}

// take records a frame's offset and credit and returns it, trimmed of any bytes the client
// already has. ok is false for a frame with nothing left to hand out.
func (p *PaneStream) take(m datastream.Msg) (Frame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owed += m.Cost
	f := Frame{Kind: m.Kind, Offset: m.Offset, Encoding: m.Encoding, Data: m.Data, Resize: m.Resize, Exited: m.Exited}
	switch m.Kind {
	case api.FrameSnapshot:
		p.offset, p.have = m.Offset, true
	case api.FrameBytes:
		if p.have && m.Offset < p.offset {
			// The hab never repeats a byte on resume (spec 5.4); this guard keeps a client
			// correct even if one did.
			skip := p.offset - m.Offset
			if skip >= uint64(len(m.Data)) {
				return Frame{}, false
			}
			f.Data, f.Offset = m.Data[skip:], p.offset
		}
		p.offset, p.have = f.Offset+uint64(len(f.Data)), true
	case api.FrameExited:
		p.exited = true
	case api.FrameResize:
	default:
		return Frame{}, false // a kind this build does not know
	}
	return f, true
}

// payCredit returns the credit owed once it is worth a frame.
func (p *PaneStream) payCredit(cur *datastream.Client) {
	p.mu.Lock()
	n := p.owed
	if n < creditBatch || p.cur != cur {
		p.mu.Unlock()
		return
	}
	p.owed = 0
	p.mu.Unlock()
	cur.Credit(n) // a failed write shows on the next read
}

// drop forgets cur if it is still the current connection.
func (p *PaneStream) drop(cur *datastream.Client) {
	cur.Close()
	p.mu.Lock()
	if p.cur == cur {
		p.cur = nil
	}
	p.mu.Unlock()
}

// wait sleeps the backoff for a reopen that failed failures times.
func (p *PaneStream) wait(ctx context.Context, failures int) error {
	t := time.NewTimer(backoff(p.c.cfg.MinBackoff, p.c.cfg.MaxBackoff, failures))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-p.c.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// open gets a ticket, dials a data connection and reads its welcome. Credit starts afresh on
// every data connection, so the credit owed on the last one is dropped.
func (p *PaneStream) open(ctx context.Context) error {
	cl, err := p.c.waitOnline(ctx)
	if err != nil {
		return err
	}
	octx, cancel := context.WithTimeout(ctx, p.c.cfg.OpenTimeout)
	defer cancel()
	t, err := api.Client{Caller: cl}.DataTicket(octx, api.TicketParams{Pane: p.pane, Observe: p.observe})
	if err != nil {
		var ce *control.Error
		if errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound {
			return fmt.Errorf("%w: %s", ErrPaneGone, ce.Data.Hint)
		}
		return fmt.Errorf("habconn: data ticket: %w", err)
	}
	rw, err := p.c.cfg.Transport.Dial(octx, KindData)
	if err != nil {
		return fmt.Errorf("habconn: dial data: %w", err)
	}
	p.mu.Lock()
	hello := api.DataHello{Ticket: t.Ticket}
	if p.have {
		off := p.offset
		hello.Offset = &off
	}
	p.mu.Unlock()
	stop := context.AfterFunc(octx, func() { rw.Close() })
	dc, w, err := datastream.Open(rw, hello)
	interrupted := !stop()
	if err != nil || interrupted {
		rw.Close()
		var rej *datastream.RejectError
		if errors.As(err, &rej) && rej.Kind == api.ErrorKindNotFound {
			return fmt.Errorf("%w: %s", ErrPaneGone, rej.Hint)
		}
		if err == nil {
			err = octx.Err()
		}
		if se, ok := rw.(stderrer); ok {
			if msg := se.Stderr(); msg != "" {
				return fmt.Errorf("habconn: open data: %w (%s)", err, msg)
			}
		}
		return fmt.Errorf("habconn: open data: %w", err)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		dc.Close()
		return ErrClosed
	}
	p.cur, p.welcome, p.owed = dc, w, 0
	p.opens++
	p.mu.Unlock()
	return nil
}

// readCtx reads one frame; when ctx ends first it closes the connection to stop the read.
func readCtx(ctx context.Context, cur *datastream.Client) (datastream.Msg, error) {
	stop := context.AfterFunc(ctx, func() { cur.Close() })
	defer stop()
	return cur.Next()
}
