// Package control serves gff-hab's control connection: JSON-RPC 2.0, one message per line, on a
// local Unix socket (docs/specs/hab-logic-api.md, sections 3 and 4.1). It runs the hello
// handshake, numbers events per connection with a gapless seq, puts the last queued seq on every
// response, and closes a client whose event backlog grows past a limit. It also holds the client
// that the CLI and tests use.
//
// session.create and pane.close run through Config.Actions, data.ticket and data connections
// through Config.Data, pane.input through Config.Input, chat.page through Config.Chat, the size
// ownership and redraw methods through Config.Size, command.list and command.output through
// Config.Commands, session.send and approval.answer through Config.Agents, and
// hab.permissions through Config.Permissions, detect.explain through Config.Detect, the push
// methods, session.focus and notice.raise through Config.Push, and the worktree.* methods
// through Config.Worktrees. The devices methods and the calls later stages build answer
// "unavailable", with a hint that names the stage.
package control

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
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

// SessionRecords renames and removes session records (session.rename, session.close). Actions
// may implement it; without it, those methods answer unavailable. An error that is an *Error
// keeps its kind.
type SessionRecords interface {
	RenameSession(session, name string) error
	CloseSession(session string) error
}

// Data serves data connections and issues their tickets (spec 5). gff-hab implements it with
// internal/datastream. An error that is an *Error keeps its kind.
type Data interface {
	// Ticket issues a single-use ticket for client to open one data connection to pane.
	Ticket(client, pane string, observe bool) (ticket string, ttl time.Duration, err error)
	// ServeConn runs a data connection. br reads nc and holds the hello's first byte.
	ServeConn(nc net.Conn, br *bufio.Reader)
}

// Input writes a client's bytes to a pane's program (spec 3.8). An error that is an *Error keeps
// its kind.
type Input interface {
	// QueueInput queues data for pane's program behind every write queued to that pane before
	// it, and returns without waiting for the write; wait returns its result. When ctx ends
	// while the write still waits in the queue, it is never written.
	QueueInput(ctx context.Context, client, pane string, data []byte) (wait func() error)
}

// Size keeps each pane's size owner (spec 6). gff-hab implements it with internal/panesize. An
// error that is an *Error keeps its kind.
type Size interface {
	// Connected and Disconnected count a client's open control connections; a client whose
	// last one closes leaves ownership (disconnect), and gets it back when it reconnects within
	// the grace period (restore).
	Connected(client string)
	Disconnected(client string)
	// Typed records a pane.input call from client (spec 6.2).
	Typed(client, pane string)
	// Created makes client, which created pane, its owner.
	Created(client, pane string)
	Take(client, pane string) (owner string, err error)
	Request(client, pane string) (owner string, err error)
	Disown(client, pane string) (owner string, err error)
	SetDesired(client, pane string, cols, rows int) (applied bool, err error)
	Redraw(client, pane string, mode api.RedrawMode) error
}

// EventChatMessage is the event type of a chat message that was added or changed.
const EventChatMessage api.EventType = "chat.message"

// Chat serves chat.page from the transcript reader (harness-adapters.md, Claude Code:
// conversation). An error that is an *Error keeps its kind.
type Chat interface {
	// ChatPage reads one page of a session's chat messages. Before and After are never both set.
	ChatPage(ctx context.Context, p api.ChatPageParams) (api.ChatMessages, error)
}

// EventCommandUpdated is the event type of a command that started or ended (spec 2.5). Its id
// is the pane id.
const EventCommandUpdated api.EventType = "command.updated"

// Commands serves a pane's shell commands found by OSC 133 marks (spec 2.5). An error that is
// an *Error keeps its kind.
type Commands interface {
	// CommandList reads a page of a pane's commands.
	CommandList(p api.CommandListParams) (api.CommandList, error)
	// CommandOutput reads a run of one command's output from the holder's ring.
	CommandOutput(ctx context.Context, p api.CommandOutputParams) (api.CommandOutput, error)
}

// Agents drives a pane's agent through its harness adapter (harness-adapters.md, Input delivery
// and Permission answers). gff-hab implements it over the agent state store and the holder link.
// An error that is an *Error keeps its kind.
type Agents interface {
	// Send delivers one message to the session's agent, only when the agent may take it.
	Send(ctx context.Context, client string, p api.SessionSendParams) error
	// Answer answers the pane's open permission request, only when the request id matches.
	Answer(ctx context.Context, client string, p api.ApprovalAnswerParams) error
}

// Trust serves trust.answer (app.md 7.10): it answers a pane's folder trust prompt from the
// dialog on screen. An error that is an *Error keeps its kind.
type Trust interface {
	Answer(ctx context.Context, client string, p api.TrustAnswerParams) (api.TrustAnswered, error)
}

// Permissions serves hab.permissions (macos-identity.md 7.2). A check may take tens of seconds,
// so it runs off the connection's read loop. An error that is an *Error keeps its kind.
type Permissions interface {
	// Permissions runs the check. It honours ctx and bounds its own time.
	Permissions(ctx context.Context, p api.PermissionsParams) (api.Permissions, error)
}

// Detect serves detect.explain (harness-adapters.md, Detection). It asks the holder and lists
// processes, so it runs off the connection's read loop. An error that is an *Error keeps its
// kind.
type Detect interface {
	// Explain explains one pane. p names a pane or a session, never both.
	Explain(ctx context.Context, p api.DetectExplainParams) (api.DetectExplain, error)
}

// Worktrees serves the worktree.* methods (spec 3.10). git and a teardown script take time, so
// they run off the connection's read loop. An error that is an *Error keeps its kind.
type Worktrees interface {
	ListWorktrees(ctx context.Context, p api.WorktreeListParams) (api.WorktreeList, error)
	CreateWorktree(ctx context.Context, client string, p api.WorktreeCreateParams) (api.WorktreeCreated, error)
	SetupWorktree(ctx context.Context, client string, p api.WorktreeSetupParams) (api.WorktreeSetup, error)
	RemoveWorktree(ctx context.Context, p api.WorktreeRemoveParams) (api.WorktreeRemoved, error)
}

// Peers keeps the hab's peer list and its links (mesh.md 3, 4). gff-hab implements it with
// internal/peers. An error that is an *Error keeps its kind.
type Peers interface {
	// List returns the peers with their link state.
	List() api.PeerList
	// Accept pins the device id the peer presented in place of the pinned one.
	Accept(label string) (api.PeerAccepted, error)
	// Linked reports a peer link that said hello with its peer field: another hab dialed this
	// one. id is unique to the connection. Unlinked reports its end.
	Linked(id uint64, p api.PeerHello)
	Unlinked(id uint64)
}

// Usage keeps the latest usage reading of each harness account (app.md 14). gff-hab implements
// it with internal/usage.
type Usage interface {
	List() api.UsageReadings
}

// Config is what New needs. Source is required.
// Stats serves hab.stats. Readers share a cached sample across clients.
type Stats interface {
	Stats(context.Context) (api.HostStats, error)
}

type Config struct {
	Build        string
	Host         api.HostInfo
	DeviceID     string   // the hab's host-key fingerprint; sent in every welcome
	Capabilities []string // strings for the features this hab offers (spec 3.4)
	Source       Source
	Actions      Actions  // nil: session.create and pane.close answer unavailable
	Data         Data     // nil: data.ticket answers unavailable and a data hello is rejected
	Input        Input    // nil: pane.input answers unavailable
	Size         Size     // nil: the size ownership and redraw methods answer unavailable
	Chat         Chat     // nil: chat.page answers unavailable
	Commands     Commands // nil: command.list and command.output answer unavailable
	Agents       Agents   // nil: session.send and approval.answer answer unavailable
	// CapcomThread serves the Capcom service's calls (capcom-service.md 7.4). Nil: they
	// answer unavailable.
	CapcomThread CapcomThread
	Permissions  Permissions // nil: hab.permissions answers unavailable
	Trust        Trust       // nil: trust.answer answers unavailable
	Detect       Detect      // nil: detect.explain answers unavailable
	Peers        Peers       // nil: peers.list and peers.accept answer unavailable
	Catalog      Catalog     // nil: the catalog methods answer unavailable
	Push         Push        // nil: the push methods, session.focus and notice.raise answer unavailable
	Stats        Stats       // nil: hab.stats answers unavailable
	Usage        Usage       // nil: usage.list answers unavailable
	Content      Content     // nil: the content.* methods answer unavailable
	Channels     Channels    // nil: the channel and mail methods answer unavailable
	Worktrees    Worktrees   // nil: the worktree.* methods answer unavailable
	Speech       Speech      // nil: speech.prepare and speech.transcribe answer unavailable
	Attachments  Attachments // nil: the attachment.* methods answer unavailable
	Settings     Settings    // nil: settings.get and settings.set answer unavailable
	Reads        Reads       // nil: the read tools (transcript.*, notice.get, file.read) answer unavailable
	Logbook      Logbook     // nil: the logbook.* methods answer unavailable
	LogbookSync  LogbookSync // nil: logbook.pull and logbook.push answer unavailable
	Confirm      Confirm     // nil: the confirm.* methods answer unavailable
	Capcom       Capcom      // nil: capcom.open and capcom.assign answer unavailable
	Rounds       Rounds      // nil: the round.* methods answer unavailable
	Heavy        Heavy       // nil: the heavy.* methods answer unavailable
	Groups       Groups      // nil: the group.* methods answer unavailable
	// ApproversChanged, if set, is called with no lock held after a connection closes or
	// changes its subscription, so Approvers may have dropped.
	ApproversChanged func()
	// ActionTimeout bounds one Actions call. Default 30 s.
	ActionTimeout time.Duration

	BacklogLimit   int           // bytes queued for one connection before it is closed
	Heartbeat      time.Duration // heartbeat after this long with nothing sent
	IdleTimeout    time.Duration // close a connection that sends nothing for this long
	ReconnectGrace time.Duration // how long a reconnect token outlives its connection
	CloseGrace     time.Duration // how long a closing connection may take to flush
	Now            func() time.Time
	Logf           func(format string, args ...any)
	// Observe hears every event Publish sends, before any client: the event triggers of
	// rounds (rounds.md 4.3). It must not block. Nil: none.
	Observe func(structure.Event)
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

	idemKeys idemTable // session.create and session.send keys (spec 3.9)
}

type token struct {
	clientID string
	live     *conn     // the connection using it; nil once it closed
	expires  time.Time // when live is nil: the end of the grace period
}

// linkIDs numbers peer links for Peers.Linked.
var linkIDs atomic.Uint64

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
	if !slices.Contains(cfg.Capabilities, IdempotencyCapability) {
		cfg.Capabilities = append(slices.Clip(cfg.Capabilities), IdempotencyCapability) // idem.go always runs
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

// ServeConn runs one connection until it closes. Tests use it with net.Pipe. The first byte
// tells a data connection from a control connection (spec 4.1): a data hello frame starts with
// its kind byte, a control connection with the JSON of hab.hello.
func (s *Server) ServeConn(nc net.Conn) {
	br := bufio.NewReaderSize(nc, 64<<10)
	if s.cfg.Data != nil {
		nc.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		first, err := br.Peek(1)
		nc.SetReadDeadline(time.Time{})
		if err != nil {
			nc.Close()
			return
		}
		if first[0] == byte(api.FrameHello) {
			s.cfg.Data.ServeConn(nc, br)
			return
		}
	}
	c := newConn(s, nc, br)
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
	c.cancel()
	c.finish(nil)
	<-c.done
	if c.hello && s.cfg.Size != nil {
		s.cfg.Size.Disconnected(c.clientID)
	}
	if c.linked {
		s.cfg.Peers.Unlinked(c.linkID)
	}

	s.mu.Lock()
	delete(s.conns, c)
	if t := s.tokens[c.token]; t != nil && t.live == c {
		t.live = nil
		t.expires = s.cfg.Now().Add(s.cfg.ReconnectGrace)
	}
	s.mu.Unlock()
	if f := s.cfg.ApproversChanged; f != nil {
		f()
	}
}

// Approvers returns the number of open connections that can answer a permission request on
// pane: an app or terminal client (a cli or script client cannot) that subscribed to events
// whose scope holds the pane, so it sees the request. Only these make a blocking permission
// hook wait (harness-adapters.md, Claude: blocking PermissionRequest hook).
func (s *Server) Approvers(pane string) int {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	n := 0
	for _, c := range conns {
		c.mu.Lock()
		if c.revision > 0 && !c.closing && c.sub != nil && (c.kind == api.ClientKindApp || c.kind == api.ClientKindTerminal) && c.sub.hasPane(pane) {
			n++
		}
		c.mu.Unlock()
	}
	return n
}

// Publish hands one structure change to every subscribed connection. It never blocks on a
// client, so it is safe to call from structure.Config.OnEvent, under the store lock.
func (s *Server) Publish(e structure.Event) {
	if s.cfg.Observe != nil {
		s.cfg.Observe(e)
	}
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

// NotifyLink sends e to every peer link that the hab with this device id dialed to this one,
// whether or not the link subscribed: the mesh.changed wake-up (mesh.md 4.2, 6.5). It never
// blocks on a client, and returns how many links got it.
func (s *Server) NotifyLink(device string, e structure.Event) int {
	data, err := json.Marshal(e.Data)
	if err != nil {
		s.cfg.Logf("event %s %s: %v", e.Type, e.ID, err)
		return 0
	}
	pe := pubEvent{ev: e, data: data}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for c := range s.conns {
		if c.notifyLink(device, pe) {
			n++
		}
	}
	return n
}

// Watching reports whether a subscription on some connection gets the events of session id:
// its scope is the host, or holds the session.
func (s *Server) Watching(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if c.watching(session) {
			return true
		}
	}
	return false
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

// claimToken finds the client id for a reconnect token, issues a new token for c and counts c
// as connected (Size.Connected). A valid
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
	// Count c before the old connection closes: the old one's Disconnected may run as soon as
	// it is finished, and if c were not counted yet the client would leave ownership (spec 6.5).
	if s.cfg.Size != nil {
		s.cfg.Size.Connected(clientID)
	}
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
