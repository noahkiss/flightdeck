// Package control serves gff-hab's control connection: JSON-RPC 2.0, one message per line, on a
// local Unix socket (docs/specs/hab-logic-api.md, sections 3 and 4.1). It runs the hello
// handshake, numbers events per connection with a gapless seq, puts the last queued seq on every
// response, and closes a client whose event backlog grows past a limit. It also holds the client
// that the CLI and tests use.
//
// session.create and pane.close run through Config.Actions. The other pane methods (input,
// tickets, size ownership, redraw), the devices methods and the calls later stages build answer
// "unavailable", with a hint that names the stage.
package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Defaults from spec 3.5 and 3.7.
const (
	DefaultBacklogLimit   = 8 << 20
	DefaultHeartbeat      = 15 * time.Second
	DefaultIdleTimeout    = 45 * time.Second
	DefaultReconnectGrace = 60 * time.Second
	DefaultCloseGrace     = 5 * time.Second
	// MaxMessage bounds one incoming line.
	MaxMessage = 16 << 20
)

// Source is the state the control connection serves. *structure.Store satisfies it.
type Source interface {
	Snapshot(host api.HostInfo) api.Structure
}

// Actions carries out the calls that start and stop programs (spec 3.9). gff-hab implements it
// over the holder link and the structure store. An error that is an *Error keeps its kind; any
// other error answers with kind unavailable.
type Actions interface {
	// CreateSession starts a program in a new pane and files it as a session.
	CreateSession(ctx context.Context, p api.SessionCreateParams) (session, pane string, err error)
	// ClosePane releases a pane; a running program gets a hang-up first.
	ClosePane(ctx context.Context, pane string) error
}

// Config is what New needs. Source is required.
type Config struct {
	Build        string
	Host         api.HostInfo
	DeviceID     string   // the hab's host-key fingerprint; sent in every welcome
	Capabilities []string // strings for the features this hab offers (spec 3.4)
	Source       Source
	Actions      Actions // nil: session.create and pane.close answer unavailable
	// ActionTimeout bounds one Actions call. Default 30 s.
	ActionTimeout time.Duration

	BacklogLimit   int           // bytes queued for one connection before it is closed
	Heartbeat      time.Duration // heartbeat after this long with nothing sent
	IdleTimeout    time.Duration // close a connection that sends nothing for this long
	ReconnectGrace time.Duration // how long a reconnect token outlives its connection
	CloseGrace     time.Duration // how long a closing connection may take to flush
	Now            func() time.Time
	Logf           func(format string, args ...any)
}

// Server serves control connections. Its epoch is new for every Server.
type Server struct {
	cfg   Config
	epoch string

	mu        sync.Mutex
	conns     map[*conn]struct{}
	tokens    map[string]*token
	listeners []net.Listener
	closed    bool
	wg        sync.WaitGroup
}

type token struct {
	clientID string
	live     *conn     // the connection using it; nil once it closed
	expires  time.Time // when live is nil: the end of the grace period
}

// New returns a server with a fresh random epoch.
func New(cfg Config) *Server {
	if cfg.BacklogLimit <= 0 {
		cfg.BacklogLimit = DefaultBacklogLimit
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.ReconnectGrace <= 0 {
		cfg.ReconnectGrace = DefaultReconnectGrace
	}
	if cfg.CloseGrace <= 0 {
		cfg.CloseGrace = DefaultCloseGrace
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{
		cfg:    cfg,
		epoch:  randomID(8),
		conns:  map[*conn]struct{}{},
		tokens: map[string]*token{},
	}
}

// Epoch returns this server's epoch.
func (s *Server) Epoch() string { return s.epoch }

// Serve accepts connections on l until Close. A Unix connection from another user is refused.
// It returns nil after Close.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		l.Close()
		return nil
	}
	s.listeners = append(s.listeners, l)
	s.mu.Unlock()
	var backoff time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			s.cfg.Logf("accept: %v", err)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		if uc, ok := nc.(*net.UnixConn); ok {
			if err := checkPeer(uc); err != nil {
				s.cfg.Logf("refused a connection: %v", err)
				nc.Close()
				continue
			}
		}
		go s.ServeConn(nc)
	}
}

// ServeConn runs one connection until it closes. Tests use it with net.Pipe.
func (s *Server) ServeConn(nc net.Conn) {
	c := newConn(s, nc)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		nc.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	go c.writeLoop()
	c.readLoop()
	c.finish(nil)
	<-c.done

	s.mu.Lock()
	delete(s.conns, c)
	if t := s.tokens[c.token]; t != nil && t.live == c {
		t.live = nil
		t.expires = s.cfg.Now().Add(s.cfg.ReconnectGrace)
	}
	s.mu.Unlock()
}

// Publish hands one structure change to every subscribed connection. It never blocks on a
// client, so it is safe to call from structure.Config.OnEvent, under the store lock.
func (s *Server) Publish(e structure.Event) {
	data, err := json.Marshal(e.Data)
	if err != nil {
		s.cfg.Logf("event %s %s: %v", e.Type, e.ID, err)
		return
	}
	pe := pubEvent{ev: e, data: data}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.publish(pe)
	}
}

// Close sends closing {reason: shutdown} to every connection, closes the listeners and waits a
// bounded time for the connections to finish.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, l := range s.listeners {
		l.Close()
	}
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.finish(closingMessage(api.ClosingReasonShutdown))
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * s.cfg.CloseGrace):
		for _, c := range conns {
			c.nc.Close()
		}
	}
}

// claimToken finds the client id for a reconnect token and issues a new token for c. A valid
// token is one from this epoch whose connection closed less than the grace period ago, or whose
// connection is still open (then that connection is presumed dead and is closed).
func (s *Server) claimToken(c *conn, presented string) (clientID, tok string) {
	s.mu.Lock()
	now := s.cfg.Now()
	for k, t := range s.tokens {
		if t.live == nil && now.After(t.expires) {
			delete(s.tokens, k)
		}
	}
	var old *conn
	if t, ok := s.tokens[presented]; ok && presented != "" {
		clientID = t.clientID
		if t.live != nil && t.live != c {
			old = t.live
		}
		delete(s.tokens, presented)
	}
	if clientID == "" {
		clientID = randomID(8)
	}
	tok = randomID(16)
	s.tokens[tok] = &token{clientID: clientID, live: c}
	s.mu.Unlock()
	if old != nil {
		old.finish(nil)
	}
	return clientID, tok
}

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
