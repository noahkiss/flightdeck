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
	"github.com/noahkiss/flightdeck/internal/holderreg"
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
	DefaultRegistryCheck = 10 * time.Second
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
	// Protocol is the set of frame kinds and the version range the holder speaks. Nil means
	// holderproto.V1(). Tests pass another registry to run a holder at another version.
	Protocol *hp.Registry
	// RegistryDir is the holder registry directory (spec 4.2). When set, Serve writes
	// <RegistryDir>/<Instance>.json, rewrites it when the holder starts draining, and removes
	// it when the holder exits. Empty writes no registry file.
	RegistryDir string
	// RegistryCheck is how often the holder checks that its registry file is still there, and
	// rewrites it when a reader's prune removed it by mistake (spec 4.2).
	RegistryCheck time.Duration
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
	if c.RegistryCheck <= 0 {
		c.RegistryCheck = DefaultRegistryCheck
	}
	if c.Instance == "" {
		c.Instance = NewInstanceID()
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Protocol == nil {
		c.Protocol = hp.V1()
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
	cfg     Config
	reg     *hp.Registry
	uid     int
	started time.Time

	// regMu guards the registry file. regGone is set once the file is removed for good, so a
	// late rewrite (a drain racing the exit) cannot bring it back.
	regMu    sync.Mutex
	regEntry holderreg.Entry
	regGone  bool

	// spawnMu serialises spawns, so two spawns of one uuid cannot both start a program.
	spawnMu sync.Mutex

	mu     sync.Mutex
	bp     *sync.Cond // pumps wait here while the connection is over QueueLimit
	panes  map[hp.Handle]*pane
	byUUID map[hp.UUID]*pane
	// owned holds every pane whose program the holder may still have to end: all panes in
	// panes, plus released panes whose exit is not yet reported. Shutdown ends them all, and
	// the holder never stops serving while it is not empty.
	owned    map[*pane]struct{}
	next     hp.Handle
	conn     *conn // the current logic connection, or nil
	previous hp.Previous
	draining bool
	closing  bool
	listener acceptor
	// drainKill is set once a draining holder with no pane left has scheduled the kill of the
	// released children that still run.
	drainKill bool

	exitOnce sync.Once
	done     chan struct{}

	// Test hooks. beforeWelcome runs after the hello is read and before the welcome is built;
	// afterWelcome runs after the welcome is queued and before the connection's writer starts.
	// Tests use them to land a child's exit inside the handshake. afterReplayFrame runs in the
	// writer after each replay data frame is written, outside the lock.
	beforeWelcome    func()
	afterWelcome     func()
	afterReplayFrame func()
	// beforeDispatch runs in the reader before each request is handled, outside the lock.
	beforeDispatch func(c *conn, f hp.Frame)

	// lockProbe is how long goSafe waits for the holder lock after a panic before it decides
	// the panicking goroutine held it. crash ends the process; tests replace it.
	lockProbe time.Duration
	crash     func(msg string)
}

// acceptor is the listener Serve accepts on. Tests wrap a real listener to inject errors.
type acceptor interface {
	AcceptUnix() (*net.UnixConn, error)
	Addr() net.Addr
	Close() error
}

// New returns a holder. It runs nothing until Serve.
func New(cfg Config) *Holder {
	cfg.fill()
	h := &Holder{
		cfg:     cfg,
		reg:     cfg.Protocol,
		uid:     os.Getuid(),
		started: time.Now(),
		panes:   map[hp.Handle]*pane{},
		byUUID:  map[hp.UUID]*pane{},
		owned:   map[*pane]struct{}{},
		done:    make(chan struct{}),

		lockProbe: 2 * time.Second,
		crash:     func(msg string) { panic(msg) },
	}
	h.bp = sync.NewCond(&h.mu)
	return h
}

// Instance returns the holder's instance id.
func (h *Holder) Instance() string { return h.cfg.Instance }

// Done is closed when the holder has exited: after shutdown, or after a drain's last release.
func (h *Holder) Done() <-chan struct{} { return h.done }

// Listen creates the holder's Unix socket at path. Missing directories are created with mode
// 0700. The socket's directory, and every directory Listen created, must be a real directory
// (not a symlink) owned by this user and closed to other users. The directory's parent must not
// let other users rename it away: when it is writable by them, it must be sticky. The socket
// gets mode 0600. A stale socket file that refuses connections is replaced; a live one is an
// error. Keep path short: macOS limits a socket path to 104 bytes.
func Listen(path string) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	created, err := mkdirs(dir)
	if err != nil {
		return nil, err
	}
	uid := os.Getuid()
	for _, d := range append(created, dir) {
		if err := holderreg.CheckPrivateDir(d, uid); err != nil {
			return nil, err
		}
	}
	if err := holderreg.CheckParent(filepath.Dir(dir), uid); err != nil {
		return nil, err
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

// mkdirs creates dir and any missing parents with mode 0700, and returns the ones it created,
// outermost first.
func mkdirs(dir string) ([]string, error) {
	var missing []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		err := os.Mkdir(missing[i], 0o700)
		if errors.Is(err, os.ErrExist) {
			continue // made by someone else meanwhile; the checks still apply to dir
		}
		if err != nil {
			return nil, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// Serve accepts logic connections on l until the holder exits. With Config.RegistryDir set, it
// first writes the holder's registry file, and returns the error if that fails. It returns nil
// after a shutdown or a drain exit; the listener is closed and its socket and registry files
// removed by then. An accept error
// never ends the holder while it owns a pane, because a holder exit ends every program: Serve
// backs off and tries again. Only a lasting error with no pane left is returned.
func (h *Holder) Serve(l *net.UnixListener) error {
	return h.serve(l)
}

// Accept backoff bounds.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
	acceptLogEvery   = time.Minute
)

func (h *Holder) serve(l acceptor) error {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		l.Close()
		return nil
	}
	h.listener = l
	h.mu.Unlock()
	if err := h.register(l.Addr().String()); err != nil {
		h.closeListener()
		h.exit()
		return err
	}
	if h.cfg.RegistryDir != "" {
		h.goSafe("registry check", h.registryCheck)
	}
	var backoff time.Duration
	var lastLog time.Time
	for {
		nc, err := l.AcceptUnix()
		if err == nil {
			backoff = 0
			h.goSafe("connection", func() { h.serveConn(nc) })
			continue
		}
		h.mu.Lock()
		closing, owns := h.closing, len(h.owned) > 0
		h.mu.Unlock()
		if closing {
			return nil
		}
		if !transientAccept(err) && !owns {
			return err
		}
		backoff = min(max(2*backoff, acceptBackoffMin), acceptBackoffMax)
		if time.Since(lastLog) >= acceptLogEvery {
			lastLog = time.Now()
			h.cfg.Logf("holder: accept: %v (retrying)", err)
		}
		select {
		case <-time.After(backoff):
		case <-h.done:
			return nil
		}
	}
}

// transientAccept reports whether an accept error is expected to clear on its own: out of
// descriptors or memory, or a peer that went away mid-accept.
func transientAccept(err error) bool {
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
		syscall.ECONNABORTED, syscall.ECONNRESET, syscall.EINTR, syscall.EAGAIN, syscall.EPROTO} {
		if errors.Is(err, e) {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// goSafe runs fn in a goroutine and logs a panic instead of ending the holder (spec 6.6): a
// holder exit ends every program. A panic that left the holder lock held cannot be recovered
// from: the state it guards may be half changed, and every other goroutine would block. goSafe
// probes the lock; if it stays held, it crashes the holder with a clear message.
func (h *Holder) goSafe(name string, fn func()) {
	go func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			h.cfg.Logf("holder: panic in %s: %v", name, r)
			// Probe with a blocking Lock raced against a timer. TryLock would fail whenever the
			// mutex is in starvation mode, so heavy contention could fake a held lock and crash
			// the holder. A blocking Lock joins the queue and gets the lock unless it is held.
			got := make(chan struct{})
			go func() {
				h.mu.Lock()
				close(got)
				h.mu.Unlock()
			}()
			select {
			case <-got:
			case <-time.After(h.lockProbe):
				h.crash(fmt.Sprintf("holder: panic in %s left the holder lock held: %v", name, r))
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
	if !h.replaceLocked() {
		h.mu.Unlock()
		nc.Close()
		return
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
	uid, err := holderreg.PeerUID(nc)
	if err != nil {
		return err
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

// replaceRead is the read deadline a newer hello gives the old connection's reader;
// replaceWait bounds how long the newer hello waits for that reader to end.
const (
	replaceRead = 100 * time.Millisecond
	replaceWait = time.Second
)

// replaceLocked ends the current connection, if any, before a newer one is installed (spec 6.8).
// A handover the old gff-hab sent just before its successor's hello may still sit unread in the
// old socket. So the holder gives the old reader a short read deadline, waits for it to read and
// handle what is there and end, and only then records handover or crash. It returns false when
// the holder is closing. It may release the lock while it waits.
func (h *Holder) replaceLocked() bool {
	for {
		if h.closing {
			return false
		}
		old := h.conn
		if old == nil {
			return true
		}
		if old.handover || old.closed {
			h.dropLocked(old, "replaced by a newer hello")
			continue
		}
		old.replacing = true
		// A deadline in the future: one already past fails the read before it takes any
		// buffered bytes.
		_ = old.nc.SetReadDeadline(time.Now().Add(replaceRead))
		h.mu.Unlock()
		select {
		case <-old.readerDone:
		case <-time.After(replaceWait):
		}
		h.mu.Lock()
		h.dropLocked(old, "replaced by a newer hello") // a no-op when the reader dropped it
	}
}

// dropLocked ends connection c. When c is the current connection, the holder records how the
// logic process left: handover if it said so, crash otherwise.
func (h *Holder) dropLocked(c *conn, reason string) {
	if c.closed {
		return
	}
	c.closed = true
	if c.writeTimer != nil {
		c.writeTimer.Stop()
	}
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
// for the children, kill what remains, reap, send bye, close. Released panes whose programs
// still run are ended too. It returns when the holder is done.
func (h *Holder) Shutdown() {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		<-h.done
		return
	}
	h.closing = true
	ps := make([]*pane, 0, len(h.owned))
	for p := range h.owned {
		ps = append(ps, p)
	}
	h.mu.Unlock()
	h.unregister()
	h.closeListener()

	for _, p := range ps {
		h.mu.Lock()
		p.hangupLocked()
		h.mu.Unlock()
	}
	waitFinal(ps, h.cfg.ShutdownGrace)
	for _, p := range ps {
		h.mu.Lock()
		p.killLocked()
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
		h.unregister()
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

// drainExitLocked starts the exit of a draining holder whose last pane is gone (spec 4.3). A
// released pane's program may still run: the release hung it up, and a program that ignores the
// hang-up outlives it. The holder must not exit before it has reaped such a child, because no
// one else would, and launchd does not end a job's processes the way a systemd unit's control
// group does. So the holder exits only once it owns no pane, and when released programs still
// run, it kills their process groups after ShutdownGrace (spec 6.1). The waiter calls this again
// as each one is reaped.
func (h *Holder) drainExitLocked() {
	if !h.draining || h.closing || len(h.panes) > 0 {
		return
	}
	if len(h.owned) == 0 {
		h.closing = true
		h.goSafe("drain exit", h.exit)
		return
	}
	if h.drainKill {
		return
	}
	h.drainKill = true
	h.cfg.Logf("holder: draining with %d released program(s) still running; killing them in %v", len(h.owned), h.cfg.ShutdownGrace)
	time.AfterFunc(h.cfg.ShutdownGrace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closing || len(h.panes) > 0 {
			return
		}
		for p := range h.owned {
			p.killLocked()
		}
	})
}

// register writes the holder's registry file (spec 4.2) for the socket at sock. A file this
// instance left behind that says draining means the holder was draining when it died, and its
// init job restarted it (spec 6.7). The restarted holder has no panes and is not the current
// build, so it starts draining and exits at once, which removes the file and the socket.
func (h *Holder) register(sock string) error {
	dir := h.cfg.RegistryDir
	if dir == "" {
		return nil
	}
	if old, err := holderreg.Read(holderreg.Path(dir, h.cfg.Instance)); err == nil && old.Draining {
		h.cfg.Logf("holder: restarted after a crash while draining (pid %d); exiting", old.Pid)
		h.mu.Lock()
		h.draining = true
		h.mu.Unlock()
	}
	h.mu.Lock()
	draining := h.draining
	h.mu.Unlock()
	r := h.reg.Range()
	h.regMu.Lock()
	h.regEntry = holderreg.Entry{
		Instance: h.cfg.Instance,
		Build:    h.cfg.Build,
		Protocol: holderreg.Protocol{Min: uint16(r.Min), Max: uint16(r.Max)},
		Pid:      os.Getpid(),
		Socket:   sock,
		Started:  h.started,
		Draining: draining,
	}
	err := holderreg.Write(dir, h.regEntry)
	h.regMu.Unlock()
	if err != nil {
		return fmt.Errorf("holder: registry file: %w", err)
	}
	if draining {
		h.mu.Lock()
		h.drainExitLocked()
		h.mu.Unlock()
	}
	return nil
}

// markDraining rewrites the registry file with the draining flag set (spec 4.3 step 4).
func (h *Holder) markDraining() {
	h.regMu.Lock()
	defer h.regMu.Unlock()
	if h.cfg.RegistryDir == "" || h.regGone || h.regEntry.Instance == "" || h.regEntry.Draining {
		return
	}
	h.regEntry.Draining = true
	if err := holderreg.Write(h.cfg.RegistryDir, h.regEntry); err != nil {
		h.cfg.Logf("holder: registry file: %v", err)
	}
}

// registryCheck rewrites the holder's registry file whenever it has gone missing, until the
// holder exits. A reader prunes a file only when it looks stale, but a prune can race a restart
// of the same instance; without its file, a live holder would be invisible to gff-hab for good.
func (h *Holder) registryCheck() {
	t := time.NewTicker(h.cfg.RegistryCheck)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-t.C:
		}
		h.regMu.Lock()
		if !h.regGone && h.regEntry.Instance != "" {
			path := holderreg.Path(h.cfg.RegistryDir, h.regEntry.Instance)
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				h.cfg.Logf("holder: registry file %s is gone; writing it again", path)
				if err := holderreg.Write(h.cfg.RegistryDir, h.regEntry); err != nil {
					h.cfg.Logf("holder: registry file: %v", err)
				}
			}
		}
		h.regMu.Unlock()
	}
}

// unregister removes the holder's own registry file, once. It leaves a file that a newer holder
// of the same instance id has written since.
func (h *Holder) unregister() {
	h.regMu.Lock()
	defer h.regMu.Unlock()
	if h.regGone {
		return
	}
	h.regGone = true
	if h.cfg.RegistryDir == "" || h.regEntry.Instance == "" {
		return
	}
	if err := holderreg.Remove(h.cfg.RegistryDir, h.regEntry); err != nil {
		h.cfg.Logf("holder: registry file: %v", err)
	}
}
