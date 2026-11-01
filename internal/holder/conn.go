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
	// writeFailed: a write to the socket failed. The writer has stopped and frames are no longer
	// queued; the reader drops the connection when it reaches end of file.
	writeFailed bool
	// writeTimer drops the connection if the reader has not done so after a write failure.
	writeTimer *time.Timer
	// replacing: a newer hello is waiting for this connection's reader to finish.
	replacing bool
	// hello numbers this connection's hello in arrival order (Holder.hellos), under the lock.
	hello uint64

	cond   *sync.Cond // the writer waits here for items
	q      []item
	queued int // bytes of encoded frames queued or being written

	// subs holds the panes this connection follows live: output frames go out as they pump. An
	// open replay turns a pane live at once; its live frames queue behind the replay.
	subs map[*pane]bool

	writerDone chan struct{}
	readerDone chan struct{} // closed when readLoop returns

	// maxQueued is the most bytes ever queued at once. Tests read it to check pacing.
	maxQueued int
}

// item is one entry in the writer queue: an encoded frame, or a replay that produces its frames
// one at a time as the writer drains.
type item struct {
	b    []byte
	job  *replayJob
	last bool // close the connection after this frame
}

// replayJob is a replay in progress. The writer asks it for one frame at a time, so a large
// replay never holds more memory than one frame (protocol section 8). Its range is fixed when
// the request arrives: [start, end), so every replay ends however fast the program writes.
type replayJob struct {
	p     *pane
	token uint32
	start uint64
	end   uint64
	next  uint64
	gap   bool
}

func newConn(h *Holder, nc *net.UnixConn, pc *hp.Conn) *conn {
	return &conn{
		h: h, nc: nc, pc: pc,
		cond:       sync.NewCond(&h.mu),
		subs:       map[*pane]bool{},
		writerDone: make(chan struct{}),
		readerDone: make(chan struct{}),
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
	if c.closed || c.writeFailed {
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
// replay is paced by how fast the socket takes bytes. Each locked part is its own step with a
// deferred unlock; the socket write runs between steps, outside the lock.
func (c *conn) writer() {
	h := c.h
	defer close(c.writerDone)
	for {
		it, b, ok := c.next()
		if !ok {
			return
		}
		if h.beforeWrite != nil {
			h.beforeWrite(c)
		}
		err := c.write(b)
		if !c.wrote(len(b), err) {
			return
		}
		if it.job != nil && h.afterReplayFrame != nil {
			h.afterReplayFrame()
		}
		if it.last {
			h.locked(func() { h.dropLocked(c, "closed after the last frame") })
			return
		}
	}
}

// next waits for the next queue item and returns it with the bytes to write. A replay item
// stays queued until its last frame. ok is false once the connection is closed.
func (c *conn) next() (it item, b []byte, ok bool) {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	for len(c.q) == 0 && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return item{}, nil, false
	}
	it = c.q[0]
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
	return it, b, true
}

// wrote accounts for n bytes the writer has written or failed to write, and wakes paused pumps.
// It returns false when the write failed and the writer must stop.
func (c *conn) wrote(n int, err error) bool {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	c.queued -= n
	c.h.bp.Broadcast()
	if err != nil {
		c.writeErrorLocked(err)
		return false
	}
	return true
}

// writeErrorLocked ends the writer after a failed write. A hung connection is dropped at once
// (spec 2.6). Any other failure means the peer is gone or going, but frames it sent before it
// closed, a handover among them, may still wait in the socket. The reader reads them and drops
// the connection at end of file, so a handover sent before the close is recorded as handover.
// If the reader has not dropped it within the hang timeout, the connection is dropped anyway.
func (c *conn) writeErrorLocked(err error) {
	h := c.h
	if errors.Is(err, errHung) || c.closed {
		h.dropLocked(c, err.Error())
		return
	}
	c.writeFailed = true
	c.q = nil
	c.queued = 0
	h.bp.Broadcast()
	h.cfg.Logf("holder: write: %v (waiting for the reader)", err)
	c.writeTimer = time.AfterFunc(h.cfg.HangTimeout, func() {
		h.runSafe("write timer", func() {
			h.locked(func() { h.dropLocked(c, "write: "+err.Error()) })
		}, nil)
	})
}

func (c *conn) popLocked() {
	c.q[0] = item{}
	c.q = c.q[1:]
}

var errHung = errors.New("connection hung: no write progress within the hang timeout")

// writeStep bounds one blocking socket write: a second, or a fifth of the hang timeout when that
// is shorter. Write reports bytes moved only when it returns, so the clock below can lag the
// real progress by one step: a hung connection is found within HangTimeout plus one step (5 to
// 6 s with the defaults).
const writeStep = time.Second

// write writes b fully. The connection is hung when no byte reaches the socket for
// HangTimeout. A slow reader that keeps taking bytes is not hung: each write that moves bytes
// restarts the clock.
func (c *conn) write(b []byte) error {
	hang := c.h.cfg.HangTimeout
	step := min(writeStep, hang/5)
	progress := time.Now()
	for len(b) > 0 {
		_ = c.nc.SetWriteDeadline(time.Now().Add(step))
		n, err := c.nc.Write(b)
		b = b[n:]
		if n > 0 {
			progress = time.Now()
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
		if n == 0 && time.Since(progress) >= hang {
			return errHung
		}
	}
	return nil
}

// stepLocked produces the next frame of a replay: a replay data frame, or the replay end. done
// is true for the replay end.
func (c *conn) stepLocked(j *replayJob) (b []byte, done bool) {
	r := j.p.ring
	// Bytes the ring overwrote before the writer reached them are gone. The next frame starts
	// later, gff-hab sees the jump in the offsets, and the replay end says gap.
	if j.next < j.end && j.next < r.oldest() {
		j.next = min(r.oldest(), j.end)
		j.gap = true
	}
	if !j.p.released && j.next < j.end {
		data := r.read(j.next, int(min(j.end-j.next, hp.MaxOutputData)))
		f := &hp.ReplayData{Handle: j.p.handle, Token: j.token, Offset: j.next, Data: data}
		j.next += uint64(len(data))
		return c.encode(f), false
	}
	end := j.end
	if j.p.released {
		end = j.next // a release cut the replay short
	}
	f := &hp.ReplayEnd{Handle: j.p.handle, Token: j.token, Start: j.start, End: end, Gap: j.gap}
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

// replay handles a replay request. The range is fixed now, at the pane's count: an open replay
// ends at that count, and the pane turns live in the same lock hold, so its first live output
// frame starts exactly at the replay's end. Live frames queue behind the replay in the one
// writer queue; none can go out inside it.
func (h *Holder) replay(c *conn, f *hp.Replay) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil || c.closed || c.writeFailed {
		return
	}
	r := p.ring
	count := r.count
	j := &replayJob{p: p, token: f.Token, start: min(f.From, count)}
	if j.start < r.oldest() {
		j.start = r.oldest()
		j.gap = true
	}
	j.end = count
	if f.HasTo {
		j.end = max(min(f.To, count), j.start)
	}
	j.next = j.start
	c.q = append(c.q, item{job: j})
	if !f.HasTo {
		c.subs[p] = true
	}
	c.cond.Signal()
}

// readLoop reads frames until the connection ends and handles each in order.
func (c *conn) readLoop() {
	h := c.h
	defer close(c.readerDone)
	for {
		f, err := c.pc.ReadFrame()
		if err != nil {
			h.locked(func() {
				reason := "read: " + err.Error()
				if c.replacing {
					reason = "replaced by a newer hello"
				}
				h.dropLocked(c, reason)
			})
			return
		}
		var closed bool
		h.locked(func() { closed = c.closed })
		if closed {
			return
		}
		if h.beforeDispatch != nil {
			h.beforeDispatch(c, f)
		}
		h.dispatch(c, f)
	}
}

func (h *Holder) dispatch(c *conn, f hp.Frame) {
	switch f := f.(type) {
	case *hp.Ping:
		h.locked(func() { c.sendLocked(&hp.Pong{Token: f.Token}) })
	case *hp.Pong:
	case *hp.Handover:
		h.locked(func() { c.handover = true })
	case *hp.Drain:
		h.locked(func() { h.draining = true })
		// The registry file says draining before the holder can exit, so a reader never sees
		// a draining holder listed as one that takes new panes.
		h.markDraining()
		h.locked(h.drainExitLocked)
	case *hp.Shutdown:
		h.goSafe("shutdown", h.Shutdown, nil)
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
		h.locked(func() {
			c.errorLocked(f.Header(), hp.CodeBadRequest, 0, fmt.Sprintf("%s is not a request", f.Header().Kind))
		})
	}
}
