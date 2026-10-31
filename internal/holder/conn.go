package holder

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// conn is one logic connection. Its frames go through one queue and one writer goroutine
// (protocol section 1), so the reader never waits on a write. Every field below pc is guarded
// by Holder.mu.
type conn struct {
	h  *Holder
	nc *net.UnixConn
	pc *hp.Conn

	ready    bool // the welcome is queued; events may follow
	closed   bool
	handover bool // the logic process said it is leaving on purpose

	cond   *sync.Cond // the writer waits here for items
	q      []item
	queued int // bytes of encoded frames queued or being written

	// subs holds the panes this connection follows. A pane is live when output frames go out as
	// it pumps; while an open replay is in flight it is not live, and job is that replay.
	subs map[*pane]*sub

	writerDone chan struct{}

	// maxQueued is the most bytes ever queued at once. Tests read it to check pacing.
	maxQueued int
}

type sub struct {
	live bool
	job  *replayJob
}

// item is one entry in the writer queue: an encoded frame, or a replay that produces its frames
// one at a time as the writer drains.
type item struct {
	b    []byte
	job  *replayJob
	last bool // close the connection after this frame
}

// replayJob is a replay in progress. The writer asks it for one frame at a time, so a large
// replay never holds more memory than one frame (protocol section 8).
type replayJob struct {
	p       *pane
	token   uint32
	from    uint64
	hasTo   bool
	to      uint64
	started bool
	start   uint64
	next    uint64
	gap     bool
}

func newConn(h *Holder, nc *net.UnixConn, pc *hp.Conn) *conn {
	return &conn{
		h: h, nc: nc, pc: pc,
		cond:       sync.NewCond(&h.mu),
		subs:       map[*pane]*sub{},
		writerDone: make(chan struct{}),
	}
}

// sendLocked queues a frame for the writer.
func (c *conn) sendLocked(f hp.Frame) {
	c.pushLocked(f, false)
}

// sendLastLocked queues a frame after which the writer closes the connection.
func (c *conn) sendLastLocked(f hp.Frame) {
	c.pushLocked(f, true)
}

func (c *conn) pushLocked(f hp.Frame, last bool) {
	if c.closed {
		return
	}
	b, err := hp.AppendFrame(nil, f, c.pc.Version())
	if err != nil {
		c.h.cfg.Logf("holder: encode %s: %v", f.Header().Kind, err)
		return
	}
	c.q = append(c.q, item{b: b, last: last})
	c.queued += len(b)
	c.maxQueued = max(c.maxQueued, c.queued)
	c.cond.Signal()
}

// errorLocked queues an error frame that answers the request with header hd.
func (c *conn) errorLocked(hd hp.Header, code hp.ErrorCode, offset uint64, msg string) {
	c.sendLocked(&hp.Error{Handle: hd.Pane, Token: hd.Token, Ref: hd.Kind, Code: code, Offset: offset, Message: msg})
}

// writer drains the queue onto the socket. It produces replay frames lazily, one per step, so
// replay is paced by how fast the socket takes bytes.
func (c *conn) writer() {
	h := c.h
	defer close(c.writerDone)
	h.mu.Lock()
	defer h.mu.Unlock()
	for {
		for len(c.q) == 0 && !c.closed {
			c.cond.Wait()
		}
		if c.closed {
			return
		}
		it := c.q[0]
		var b []byte
		if it.job != nil {
			var done bool
			b, done = c.stepLocked(it.job)
			if done {
				c.popLocked()
			}
			c.queued += len(b)
			c.maxQueued = max(c.maxQueued, c.queued)
		} else {
			b = it.b
			c.popLocked()
		}
		h.mu.Unlock()
		err := c.write(b)
		h.mu.Lock()
		c.queued -= len(b)
		h.bp.Broadcast()
		if err != nil {
			h.dropLocked(c, err.Error())
			return
		}
		if it.job != nil && h.afterReplayFrame != nil {
			h.mu.Unlock()
			h.afterReplayFrame()
			h.mu.Lock()
		}
		if it.last {
			h.dropLocked(c, "closed after the last frame")
			return
		}
	}
}

func (c *conn) popLocked() {
	c.q[0] = item{}
	c.q = c.q[1:]
}

var errHung = errors.New("connection hung: no write progress within the hang timeout")

// write writes b fully. The connection is hung when no byte reaches the socket for
// HangTimeout. A slow reader that keeps taking bytes renews the deadline with each write that
// makes progress.
func (c *conn) write(b []byte) error {
	hang := c.h.cfg.HangTimeout
	for len(b) > 0 {
		_ = c.nc.SetWriteDeadline(time.Now().Add(hang))
		n, err := c.nc.Write(b)
		b = b[n:]
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if n > 0 {
				continue
			}
			return errHung
		}
		return err
	}
	return nil
}

// stepLocked produces the next frame of a replay: a replay data frame, or the replay end. done
// is true for the replay end.
func (c *conn) stepLocked(j *replayJob) (b []byte, done bool) {
	r := j.p.ring
	count := r.count
	if !j.started {
		j.started = true
		j.start = min(j.from, count)
		if j.start < r.oldest() {
			j.start = r.oldest()
			j.gap = true
		}
		j.next = j.start
	}
	// Bytes the ring overwrote before the writer reached them are gone; the next frame starts
	// later, and gff-hab sees the jump in the offsets.
	j.next = max(j.next, r.oldest())
	limit := count
	if j.hasTo {
		limit = max(min(j.to, count), j.start)
	}
	if !j.p.released && j.next < limit {
		data := r.read(j.next, int(min(limit-j.next, hp.MaxOutputData)))
		f := &hp.ReplayData{Handle: j.p.handle, Token: j.token, Offset: j.next, Data: data}
		j.next += uint64(len(data))
		return c.encode(f), false
	}
	end := j.next
	if j.hasTo {
		end = max(min(end, limit), j.start)
	}
	f := &hp.ReplayEnd{Handle: j.p.handle, Token: j.token, Start: j.start, End: end, Gap: j.gap}
	if !j.hasTo {
		// An open replay subscribes the pane. The flip happens in the same lock hold that saw
		// the replay reach the pane's count, so the next output frame starts exactly at end.
		if s := c.subs[j.p]; s != nil && s.job == j {
			s.live = true
			s.job = nil
		}
	}
	return c.encode(f), true
}

func (c *conn) encode(f hp.Frame) []byte {
	b, err := hp.AppendFrame(nil, f, c.pc.Version())
	if err != nil {
		// Replay frames are built within the limits; this cannot happen.
		panic(fmt.Sprintf("holder: encode %s: %v", f.Header().Kind, err))
	}
	return b
}

// replay handles a replay request.
func (h *Holder) replay(c *conn, f *hp.Replay) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil || c.closed {
		return
	}
	j := &replayJob{p: p, token: f.Token, from: f.From, hasTo: f.HasTo, to: f.To}
	if !f.HasTo {
		// Re-open: from now until this replay's end, no live output for the pane is queued.
		// Output queued before this point still goes out first; it precedes the replay.
		s := c.subs[p]
		if s == nil {
			s = &sub{}
			c.subs[p] = s
		}
		s.live = false
		s.job = j
	}
	c.q = append(c.q, item{job: j})
	c.cond.Signal()
}

// readLoop reads frames until the connection ends and handles each in order.
func (c *conn) readLoop() {
	h := c.h
	for {
		f, err := c.pc.ReadFrame()
		if err != nil {
			h.mu.Lock()
			h.dropLocked(c, "read: "+err.Error())
			h.mu.Unlock()
			return
		}
		h.mu.Lock()
		closed := c.closed
		h.mu.Unlock()
		if closed {
			return
		}
		h.dispatch(c, f)
	}
}

func (h *Holder) dispatch(c *conn, f hp.Frame) {
	switch f := f.(type) {
	case *hp.Ping:
		h.mu.Lock()
		c.sendLocked(&hp.Pong{Token: f.Token})
		h.mu.Unlock()
	case *hp.Pong:
	case *hp.Handover:
		h.mu.Lock()
		c.handover = true
		h.mu.Unlock()
	case *hp.Drain:
		h.mu.Lock()
		h.draining = true
		h.drainExitLocked()
		h.mu.Unlock()
	case *hp.Shutdown:
		h.goSafe("shutdown", h.Shutdown)
	case *hp.Spawn:
		h.spawn(c, f)
	case *hp.Input:
		h.input(c, f)
	case *hp.Resize:
		h.resize(c, f)
	case *hp.Signal:
		h.signal(c, f)
	case *hp.ForegroundQuery:
		h.foreground(c, f)
	case *hp.Release:
		h.release(c, f)
	case *hp.Replay:
		h.replay(c, f)
	default:
		// A kind the holder knows but only ever sends, or a second hello.
		h.mu.Lock()
		c.errorLocked(f.Header(), hp.CodeBadRequest, 0, fmt.Sprintf("%s is not a request", f.Header().Kind))
		h.mu.Unlock()
	}
}
