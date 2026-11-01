package datastream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/link"
)

type fakeSub struct {
	c      chan link.Chunk
	mu     sync.Mutex
	closed bool
	err    error
}

func (s *fakeSub) Chunks() <-chan link.Chunk { return s.c }

func (s *fakeSub) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeSub) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed, s.err = true, link.ErrClosed
		close(s.c)
	}
}

// send queues ch unless the sub is closed.
func (s *fakeSub) send(ch link.Chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.c <- ch
	}
}

// fakeSource serves one pane, "p". Each Subscribe gets a fresh sub; FromNow ones start with a
// snapshot covering off.
type fakeSource struct {
	mu   sync.Mutex
	off  uint64
	subs []*fakeSub
	from []uint64
	new  chan *fakeSub
	snap []byte // the snapshot's screen; "SNAP" when nil
	fail error  // when set, Subscribe fails with it
	n    int    // Subscribe calls
	// entered and gate, when set, make Subscribe say it was called, then wait for gate.
	entered chan struct{}
	gate    chan struct{}
}

func newFakeSource() *fakeSource { return &fakeSource{new: make(chan *fakeSub, 16)} }

func (f *fakeSource) Subscribe(_ context.Context, pane string, from uint64) (Sub, error) {
	if pane != "p" {
		return nil, link.ErrNoPane
	}
	if f.gate != nil {
		f.entered <- struct{}{}
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.fail != nil {
		return nil, f.fail
	}
	if from != link.FromNow && from > f.off {
		return nil, link.ErrAhead
	}
	s := &fakeSub{c: make(chan link.Chunk, 4096)}
	if from == link.FromNow {
		snap := f.snap
		if snap == nil {
			snap = []byte("SNAP")
		}
		s.c <- link.Chunk{Kind: link.ChunkSnapshot, Offset: f.off, Data: snap, Encoding: "snapshot-vt"}
	}
	f.subs = append(f.subs, s)
	f.from = append(f.from, from)
	f.new <- s
	return s, nil
}

func (f *fakeSource) Size(pane string) (int, int, bool) { return 80, 24, pane == "p" }
func (f *fakeSource) Owner(string) string               { return "" }

// out sends program output to sub at the source's offset.
func (f *fakeSource) out(s *fakeSub, data string) {
	f.mu.Lock()
	off := f.off
	f.off += uint64(len(data))
	f.mu.Unlock()
	s.send(link.Chunk{Kind: link.ChunkBytes, Offset: off, Data: []byte(data), Live: true})
}

func open(t *testing.T, srv *Server, hello api.DataHello) (*Client, api.DataWelcome, error) {
	t.Helper()
	a, b := net.Pipe()
	go srv.ServeConn(a, nil)
	t.Cleanup(func() { b.Close() })
	return Open(b, hello)
}

func next(t *testing.T, c *Client) Msg {
	t.Helper()
	type res struct {
		m   Msg
		err error
	}
	ch := make(chan res, 1)
	go func() { m, err := c.Next(); ch <- res{m, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("next: %v", r.err)
		}
		return r.m
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return Msg{}
}

func TestTickets(t *testing.T) {
	now := time.Unix(1000, 0)
	tk := NewTickets(func() time.Time { return now })
	tok, ttl := tk.Issue("c1", "p", true)
	if ttl != 30*time.Second {
		t.Errorf("ttl %v", ttl)
	}
	if got, ok := tk.Redeem(tok); !ok || got.Pane != "p" || got.Client != "c1" || !got.Observe {
		t.Fatalf("redeem: %+v %v", got, ok)
	}
	if _, ok := tk.Redeem(tok); ok {
		t.Error("a ticket worked twice")
	}
	tok, _ = tk.Issue("c1", "p", false)
	if o, f := tk.Held("c1", "p"); o || !f {
		t.Errorf("held with one full ticket: observe %v, full %v", o, f)
	}
	tk.Issue("c1", "p", true)
	if o, f := tk.Held("c1", "p"); !o || !f {
		t.Errorf("held with a full and an observer ticket: observe %v, full %v", o, f)
	}
	if o, f := tk.Held("c2", "p"); o || f {
		t.Errorf("another client holds: observe %v, full %v", o, f)
	}
	now = now.Add(30 * time.Second)
	if o, f := tk.Held("c1", "p"); o || f {
		t.Errorf("expired tickets still held: observe %v, full %v", o, f)
	}
	if _, ok := tk.Redeem(tok); ok {
		t.Error("an expired ticket worked")
	}
}

// TestObserverFromTicket: a client is an observer of a pane from the moment it holds an observer
// ticket for it, through the stream's opening, to its open stream (spec 6.8). A full ticket or
// stream makes it not an observer.
func TestObserverFromTicket(t *testing.T) {
	src := newFakeSource()
	src.entered, src.gate = make(chan struct{}, 1), make(chan struct{})
	srv := New(Config{Source: src})
	defer srv.Close()
	if srv.Observer("thumb", "p") {
		t.Fatal("observer with no ticket and no stream")
	}
	tok, _, err := srv.Ticket("thumb", "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if !srv.Observer("thumb", "p") {
		t.Fatal("not an observer while it holds an observer ticket")
	}
	type opened struct {
		c   *Client
		err error
	}
	done := make(chan opened, 1)
	go func() {
		c, _, err := open(t, srv, api.DataHello{Ticket: tok})
		done <- opened{c, err}
	}()
	<-src.entered // the ticket is redeemed and the stream not yet open
	if !srv.Observer("thumb", "p") {
		t.Error("not an observer while its stream opens")
	}
	close(src.gate)
	o := <-done
	if o.err != nil {
		t.Fatal(o.err)
	}
	if !srv.Observer("thumb", "p") {
		t.Error("not an observer with an open observer stream")
	}
	if _, _, err := srv.Ticket("thumb", "p", false); err != nil {
		t.Fatal(err)
	}
	if srv.Observer("thumb", "p") {
		t.Error("an observer while it holds a full ticket")
	}
	if srv.Observer("other", "p") {
		t.Error("another client is an observer")
	}
}

func TestRejects(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src})
	defer srv.Close()
	if _, _, err := srv.Ticket("c", "nope", false); err == nil {
		t.Error("a ticket for an unknown pane")
	}
	_, _, err := open(t, srv, api.DataHello{Ticket: "forged"})
	var re *RejectError
	if !errors.As(err, &re) || re.Kind != api.ErrorKindTicketInvalid {
		t.Errorf("forged ticket: %v", err)
	}
}

// TestStripAndCredit: a query is left out with a skip in the offsets; the stream stops at its
// credit and goes on when credit comes back; the exited frame ends it.
func TestStripAndCredit(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src, InitialCredit: 10})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, w, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	if w.Cols != 80 || w.Rows != 24 || w.Stream == "" {
		t.Errorf("welcome %+v", w)
	}
	sub := <-src.new
	if m := next(t, c); m.Kind != api.FrameSnapshot || string(m.Data) != "SNAP" || m.Offset != 0 {
		t.Fatalf("first frame %+v", m)
	}
	// The 4-byte snapshot used 4 of 10 credit.
	src.out(sub, "ab\x1b[6ncdefghij")
	m := next(t, c)
	if m.Kind != api.FrameBytes || m.Offset != 0 || string(m.Data) != "ab" {
		t.Fatalf("frame %+v", m)
	}
	m = next(t, c)
	if m.Offset != 6 || string(m.Data) != "cdef" {
		t.Fatalf("after the query: %+v", m)
	}
	got := make(chan Msg, 1)
	go func() { m, _ := c.Next(); got <- m }()
	select {
	case m := <-got:
		t.Fatalf("a frame past the credit: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	c.Credit(3)
	m = <-got
	if m.Offset != 10 || string(m.Data) != "ghi" {
		t.Fatalf("after credit: %+v", m)
	}
	c.Credit(100)
	if m := next(t, c); m.Offset != 13 || string(m.Data) != "j" {
		t.Fatalf("rest: %+v", m)
	}
	sub.send(link.Chunk{Kind: link.ChunkExited, Offset: 14})
	if m := next(t, c); m.Kind != api.FrameExited || m.Exited == nil {
		t.Fatalf("exit: %+v", m)
	}
	if _, err := c.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after exit: %v", err)
	}
}

// TestBehindGetsSnapshot: a client that falls more than MaxBehind behind gets a snapshot at
// the current offset instead of the backlog, once it returns credit.
func TestBehindGetsSnapshot(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src, InitialCredit: 4, MaxBehind: 100, Logf: t.Logf})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	next(t, c) // snapshot, uses all the credit
	src.out(sub, string(bytes.Repeat([]byte("0123456789"), 30)))
	for len(sub.c) > 0 { // the stream took the chunk, and handles it before it reads credit
		time.Sleep(time.Millisecond)
	}
	c.Credit(1000)
	select {
	case <-src.new:
	case <-time.After(5 * time.Second):
		t.Fatal("no resubscribe")
	}
	m := next(t, c)
	if m.Kind != api.FrameSnapshot || m.Offset != 300 {
		t.Fatalf("got %+v, want a snapshot at 300", m)
	}
}

// TestResume: a resume names the offset; the stream goes on from there with no snapshot.
func TestResume(t *testing.T) {
	src := newFakeSource()
	src.off = 50
	srv := New(Config{Source: src})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	off := uint64(40)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok, Offset: &off})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	sub.send(link.Chunk{Kind: link.ChunkBytes, Offset: 40, Data: bytes.Repeat([]byte("x"), 10)})
	if m := next(t, c); m.Kind != api.FrameBytes || m.Offset != 40 || len(m.Data) != 10 {
		t.Fatalf("got %+v", m)
	}
	// An offset past the pane's output starts over with a snapshot.
	tok, _, _ = srv.Ticket("c", "p", false)
	off = 999
	c2, _, err := open(t, srv, api.DataHello{Ticket: tok, Offset: &off})
	if err != nil {
		t.Fatal(err)
	}
	if m := next(t, c2); m.Kind != api.FrameSnapshot || m.Offset != 50 {
		t.Fatalf("got %+v", m)
	}
}

// TestSnapshotOverMaxBehind: a snapshot larger than MaxBehind reaches the client, and the
// stream subscribes once rather than dropping it and fetching it again for ever.
func TestSnapshotOverMaxBehind(t *testing.T) {
	src := newFakeSource()
	src.snap = bytes.Repeat([]byte("s"), 1000)
	srv := New(Config{Source: src, InitialCredit: 10, MaxBehind: 100, Logf: t.Logf})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	if m := next(t, c); m.Kind != api.FrameSnapshot || len(m.Data) != 1000 {
		t.Fatalf("got %+v, want the 1000-byte snapshot", m.Kind)
	}
	c.Credit(2000)
	src.out(sub, "after")
	if m := next(t, c); m.Kind != api.FrameBytes || string(m.Data) != "after" {
		t.Fatalf("got %+v", m)
	}
	src.mu.Lock()
	n := src.n
	src.mu.Unlock()
	if n != 1 {
		t.Errorf("%d subscribes, want 1", n)
	}
}

// TestCreditCapped: the credit a stream holds stops at MaxBehind; credit past it is ignored.
func TestCreditCapped(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src, InitialCredit: 4, MaxBehind: 100, Logf: t.Logf})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	next(t, c) // the 4-byte snapshot uses all the credit
	c.Credit(1 << 40)
	src.out(sub, "x") // flows only once the credit frame was read
	if m := next(t, c); string(m.Data) != "x" {
		t.Fatalf("got %+v", m)
	}
	src.out(sub, string(bytes.Repeat([]byte("a"), 60)))
	src.out(sub, string(bytes.Repeat([]byte("b"), 60)))
	got := 0
	for got < 99 {
		got += len(next(t, c).Data)
	}
	if got != 99 {
		t.Fatalf("got %d bytes, want 99: the credit was capped at 100", got)
	}
	ch := make(chan Msg, 1)
	go func() { m, _ := c.Next(); ch <- m }()
	select {
	case m := <-ch:
		t.Fatalf("a frame past the capped credit: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	c.Credit(21)
	if m := <-ch; len(m.Data) != 21 {
		t.Fatalf("after credit: %+v", m)
	}
}

// TestResubscribeFails: a stream whose resubscribe fails ends with a reject frame, then closes.
func TestResubscribeFails(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src, InitialCredit: 4, MaxBehind: 100, Logf: t.Logf})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	next(t, c)
	src.out(sub, string(bytes.Repeat([]byte("0123456789"), 30))) // past MaxBehind: dropped
	for len(sub.c) > 0 {
		time.Sleep(time.Millisecond)
	}
	src.mu.Lock()
	src.fail = link.ErrGone
	src.mu.Unlock()
	c.Credit(1000)
	_, err = c.Next()
	var re *RejectError
	if !errors.As(err, &re) || re.Kind != api.ErrorKindNotFound {
		t.Fatalf("got %v, want a not_found reject", err)
	}
	if _, err := c.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the reject: %v", err)
	}
}

// TestSubscriptionLost: a subscription that ends without an exit (the pane was lost) ends the
// stream with an unavailable reject.
func TestSubscriptionLost(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src, Logf: t.Logf})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	next(t, c)
	sub.mu.Lock()
	sub.closed, sub.err = true, link.ErrLost
	close(sub.c)
	sub.mu.Unlock()
	_, err = c.Next()
	var re *RejectError
	if !errors.As(err, &re) || re.Kind != api.ErrorKindUnavailable {
		t.Fatalf("got %v, want an unavailable reject", err)
	}
}

// TestResizeAfterHeldBytes: a resize at an offset the stream's scanner has not passed (it holds
// back the start of a sequence) goes out after the bytes before that offset.
func TestResizeAfterHeldBytes(t *testing.T) {
	src := newFakeSource()
	srv := New(Config{Source: src, Logf: t.Logf})
	defer srv.Close()
	tok, _, _ := srv.Ticket("c", "p", false)
	c, _, err := open(t, srv, api.DataHello{Ticket: tok})
	if err != nil {
		t.Fatal(err)
	}
	sub := <-src.new
	next(t, c)
	src.out(sub, "ab\x1b[") // the scanner holds back ESC [
	sub.send(link.Chunk{Kind: link.ChunkResize, Offset: 4, Cols: 100, Rows: 30})
	src.out(sub, "1mcd")
	var off uint64
	for {
		m := next(t, c)
		if m.Kind == api.FrameResize {
			if off != 4 || m.Resize.Cols != 100 {
				t.Fatalf("resize after offset %d, want 4: %+v", off, m.Resize)
			}
			break
		}
		if m.Kind != api.FrameBytes || m.Offset != off {
			t.Fatalf("frame %+v at %d", m, off)
		}
		off = m.End()
	}
	if m := next(t, c); m.Kind != api.FrameBytes || m.Offset != 4 || string(m.Data) != "1mcd" {
		t.Fatalf("after the resize: %+v", m)
	}
}
