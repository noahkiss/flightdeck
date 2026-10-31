// Package holder is the core of gff-hab-holder: the process that owns every PTY on a host.
//
// The holder spawns programs in PTYs, pumps each pane's output into a raw byte ring with an
// absolute byte count, resizes, signals and reaps. It serves the holder protocol
// (internal/holderproto, docs/protocol/holder.md) to one logic process at a time over a local
// Unix socket. It never parses output, and it never depends on a client being connected: every
// pane keeps pumping into its ring whether or not anyone listens.
//
// Concurrency: one mutex (Holder.mu) guards every pane, ring and connection. Pumps, waiters,
// input writers and the connection writer take it briefly and never hold it across a blocking
// read or write. Frames to the logic process go through one queue and one writer per
// connection, so the reader never waits on a write.
//
// The behaviour is specified in docs/specs/hab-holder.md, section 2.
package holder

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// Defaults from the spec. Tests shrink them.
const (
	DefaultRingSize      = 4 << 20                // spec 2.4
	DefaultInputQueue    = 1 << 20                // spec 2.5
	DefaultQueueLimit    = 8 << 20                // spec 2.6, backpressure
	DefaultHangTimeout   = 5 * time.Second        // spec 2.6, protocol 8
	DefaultExitWindow    = 200 * time.Millisecond // spec 2.7
	DefaultShutdownGrace = 5 * time.Second        // spec 2.8
	DefaultHelloTimeout  = 10 * time.Second
)

// Config sets up a holder. Zero fields take the defaults above.
type Config struct {
	// Build is the build id the welcome reports.
	Build string
	// Instance is the holder's instance id. Empty picks a random one.
	Instance string
	// RingSize is the byte ring size per pane.
	RingSize int
	// InputQueue is the bound on each pane's queue of accepted input not yet written.
	InputQueue int
	// QueueLimit is the number of bytes queued to the logic process above which every pump
	// pauses.
	QueueLimit int
	// HangTimeout is how long the connection writer may make no progress, with frames queued,
	// before the connection counts as hung and is dropped.
	HangTimeout time.Duration
	// ExitWindow is how long after a child exits the holder waits for the pane's reader to reach
	// end of file before it reports the exit anyway. Time the reader spends paused by
	// backpressure does not count.
	ExitWindow time.Duration
	// ShutdownGrace is how long shutdown waits for children after the hang-up before it kills
	// them.
	ShutdownGrace time.Duration
	// HelloTimeout bounds the wait for a new connection's hello.
	HelloTimeout time.Duration
	// Logf receives diagnostics. Nil discards them.
	Logf func(format string, args ...any)
}

func (c *Config) fill() {
	if c.RingSize <= 0 {
		c.RingSize = DefaultRingSize
	}
	if c.InputQueue <= 0 {
		c.InputQueue = DefaultInputQueue
	}
	if c.QueueLimit <= 0 {
		c.QueueLimit = DefaultQueueLimit
	}
	if c.HangTimeout <= 0 {
		c.HangTimeout = DefaultHangTimeout
	}
	if c.ExitWindow <= 0 {
		c.ExitWindow = DefaultExitWindow
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = DefaultShutdownGrace
	}
	if c.HelloTimeout <= 0 {
		c.HelloTimeout = DefaultHelloTimeout
	}
	if c.Instance == "" {
		c.Instance = NewInstanceID()
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

// NewInstanceID returns a random holder instance id.
func NewInstanceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Holder owns the panes and serves the protocol.
type Holder struct {
	cfg Config
	reg *hp.Registry
	uid int

	// spawnMu serialises spawns, so two spawns of one uuid cannot both start a program.
	spawnMu sync.Mutex

	mu       sync.Mutex
	bp       *sync.Cond // pumps wait here while the connection is over QueueLimit
	panes    map[hp.Handle]*pane
	byUUID   map[hp.UUID]*pane
	next     hp.Handle
	conn     *conn // the current logic connection, or nil
	previous hp.Previous
	draining bool
	closing  bool
	listener *net.UnixListener

	exitOnce sync.Once
	done     chan struct{}

	// Test hooks. beforeWelcome runs after the hello is read and before the welcome is built;
	// afterWelcome runs after the welcome is queued and before the connection's writer starts.
	// Tests use them to land a child's exit inside the handshake. afterReplayFrame runs in the
	// writer after each replay data frame is written, outside the lock.
	beforeWelcome    func()
	afterWelcome     func()
	afterReplayFrame func()
}

// New returns a holder. It runs nothing until Serve.
func New(cfg Config) *Holder {
	cfg.fill()
	h := &Holder{
		cfg:    cfg,
		reg:    hp.V1(),
		uid:    os.Getuid(),
		panes:  map[hp.Handle]*pane{},
		byUUID: map[hp.UUID]*pane{},
		done:   make(chan struct{}),
	}
	h.bp = sync.NewCond(&h.mu)
	return h
}

// Instance returns the holder's instance id.
func (h *Holder) Instance() string { return h.cfg.Instance }

// Done is closed when the holder has exited: after shutdown, or after a drain's last release.
func (h *Holder) Done() <-chan struct{} { return h.done }

// Listen creates the holder's Unix socket at path. The directory is created with mode 0700 if
// missing and must not be open to other users; the socket gets mode 0600. A stale socket file
// that refuses connections is replaced; a live one is an error. Keep path short: macOS limits a
// socket path to 104 bytes.
func Listen(path string) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("holder: socket directory %s is open to other users (mode %v)", dir, fi.Mode().Perm())
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("holder: socket %s is in use", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Serve accepts logic connections on l until the holder exits. It returns nil after a shutdown
// or a drain exit; the listener is closed and its socket file removed by then.
func (h *Holder) Serve(l *net.UnixListener) error {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		l.Close()
		return nil
	}
	h.listener = l
	h.mu.Unlock()
	for {
		nc, err := l.AcceptUnix()
		if err != nil {
			h.mu.Lock()
			closing := h.closing
			h.mu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		h.goSafe("connection", func() { h.serveConn(nc) })
	}
}

// goSafe runs fn in a goroutine and logs a panic instead of ending the holder (spec 6.6): a
// holder exit ends every program.
func (h *Holder) goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.cfg.Logf("holder: panic in %s: %v", name, r)
			}
		}()
		fn()
	}()
}

func (h *Holder) closeListener() {
	h.mu.Lock()
	l := h.listener
	h.mu.Unlock()
	if l != nil {
		l.Close()
	}
}

// serveConn runs one logic connection: peer check, handshake, then the read loop.
func (h *Holder) serveConn(nc *net.UnixConn) {
	if err := h.checkPeer(nc); err != nil {
		h.cfg.Logf("holder: refused connection: %v", err)
		nc.Close()
		return
	}
	_ = nc.SetReadDeadline(time.Now().Add(h.cfg.HelloTimeout))
	pc := hp.NewConn(nc, h.reg)
	if _, err := pc.AcceptHello(); err != nil {
		h.cfg.Logf("holder: handshake: %v", err)
		nc.Close()
		return
	}
	_ = nc.SetReadDeadline(time.Time{})
	c := newConn(h, nc, pc)
	pc.OnSkip = func(hd hp.Header) {
		h.mu.Lock()
		c.sendLocked(hp.UnknownKind(hd))
		h.mu.Unlock()
	}

	if h.beforeWelcome != nil {
		h.beforeWelcome()
	}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		nc.Close()
		return
	}
	if old := h.conn; old != nil {
		h.dropLocked(old, "replaced by a newer hello")
	}
	// The welcome snapshot and installing c as the current connection happen under one lock
	// hold. A child whose exit is reported before this point shows in the welcome; one reported
	// after it gets an exited frame on c. Never both, never neither.
	c.sendLocked(h.welcomeLocked(pc.Version()))
	h.conn = c
	c.ready = true
	h.mu.Unlock()

	if h.afterWelcome != nil {
		h.afterWelcome()
	}
	h.goSafe("writer", c.writer)
	c.readLoop()
}

func (h *Holder) checkPeer(nc *net.UnixConn) error {
	sc, err := nc.SyscallConn()
	if err != nil {
		return err
	}
	uid := -1
	var uerr error
	if err := sc.Control(func(fd uintptr) { uid, uerr = peerUID(int(fd)) }); err != nil {
		return err
	}
	if uerr != nil {
		return uerr
	}
	if uid != h.uid {
		return fmt.Errorf("peer uid %d is not the holder's uid %d", uid, h.uid)
	}
	return nil
}

func (h *Holder) welcomeLocked(v hp.Version) *hp.Welcome {
	w := &hp.Welcome{
		Version:  v,
		Instance: h.cfg.Instance,
		Pid:      uint32(os.Getpid()),
		Build:    h.cfg.Build,
		RingSize: uint64(h.cfg.RingSize),
		Draining: h.draining,
		Previous: h.previous,
		Panes:    []hp.PaneInfo{},
	}
	handles := make([]hp.Handle, 0, len(h.panes))
	for hd := range h.panes {
		handles = append(handles, hd)
	}
	slices.Sort(handles)
	for _, hd := range handles {
		w.Panes = append(w.Panes, h.panes[hd].infoLocked())
	}
	return w
}

// dropLocked ends connection c. When c is the current connection, the holder records how the
// logic process left: handover if it said so, crash otherwise.
func (h *Holder) dropLocked(c *conn, reason string) {
	if c.closed {
		return
	}
	c.closed = true
	c.cond.Broadcast()
	c.nc.Close()
	if h.conn == c {
		h.conn = nil
		if c.handover {
			h.previous = hp.PreviousHandover
		} else {
			h.previous = hp.PreviousCrash
		}
		h.cfg.Logf("holder: connection dropped: %s", reason)
	}
	// Pumps paused for c's queue resume into the rings alone.
	h.bp.Broadcast()
}

// backpressureLocked reports whether pumps must pause: the current connection has more than
// QueueLimit bytes queued.
func (h *Holder) backpressureLocked() bool {
	c := h.conn
	return c != nil && !c.closed && c.queued > h.cfg.QueueLimit
}

// Shutdown ends every pane and exits (spec 2.8): hang up every pane, wait up to ShutdownGrace
// for the children, kill what remains, reap, send bye, close. It returns when the holder is
// done.
func (h *Holder) Shutdown() {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		<-h.done
		return
	}
	h.closing = true
	ps := make([]*pane, 0, len(h.panes))
	for _, p := range h.panes {
		ps = append(ps, p)
	}
	h.mu.Unlock()
	h.closeListener()

	for _, p := range ps {
		h.mu.Lock()
		p.hangupLocked()
		h.mu.Unlock()
	}
	waitFinal(ps, h.cfg.ShutdownGrace)
	for _, p := range ps {
		h.mu.Lock()
		if !p.reaped {
			_ = p.cmd.Process.Signal(syscall.SIGKILL)
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
		h.mu.Unlock()
	}
	waitFinal(ps, h.cfg.ShutdownGrace)
	h.exit()
}

// waitFinal waits until every pane's exit is reported, or until d passes.
func waitFinal(ps []*pane, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for _, p := range ps {
		select {
		case <-p.final:
		case <-t.C:
			return
		}
	}
}

// exit sends bye, closes the connection and the listener, and closes Done.
func (h *Holder) exit() {
	h.exitOnce.Do(func() {
		h.mu.Lock()
		h.closing = true
		c := h.conn
		if c != nil && !c.closed {
			c.sendLastLocked(&hp.Bye{})
		}
		h.mu.Unlock()
		h.closeListener()
		if c != nil {
			select {
			case <-c.writerDone:
			case <-time.After(h.cfg.HangTimeout):
			}
			h.mu.Lock()
			h.dropLocked(c, "holder exit")
			h.mu.Unlock()
		}
		close(h.done)
	})
}

// drainExitLocked starts the exit of a draining holder whose last pane is gone (spec 4.3).
func (h *Holder) drainExitLocked() {
	if h.draining && len(h.panes) == 0 && !h.closing {
		h.closing = true
		h.goSafe("drain exit", h.exit)
	}
}
