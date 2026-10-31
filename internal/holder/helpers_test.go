package holder

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// waitLong bounds every wait in these tests. Conditions are waited for, never slept on.
// GFF_TEST_TIMEOUT_SCALE multiplies it on slow machines; see docs/testing.md.
var waitLong = scaleTimeout(10 * time.Second)

// scaleTimeout multiplies d by $GFF_TEST_TIMEOUT_SCALE, a positive number such as 3 or 2.5.
// Unset, it returns d. A value that does not parse panics, so a typo is not silently ignored.
func scaleTimeout(d time.Duration) time.Duration {
	v := os.Getenv("GFF_TEST_TIMEOUT_SCALE")
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		panic(fmt.Sprintf("GFF_TEST_TIMEOUT_SCALE=%q: want a positive number", v))
	}
	return time.Duration(float64(d) * f)
}

var testEnv = []string{"PATH=/usr/bin:/bin", "TERM=dumb", "LC_ALL=C", "HOME=/"}

// testHolder starts a holder on a socket in a short temporary directory (macOS caps socket
// paths at 104 bytes, and t.TempDir paths can be longer). prep runs before Serve, so hooks set
// there need no lock.
func testHolder(t *testing.T, cfg Config, prep func(h *Holder)) (*Holder, string) {
	t.Helper()
	return testHolderOn(t, cfg, prep, nil)
}

// testHolderOn is testHolder with the listener wrapped by wrap, when wrap is not nil.
func testHolderOn(t *testing.T, cfg Config, prep func(h *Holder), wrap func(*net.UnixListener) acceptor) (*Holder, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "h.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Build == "" {
		cfg.Build = "test"
	}
	var logMu sync.Mutex
	var logBuf strings.Builder
	cfg.Logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&logBuf, format+"\n", args...)
	}
	h := New(cfg)
	if prep != nil {
		prep(h)
	}
	var a acceptor = l
	if wrap != nil {
		a = wrap(l)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := h.serve(a); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		h.Shutdown()
		<-served
		os.RemoveAll(dir)
		if t.Failed() {
			logMu.Lock()
			t.Logf("holder log:\n%s", logBuf.String())
			logMu.Unlock()
		}
	})
	return h, path
}

// client is a test logic process. With a background reader, frames land in a channel; without
// one, the test reads the socket itself (pacing and hang tests).
type client struct {
	t      *testing.T
	nc     net.Conn
	pc     *hp.Conn
	w      *hp.Welcome
	frames chan hp.Frame
	rerr   chan error
	token  atomic.Uint32
	all    []hp.Frame // every frame next returned, in order
}

func dial(t *testing.T, path string, background bool) *client {
	t.Helper()
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, nc: nc, pc: hp.NewConn(nc, hp.V1()), frames: make(chan hp.Frame, 1<<16), rerr: make(chan error, 1)}
	t.Cleanup(func() { nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(waitLong))
	w, err := c.pc.Hello("test-client")
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.SetDeadline(time.Time{})
	c.w = w
	if background {
		go func() {
			for {
				f, err := c.pc.ReadFrame()
				if err != nil {
					c.rerr <- err
					return
				}
				c.frames <- f
			}
		}()
	}
	return c
}

func (c *client) tok() uint32 { return c.token.Add(1) }

func (c *client) send(f hp.Frame) {
	c.t.Helper()
	if err := c.pc.WriteFrame(f); err != nil {
		c.t.Fatalf("send %s: %v", f.Header().Kind, err)
	}
}

// next returns the next frame from the background reader.
func (c *client) next() hp.Frame {
	c.t.Helper()
	select {
	case f := <-c.frames:
		c.all = append(c.all, f)
		return f
	case err := <-c.rerr:
		// The reader queues every frame before it reports the end; take those first.
		c.rerr <- err
		select {
		case f := <-c.frames:
			c.all = append(c.all, f)
			return f
		default:
		}
		c.t.Fatalf("connection ended: %v", err)
	case <-time.After(waitLong):
		c.t.Fatalf("no frame within %v", waitLong)
	}
	return nil
}

// until returns the first frame that matches; frames before it stay in c.all.
func (c *client) until(match func(hp.Frame) bool) hp.Frame {
	c.t.Helper()
	for {
		if f := c.next(); match(f) {
			return f
		}
	}
}

// closed waits for the background reader to see the connection end.
func (c *client) closed() error {
	c.t.Helper()
	for {
		select {
		case f := <-c.frames:
			c.all = append(c.all, f)
		case err := <-c.rerr:
			return err
		case <-time.After(waitLong):
			c.t.Fatal("connection did not end")
		}
	}
}

// sync sends a ping and waits for its pong. Every earlier request has been handled by then.
func (c *client) sync() {
	c.t.Helper()
	tk := c.tok()
	c.send(&hp.Ping{Token: tk})
	c.until(func(f hp.Frame) bool { p, ok := f.(*hp.Pong); return ok && p.Token == tk })
}

func uuidN(n int) hp.UUID {
	var u hp.UUID
	u[0], u[15] = 0xfd, byte(n)
	u[14] = byte(n >> 8)
	return u
}

// spawn starts argv in a new pane and returns the spawned frame.
func (c *client) spawn(n int, cols, rows uint16, argv ...string) *hp.Spawned {
	c.t.Helper()
	tk := c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: uuidN(n), Cols: cols, Rows: rows, Cwd: "/", Argv: argv, Env: testEnv})
	f := c.until(func(f hp.Frame) bool { return f.Header().Token == tk })
	s, ok := f.(*hp.Spawned)
	if !ok {
		c.t.Fatalf("spawn %v: got %#v", argv, f)
	}
	return s
}

func (c *client) sh(n int, script string) *hp.Spawned {
	c.t.Helper()
	return c.spawn(n, 80, 24, "/bin/sh", "-c", script)
}

// output gathers the pane's live output frames seen so far and checks they are contiguous.
func (c *client) output(hd hp.Handle) []byte {
	c.t.Helper()
	var b []byte
	var next uint64
	first := true
	for _, f := range c.all {
		o, ok := f.(*hp.Output)
		if !ok || o.Handle != hd {
			continue
		}
		if !first && o.Offset != next {
			c.t.Fatalf("pane %d output at %d, expected %d", hd, o.Offset, next)
		}
		first = false
		b = append(b, o.Data...)
		next = o.Offset + uint64(len(o.Data))
	}
	return b
}

// waitOutput reads frames until the pane's output contains s.
func (c *client) waitOutput(hd hp.Handle, s string) {
	c.t.Helper()
	for !bytes.Contains(c.output(hd), []byte(s)) {
		f := c.next()
		if e, ok := f.(*hp.Exited); ok && e.Handle == hd && !bytes.Contains(c.output(hd), []byte(s)) {
			c.t.Fatalf("pane %d exited before %q; output %q", hd, s, c.output(hd))
		}
	}
}

func (c *client) exited(hd hp.Handle) *hp.Exited {
	c.t.Helper()
	return c.until(func(f hp.Frame) bool { e, ok := f.(*hp.Exited); return ok && e.Handle == hd }).(*hp.Exited)
}

// errorFor waits for the error answering token tk.
func (c *client) errorFor(tk uint32) *hp.Error {
	c.t.Helper()
	f := c.until(func(f hp.Frame) bool { return f.Header().Token == tk })
	e, ok := f.(*hp.Error)
	if !ok {
		c.t.Fatalf("token %d: want error, got %#v", tk, f)
	}
	return e
}

// noErrorFor checks, after a sync, that no frame answered token tk.
func (c *client) noErrorFor(tk uint32) {
	c.t.Helper()
	c.sync()
	for _, f := range c.all {
		if e, ok := f.(*hp.Error); ok && e.Token == tk {
			c.t.Fatalf("token %d: unexpected error %+v", tk, e)
		}
	}
}

// eventually waits for cond, checked under the holder lock.
func eventually(t *testing.T, h *Holder, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		h.mu.Lock()
		ok := cond()
		h.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// paneOf returns the holder's pane for a handle. Read its fields under h.mu.
func paneOf(t *testing.T, h *Holder, hd hp.Handle) *pane {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.panes[hd]
	if p == nil {
		t.Fatalf("no pane %d", hd)
	}
	return p
}

// readFrame reads one frame from a client with no background reader.
func (c *client) readFrame(d time.Duration) (hp.Frame, error) {
	_ = c.nc.SetReadDeadline(time.Now().Add(d))
	defer c.nc.SetReadDeadline(time.Time{})
	return c.pc.ReadFrame()
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "connection reset")
}

func describe(f hp.Frame) string {
	switch f := f.(type) {
	case *hp.Output:
		return fmt.Sprintf("output(%d @%d +%d)", f.Handle, f.Offset, len(f.Data))
	case *hp.ReplayData:
		return fmt.Sprintf("replay-data(%d tok %d @%d +%d)", f.Handle, f.Token, f.Offset, len(f.Data))
	default:
		return fmt.Sprintf("%s %+v", f.Header().Kind, f)
	}
}
