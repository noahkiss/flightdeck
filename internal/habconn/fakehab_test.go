package habconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/datastream"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// waitLong bounds every wait; GFF_TEST_TIMEOUT_SCALE raises it on a slow machine.
func waitLong() time.Duration {
	d := 15 * time.Second
	if s := os.Getenv("GFF_TEST_TIMEOUT_SCALE"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 {
			panic(fmt.Sprintf("GFF_TEST_TIMEOUT_SCALE=%q: want a positive number", s))
		}
		d = time.Duration(float64(d) * f)
	}
	return d
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 4*waitLong())
	t.Cleanup(cancel)
	return ctx
}

// shortDir returns a private directory with a short path (macOS socket paths max 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffhc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

const testPane = "p1"

// fakeHab is gff-hab built from the real control and data stream servers and the real
// structure store, over a fake holder and a fake pane source. Restart starts a new process
// in all but name: a new control server (so a new epoch), a new data stream server and a
// reopened store, while the holder and the pane's output live on. Every connection is
// tracked, so a test can drop the data or control connections from the hab's side.
type fakeHab struct {
	t      *testing.T
	sock   string
	holder *structure.FakeHolder
	src    *paneSource

	mu     sync.Mutex
	dir    string
	store  *structure.Store
	srv    *control.Server
	data   *datastream.Server
	served chan struct{}
	conns  map[*trackConn]struct{}
}

func startFakeHab(t *testing.T) *fakeHab {
	t.Helper()
	d := shortDir(t)
	h := &fakeHab{
		t:      t,
		sock:   filepath.Join(d, "run", "hab.sock"),
		holder: structure.NewFakeHolder(structure.HolderPane{ID: testPane, HolderID: "h1", HolderBuild: "test", Cols: 80, Rows: 24}),
		src:    newPaneSource(),
		dir:    filepath.Join(d, "state"),
		conns:  map[*trackConn]struct{}{},
	}
	h.start()
	t.Cleanup(h.stop)
	return h
}

func (h *fakeHab) start() {
	t := h.t
	var pub atomic.Pointer[control.Server]
	st, err := structure.Open(structure.Config{
		Dir:     h.dir,
		Holders: h.holder,
		OnEvent: func(e structure.Event) {
			if s := pub.Load(); s != nil {
				s.Publish(e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data := datastream.New(datastream.Config{Source: h.src})
	srv := control.New(control.Config{
		Build:        "test",
		Host:         api.HostInfo{ID: "host-a"},
		DeviceID:     "SHA256:test",
		Capabilities: []string{"data-streams", "snapshot-vt"},
		Source:       st,
		Data:         dataService{data},
	})
	pub.Store(srv)
	l, err := control.Listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Serve(&trackListener{Listener: l, h: h})
	}()
	h.mu.Lock()
	h.store, h.srv, h.data, h.served = st, srv, data, served
	h.mu.Unlock()
}

func (h *fakeHab) stop() {
	h.mu.Lock()
	srv, data, st, served := h.srv, h.data, h.store, h.served
	h.srv, h.data, h.store = nil, nil, nil
	h.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Close()
	data.Close()
	<-served
	st.Close()
}

// restart stops the hab and starts it again. With lose, the new hab has an empty state
// directory, as after a lost state file: revisions restart lower than the client holds.
func (h *fakeHab) restart(lose bool) {
	h.stop()
	if lose {
		h.mu.Lock()
		h.dir += "x"
		h.mu.Unlock()
	}
	h.start()
}

func (h *fakeHab) epoch() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.srv.Epoch()
}

func (h *fakeHab) snapshot() api.Structure {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.store.Snapshot(api.HostInfo{ID: "host-a"})
}

func (h *fakeHab) createSession(name string) api.Session {
	h.mu.Lock()
	st := h.store
	h.mu.Unlock()
	s, err := st.CreateSession(structure.SessionSpec{Name: name, Pane: testPane, Index: -1})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// serveConn serves one in-process connection, for the pipe transport. With the hab down, the
// connection closes at once, as a refused dial would.
func (h *fakeHab) serveConn(nc net.Conn) {
	h.mu.Lock()
	srv := h.srv
	h.mu.Unlock()
	if srv == nil {
		nc.Close()
		return
	}
	srv.ServeConn(h.track(nc))
}

func (h *fakeHab) track(nc net.Conn) *trackConn {
	c := &trackConn{Conn: nc, h: h}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// drop closes, from the hab's side, every connection whose first byte is first.
func (h *fakeHab) drop(first byte) int {
	h.mu.Lock()
	var cs []*trackConn
	for c := range h.conns {
		if c.first.Load() == int32(first)+1 {
			cs = append(cs, c)
		}
	}
	h.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	return len(cs)
}

func (h *fakeHab) dropData() int    { return h.drop(byte(api.FrameHello)) }
func (h *fakeHab) dropControl() int { return h.drop('{') }

type trackListener struct {
	net.Listener
	h *fakeHab
}

func (l *trackListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.h.track(nc), nil
}

// trackConn notes the first byte the hab reads, which tells a data connection from a
// control connection (spec 4.1), and forgets itself on Close.
type trackConn struct {
	net.Conn
	h     *fakeHab
	first atomic.Int32 // first byte + 1; 0 until read
}

func (c *trackConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.first.CompareAndSwap(0, int32(p[0])+1)
	}
	return n, err
}

func (c *trackConn) Close() error {
	c.h.mu.Lock()
	delete(c.h.conns, c)
	c.h.mu.Unlock()
	return c.Conn.Close()
}

// paneSource is the holder link as the data streams see it, for one pane whose whole output
// it keeps from offset start on. A subscriber tails the output from its offset; a
// subscription from FromNow or from before start begins with a snapshot.
type paneSource struct {
	mu    sync.Mutex
	out   []byte // output from offset start
	start uint64
	wake  chan struct{} // closed and replaced on every write
	froms []uint64      // the from of every Subscribe: a data hello's offset, or FromNow
}

func newPaneSource() *paneSource { return &paneSource{wake: make(chan struct{})} }

// write appends program output.
func (s *paneSource) write(b []byte) {
	s.mu.Lock()
	s.out = append(s.out, b...)
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// output returns a copy of everything written from offset start on, and the offset after it.
func (s *paneSource) output() ([]byte, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.out...), s.start + uint64(len(s.out))
}

// subscribes returns the from of every Subscribe so far, in order.
func (s *paneSource) subscribes() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.froms...)
}

// trim forgets the output before off, so a resume from earlier gets a snapshot.
func (s *paneSource) trim(off uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append([]byte(nil), s.out[off-s.start:]...)
	s.start = off
}

func (s *paneSource) Subscribe(_ context.Context, pane string, from uint64) (datastream.Sub, error) {
	if pane != testPane {
		return nil, link.ErrNoPane
	}
	s.mu.Lock()
	s.froms = append(s.froms, from)
	end := s.start + uint64(len(s.out))
	if from != link.FromNow && from > end {
		s.mu.Unlock()
		return nil, link.ErrAhead
	}
	sub := &fakeSub{c: make(chan link.Chunk), stop: make(chan struct{})}
	cursor := from
	var first *link.Chunk
	if from == link.FromNow || from < s.start {
		cursor = end
		first = &link.Chunk{Kind: link.ChunkSnapshot, Offset: end, Data: []byte(fmt.Sprintf("SNAP@%d\r\n", end)),
			Encoding: "snapshot-vt", Cols: 80, Rows: 24}
	}
	s.mu.Unlock()
	go sub.tail(s, cursor, first)
	return sub, nil
}

func (s *paneSource) Size(pane string) (int, int, bool) { return 80, 24, pane == testPane }
func (s *paneSource) Owner(string) string               { return "" }

type fakeSub struct {
	c    chan link.Chunk
	stop chan struct{}
	once sync.Once
}

func (f *fakeSub) Chunks() <-chan link.Chunk { return f.c }
func (f *fakeSub) Err() error                { return link.ErrClosed }
func (f *fakeSub) Close()                    { f.once.Do(func() { close(f.stop) }) }

func (f *fakeSub) tail(s *paneSource, cursor uint64, first *link.Chunk) {
	defer close(f.c)
	send := func(ch link.Chunk) bool {
		select {
		case f.c <- ch:
			return true
		case <-f.stop:
			return false
		}
	}
	if first != nil && !send(*first) {
		return
	}
	for {
		s.mu.Lock()
		end := s.start + uint64(len(s.out))
		wake := s.wake
		if cursor < s.start {
			s.mu.Unlock() // trimmed under a live subscriber: end it, as a lost pane would
			return
		}
		var data []byte
		if cursor < end {
			n := min(end-cursor, 8<<10)
			data = append([]byte(nil), s.out[cursor-s.start:cursor-s.start+n]...)
		}
		s.mu.Unlock()
		if data != nil {
			if !send(link.Chunk{Kind: link.ChunkBytes, Offset: cursor, Data: data, Live: true}) {
				return
			}
			cursor += uint64(len(data))
			continue
		}
		select {
		case <-wake:
		case <-f.stop:
			return
		}
	}
}

// dataService turns the data stream server's refusals into API errors, as gff-hab does.
type dataService struct{ *datastream.Server }

func (d dataService) Ticket(client, pane string, observe bool) (string, time.Duration, error) {
	tok, ttl, err := d.Server.Ticket(client, pane, observe)
	var re *datastream.RefusedError
	if errors.As(err, &re) {
		return "", 0, control.Errorf(re.Kind, "%s", re.Hint)
	}
	return tok, ttl, err
}
