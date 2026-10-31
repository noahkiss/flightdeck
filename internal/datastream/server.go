package datastream

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/termq"
)

// Defaults from spec 5.7.
const (
	DefaultInitialCredit = 256 << 10
	DefaultMaxBehind     = 4 << 20
	DefaultHelloTimeout  = 10 * time.Second
	// subscribeTimeout bounds the wait for a pane's replay to end before the welcome.
	subscribeTimeout = 30 * time.Second
)

// Sub is one subscription to a pane's output, as link.Subscription gives it.
type Sub interface {
	Chunks() <-chan link.Chunk
	Err() error
	Close()
}

// Source is the holder link as the data streams use it.
type Source interface {
	// Subscribe follows a pane from offset from, or from a snapshot with link.FromNow. It fails
	// with link.ErrAhead for an offset past the pane's output, and with link.ErrNoPane,
	// link.ErrLost or link.ErrGone when the pane cannot be followed.
	Subscribe(ctx context.Context, pane string, from uint64) (Sub, error)
	// Size returns the pane's size; ok is false for a pane the link does not follow.
	Size(pane string) (cols, rows int, ok bool)
}

// Config sets up a Server. Source is required.
type Config struct {
	Source Source
	// InitialCredit is a stream's credit at the start, in bytes of screen and output (the data
	// of snapshot and bytes frames). Default 256 KiB.
	InitialCredit int
	// MaxBehind is how many bytes of output may wait for credit before the stream drops them and
	// sends a fresh snapshot instead. It also caps the credit a stream holds. Default 4 MiB.
	MaxBehind int
	// HelloTimeout bounds the wait for the hello frame. Default 10 s.
	HelloTimeout time.Duration
	Logf         func(format string, args ...any)
}

// Server serves data connections and issues their tickets.
type Server struct {
	cfg     Config
	tickets *Tickets
	ctx     context.Context // ends at Close, so no stream waits on a pane's replay past it
	cancel  context.CancelFunc

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns a server.
func New(cfg Config) *Server {
	if cfg.InitialCredit <= 0 {
		cfg.InitialCredit = DefaultInitialCredit
	}
	if cfg.MaxBehind <= 0 {
		cfg.MaxBehind = DefaultMaxBehind
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = DefaultHelloTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, tickets: NewTickets(nil), conns: map[net.Conn]struct{}{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// RefusedError is a refusal with an API error kind.
type RefusedError struct {
	Kind api.ErrorKind
	Hint string
}

func (e *RefusedError) Error() string { return e.Hint }

// Ticket issues a data ticket for client to open one data connection to pane (spec 5.2). An
// unknown pane is refused with not_found.
func (s *Server) Ticket(client, pane string, observe bool) (string, time.Duration, error) {
	if _, _, ok := s.cfg.Source.Size(pane); !ok {
		return "", 0, &RefusedError{Kind: api.ErrorKindNotFound, Hint: "no pane " + pane}
	}
	tok, ttl := s.tickets.Issue(client, pane, observe)
	return tok, ttl, nil
}

// Close ends every data connection and waits for them.
func (s *Server) Close() {
	s.cancel()
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// ServeConn runs one data connection until it ends. br reads nc and may already hold the hello.
func (s *Server) ServeConn(nc net.Conn, br *bufio.Reader) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		nc.Close()
		return
	}
	s.conns[nc] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		nc.Close()
		s.mu.Lock()
		delete(s.conns, nc)
		s.mu.Unlock()
		s.wg.Done()
	}()
	if br == nil {
		br = bufio.NewReaderSize(nc, 64<<10)
	}
	st, err := s.open(nc, br)
	if err != nil {
		var re *RefusedError
		if !errors.As(err, &re) {
			re = &RefusedError{Kind: api.ErrorKindInvalid, Hint: err.Error()}
		}
		payload, _ := json.Marshal(api.DataReject{Kind: re.Kind, Hint: re.Hint})
		nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		nc.Write(AppendFrame(nil, api.FrameReject, payload))
		return
	}
	st.run(br)
}

// open reads the hello, redeems the ticket, subscribes and sends the welcome.
func (s *Server) open(nc net.Conn, br *bufio.Reader) (*stream, error) {
	nc.SetReadDeadline(time.Now().Add(s.cfg.HelloTimeout))
	f, err := ReadFrame(br)
	if err != nil {
		return nil, err
	}
	nc.SetReadDeadline(time.Time{})
	if f.Kind != api.FrameHello {
		return nil, errors.New("the first frame on a data connection must be a hello")
	}
	var h api.DataHello
	if err := json.Unmarshal(f.Payload, &h); err != nil {
		return nil, errors.New("hello: " + err.Error())
	}
	t, ok := s.tickets.Redeem(h.Ticket)
	if !ok {
		return nil, &RefusedError{Kind: api.ErrorKindTicketInvalid, Hint: "the ticket is unknown, used or expired"}
	}
	from := link.FromNow
	if h.Offset != nil {
		from = *h.Offset
	}
	ctx, cancel := context.WithTimeout(s.ctx, subscribeTimeout)
	defer cancel()
	sub, err := s.cfg.Source.Subscribe(ctx, t.Pane, from)
	if errors.Is(err, link.ErrAhead) {
		// The client holds an offset this pane never reached: start over.
		sub, err = s.cfg.Source.Subscribe(ctx, t.Pane, link.FromNow)
	}
	if err != nil {
		return nil, refusal(t.Pane, err)
	}
	cols, rows, _ := s.cfg.Source.Size(t.Pane)
	id := make([]byte, 8)
	rand.Read(id)
	w := api.DataWelcome{Stream: hex.EncodeToString(id), Cols: cols, Rows: rows}
	payload, _ := json.Marshal(w)
	st := &stream{
		s: s, nc: nc, t: t, sub: sub,
		credit: int64(s.cfg.InitialCredit),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	st.out = append(st.out, AppendFrame(nil, api.FrameWelcome, payload))
	return st, nil
}

// refusal names why pane cannot be followed: not_found for a pane that is gone or unknown,
// unavailable for the rest.
func refusal(pane string, err error) *RefusedError {
	kind := api.ErrorKindUnavailable
	if errors.Is(err, link.ErrNoPane) || errors.Is(err, link.ErrGone) {
		kind = api.ErrorKindNotFound
	}
	return &RefusedError{Kind: kind, Hint: "pane " + pane + ": " + err.Error()}
}

// item is one frame waiting for credit. cost is the screen or output bytes it carries (snapshot
// and bytes frames), and 0 for the rest.
type item struct {
	kind  api.FrameKind
	off   uint64 // bytes frames: offset of data[0]; resize frames: the offset they apply at
	data  []byte // bytes frames: program output; other frames: the payload
	cost  int
	final bool // the stream ends after this frame
}

// stream is one data connection after its welcome.
type stream struct {
	s   *Server
	nc  net.Conn
	t   Ticket
	sub Sub

	scan    *termq.Scanner // nil until the first snapshot or bytes chunk
	pending []item         // frames waiting for credit, in order
	behind  int            // output bytes in pending; a snapshot does not count
	// held are resize frames at offsets the scanner has not passed: they follow the bytes it
	// holds back, so they wait until those come out.
	held   []item
	resync bool // the backlog was dropped: resubscribe for a snapshot once there is credit
	ended  bool // the subscription ended; finish once pending is sent

	wake chan struct{} // the reader or the writer has news
	done chan struct{} // the reader or the writer stopped

	mu      sync.Mutex
	credit  int64    // may go below zero by one snapshot
	out     [][]byte // frames for the writer
	outWake chan struct{}
	closed  bool
}

func (st *stream) signal() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// run serves the stream: a reader for credit frames, a writer, and this loop, which moves
// chunks from the subscription into frames and hands them to the writer within the credit.
func (st *stream) run(br *bufio.Reader) {
	st.outWake = make(chan struct{}, 1)
	var once sync.Once
	stop := func() { once.Do(func() { close(st.done) }) }
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); st.writeLoop(stop) }()
	go st.readLoop(br, stop)
	defer func() {
		if st.sub != nil {
			st.sub.Close()
		}
		st.nc.Close() // unblocks a writer stuck on a client that stopped reading
		st.mu.Lock()
		st.closed = true
		st.mu.Unlock()
		st.wakeWriter()
		<-writerDone
	}()
	for {
		var chunks <-chan link.Chunk
		if st.sub != nil && !st.ended {
			chunks = st.sub.Chunks()
		}
		select {
		case ch, ok := <-chunks:
			if !ok {
				st.subEnded()
			} else {
				st.chunk(ch)
			}
		case <-st.wake:
		case <-st.done:
			return
		}
		if !st.flush() {
			return
		}
	}
}

// subEnded handles the end of the subscription. A subscriber that fell behind the link's own
// queue resubscribes for a snapshot; any other end finishes the stream once pending is sent.
func (st *stream) subEnded() {
	err := st.sub.Err()
	st.sub = nil
	if errors.Is(err, link.ErrSlow) {
		st.dropBacklog()
		return
	}
	st.s.cfg.Logf("data stream for pane %s: %v", st.t.Pane, err)
	st.ended = true
	st.endWith(refusal(st.t.Pane, err))
}

// endWith queues a reject frame naming why the stream cannot go on; the stream ends after it.
// After an exited frame it is never sent, since the stream already ended there.
func (st *stream) endWith(re *RefusedError) {
	st.resync = false
	payload, _ := json.Marshal(api.DataReject{Kind: re.Kind, Hint: re.Hint})
	st.pending = append(st.pending, item{kind: api.FrameReject, data: payload, final: true})
}

// dropBacklog drops every frame that waits for credit; a fresh snapshot follows (spec 5.7).
func (st *stream) dropBacklog() {
	if st.sub != nil {
		st.sub.Close()
		st.sub = nil
	}
	st.pending, st.behind, st.held, st.resync = nil, 0, nil, true
}

// add queues a frame. Output past MaxBehind drops the backlog. A snapshot does not count
// towards it: it cannot be split or skipped, and a snapshot larger than MaxBehind would
// otherwise be dropped and fetched again for ever.
func (st *stream) add(it item) {
	st.pending = append(st.pending, it)
	if it.kind != api.FrameBytes {
		return
	}
	st.behind += it.cost
	if st.behind > st.s.cfg.MaxBehind {
		st.dropBacklog()
	}
}

// release queues the held resize frames whose offset the scanner has passed: every byte before
// it has come out as text or a query.
func (st *stream) release() {
	out := st.scan.Offset() - uint64(st.scan.Held())
	for len(st.held) > 0 && st.held[0].off <= out && !st.resync {
		h := st.held[0]
		st.held = st.held[1:]
		st.add(h)
	}
}

// releaseAll queues every held resize frame, as before a snapshot or an exit.
func (st *stream) releaseAll() {
	for len(st.held) > 0 && !st.resync {
		h := st.held[0]
		st.held = st.held[1:]
		st.add(h)
	}
}

// chunk turns one subscription chunk into frames. Terminal queries are left out: each bytes
// frame covers one contiguous run of holder offsets.
func (st *stream) chunk(ch link.Chunk) {
	if st.resync {
		return
	}
	switch ch.Kind {
	case link.ChunkSnapshot:
		st.releaseAll()
		st.scan = termq.NewScanner(ch.Offset)
		p := SnapshotPayload(ch.Offset, string(ch.Encoding), ch.Data)
		st.add(item{kind: api.FrameSnapshot, data: p, cost: len(ch.Data)})
	case link.ChunkBytes:
		if st.scan == nil || st.scan.Offset() != ch.Offset {
			st.scan = termq.NewScanner(ch.Offset)
		}
		st.scan.Feed(ch.Data, st.text)
		st.release()
	case link.ChunkResize:
		// Owners are stage 8 (spec 6): until then a resize names no owner, reason disown.
		p, _ := json.Marshal(api.ResizeFrame{Cols: ch.Cols, Rows: ch.Rows, Reason: api.OwnerReasonDisown})
		it := item{kind: api.FrameResize, off: ch.Offset, data: p}
		if st.scan != nil && st.scan.Held() > 0 {
			// The scanner holds back bytes from before the resize: it goes out after them.
			st.held = append(st.held, it)
			return
		}
		st.add(it)
	case link.ChunkExited:
		if st.scan != nil {
			st.scan.Flush(st.text) // a sequence cut off by the exit is text
		}
		st.releaseAll()
		p, _ := json.Marshal(api.ExitedFrame{ExitStatus: link.APIExit(ch.Exit)})
		st.add(item{kind: api.FrameExited, data: p, final: true})
	}
}

// text queues program output, joining it to the last bytes frame when the offsets touch.
func (st *stream) text(seg termq.Segment) {
	// A held resize inside this segment splits it, so the resize goes out at its offset.
	for len(st.held) > 0 && !st.resync {
		h := st.held[0]
		if h.off > seg.Off {
			if seg.Query != nil || h.off >= seg.Off+uint64(len(seg.Data)) {
				break
			}
			n := h.off - seg.Off
			st.text(termq.Segment{Off: seg.Off, Data: seg.Data[:n]})
			seg.Off, seg.Data = h.off, seg.Data[n:]
			continue
		}
		st.held = st.held[1:]
		st.add(h)
	}
	if seg.Query != nil || st.resync {
		return
	}
	data := seg.Data
	if n := len(st.pending); n > 0 {
		last := &st.pending[n-1]
		if last.kind == api.FrameBytes && last.off+uint64(len(last.data)) == seg.Off && len(last.data) < maxBytesFrame {
			k := min(len(data), maxBytesFrame-len(last.data))
			last.data = append(last.data[:len(last.data):len(last.data)], data[:k]...)
			last.cost += k
			st.behind += k
			data = data[k:]
			seg.Off += uint64(k)
		}
	}
	for len(data) > 0 {
		k := min(len(data), maxBytesFrame)
		st.add(item{kind: api.FrameBytes, off: seg.Off, data: data[:k:k], cost: k})
		if st.resync {
			return
		}
		data = data[k:]
		seg.Off += uint64(k)
	}
	if st.behind > st.s.cfg.MaxBehind {
		st.dropBacklog()
	}
}

// flush hands frames to the writer within the credit. It returns false when the stream is done.
func (st *stream) flush() bool {
	st.mu.Lock()
	credit := st.credit
	st.mu.Unlock()
	start := credit
	if st.resync && credit > 0 {
		ctx, cancel := context.WithTimeout(st.s.ctx, subscribeTimeout)
		sub, err := st.s.cfg.Source.Subscribe(ctx, st.t.Pane, link.FromNow)
		cancel()
		if err != nil {
			// Tell the client why the stream ends rather than closing on it.
			st.s.cfg.Logf("data stream for pane %s: resubscribe: %v", st.t.Pane, err)
			st.ended = true
			st.endWith(refusal(st.t.Pane, err))
		} else {
			st.sub, st.resync, st.scan = sub, false, nil
			return true // the snapshot arrives as the next chunk
		}
	}
	var frames [][]byte
	final := false
	for len(st.pending) > 0 && !final {
		it := &st.pending[0]
		if it.cost > 0 && credit <= 0 {
			break
		}
		var payload []byte
		cost := it.cost
		switch it.kind {
		case api.FrameBytes:
			n := len(it.data)
			if int64(n) > credit {
				n = int(credit) // split at the credit; the rest waits
			}
			payload = BytesPayload(it.off, it.data[:n])
			cost = n
			if n < len(it.data) {
				it.data, it.off, it.cost = it.data[n:], it.off+uint64(n), it.cost-n
				credit -= int64(cost)
				st.behind -= cost
				frames = append(frames, AppendFrame(nil, it.kind, payload))
				continue
			}
		default:
			payload = it.data
		}
		credit -= int64(cost)
		if it.kind == api.FrameBytes {
			st.behind -= cost
		}
		final = it.final
		frames = append(frames, AppendFrame(nil, it.kind, payload))
		st.pending = st.pending[1:]
	}
	if len(frames) > 0 {
		st.mu.Lock()
		st.credit -= start - credit
		st.out = append(st.out, frames...)
		st.mu.Unlock()
		st.wakeWriter()
	}
	if final || (st.ended && len(st.pending) == 0) {
		st.mu.Lock()
		st.closed = true // the writer drains, then closes
		st.mu.Unlock()
		st.wakeWriter()
		<-st.done
		return false
	}
	return true
}

func (st *stream) wakeWriter() {
	select {
	case st.outWake <- struct{}{}:
	default:
	}
}

// writeLoop writes queued frames. It ends after a write error, or once the stream is closed and
// its frames are written.
func (st *stream) writeLoop(stop func()) {
	defer stop()
	for {
		st.mu.Lock()
		batch, closed := st.out, st.closed
		st.out = nil
		st.mu.Unlock()
		if len(batch) > 0 {
			st.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
			bufs := net.Buffers(batch)
			if _, err := bufs.WriteTo(st.nc); err != nil {
				st.nc.Close()
				return
			}
			continue
		}
		if closed {
			st.nc.Close()
			return
		}
		select {
		case <-st.outWake:
		case <-st.done:
			st.mu.Lock()
			closed = st.closed
			st.mu.Unlock()
			if !closed {
				return
			}
		}
	}
}

// writeTimeout bounds one write. Writes stay within the credit, so a client that reads nothing
// for this long is gone.
const writeTimeout = 45 * time.Second

// readLoop reads credit frames until the client closes the connection.
func (st *stream) readLoop(br *bufio.Reader, stop func()) {
	defer stop()
	for {
		f, err := ReadFrame(br)
		if err != nil {
			return
		}
		if f.Kind != api.FrameCredit {
			continue // the server ignores frames it does not expect
		}
		var c api.CreditFrame
		if json.Unmarshal(f.Payload, &c) != nil {
			continue
		}
		// The credit a stream holds is capped at MaxBehind: credit past it is ignored, so a
		// client cannot make gff-hab queue more than that for it.
		lim := int64(st.s.cfg.MaxBehind)
		st.mu.Lock()
		if room := lim - st.credit; room <= 0 || c.Bytes >= uint64(room) {
			st.credit = max(st.credit, lim)
		} else {
			st.credit += int64(c.Bytes)
		}
		st.mu.Unlock()
		st.signal()
	}
}
