package link

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/termq"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// maxChunk is the most bytes in one chunk a subscriber gets from the buffer, the holder's own
// frame limit.
const maxChunk = 64 << 10

// subQueue is how many chunks a subscriber may have waiting before it is dropped with ErrSlow.
// Flow control by credit is the data connection's job (spec 5.7).
const subQueue = 4096

// PaneStatus describes one pane as the link sees it.
type PaneStatus struct {
	UUID     hp.UUID
	Instance string // the holder that owns the pane
	Build    string // that holder's build
	Handle   hp.Handle
	Pid      uint32
	Cols     int
	Rows     int
	// Offset is the absolute holder offset after the last byte the parser holds (spec 5.4).
	Offset uint64
	// Ready is false until the pane's replay after attach has ended.
	Ready bool
	// Suspect means the screen was rebuilt from incomplete bytes (spec 2.3, 6.7).
	Suspect bool
	// Restored means the screen was restored from the pane's checkpoint when the link attached.
	Restored bool
	// Lost means the pane's holder connection ended. Gone means the holder released it.
	Lost   bool
	Gone   bool
	Exited bool
	Exit   hp.ExitStatus
	// Owner is the client id of the pane's size owner, empty for none, and Reason why it is
	// (SetOwner).
	Owner  string
	Reason api.OwnerReason
}

func sortStatus(s []PaneStatus) {
	slices.SortFunc(s, func(a, b PaneStatus) int { return strings.Compare(a.UUID.String(), b.UUID.String()) })
}

// Screen is a pane's parsed screen at one offset.
type Screen struct {
	Offset  uint64
	Cols    int
	Rows    int
	Suspect bool
	// Text is the active screen with its scrollback as plain text (vt.Terminal.PlainText).
	Text string
	// VT is a snapshot-vt of the screen and at most Config.SnapshotScrollback scrollback rows.
	VT []byte
}

// NoticeKind says what changed on a pane.
type NoticeKind int

const (
	PaneAdded   NoticeKind = iota + 1 // the link started following a pane, or one came back
	PaneExited                        // the pane's child was reaped
	PaneGone                          // the pane is lost or released
	PaneResized                       // Resize changed the pane's size
)

// Notice is one pane change.
type Notice struct {
	Kind NoticeKind
	Pane PaneStatus
}

// ChunkKind says what a Chunk carries.
type ChunkKind int

const (
	ChunkBytes    ChunkKind = iota + 1 // program output from Offset
	ChunkSnapshot                      // a snapshot covering output up to Offset
	ChunkExited                        // the pane's child was reaped; Exit is set
	ChunkResize                        // the pane's size changed to Cols x Rows at Offset
)

// Chunk is one item on a subscription.
type Chunk struct {
	Kind ChunkKind
	// Offset is the absolute holder offset of the first byte (ChunkBytes), or the offset a
	// snapshot covers (ChunkSnapshot). The next ChunkBytes starts at Offset+len(Data) for bytes,
	// or at Offset for a snapshot.
	Offset   uint64
	Data     []byte
	Encoding vt.Encoding // for ChunkSnapshot
	// Live is true when the bytes reached gff-hab as live output and this is their first
	// delivery. Replayed and buffered bytes are not live and get no answer to terminal queries
	// (spec 5.6).
	Live bool
	Exit hp.ExitStatus
	// Cols and Rows are the new size, for ChunkResize, and the size the snapshot was taken at,
	// for ChunkSnapshot.
	Cols, Rows int
	// Owner and Reason are the size owner and why it is the owner (SetOwner), for ChunkResize
	// and ChunkSnapshot.
	Owner  string
	Reason api.OwnerReason
}

// Subscription delivers one pane's output in offset order with no gap and no repeat.
type Subscription struct {
	C <-chan Chunk
	c chan Chunk
	p *pane

	mu   sync.Mutex
	done bool
	err  error
}

// Err says why the subscription ended once C is closed.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.p.mu.Lock()
	delete(s.p.subs, s)
	s.p.mu.Unlock()
	s.end(ErrClosed)
}

func (s *Subscription) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done, s.err = true, err
	close(s.c)
}

// send queues ch, or ends a subscriber that fell too far behind. The caller holds p.mu.
func (s *Subscription) send(ch Chunk) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return false
	}
	select {
	case s.c <- ch:
		return true
	default:
		s.done, s.err = true, ErrSlow
		close(s.c)
		return false
	}
}

// FromNow asks Subscribe for a snapshot of the screen now, then live output.
const FromNow = ^uint64(0)

// pane is the link's state for one pane. The event loop feeds it; other goroutines read it under
// mu.
type pane struct {
	uuid hp.UUID
	ckMu sync.Mutex // one checkpoint write at a time

	mu        sync.Mutex
	holder    *holderclient.Holder
	handle    hp.Handle
	pid       uint32
	term      vt.Terminal
	off       uint64     // the parser holds every byte before off
	seq       seqScanner // whether off falls in the parser's ground state
	buf       []byte     // recent output: bytes [bufStart, off)
	bufStart  uint64
	ready     chan struct{}
	isReady   bool
	replayTok uint32
	suspect   bool
	restored  bool
	lost      error // ErrLost or ErrGone once the pane is no longer followed
	lostAt    time.Time
	exited    bool
	exit      hp.ExitStatus
	final     uint64 // the byte count when the child was reaped; valid once exited
	releasing bool   // a release of the ended pane is in flight
	lastOut   time.Time
	ckOff     uint64
	ckValid   bool
	ckAt      time.Time
	subs      map[*Subscription]struct{}
	snapRows  int // scrollback rows in a snapshot: Config.SnapshotScrollback
	// queries follows the stream for terminal queries; the link answers those in live bytes
	// (spec 5.6). Its offset is off whenever it holds nothing back.
	queries *termq.Scanner
	// inq holds writes to the program's input in order: client input and query answers.
	// inBusy is true while a writer goroutine drains it.
	inq    []*inputReq
	inBusy bool
	inWake chan struct{} // wakes a writer that waits for an answer: a write was queued
	// ansN and ansBytes count the answers in inq or sent and not yet answered; past maxAnswers or maxAnswerBytes a new
	// answer is dropped. ansRefused is set while the holder refuses input as queue full:
	// answers are dropped until it is ready again. ansDropped counts the answers dropped in
	// the current burst, which ends when no answer waits and the holder takes input.
	ansN, ansBytes int
	ansRefused     bool
	ansDropped     int
	resent         int // writes sent again after an abort or a wrong offset
	// owner and reason tag every size change for the subscribers (SetOwner, spec 6.5).
	owner  string
	reason api.OwnerReason
}

// Bounds on a pane's answers to terminal queries waiting to be written. A program that floods
// queries gets some answers, never an unbounded queue, and client input waits behind at most
// this many.
const (
	maxAnswers     = 64
	maxAnswerBytes = 4 << 10
)

// inputReq is one write to a pane's input: client input, with done for the result, or an
// answer to a terminal query. The fields after data are guarded by the pane's mu.
type inputReq struct {
	data   []byte
	done   chan error // buffered; nil for an answer
	answer bool
	n      int // answers in data; 0 for client input
	// retried is set once the write was sent again after a wrong offset.
	retried bool
	// deadline is set when the write is first sent: it is never sent again after it, so a
	// write lands within inputTimeout of first going out or fails.
	deadline time.Time
	// taken is set when the writer takes the request; cancelled when its caller gave up
	// first, so the writer skips it and it is never written.
	taken, cancelled bool
}

func newPane(u hp.UUID, snapRows int) *pane {
	return &pane{uuid: u, ready: make(chan struct{}), subs: map[*Subscription]struct{}{}, snapRows: snapRows,
		queries: termq.NewScanner(0), inWake: make(chan struct{}, 1)}
}

func (p *pane) markReady() {
	if !p.isReady {
		p.isReady = true
		close(p.ready)
	}
}

func size(cols, rows uint16) (int, int) {
	if cols == 0 || rows == 0 {
		return 80, 24
	}
	return int(cols), int(rows)
}

// prepare sets the pane up for an open replay from holder h and returns the offset to replay
// from. back is true when a lost pane came back. It runs on the event loop.
func (p *pane) prepare(l *Link, h *holderclient.Holder, pi hp.PaneInfo) (from uint64, back bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	back = p.lost != nil
	if p.term != nil && p.holder != nil && p.holder.Instance() != h.Instance() {
		// Another holder lists this uuid: the screen held belongs to a different pane.
		l.cfg.Logf("link: pane %s: was on holder %s, now on %s: starting a new screen", pi.UUID, p.holder.Instance(), h.Instance())
		p.term.Close()
		p.term = nil
	}
	// A release in flight for this very pane stays marked, so a scan does not send a second.
	same := p.holder == h && p.handle == pi.Handle
	p.holder, p.handle, p.pid = h, pi.Handle, pi.Pid
	p.lost = nil
	p.ansRefused = false // a refusal belongs to the old holder connection
	p.exited, p.exit, p.final = pi.Exited, pi.Exit, pi.Count
	if !same {
		p.releasing = false
	}
	if p.isReady {
		p.ready, p.isReady = make(chan struct{}), false
	}
	cols, rows := size(pi.Cols, pi.Rows)
	now := time.Now()
	p.ckAt, p.lastOut = now, now
	if p.term != nil {
		// The same gff-hab reconnects to a holder that dropped it: keep the screen and replay
		// what it missed. A gap shows in the replay end.
		if c, r := p.term.Size(); c != cols || r != rows {
			p.resizeLocked(cols, rows)
		}
		return p.off, back
	}
	cfg := vt.Config{Cols: cols, Rows: rows, ScrollbackBytes: int(l.sbCap.Load())}
	ck, err := readCheckpoint(l.cfg.StateDir, pi.UUID)
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		l.cfg.Logf("link: pane %s: unusable checkpoint: %v", pi.UUID, err)
		ck = nil
	case ck != nil && ck.Holder != h.Instance():
		l.cfg.Logf("link: pane %s: checkpoint is from holder %q, the pane is on %q: ignoring it", pi.UUID, ck.Holder, h.Instance())
		ck = nil
	case ck != nil && !slices.Contains(l.cfg.Parser.Encodings(), ck.Encoding):
		l.cfg.Logf("link: pane %s: checkpoint encoding %s not supported by this parser", pi.UUID, ck.Encoding)
		ck = nil
	case ck != nil && ck.Offset > pi.Count:
		l.cfg.Logf("link: pane %s: checkpoint offset %d is past the pane's output %d", pi.UUID, ck.Offset, pi.Count)
		ck = nil
	case ck != nil && ck.Offset < pi.Oldest:
		l.cfg.Logf("link: pane %s: the ring starts at %d, after the checkpoint at %d: rebuilding from the ring alone", pi.UUID, pi.Oldest, ck.Offset)
		ck = nil
	}
	if ck != nil {
		t, err := l.cfg.Parser.Restore(ck.Encoding, ck.Snapshot, vt.Config{Cols: ck.Cols, Rows: ck.Rows, ScrollbackBytes: int(l.sbCap.Load())})
		if err != nil {
			l.cfg.Logf("link: pane %s: restore checkpoint: %v", pi.UUID, err)
			ck = nil
		} else {
			p.term = t
			p.suspect = ck.Suspect
			if ck.Cols != cols || ck.Rows != rows {
				// The pane was resized after the checkpoint; where in the stream is unknown.
				t.Resize(cols, rows)
				p.suspect = true
			}
			p.off, p.bufStart, p.buf = ck.Offset, ck.Offset, nil
			p.ckOff, p.ckValid, p.restored = ck.Offset, true, true
			p.seq.reset()
			p.queries.Reset(p.off) // a checkpoint is taken in the ground state only
			return p.off, back
		}
	}
	t, err := l.cfg.Parser.New(cfg)
	if err != nil {
		l.cfg.Logf("link: pane %s: new terminal: %v", pi.UUID, err)
		t, _ = vt.Fake{}.New(vt.Config{Cols: 80, Rows: 24})
	}
	p.term = t
	p.suspect = pi.Oldest > 0
	if p.suspect {
		l.cfg.Logf("link: pane %s: no checkpoint and the ring starts at %d: screen is suspect", pi.UUID, pi.Oldest)
	}
	p.off, p.bufStart, p.buf = pi.Oldest, pi.Oldest, nil
	p.ckValid, p.restored = false, false
	p.seq.reset()
	p.queries.Reset(p.off)
	return p.off, back
}

// spawned sets up a pane this link just spawned: subscribed from offset 0 at once.
func (p *pane) spawned(l *Link, h *holderclient.Holder, pi hp.PaneInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cols, rows := size(pi.Cols, pi.Rows)
	t, err := l.cfg.Parser.New(vt.Config{Cols: cols, Rows: rows, ScrollbackBytes: int(l.sbCap.Load())})
	if err != nil {
		l.cfg.Logf("link: pane %s: new terminal: %v", pi.UUID, err)
		t, _ = vt.Fake{}.New(vt.Config{Cols: 80, Rows: 24})
	}
	p.holder, p.handle, p.pid, p.term = h, pi.Handle, pi.Pid, t
	p.queries.Reset(0)
	now := time.Now()
	p.ckAt, p.lastOut = now, now
	p.markReady()
}

// feed applies output at offset off. Bytes the parser already holds are dropped; a jump forward
// is a gap and marks the screen suspect. It runs on the event loop.
func (p *pane) feed(l *Link, off uint64, data []byte, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.term == nil || p.lost != nil || len(data) == 0 {
		return
	}
	end := off + uint64(len(data))
	if end <= p.off {
		return
	}
	if off < p.off {
		data = data[p.off-off:]
		off = p.off
	}
	if off > p.off {
		l.cfg.Logf("link: pane %s: gap: bytes %d to %d never arrived; screen is suspect", p.uuid, p.off, off)
		p.jump(off)
	}
	p.parse(l, data, live)
	p.seq.write(data)
	p.off = end
	p.buf = append(p.buf, data...)
	if lim := l.cfg.BufferBytes; len(p.buf) > 2*lim {
		cut := len(p.buf) - lim
		p.buf = append([]byte(nil), p.buf[cut:]...)
		p.bufStart += uint64(cut)
	}
	p.lastOut = time.Now()
	ch := Chunk{Kind: ChunkBytes, Offset: off, Data: data, Live: live}
	for s := range p.subs {
		if !s.send(ch) {
			delete(p.subs, s)
		}
	}
}

// parse writes data to the parser and follows it for terminal queries. In live bytes it answers
// each query once, from the parser's state just after the query, and queues the answer to the
// program's input (spec 5.6). Replayed bytes get no answer. The caller holds p.mu.
func (p *pane) parse(l *Link, data []byte, live bool) {
	if !live {
		p.queries.Feed(data, func(termq.Segment) {})
		p.term.Write(data)
		return
	}
	start, done := p.queries.Offset(), 0
	p.queries.Feed(data, func(seg termq.Segment) {
		if seg.Query == nil {
			return
		}
		end := int(seg.Off + uint64(len(seg.Data)) - start) // the query ends inside data
		p.term.Write(data[done:end])
		done = end
		if ans := termq.Answer(*seg.Query, p.factsLocked(l)); len(ans) > 0 {
			p.queueAnswerLocked(l, ans)
		}
	})
	p.term.Write(data[done:])
}

// factsLocked reads what an answer needs from the parser. The caller holds p.mu.
func (p *pane) factsLocked(l *Link) termq.Facts {
	col, row := p.term.Cursor()
	cols, rows := p.term.Size()
	return termq.Facts{
		Col: col, Row: row, Cols: cols, Rows: rows,
		Mode: p.term.Mode, KeyboardFlags: p.term.KeyboardFlags(),
		Colors: l.colors, Version: l.cfg.Build,
	}
}

// queueInputLocked queues a write to the program's input and starts a writer when none runs.
// Writes reach the holder in queue order. The caller holds p.mu.
func (p *pane) queueInputLocked(l *Link, r *inputReq) {
	p.inq = append(p.inq, r)
	if !p.inBusy {
		p.inBusy = true
		go p.inputWriter(l)
		return
	}
	select {
	case p.inWake <- struct{}{}:
	default:
	}
}

// queueAnswerLocked queues an answer to a terminal query, or drops it when too many wait or
// the holder refuses input. One answer larger than maxAnswerBytes may wait alone. The first
// drop of a burst is logged. The caller holds p.mu.
func (p *pane) queueAnswerLocked(l *Link, ans []byte) {
	if p.ansRefused || p.ansN >= maxAnswers || p.ansN > 0 && p.ansBytes+len(ans) > maxAnswerBytes {
		if p.ansDropped == 0 {
			why := "too many wait"
			if p.ansRefused {
				why = "the holder refuses input"
			}
			l.cfg.Logf("link: pane %s: dropping answers to terminal queries: %s", p.uuid, why)
		}
		p.ansDropped++
		return
	}
	p.ansN++
	p.ansBytes += len(ans)
	p.queueInputLocked(l, &inputReq{data: ans, answer: true, n: 1})
}

// endBurstLocked logs the end of a burst of dropped answers once answers flow again. The
// caller holds p.mu.
func (p *pane) endBurstLocked(l *Link) {
	if p.ansDropped > 0 && p.ansN == 0 && !p.ansRefused {
		l.cfg.Logf("link: pane %s: dropped %d answers to terminal queries", p.uuid, p.ansDropped)
		p.ansDropped = 0
	}
}

// dropAnswersLocked removes the answers waiting in the queue. Answers already sent stay counted
// until the holder answers them. The caller holds p.mu.
func (p *pane) dropAnswersLocked() {
	kept := p.inq[:0]
	for _, r := range p.inq {
		if r.answer {
			p.ansDropped += r.n
			p.ansN, p.ansBytes = p.ansN-r.n, p.ansBytes-len(r.data)
			continue
		}
		kept = append(kept, r)
	}
	clear(p.inq[len(kept):])
	p.inq = kept
}

// inputReady is the holder's word that it takes input again after refusing it.
func (p *pane) inputReady(l *Link) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ansRefused = false
	p.endBurstLocked(l)
}

// inputWriter drains the input queue. It sends each write as soon as it is queued and reads
// the holder's answers in order behind them: the holder answers behind the pane's output, so
// under heavy output an answer comes seconds after the write reached the program, and input must
// not wait for the answer to the write before it. Answers next to each other go in one write.
// Client input gets its error back; a failed answer is logged. A refusal for a full queue drops
// the waiting answers until the holder is ready again.
//
// A failed write makes the writes sent after it fail as aborted, unwritten. The writer then
// sends nothing new until every sent write is answered, and sends the aborted ones again, in
// order, before anything queued after them. Two cases fail them instead. A write sent after
// them was taken (its offset matched once another writer moved the holder's count), so a
// resend would land out of order. Or the write's deadline passed.
func (p *pane) inputWriter(l *Link) {
	type sentWrite struct {
		r   *inputReq
		res chan error // the holder's answer
	}
	type redoWrite struct {
		r   *inputReq
		err error // why it was not written
	}
	var (
		sent    []sentWrite
		redo    []redoWrite // aborted writes, oldest first
		stalled bool        // a write failed: send nothing new until sent is empty
	)
	// fail ends a write that is not sent again. The caller holds p.mu; fail unlocks it.
	fail := func(r *inputReq, err error) {
		if r.answer {
			p.ansN, p.ansBytes = p.ansN-r.n, p.ansBytes-len(r.data)
		}
		p.endBurstLocked(l)
		p.mu.Unlock()
		if r.done != nil {
			r.done <- err
		} else {
			l.cfg.Logf("link: pane %s: answer to a terminal query: %v", p.uuid, err)
		}
	}
	for {
		p.mu.Lock()
		if !stalled {
			for len(p.inq) > 0 && p.inq[0].cancelled {
				p.inq = p.inq[1:]
			}
		}
		if !stalled && len(p.inq) > 0 {
			r := p.inq[0]
			p.inq = p.inq[1:]
			r.taken = true
			if r.answer && len(p.inq) > 0 && p.inq[0].answer && len(r.data)+len(p.inq[0].data) <= maxAnswerBytes {
				c := &inputReq{data: append([]byte{}, r.data...), answer: true, n: r.n, taken: true}
				for len(p.inq) > 0 && p.inq[0].answer && len(c.data)+len(p.inq[0].data) <= maxAnswerBytes {
					c.data = append(c.data, p.inq[0].data...)
					c.n += p.inq[0].n
					p.inq = p.inq[1:]
				}
				r = c
			}
			if r.deadline.IsZero() {
				r.deadline = time.Now().Add(inputTimeout)
			}
			deadline := r.deadline
			h, hd, lost := p.holder, p.handle, p.lost
			p.mu.Unlock()
			w := sentWrite{r: r, res: make(chan error, 1)}
			switch {
			case lost != nil:
				w.res <- lost
			case h == nil:
				w.res <- ErrClosed
			default:
				if ic, err := h.SendInput(hd, r.data); err != nil {
					w.res <- err
				} else {
					go func() {
						ctx, cancel := context.WithDeadline(l.ctx, deadline)
						defer cancel()
						w.res <- ic.Wait(ctx)
					}()
				}
			}
			sent = append(sent, w)
			continue
		}
		if len(sent) == 0 {
			stalled = false
			if len(redo) > 0 {
				var keep []*inputReq
				now := time.Now()
				for _, w := range redo {
					r := w.r
					switch {
					case r.answer && p.ansRefused:
						p.ansN, p.ansBytes = p.ansN-r.n, p.ansBytes-len(r.data)
						p.ansDropped += r.n
					case !now.Before(r.deadline):
						fail(r, fmt.Errorf("input not written within %v: %w", inputTimeout, w.err))
						p.mu.Lock()
					default:
						p.resent++
						keep = append(keep, r)
					}
				}
				p.inq = append(keep, p.inq...)
				redo = nil
				p.endBurstLocked(l)
				p.mu.Unlock()
				continue
			}
			if len(p.inq) == 0 {
				p.inq = nil
				p.inBusy = false
				p.endBurstLocked(l)
				p.mu.Unlock()
				return
			}
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()

		// Wait for the oldest answer, or for a new write to send.
		var err error
		select {
		case err = <-sent[0].res:
		case <-p.inWake:
			continue
		}
		r := sent[0].r
		sent = sent[1:]
		var re *holderclient.RequestError
		refused := errors.As(err, &re) && re.Code == hp.CodeQueueFull
		// Aborted writes never reached the program; send them again. So is a write refused for
		// its offset, which other input moved, once.
		again := errors.Is(err, holderclient.ErrInputAborted) ||
			errors.As(err, &re) && re.Code == hp.CodeInputOffset && !r.retried
		p.mu.Lock()
		if err != nil {
			stalled = true
		}
		if again {
			r.retried = r.retried || !errors.Is(err, holderclient.ErrInputAborted)
			redo = append(redo, redoWrite{r, err})
			p.mu.Unlock()
			continue
		}
		if err == nil && len(redo) > 0 {
			// This write was taken after the ones in redo were not: sending them now would
			// reorder them, so they fail.
			for _, w := range redo {
				fail(w.r, fmt.Errorf("a later input was written first: %w", w.err))
				p.mu.Lock()
			}
			redo = nil
		}
		if r.answer {
			p.ansN, p.ansBytes = p.ansN-r.n, p.ansBytes-len(r.data)
		}
		first := refused && !p.ansRefused
		switch {
		case refused:
			p.ansRefused = true
			p.dropAnswersLocked()
		case err == nil:
			p.ansRefused = false
		}
		p.endBurstLocked(l)
		p.mu.Unlock()
		switch {
		case r.done != nil:
			r.done <- err
		case first:
			l.cfg.Logf("link: pane %s: the holder refuses input: dropping answers to terminal queries until it is ready", p.uuid)
		case err != nil && !refused:
			l.cfg.Logf("link: pane %s: answer to a terminal query: %v", p.uuid, err)
		}
	}
}

// inputTimeout bounds one write to a pane's input.
const inputTimeout = 10 * time.Second

// resizeLocked resizes the parser and tells every subscriber, at the current offset. It reports
// whether the size changed. The caller holds p.mu.
func (p *pane) resizeLocked(cols, rows int) bool {
	if p.term == nil {
		return false
	}
	if c, r := p.term.Size(); c == cols && r == rows {
		return false
	}
	p.term.Resize(cols, rows)
	p.sendSizeLocked()
	return true
}

// sendSizeLocked sends every subscriber a ChunkResize with the parser's size and the owner, at
// the current offset. The caller holds p.mu.
func (p *pane) sendSizeLocked() {
	cols, rows := p.term.Size()
	ch := Chunk{Kind: ChunkResize, Offset: p.off, Cols: cols, Rows: rows, Owner: p.owner, Reason: p.ownerReason()}
	for s := range p.subs {
		if !s.send(ch) {
			delete(p.subs, s)
		}
	}
}

// ownerReason is the reason of the last owner change, or disown when no owner was ever set.
func (p *pane) ownerReason() api.OwnerReason {
	if p.reason == "" {
		return api.OwnerReasonDisown
	}
	return p.reason
}

func (p *pane) replayEnd(l *Link, e *hp.ReplayEnd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.Token != p.replayTok || p.isReady {
		return
	}
	if e.Gap {
		l.cfg.Logf("link: pane %s: replay lost bytes to the ring (sent %d to %d): screen is suspect", p.uuid, e.Start, e.End)
		p.suspect = true
	}
	if e.End > p.off {
		p.jump(e.End)
	}
	p.markReady()
}

// jump moves the pane past bytes that never arrived: the screen is suspect, and every subscriber
// gets a snapshot covering the new offset so its stream stays contiguous. The caller holds p.mu.
func (p *pane) jump(to uint64) {
	p.suspect = true
	p.buf, p.bufStart, p.off = nil, to, to
	p.seq.reset()
	p.queries.Reset(to)
	if len(p.subs) == 0 {
		return
	}
	snap, err := p.term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: p.snapRows})
	for s := range p.subs {
		if err != nil {
			s.end(err)
			delete(p.subs, s)
			continue
		}
		if !s.send(Chunk{Kind: ChunkSnapshot, Offset: to, Data: snap, Encoding: vt.EncodingVT}) {
			delete(p.subs, s)
		}
	}
}

func (p *pane) reaped(st hp.ExitStatus, final uint64) (Notice, bool) {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return Notice{}, false
	}
	p.exited, p.exit, p.final = true, st, final
	for s := range p.subs {
		if !s.send(Chunk{Kind: ChunkExited, Offset: p.off, Exit: st}) {
			delete(p.subs, s)
		}
	}
	p.mu.Unlock()
	return Notice{Kind: PaneExited, Pane: p.status()}, true
}

// lose marks the pane no longer followed and ends its subscriptions with why.
func (p *pane) lose(why error) (Notice, bool) {
	p.mu.Lock()
	if p.lost != nil {
		p.mu.Unlock()
		return Notice{}, false
	}
	p.lost, p.lostAt = why, time.Now()
	// The hab forgets a lost pane's owner (PaneGone), so the pane keeps none: if it comes back,
	// its frames and welcome name no owner until the hab sets one (spec 6.5).
	p.owner, p.reason = "", ""
	for s := range p.subs {
		s.end(why)
		delete(p.subs, s)
	}
	p.markReady()
	p.mu.Unlock()
	return Notice{Kind: PaneGone, Pane: p.status()}, true
}

func (p *pane) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for s := range p.subs {
		s.end(ErrClosed)
		delete(p.subs, s)
	}
	if p.term != nil {
		p.term.Close()
		p.term = nil
	}
	p.markReady()
}

func (p *pane) scrollback(skip, n int) (ScrollbackPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.term == nil {
		return ScrollbackPage{}, ErrClosed
	}
	pg, err := p.term.Scrollback(skip, n)
	if err != nil {
		return ScrollbackPage{}, err
	}
	return ScrollbackPage{ScrollbackPage: pg, Offset: p.off}, nil
}

func (p *pane) setScrollback(c int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.term != nil {
		p.term.SetScrollbackBytes(c)
	}
}

func (p *pane) status() PaneStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PaneStatus{
		UUID: p.uuid, Handle: p.handle, Pid: p.pid, Offset: p.off, Ready: p.isReady,
		Suspect: p.suspect, Restored: p.restored, Lost: errors.Is(p.lost, ErrLost), Gone: errors.Is(p.lost, ErrGone),
		Exited: p.exited, Exit: p.exit, Owner: p.owner, Reason: p.ownerReason(),
	}
	if p.holder != nil {
		st.Instance, st.Build = p.holder.Instance(), p.holder.Build()
	}
	if p.term != nil {
		st.Cols, st.Rows = p.term.Size()
	}
	return st
}

func (p *pane) screen() (Screen, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.term == nil {
		return Screen{}, ErrClosed
	}
	text, err := p.term.PlainText()
	if err != nil {
		return Screen{}, err
	}
	snap, err := p.term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: p.snapRows})
	if err != nil {
		return Screen{}, err
	}
	c, r := p.term.Size()
	return Screen{Offset: p.off, Cols: c, Rows: r, Suspect: p.suspect, Text: text, VT: snap}, nil
}

// checkpoint writes the pane's checkpoint when its screen changed since the last one and, unless
// force, the pane is idle or the interval has passed.
func (p *pane) checkpoint(l *Link, force bool) error {
	p.ckMu.Lock()
	defer p.ckMu.Unlock()
	p.mu.Lock()
	if p.term == nil || !p.isReady || p.lost != nil || (p.ckValid && p.ckOff == p.off) {
		p.mu.Unlock()
		return nil
	}
	if !p.seq.ground() {
		// The stream ends inside an escape sequence or a UTF-8 character, which a snapshot
		// cannot carry. Keep the older checkpoint; the next tick tries again.
		if force {
			l.cfg.Logf("link: pane %s: output ends mid-sequence at %d: keeping the checkpoint at %d", p.uuid, p.off, p.ckOff)
		}
		p.mu.Unlock()
		return nil
	}
	if !force {
		now := time.Now()
		idle := l.cfg.IdleAfter > 0 && now.Sub(p.lastOut) >= l.cfg.IdleAfter
		timed := l.cfg.CheckpointEvery > 0 && now.Sub(p.ckAt) >= l.cfg.CheckpointEvery
		if !idle && !timed {
			p.mu.Unlock()
			return nil
		}
	}
	snap, err := p.term.Snapshot(l.enc, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		p.mu.Unlock()
		return err
	}
	cols, rows := p.term.Size()
	ck := &checkpoint{
		Version: checkpointVersion, Pane: p.uuid.String(), Offset: p.off, Encoding: l.enc,
		Cols: cols, Rows: rows, Suspect: p.suspect, Written: time.Now().UTC(), Snapshot: snap,
	}
	if p.holder != nil {
		ck.Holder = p.holder.Instance()
	}
	p.mu.Unlock()
	if err := writeCheckpoint(l.cfg.StateDir, p.uuid, ck); err != nil {
		return err
	}
	p.mu.Lock()
	p.ckOff, p.ckValid, p.ckAt = ck.Offset, true, time.Now()
	p.mu.Unlock()
	return nil
}

// Subscribe follows pane u's output from offset from, or from a snapshot with FromNow. It waits
// for the pane's replay to end. When the link no longer holds the bytes from from, it asks the
// holder's ring for them; when the ring has lost them too, the subscription starts with a
// snapshot instead (spec 5.4). A resume never repeats a byte.
func (l *Link) Subscribe(ctx context.Context, u hp.UUID, from uint64) (*Subscription, error) {
	p := l.pane(u)
	if p == nil {
		return nil, ErrNoPane
	}
	p.mu.Lock()
	ready := p.ready
	p.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for try := 0; ; try++ {
		p.mu.Lock()
		if p.lost != nil {
			err := p.lost
			p.mu.Unlock()
			return nil, err
		}
		if p.term == nil {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		if from != FromNow && from > p.off {
			p.mu.Unlock()
			return nil, ErrAhead
		}
		if from == FromNow || from >= p.bufStart || try > 0 {
			defer p.mu.Unlock()
			return p.subscribe(l, from, nil)
		}
		// The bytes before the buffer are in the holder's ring, if still anywhere.
		h, hd, upTo := p.holder, p.handle, p.bufStart
		p.mu.Unlock()
		data, end, err := h.ReplayRange(ctx, hd, from, upTo)
		if err != nil || end.Gap || end.Start != from || end.End != upTo || uint64(len(data)) != upTo-from {
			l.cfg.Logf("link: pane %s: resume from %d: ring cannot serve it; sending a snapshot", u, from)
			from = FromNow
			continue
		}
		p.mu.Lock()
		if p.bufStart > upTo { // the buffer moved on meanwhile
			p.mu.Unlock()
			from = FromNow
			continue
		}
		defer p.mu.Unlock()
		return p.subscribe(l, from, data)
	}
}

// subscribe registers a subscription. pre holds the bytes from from up to the buffer's start, when
// from is before it. The caller holds p.mu.
func (p *pane) subscribe(l *Link, from uint64, pre []byte) (*Subscription, error) {
	c := make(chan Chunk, subQueue)
	s := &Subscription{C: c, c: c, p: p}
	if from == FromNow || (pre == nil && from < p.bufStart) {
		snap, err := p.term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: p.snapRows})
		if err != nil {
			return nil, err
		}
		cols, rows := p.term.Size()
		s.send(Chunk{Kind: ChunkSnapshot, Offset: p.off, Data: snap, Encoding: vt.EncodingVT,
			Cols: cols, Rows: rows, Owner: p.owner, Reason: p.ownerReason()})
	} else {
		off := from
		var rest []byte
		if pre != nil {
			rest = append(pre, p.buf...)
		} else {
			rest = p.buf[from-p.bufStart:]
		}
		for len(rest) > 0 {
			n := min(len(rest), maxChunk)
			s.send(Chunk{Kind: ChunkBytes, Offset: off, Data: rest[:n:n]})
			off += uint64(n)
			rest = rest[n:]
		}
	}
	if p.exited {
		s.send(Chunk{Kind: ChunkExited, Offset: p.off, Exit: p.exit})
	}
	p.subs[s] = struct{}{}
	return s, nil
}
