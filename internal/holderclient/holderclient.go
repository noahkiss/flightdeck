// Package holderclient is gff-hab's side of the holders on one host: it finds every holder
// through the registry directory, keeps one connection per live holder at that holder's own
// protocol version, picks the holder that takes new panes, and drains holders of other builds
// (docs/specs/hab-holder.md 4.2 and 4.3, docs/protocol/holder.md 9).
//
// Starting a holder process is the init system's job, never gff-hab's (spec 5.4). The client
// takes a Start function that asks the init system for a holder of its own build, then waits
// for that holder's registry file and socket.
//
// Frames from every holder that answer no request (output, exited, input ready, bye) arrive on
// one channel, Events. The caller must keep reading it: a full channel holds back the holder
// connection that feeds it, and the holder then applies backpressure to its programs.
//
// The package is pure Go.
package holderclient

import (
	"context"
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

// Config sets up a client. Zero fields take the defaults below.
type Config struct {
	// Dir is the holder registry directory (spec 4.2).
	Dir string
	// Build is this gff-hab's build id. New panes go to a holder of this build; holders of
	// other builds are drained.
	Build string
	// Protocol is the set of kinds and versions this gff-hab speaks. Nil means holderproto.V1().
	Protocol *hp.Registry
	// Start asks the init system to start a holder of this build in Dir (spec 5.4). It may
	// return before the holder listens; Upgrade waits for the registry file. Nil means the
	// client cannot start holders.
	Start func(ctx context.Context) error
	// DialTimeout bounds connecting to one holder and its handshake. Default 5 s.
	DialTimeout time.Duration
	// WriteTimeout bounds one frame write to a holder. Default 10 s.
	WriteTimeout time.Duration
	// Poll is how often Upgrade lists the directory while it waits for a started holder.
	// Default 20 ms.
	Poll time.Duration
	// Events is the capacity of the event channel. Default 1024.
	Events int
	// Logf receives diagnostics. Nil discards them.
	Logf func(format string, args ...any)
}

// ErrNoHolder means no holder of this build is connected and not draining.
var ErrNoHolder = errors.New("holderclient: no current holder")

// ErrClosed means the holder connection has ended.
var ErrClosed = errors.New("holderclient: holder connection closed")

// ErrInputAborted means an input frame was not written because an input frame sent before it,
// to the same pane, failed. The holder takes input frames in order, so the later ones fail on
// their offset; nothing of them reached the program. Sending the input again is safe.
var ErrInputAborted = errors.New("holderclient: input aborted: an earlier input to the pane failed")

// RequestError is a holder's error or spawn failed answer to a request.
type RequestError struct {
	Ref     hp.Kind
	Code    hp.ErrorCode
	Offset  uint64
	Message string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("holderclient: %s refused: code %d: %s", e.Ref, e.Code, e.Message)
}

// Event is a frame from a holder that answers no request, or the end of a holder connection.
type Event struct {
	Holder *Holder
	// Frame is the frame, or nil when the connection ended.
	Frame hp.Frame
	// Err says why the connection ended, when Frame is nil. It is nil after a bye.
	Err error
}

// Client holds the connections to every holder on the host.
type Client struct {
	cfg    Config
	events chan Event
	quit   chan struct{} // closed by Close

	mu          sync.Mutex
	holders     map[string]*Holder // by instance id
	unreachable map[string]holderreg.Entry
	closed      bool
}

// New returns a client. It connects to nothing until Scan or Upgrade.
func New(cfg Config) *Client {
	if cfg.Protocol == nil {
		cfg.Protocol = hp.V1()
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 20 * time.Millisecond
	}
	if cfg.Events <= 0 {
		cfg.Events = 1024
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Client{
		cfg:         cfg,
		events:      make(chan Event, cfg.Events),
		quit:        make(chan struct{}),
		holders:     map[string]*Holder{},
		unreachable: map[string]holderreg.Entry{},
	}
}

// Events returns the channel of events from every holder.
func (c *Client) Events() <-chan Event { return c.events }

// Scan lists the registry directory. It removes stale files (spec 4.2: the pid is gone or the
// socket refuses connections), connects to every live holder it is not yet connected to, and
// refreshes the draining flag of the ones it is. A holder that shares no protocol version with
// this gff-hab is recorded as unreachable (spec 4.3) and left alone.
//
// Scan trusts the directory only as far as the holder does: it must be a private directory of
// this user (Listen's check), or Scan fails. It also removes temporary and unreadable files that
// a crash left there.
func (c *Client) Scan(ctx context.Context) error {
	if err := holderreg.CheckDir(c.cfg.Dir); err != nil {
		return fmt.Errorf("holderclient: registry directory: %w", err)
	}
	if err := holderreg.PruneJunk(c.cfg.Dir, time.Now()); err != nil {
		c.cfg.Logf("holderclient: %v", err)
	}
	entries, errs := holderreg.List(c.cfg.Dir)
	for _, err := range errs {
		c.cfg.Logf("holderclient: %v", err)
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Instance] = true
	}
	c.mu.Lock()
	for id := range c.unreachable {
		if !listed[id] {
			delete(c.unreachable, id) // gone from the registry
		}
	}
	c.mu.Unlock()
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return ErrClosed
		}
		h := c.holders[e.Instance]
		u, refused := c.unreachable[e.Instance]
		if refused && (u.Pid != e.Pid || !u.Started.Equal(e.Started)) {
			delete(c.unreachable, e.Instance) // restarted: try again
			refused = false
		}
		c.mu.Unlock()
		if h != nil && h.sameHolder(e) {
			h.mu.Lock()
			h.entry = e
			h.draining = h.draining || e.Draining
			h.mu.Unlock()
			continue
		}
		if refused {
			if stale, why := holderreg.Stale(e, false); stale {
				c.mu.Lock()
				delete(c.unreachable, e.Instance)
				c.mu.Unlock()
				c.prune(e, why)
			}
			continue
		}
		if stale, why := holderreg.Stale(e, false); stale {
			c.prune(e, why)
			continue
		}
		if err := c.connect(ctx, e); err != nil {
			c.cfg.Logf("holderclient: holder %s: %v", e.Instance, err)
		}
	}
	return nil
}

func (c *Client) prune(e holderreg.Entry, why string) {
	c.cfg.Logf("holderclient: removing stale holder %s: %s", e.Instance, why)
	if err := holderreg.RemoveStale(c.cfg.Dir, e); err != nil {
		c.cfg.Logf("holderclient: %v", err)
	}
}

// deadlineWriter bounds every write to a holder, so a hung holder cannot block gff-hab forever.
type deadlineWriter struct {
	net.Conn
	d time.Duration
}

func (w deadlineWriter) Write(b []byte) (int, error) {
	_ = w.Conn.SetWriteDeadline(time.Now().Add(w.d))
	return w.Conn.Write(b)
}

// connect dials the holder e describes. It trusts the registry file only as far as it can check
// it: the socket must be the conventional one for the instance, in the registry directory, a real
// socket of this user; no other connected holder may listen on the same socket (a hard link to
// it); the process that accepts
// must run as this user; and its welcome must name the instance and pid of the file. A file that
// fails a check is skipped, never pruned. Without these checks a file could point at another
// holder's socket, and the two connections to that one holder would take each other's place by
// hello takeover (protocol section 9) in a loop.
func (c *Client) connect(ctx context.Context, e holderreg.Entry) error {
	if want := holderreg.SocketPath(c.cfg.Dir, e.Instance); filepath.Clean(e.Socket) != filepath.Clean(want) {
		return fmt.Errorf("registry file names socket %s, want %s; skipped", e.Socket, want)
	}
	if err := holderreg.CheckSocket(e.Socket); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.prune(e, err.Error())
			return nil
		}
		return fmt.Errorf("%w; skipped", err)
	}
	sock, err := socketID(e.Socket)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for id, o := range c.holders {
		if id != e.Instance && o.sock == sock && !o.Closed() {
			c.mu.Unlock()
			return fmt.Errorf("socket is holder %s's, already connected; skipped", id)
		}
	}
	c.mu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(dctx, "unix", e.Socket)
	if err != nil {
		if holderreg.Refused(err) {
			c.prune(e, err.Error())
			return nil
		}
		return err
	}
	if uc, ok := nc.(*net.UnixConn); !ok {
		nc.Close()
		return errors.New("not a Unix socket connection")
	} else if uid, err := holderreg.PeerUID(uc); err != nil || uid != os.Getuid() {
		nc.Close()
		if err == nil {
			err = fmt.Errorf("holder runs as uid %d, not %d", uid, os.Getuid())
		}
		return err
	}
	deadline, _ := dctx.Deadline()
	_ = nc.SetDeadline(deadline)
	pc := hp.NewConn(deadlineWriter{nc, c.cfg.WriteTimeout}, c.cfg.Protocol)
	w, err := pc.Hello(c.cfg.Build)
	if err != nil {
		nc.Close()
		var re *hp.RefusedError
		if errors.As(err, &re) {
			c.mu.Lock()
			c.unreachable[e.Instance] = e
			c.mu.Unlock()
			return fmt.Errorf("unreachable: %w", err)
		}
		return err
	}
	if w.Instance != e.Instance || int(w.Pid) != e.Pid {
		nc.Close()
		return fmt.Errorf("welcome names holder %s pid %d, registry file %s pid %d; skipped", w.Instance, w.Pid, e.Instance, e.Pid)
	}
	_ = nc.SetDeadline(time.Time{})
	h := &Holder{
		c: c, nc: nc, pc: pc, welcome: w, entry: e, sock: sock,
		draining: w.Draining || e.Draining,
		done:     make(chan struct{}),
		byeC:     make(chan struct{}),
		waits:    map[uint32]chan hp.Frame{},
		inputs:   map[uint32]*InputCall{},
		panes:    map[hp.Handle]*pane{},
	}
	for _, p := range w.Panes {
		h.panes[p.Handle] = &pane{info: p, input: p.InputCount}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		nc.Close()
		return ErrClosed
	}
	if old := c.holders[e.Instance]; old != nil {
		old.close(errors.New("replaced by a restarted holder"))
	}
	c.holders[e.Instance] = h
	c.mu.Unlock()
	c.cfg.Logf("holderclient: connected to holder %s (build %s) at version %d", e.Instance, w.Build, pc.Version())
	go h.readLoop()
	return nil
}

// fileID names a file by device and inode, so two paths to one socket compare equal.
type fileID struct{ dev, ino uint64 }

func socketID(path string) (fileID, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return fileID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, fmt.Errorf("cannot read the inode of %s", path)
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// Holders returns the connected holders, oldest start first.
func (c *Client) Holders() []*Holder {
	c.mu.Lock()
	hs := make([]*Holder, 0, len(c.holders))
	for _, h := range c.holders {
		hs = append(hs, h)
	}
	c.mu.Unlock()
	slices.SortStableFunc(hs, func(a, b *Holder) int { return a.Entry().Started.Compare(b.Entry().Started) })
	return hs
}

// Unreachable returns the holders that share no protocol version with this gff-hab.
func (c *Client) Unreachable() []holderreg.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var es []holderreg.Entry
	for _, e := range c.unreachable {
		es = append(es, e)
	}
	slices.SortStableFunc(es, func(a, b holderreg.Entry) int { return a.Started.Compare(b.Started) })
	return es
}

// Current returns the holder that takes new panes: connected, not draining, of this build, and
// the newest such holder by start time. It is nil when there is none.
func (c *Client) Current() *Holder {
	var cur *Holder
	for _, h := range c.Holders() {
		if h.Build() == c.cfg.Build && !h.Draining() && !h.Closed() {
			cur = h // Holders is sorted oldest first
		}
	}
	return cur
}

// Upgrade makes sure a holder of this build takes new panes, and drains the rest (spec 4.3).
// It scans the registry (with Start set, it makes the directory first when it is missing); when
// no current holder exists it calls Start and waits, until ctx
// ends, for the new holder's registry file and a connection to it. Then it sends drain to every
// connected holder other than the current one that is not draining yet. Their panes stay where they are
// and stay served. It returns the current holder.
func (c *Client) Upgrade(ctx context.Context) (*Holder, error) {
	if c.cfg.Start != nil {
		// The first holder makes the directory; until then a scan would fail on it. The holder
		// checks the directory itself before it listens (spec 2.10).
		if err := os.MkdirAll(c.cfg.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := c.Scan(ctx); err != nil {
		return nil, err
	}
	cur := c.Current()
	if cur == nil {
		if c.cfg.Start == nil {
			return nil, ErrNoHolder
		}
		if err := c.cfg.Start(ctx); err != nil {
			return nil, fmt.Errorf("holderclient: start holder: %w", err)
		}
		t := time.NewTicker(c.cfg.Poll)
		defer t.Stop()
		for cur == nil {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("holderclient: waiting for the started holder: %w", ctx.Err())
			case <-t.C:
			}
			if err := c.Scan(ctx); err != nil {
				return nil, err
			}
			cur = c.Current()
		}
	}
	var errs []error
	for _, h := range c.Holders() {
		// Every other holder drains: one of another build, and an older one of this build too
		// (a slow start that was retried leaves two).
		if h == cur || h.Draining() || h.Closed() {
			continue
		}
		if err := h.Drain(ctx); err != nil {
			errs = append(errs, fmt.Errorf("drain %s: %w", h.Instance(), err))
		}
	}
	return cur, errors.Join(errs...)
}

// Spawn starts a pane on the current holder. The Spawn's token is set by the client.
func (c *Client) Spawn(ctx context.Context, s hp.Spawn) (*Pane, error) {
	h := c.Current()
	if h == nil {
		return nil, ErrNoHolder
	}
	return h.Spawn(ctx, s)
}

// PaneStatus says where a pane lives.
type PaneStatus struct {
	UUID     hp.UUID
	Handle   hp.Handle
	Pid      uint32
	Exited   bool
	Instance string
	Build    string
	Version  hp.Version
	Draining bool
}

// Panes reports every pane on every connected holder and which holder it lives on.
func (c *Client) Panes() []PaneStatus {
	var out []PaneStatus
	for _, h := range c.Holders() {
		h.mu.Lock()
		for _, p := range h.panes {
			out = append(out, PaneStatus{
				UUID: p.info.UUID, Handle: p.info.Handle, Pid: p.info.Pid, Exited: p.info.Exited,
				Instance: h.entry.Instance, Build: h.welcome.Build, Version: h.pc.Version(),
				Draining: h.draining,
			})
		}
		h.mu.Unlock()
	}
	slices.SortStableFunc(out, func(a, b PaneStatus) int {
		if a.Instance != b.Instance {
			if a.Instance < b.Instance {
				return -1
			}
			return 1
		}
		return int(a.Handle) - int(b.Handle)
	})
	return out
}

// Pane finds a pane by uuid on any connected holder.
func (c *Client) Pane(u hp.UUID) *Pane {
	for _, h := range c.Holders() {
		h.mu.Lock()
		for hd, p := range h.panes {
			if p.info.UUID == u {
				h.mu.Unlock()
				return &Pane{UUID: u, Handle: hd, Holder: h}
			}
		}
		h.mu.Unlock()
	}
	return nil
}

// Close sends handover to every holder and closes every connection (protocol section 9). The
// holders keep their panes for the next gff-hab.
func (c *Client) Close() {
	c.mu.Lock()
	if !c.closed {
		close(c.quit)
	}
	c.closed = true
	hs := make([]*Holder, 0, len(c.holders))
	for _, h := range c.holders {
		hs = append(hs, h)
	}
	c.mu.Unlock()
	for _, h := range hs {
		_ = h.pc.WriteFrame(&hp.Handover{Token: h.nextToken()})
		h.close(ErrClosed)
	}
}

func (c *Client) forget(h *Holder) {
	c.mu.Lock()
	if c.holders[h.entry.Instance] == h {
		delete(c.holders, h.entry.Instance)
	}
	c.mu.Unlock()
}

// emit delivers an event, or gives up when the holder connection is closed.
func (c *Client) emit(h *Holder, ev Event) {
	select {
	case c.events <- ev:
	case <-h.done:
	}
}
