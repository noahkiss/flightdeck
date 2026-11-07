package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type pubEvent struct {
	ev   structure.Event
	data json.RawMessage
}

// conn is one control connection. The reader goroutine handles requests one at a time, in
// order. Every outgoing message goes through one queue, drained by the writer goroutine, so
// seq order on the wire is the order in which messages were queued.
type conn struct {
	s    *Server
	nc   net.Conn
	br   *bufio.Reader
	wake chan struct{}
	done chan struct{} // closed when the writer has exited and the socket is closed

	// Set by the reader goroutine only.
	hello    bool
	token    string
	clientID string
	linked   bool           // a peer link reported to Peers.Linked
	linkID   uint64         // its id
	peer     *api.PeerHello // the hello's peer field from a client of kind hab, else nil

	mu       sync.Mutex
	queue    [][]byte
	queued   int  // bytes in queue
	inflight int  // bytes the writer took from the queue and has not finished writing
	closing  bool // nothing more is queued; the writer drains, then closes
	seq      uint64
	revision int
	kind     api.ClientKind // from hello
	label    string         // from hello: the label, else the client name
	linkFrom string         // from hello: the device id of a peer link's dialer, else ""
	sub      *filter        // nil until events.subscribe
	seeding  bool           // a subscribe is taking its snapshot; events wait in pending
	pending  []pubEvent     // events that arrived while seeding, in order
	// lanes run pane.input calls off the read loop, one goroutine per pane while it has calls,
	// so calls to one pane keep their order and a stuck pane holds up nothing else.
	lanes       map[string]*lane
	waiting     int // pane.input calls in lanes, until their reply
	chats       int // chat.page calls running, until their reply
	outputs     int // command.output calls running, until their reply
	contents    int // content.* calls running, until their reply
	attachments int // attachment.* calls running, until their reply
	speeches    int // speech calls running or waiting for the decoder, until their reply

	// ctx ends when the connection's read loop ends: calls running off the loop stop with it.
	ctx    context.Context
	cancel context.CancelFunc
}

// lane holds one pane's pane.input calls that are read and not yet queued to Input.
type lane struct {
	calls []inputCall
	wake  chan struct{} // a call was added
}

// inputCall is one pane.input call waiting in its pane's lane.
type inputCall struct {
	m      message
	seq    uint64
	data   []byte
	ctx    context.Context // its deadline runs from when the call was read
	cancel context.CancelFunc
}

// welcomeTime is the layout of the welcome's time: RFC 3339 with milliseconds, so a client can
// measure its clock's offset finer than a second.
const welcomeTime = "2006-01-02T15:04:05.000Z07:00"

// maxWaitingInput bounds the pane.input calls one connection may have waiting.
const maxWaitingInput = 256

// maxRunningChat bounds the chat.page calls one connection may have running.
const maxRunningChat = 16

// maxRunningOutput bounds the command.output calls one connection may have running. Each holds
// up to 1 MiB of output until its reply is queued.
const maxRunningOutput = 4

func newConn(s *Server, nc net.Conn, br *bufio.Reader) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{
		s:      s,
		nc:     nc,
		br:     br,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}
}

// --- Outgoing ---

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// enqueueLocked queues one message. When the bytes queued plus the bytes the writer is still
// writing would pass the backlog limit, it drops the queue, queues a final closing
// {reason: event_backlog} and stops the connection (spec 3.5). Callers hold c.mu.
func (c *conn) enqueueLocked(b []byte) {
	if c.closing {
		return
	}
	if c.inflight+c.queued+len(b) > c.s.cfg.BacklogLimit {
		c.queue = [][]byte{closingMessage(api.ClosingReasonEventBacklog)}
		c.queued = len(c.queue[0])
		c.closeLocked()
		return
	}
	c.queue = append(c.queue, b)
	c.queued += len(b)
	c.signal()
}

// closeLocked stops the connection after the queue drains. The writer gets CloseGrace to flush.
func (c *conn) closeLocked() {
	c.closing = true
	c.nc.SetWriteDeadline(time.Now().Add(c.s.cfg.CloseGrace))
	c.signal()
}

// finish queues an optional last message and closes the connection after it.
func (c *conn) finish(last []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return
	}
	if last != nil {
		c.queue = append(c.queue, last)
		c.queued += len(last)
	}
	c.closeLocked()
}

func (c *conn) writeLoop() {
	defer close(c.done)
	defer c.nc.Close()
	hb := time.NewTimer(c.s.cfg.Heartbeat)
	defer hb.Stop()
	for {
		c.mu.Lock()
		batch, closing := c.queue, c.closing
		c.queue = nil
		c.inflight, c.queued = c.queued, 0
		c.mu.Unlock()
		if len(batch) > 0 {
			bufs := net.Buffers(batch) // writev where the socket allows; no copy
			_, err := bufs.WriteTo(c.nc)
			c.mu.Lock()
			c.inflight = 0
			if err != nil {
				c.closing = true
				c.queue = nil
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
			hb.Reset(c.s.cfg.Heartbeat)
			continue
		}
		if closing {
			return
		}
		select {
		case <-c.wake:
		case <-hb.C:
			c.mu.Lock()
			if c.revision != 0 && !c.closing {
				hbm := notification("heartbeat", api.Heartbeat{Seq: c.seq})
				c.queue = append(c.queue, hbm)
				c.queued += len(hbm)
			}
			c.mu.Unlock()
			hb.Reset(c.s.cfg.Heartbeat)
		}
	}
}

func notification(method string, params any) []byte {
	b, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", method, api.NonNil(params)})
	if err != nil {
		panic(err) // our own types always marshal
	}
	return append(b, '\n')
}

func closingMessage(reason api.ClosingReason) []byte {
	return notification("closing", api.Closing{Reason: reason})
}

func response(id json.RawMessage, result json.RawMessage, e *Error) []byte {
	m := message{JSONRPC: "2.0", ID: id, Result: result}
	if len(m.ID) == 0 {
		m.ID = json.RawMessage("null")
	}
	if e != nil {
		m.Result = nil
		m.Error = &wireError{Code: e.Code, Message: e.Message, Data: e.Data}
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// --- Events ---

func (c *conn) publish(pe pubEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.sub == nil {
		return
	}
	if c.seeding {
		c.pending = append(c.pending, pe)
		return
	}
	c.deliverLocked(pe)
}

// watching reports whether this connection's subscription gets the events of session id.
func (c *conn) watching(session string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sub != nil && !c.closing && (c.sub.kind == api.ScopeKindHost || c.sub.sessions[session])
}

// deliverLocked gives an in-scope event the next seq and queues it.
func (c *conn) deliverLocked(pe pubEvent) {
	if !c.sub.pass(pe.ev) {
		return
	}
	c.queueEventLocked(pe)
}

// notifyLink queues pe for a peer link that device dialed, whether or not it subscribed
// (mesh.md 4.2). It reports false for any other connection, and while a subscribe takes its
// snapshot: the dialer's next mail round covers a notice dropped then.
func (c *conn) notifyLink(device string, pe pubEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || device == "" || c.kind != api.ClientKindHab || c.linkFrom != device || c.seeding {
		return false
	}
	return c.queueEventLocked(pe)
}

// queueEventLocked gives an event the next seq and queues it. It reports false when it
// dropped the event: no translation to the connection's revision keeps it.
func (c *conn) queueEventLocked(pe pubEvent) bool {
	e := api.Event{Seq: c.seq + 1, Type: pe.ev.Type, ID: pe.ev.ID, Data: pe.data}
	if !structure.Ephemeral(pe.ev.Type) { // an ephemeral event carries no revision (spec 3.9)
		rev := pe.ev.Revision
		e.Revision = &rev
	}
	e, ok, err := downEvent(c.revision, e)
	if err != nil {
		c.s.cfg.Logf("translate event %s: %v", e.Type, err)
		return false
	}
	if !ok {
		return false
	}
	c.seq++
	c.enqueueLocked(notification("event", e))
	return true
}

// downEvent translates an event from api.Revision down to rev (spec 3.3).
func downEvent(rev int, e api.Event) (api.Event, bool, error) {
	for r := api.Revision; r > rev; r-- {
		t, ok := api.Translators[r]
		if !ok {
			return e, false, errors.New("no translator")
		}
		var keep bool
		var err error
		if e, keep, err = t.DownEvent(e); err != nil || !keep {
			return e, false, err
		}
	}
	return e, true, nil
}

func downResult(rev int, method string, result json.RawMessage) (json.RawMessage, error) {
	for r := api.Revision; r > rev; r-- {
		t, ok := api.Translators[r]
		if !ok {
			return nil, errors.New("no translator")
		}
		var err error
		if result, err = t.DownResult(method, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func upParams(rev int, method string, params json.RawMessage) (json.RawMessage, error) {
	for r := rev + 1; r <= api.Revision; r++ {
		t, ok := api.Translators[r]
		if !ok {
			return nil, errors.New("no translator")
		}
		var err error
		if params, err = t.UpParams(method, params); err != nil {
			return nil, err
		}
	}
	return params, nil
}

// --- Incoming ---

var errTooLong = errors.New("message longer than the limit")

// readLine reads one line, without its newline, up to MaxMessage bytes.
func (c *conn) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := c.br.ReadSlice('\n')
		if len(line)+len(chunk) > MaxMessage {
			return nil, errTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

func (c *conn) readLoop() {
	c.nc.SetReadDeadline(time.Now().Add(c.s.cfg.IdleTimeout))
	if first, err := c.br.Peek(1); err == nil && first[0] == byte(api.FrameHello) {
		c.rejectData()
		return
	}
	for {
		c.nc.SetReadDeadline(time.Now().Add(c.s.cfg.IdleTimeout))
		line, err := c.readLine()
		if errors.Is(err, errTooLong) {
			c.finish(response(nil, nil, protoErr(codeInvalidRequest, "a message is longer than %d bytes", MaxMessage)))
			return
		}
		if err != nil {
			return // EOF, idle timeout (reason disconnect) or a closed socket
		}
		c.handleLine(line)
	}
}

// rejectData answers a data hello: data connections are not built yet (spec 5.2).
func (c *conn) rejectData() {
	payload, _ := json.Marshal(api.DataReject{Kind: api.ErrorKindUnavailable, Hint: "data connections are not available in this build"})
	frame := make([]byte, 5, 5+len(payload))
	frame[0] = byte(api.FrameReject)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	c.finish(append(frame, payload...))
}

func (c *conn) currentSeq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

func (c *conn) send(b []byte) {
	c.mu.Lock()
	c.enqueueLocked(b)
	c.mu.Unlock()
}

func (c *conn) handleLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var m message
	if line[0] != '{' {
		c.send(response(nil, nil, c.withSeq(protoErr(codeInvalidRequest, "each line must be one JSON-RPC object; batches are not supported"))))
		return
	}
	if err := json.Unmarshal(line, &m); err != nil {
		c.send(response(nil, nil, c.withSeq(protoErr(codeParse, "parse error: %v", err))))
		return
	}
	if m.JSONRPC != "2.0" || m.Method == "" {
		if len(m.ID) == 0 {
			return
		}
		c.send(response(m.ID, nil, c.withSeq(protoErr(codeInvalidRequest, "not a JSON-RPC 2.0 request"))))
		return
	}
	if len(m.ID) == 0 {
		return // a notification from the client; reading it already counted as liveness
	}
	if !c.hello {
		if m.Method != "hab.hello" {
			c.finish(response(m.ID, nil, c.withSeq(appErr(api.ErrorKindInvalid, "the first request must be hab.hello"))))
			return
		}
		c.handleHello(m)
		return
	}
	if m.Method == "events.subscribe" {
		c.handleSubscribe(m)
		return
	}
	if m.Method == "pane.input" {
		c.handleInput(m)
		return
	}
	if m.Method == "chat.page" {
		c.handleChat(m)
		return
	}
	if m.Method == "command.output" {
		c.handleCommandOutput(m)
		return
	}
	if m.Method == "session.send" || m.Method == "approval.answer" {
		c.handleAgent(m)
		return
	}
	if m.Method == "trust.answer" {
		c.handleTrust(m)
		return
	}
	if m.Method == "hab.stats" {
		c.handleStats(m)
		return
	}
	if m.Method == "hab.permissions" {
		c.handlePermissions(m)
		return
	}
	if m.Method == "detect.explain" {
		c.handleExplain(m)
		return
	}
	switch m.Method {
	case "content.show", "content.list", "content.read", "content.diff":
		c.handleContent(m)
		return
	case "speech.prepare", "speech.transcribe":
		c.handleSpeech(m)
		return
	case "attachment.put", "attachment.write", "attachment.get":
		c.handleAttachment(m)
		return
	}
	if isChannelMethod(m.Method) {
		c.handleChannels(m)
		return
	}
	if m.Method == "logbook.pull" || m.Method == "logbook.push" {
		c.handleLogbookSync(m)
		return
	}
	if isReadMethod(m.Method) {
		c.handleReads(m)
		return
	}
	if isLogbookMethod(m.Method) {
		c.handleLogbook(m)
		return
	}
	if isConfirmMethod(m.Method) {
		c.handleConfirm(m)
		return
	}
	if isRoundMethod(m.Method) {
		c.handleRounds(m)
		return
	}
	if strings.HasPrefix(m.Method, "heavy.") {
		c.handleHeavy(m)
		return
	}
	if strings.HasPrefix(m.Method, "worktree.") {
		c.handleWorktree(m)
		return
	}
	seq := c.currentSeq()
	result, e := c.dispatch(m, seq)
	c.reply(m, seq, result, e)
}

// handleInput checks a pane.input call and queues it on its pane's lane (spec 3.8). The read
// loop goes on at once: a pane that takes its input slowly holds up neither other calls nor
// other panes. Calls to one pane are written in the order they were read. A call that times out
// while it waits is never written.
func (c *conn) handleInput(m message) {
	seq := c.currentSeq()
	var p api.InputParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	data := p.Data
	if len(data) == 0 {
		data = []byte(p.Text)
	}
	switch {
	case c.s.cfg.Input == nil:
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method))
		return
	case p.Pane == "" || len(data) == 0:
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "pane.input needs a pane and data or text"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
	call := inputCall{m: m, seq: seq, data: data, ctx: ctx, cancel: cancel}
	c.mu.Lock()
	if c.waiting >= maxWaitingInput {
		c.mu.Unlock()
		cancel()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d pane.input calls already wait on this connection", maxWaitingInput))
		return
	}
	if c.lanes == nil {
		c.lanes = map[string]*lane{}
	}
	ln, running := c.lanes[p.Pane]
	if !running {
		ln = &lane{wake: make(chan struct{}, 1)}
		c.lanes[p.Pane] = ln
	}
	ln.calls = append(ln.calls, call)
	c.waiting++
	c.mu.Unlock()
	if c.s.cfg.Size != nil {
		c.s.cfg.Size.Typed(c.clientID, p.Pane) // input counts for size ownership (spec 3.8, 6.2)
	}
	if !running {
		go c.runLane(p.Pane, ln)
		return
	}
	select {
	case ln.wake <- struct{}{}:
	default:
	}
}

// handleChat checks a chat.page call and runs it on its own goroutine. The first page of a long
// transcript reads the whole file; the read loop goes on meanwhile, so pings and other calls are
// answered. The call stops when its timeout passes or the connection ends.
func (c *conn) handleChat(m message) {
	seq := c.currentSeq()
	var p api.ChatPageParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	switch {
	case p.Session == "":
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "chat.page needs a session"))
		return
	case p.Before != "" && p.After != "":
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "chat.page takes before or after, not both"))
		return
	case c.s.cfg.Chat == nil:
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, hintNoChat, m.Method))
		return
	}
	c.mu.Lock()
	if c.chats >= maxRunningChat {
		c.mu.Unlock()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d chat.page calls already run on this connection", maxRunningChat))
		return
	}
	c.chats++
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, c.s.cfg.ActionTimeout)
		defer cancel()
		page, err := c.s.cfg.Chat.ChatPage(ctx, p)
		c.mu.Lock()
		c.chats--
		c.mu.Unlock()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		if page.Messages == nil {
			page.Messages = []api.ChatMessage{}
		}
		page.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, page, nil)
	}()
}

// handleCommandOutput checks a command.output call and runs it on its own goroutine: it waits
// for a replay from the pane's holder, which queues behind that holder's live output, and the
// read loop goes on meanwhile.
func (c *conn) handleCommandOutput(m message) {
	seq := c.currentSeq()
	var p api.CommandOutputParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	switch {
	case c.s.cfg.Commands == nil:
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method))
		return
	case p.Pane == "":
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "command.output needs a pane"))
		return
	case p.Limit < 0:
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "command.output limit must not be negative"))
		return
	}
	c.mu.Lock()
	if c.outputs >= maxRunningOutput {
		c.mu.Unlock()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d command.output calls already run on this connection", maxRunningOutput))
		return
	}
	c.outputs++
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, c.s.cfg.ActionTimeout)
		defer cancel()
		out, err := c.s.cfg.Commands.CommandOutput(ctx, p)
		c.mu.Lock()
		c.outputs--
		c.mu.Unlock()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		if out.Data == nil {
			out.Data = []byte{}
		}
		out.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, out, nil)
	}()
}

// maxPermissionsTime bounds one hab.permissions call. A walk's request keeps a probe alive for
// up to two minutes while the user answers a dialog.
// handleStats keeps resource reads off the connection's request loop.
func (c *conn) handleStats(m message) {
	seq := c.currentSeq()
	var p api.HabStatsParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	if c.s.cfg.Stats == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "hab.stats is unavailable on this hab"))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, c.s.cfg.ActionTimeout)
		defer cancel()
		res, err := c.s.cfg.Stats.Stats(ctx)
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, res, nil)
	}()
}

const maxPermissionsTime = 3 * time.Minute

// handlePermissions runs a hab.permissions call on its own goroutine: a probe pane takes seconds,
// and the read loop goes on meanwhile. Permissions serialises the checks itself.
func (c *conn) handlePermissions(m message) {
	seq := c.currentSeq()
	var p api.PermissionsParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	if c.s.cfg.Permissions == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "hab.permissions needs gff-hab serve with its holder link"))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxPermissionsTime)
		defer cancel()
		res, err := c.s.cfg.Permissions.Permissions(ctx, p)
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		if res.Rows == nil {
			res.Rows = []api.PermissionRow{}
		}
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, res, nil)
	}()
}

// maxExplainTime bounds one detect.explain call: a holder round trip and a process listing.
// explainGrace is how long past it the reply waits for an answer before it reports the overrun.
// They are variables so a test can shorten them.
var (
	maxExplainTime = 15 * time.Second
	explainGrace   = 500 * time.Millisecond
)

// handleExplain runs a detect.explain call on its own goroutine: it asks the holder for the
// pane's foreground group and lists its processes, and the read loop goes on meanwhile.
func (c *conn) handleExplain(m message) {
	seq := c.currentSeq()
	var p api.DetectExplainParams
	if e := c.decodeUp(m, &p); e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	if (p.Pane == "") == (p.Session == "") {
		c.reply(m, seq, nil, appErr(api.ErrorKindInvalid, "detect.explain needs pane or session, not both"))
		return
	}
	if c.s.cfg.Detect == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "detect.explain needs gff-hab serve with its holder link"))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, maxExplainTime)
		defer cancel()
		type result struct {
			res api.DetectExplain
			err error
		}
		ch := make(chan result, 1)
		go func() {
			res, err := c.s.cfg.Detect.Explain(ctx, p)
			ch <- result{res, err}
		}()
		// Explain stops when ctx ends. If a step under it does not, the caller still gets an
		// answer: an error after a short grace, never silence.
		var r result
		select {
		case r = <-ch:
		case <-ctx.Done():
			select {
			case r = <-ch:
			case <-time.After(explainGrace):
				r.err = appErr(api.ErrorKindUnavailable, "detect.explain did not finish within %s", maxExplainTime)
			}
		}
		res, err := r.res, r.err
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, api.NonNil(res), nil)
	}()
}

// maxWorktreeTime bounds one worktree call: git, copying the included files, and a teardown script
// (which gff-hab bounds itself to less).
const maxWorktreeTime = 5 * time.Minute

// handleWorktree runs a worktree.* call on its own goroutine (spec 3.10): git and a teardown
// script take time, and the read loop goes on meanwhile. A setup pane belongs to the caller, as a
// pane from session.create does.
func (c *conn) handleWorktree(m message) {
	seq := c.currentSeq()
	var (
		list   api.WorktreeListParams
		create api.WorktreeCreateParams
		setup  api.WorktreeSetupParams
		remove api.WorktreeRemoveParams
		target *api.WorktreeProject
		e      *Error
	)
	switch m.Method {
	case "worktree.list":
		e, target = c.decodeUp(m, &list), &list.WorktreeProject
	case "worktree.create":
		e, target = c.decodeUp(m, &create), &create.WorktreeProject
		if e == nil && create.Setup != "" && !isWord("SetupMode", string(create.Setup)) {
			e = appErr(api.ErrorKindInvalid, "setup %q is not one of %v", create.Setup, api.Enums["SetupMode"])
		}
	case "worktree.setup":
		e, target = c.decodeUp(m, &setup), &setup.WorktreeProject
		if e == nil && setup.Path == "" {
			e = appErr(api.ErrorKindInvalid, "worktree.setup needs a path")
		}
	case "worktree.remove":
		e, target = c.decodeUp(m, &remove), &remove.WorktreeProject
		if e == nil && remove.Path == "" {
			e = appErr(api.ErrorKindInvalid, "worktree.remove needs a path")
		}
	default:
		c.reply(m, seq, nil, protoErr(codeMethodNotFound, "method %q does not exist", m.Method))
		return
	}
	if e == nil && (target.Project == "") == (target.ProjectDir == "") {
		e = appErr(api.ErrorKindInvalid, "%s needs a project or a project_dir, not both", m.Method)
	}
	if e == nil && c.s.cfg.Worktrees == nil {
		e = appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	w := c.s.cfg.Worktrees
	go func() {
		// A call runs to its end when the connection drops: git cut off halfway leaves a
		// worktree half made.
		ctx, cancel := context.WithTimeout(context.Background(), maxWorktreeTime)
		defer cancel()
		var res any
		var pane string
		var err error
		switch m.Method {
		case "worktree.list":
			var r api.WorktreeList
			if r, err = w.ListWorktrees(ctx, list); r.Worktrees == nil {
				r.Worktrees = []api.Worktree{}
			}
			r.Seq = api.Seq{Seq: seq}
			res = r
		case "worktree.create":
			var r api.WorktreeCreated
			if r, err = w.CreateWorktree(ctx, c.clientID, create); r.Copied == nil {
				r.Copied = []string{}
			}
			r.Seq, pane = api.Seq{Seq: seq}, r.Setup.Pane
			res = r
		case "worktree.setup":
			var r api.WorktreeSetup
			r, err = w.SetupWorktree(ctx, c.clientID, setup)
			res, pane = api.WorktreeSetupResult{Seq: api.Seq{Seq: seq}, Setup: r}, r.Pane
		case "worktree.remove":
			var r api.WorktreeRemoved
			r, err = w.RemoveWorktree(ctx, remove)
			r.Seq = api.Seq{Seq: seq}
			res = r
		}
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		if pane != "" && c.s.cfg.Size != nil {
			c.s.cfg.Size.Created(c.clientID, pane)
		}
		c.reply(m, seq, res, nil)
	}()
}

// runLane queues one pane's pane.input calls to Input in order, each as soon as it is read, and
// replies to them in the same order as their results come; it ends when none is left. It never
// waits for one call's result before queueing the next: each keystroke is its own call, and
// under heavy output a result comes seconds after the write reached the program.
func (c *conn) runLane(pane string, ln *lane) {
	type queued struct {
		call inputCall
		res  chan error
	}
	var pending []queued
	for {
		c.mu.Lock()
		calls := ln.calls
		ln.calls = nil
		if len(calls) == 0 && len(pending) == 0 {
			delete(c.lanes, pane)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		for _, call := range calls {
			q := queued{call: call, res: make(chan error, 1)}
			if err := call.ctx.Err(); err != nil {
				q.res <- err // timed out while it waited: never written
			} else {
				wait := c.s.cfg.Input.QueueInput(call.ctx, c.clientID, pane, call.data)
				go func() { q.res <- wait() }()
			}
			pending = append(pending, q)
		}
		if len(calls) > 0 {
			continue
		}

		// Wait for the oldest call's result, or for a new call to queue.
		var err error
		select {
		case err = <-pending[0].res:
		case <-ln.wake:
			continue
		}
		call := pending[0].call
		pending = pending[1:]
		call.cancel()
		c.mu.Lock()
		c.waiting--
		c.mu.Unlock()
		if err != nil {
			c.reply(call.m, call.seq, nil, actionErr(call.m.Method, err))
		} else {
			c.reply(call.m, call.seq, api.Empty{Seq: api.Seq{Seq: call.seq}}, nil)
		}
	}
}

// handleAgent runs session.send or approval.answer off the read loop: each waits for its writes
// to reach the program, and an answer by keys then waits to see the request close.
func (c *conn) handleAgent(m message) {
	seq := c.currentSeq()
	var send api.SessionSendParams
	var answer api.ApprovalAnswerParams
	var e *Error
	if m.Method == "session.send" {
		if e = c.decodeUp(m, &send); e == nil && (send.Session == "" || (send.Text == "" && len(send.Attachments) == 0 && !c.sendsContext(send.Session, send.IdempotencyKey))) {
			e = appErr(api.ErrorKindInvalid, "session.send needs a session, and text or attachments")
		}
		if e == nil && !isWord("SendOrigin", string(send.Origin)) && send.Origin != "" {
			e = appErr(api.ErrorKindInvalid, "origin %q is not one of %v", send.Origin, api.Enums["SendOrigin"])
		}
		if e == nil {
			e = checkKey(m.Method, send.IdempotencyKey)
		}
	} else {
		if e = c.decodeUp(m, &answer); e == nil && (answer.Pane == "" || answer.Request == "") {
			e = appErr(api.ErrorKindInvalid, "approval.answer needs a pane and a request")
		}
		if e == nil && !isWord("ApprovalChoice", string(answer.Choice)) {
			e = appErr(api.ErrorKindInvalid, "choice %q is not one of %v", answer.Choice, api.Enums["ApprovalChoice"])
		}
	}
	if e == nil && c.s.cfg.Agents == nil {
		e = appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
		defer cancel()
		var err error
		if m.Method == "session.send" {
			key, bare := send.IdempotencyKey, send
			bare.IdempotencyKey, bare.Origin = "", "" // a retry may be typed where the first was dictated
			_, e := c.s.idem(ctx, m.Method, key, bare, func() (any, *Error) {
				if err := c.s.cfg.Agents.Send(ctx, c.clientID, send); err != nil {
					return nil, actionErr(m.Method, err)
				}
				return nil, nil
			})
			if e != nil {
				err = e
			}
		} else {
			err = c.s.cfg.Agents.Answer(ctx, c.clientID, answer)
		}
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, api.Empty{Seq: api.Seq{Seq: seq}}, nil)
	}()
}

// handleTrust runs trust.answer off the read loop: it waits for its keys to reach the program
// and for the dialog to leave the screen (app.md 7.10).
func (c *conn) handleTrust(m message) {
	seq := c.currentSeq()
	var p api.TrustAnswerParams
	e := c.decodeUp(m, &p)
	if e == nil && p.Pane == "" {
		e = appErr(api.ErrorKindInvalid, "trust.answer needs a pane")
	}
	if e == nil && !isWord("TrustChoice", string(p.Choice)) {
		e = appErr(api.ErrorKindInvalid, "choice %q is not one of %v", p.Choice, api.Enums["TrustChoice"])
	}
	if e == nil {
		c.mu.Lock()
		kind := c.kind
		c.mu.Unlock()
		if kind == api.ClientKindHab {
			// A peer link carries an agent's calls from another hab, never the owner's answer.
			e = appErr(api.ErrorKindInvalid, "trust.answer is the owner's answer; a peer link cannot send it")
		}
	}
	if e == nil && c.s.cfg.Trust == nil {
		e = appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
		defer cancel()
		res, err := c.s.cfg.Trust.Answer(ctx, c.clientID, p)
		seq := c.currentSeq()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		res.Seq = api.Seq{Seq: seq}
		c.reply(m, seq, res, nil)
	}()
}

func (c *conn) withSeq(e *Error) *Error {
	e.Data.Seq = c.currentSeq()
	return e
}

// reply translates and queues a result or an error.
func (c *conn) reply(m message, seq uint64, result any, e *Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enqueueLocked(c.encodeReply(m, seq, result, e))
}

func (c *conn) encodeReply(m message, seq uint64, result any, e *Error) []byte {
	if e != nil {
		e.Data.Seq = seq
		return response(m.ID, nil, e)
	}
	var raw json.RawMessage
	if r, ok := result.(json.RawMessage); ok {
		raw = r
	} else {
		b, err := json.Marshal(api.NonNil(result))
		if err != nil {
			return response(m.ID, nil, &Error{Code: api.ErrorCode, Message: err.Error(),
				Data: api.ErrorData{Kind: api.ErrorKindUnavailable, Hint: "result could not be encoded", Seq: seq}})
		}
		raw = b
	}
	if m.Method != "rpc.discover" {
		var err error
		if raw, err = downResult(c.revision, m.Method, raw); err != nil {
			return response(m.ID, nil, &Error{Code: api.ErrorCode, Message: err.Error(),
				Data: api.ErrorData{Kind: api.ErrorKindUnavailable, Hint: "result could not be translated", Seq: seq}})
		}
	}
	return response(m.ID, raw, nil)
}

func decodeParams(m message, v any) *Error {
	if len(m.Params) == 0 || string(m.Params) == "null" {
		return nil
	}
	if err := json.Unmarshal(m.Params, v); err != nil {
		return protoErr(codeInvalidParams, "invalid params for %s: %v", m.Method, err)
	}
	return nil
}

func isWord(set, w string) bool { return slices.Contains(api.Enums[set], w) }

func (c *conn) handleHello(m message) {
	var p api.HelloParams
	if e := decodeParams(m, &p); e != nil {
		c.finish(response(m.ID, nil, e))
		return
	}
	if !isWord("ClientKind", string(p.Client.Kind)) {
		c.finish(response(m.ID, nil, appErr(api.ErrorKindInvalid, "client.kind %q is not one of %v", p.Client.Kind, api.Enums["ClientKind"])))
		return
	}
	if p.MinRevision < 1 || p.Revision < p.MinRevision {
		c.finish(response(m.ID, nil, appErr(api.ErrorKindInvalid, "revision %d and min_revision %d do not form a range", p.Revision, p.MinRevision)))
		return
	}
	chosen := min(p.Revision, api.Revision)
	if chosen < max(p.MinRevision, api.MinRevision) {
		e := appErr(api.ErrorKindRevisionUnsupported,
			"no shared API revision: this hab speaks %d to %d, the client %d to %d", api.MinRevision, api.Revision, p.MinRevision, p.Revision)
		e.Data.Server = &api.RevisionRange{Min: api.MinRevision, Max: api.Revision}
		e.Data.Client = &api.RevisionRange{Min: p.MinRevision, Max: p.Revision}
		c.finish(response(m.ID, nil, e))
		return
	}
	c.clientID, c.token = c.s.claimToken(c, p.ReconnectToken)
	c.hello = true
	caps := c.s.cfg.Capabilities
	if ps := c.s.cfg.Push; ps != nil && ps.Enabled() && !slices.Contains(caps, PushCapability) {
		caps = append(slices.Clip(caps), PushCapability)
	}
	if caps == nil {
		caps = []string{}
	}
	w := api.Welcome{
		Revision:       chosen,
		Epoch:          c.s.epoch,
		Build:          c.s.cfg.Build,
		Host:           c.s.cfg.Host,
		Capabilities:   caps,
		ClientID:       c.clientID,
		ReconnectToken: c.token,
		DeviceID:       c.s.cfg.DeviceID,
		Time:           c.s.cfg.Now().UTC().Format(welcomeTime),
	}
	c.mu.Lock()
	c.revision = chosen
	c.kind = p.Client.Kind
	c.label = p.Label
	if c.label == "" {
		c.label = p.Client.Name
	}
	if p.Client.Kind == api.ClientKindHab && p.Peer != nil {
		c.linkFrom = p.Peer.DeviceID // set with the welcome queued, so no notice goes before it
	}
	c.enqueueLocked(c.encodeReply(m, 0, w, nil))
	c.mu.Unlock()
	// A hab that dials this one says who it is (mesh.md 3.2). It counts as a link from that
	// device for as long as the connection lasts.
	if p.Client.Kind == api.ClientKindHab && p.Peer != nil && p.Peer.DeviceID != "" {
		pc := *p.Peer
		c.peer = &pc
	}
	if p.Client.Kind == api.ClientKindHab && p.Peer != nil && p.Peer.DeviceID != "" && c.s.cfg.Peers != nil {
		c.linked, c.linkID = true, linkIDs.Add(1)
		c.s.cfg.Peers.Linked(c.linkID, *p.Peer)
	}
}

// handleSubscribe answers events.subscribe (spec 3.5). It marks the connection subscribed and
// reads the seq S under the connection lock, then takes the snapshot. Every event queued with a
// seq up to S was emitted after its change was stored, so the snapshot holds it. Events that
// arrive while the snapshot is taken wait, then get seqs above S after the response.
func (c *conn) handleSubscribe(m message) {
	var p api.SubscribeParams
	var e *Error
	if e = c.decodeUp(m, &p); e == nil {
		e = checkScope(p.Scope)
	}
	if e != nil {
		c.reply(m, c.currentSeq(), nil, e)
		return
	}
	if f := c.s.cfg.ApproversChanged; f != nil {
		defer f() // after the unlock below: a narrower scope may drop the pane's approvers
	}
	c.mu.Lock()
	c.sub = newFilter(p.Scope)
	c.seeding = true
	c.pending = nil
	seq := c.seq
	c.mu.Unlock()

	st := c.s.cfg.Source.Snapshot(c.s.cfg.Host)

	c.mu.Lock()
	defer c.mu.Unlock()
	res := api.StructureResult{Seq: api.Seq{Seq: seq}, Structure: c.sub.seed(st)}
	c.enqueueLocked(c.encodeReply(m, seq, res, nil))
	pending := c.pending
	c.pending, c.seeding = nil, false
	for _, pe := range pending {
		if c.closing {
			break
		}
		c.deliverLocked(pe)
	}
}

func checkScope(sc api.Scope) *Error {
	if !isWord("ScopeKind", string(sc.Kind)) {
		return appErr(api.ErrorKindInvalid, "scope.kind %q is not one of %v", sc.Kind, api.Enums["ScopeKind"])
	}
	if sc.Kind != api.ScopeKindHost && len(sc.IDs) == 0 {
		return appErr(api.ErrorKindInvalid, "scope %s needs ids", sc.Kind)
	}
	return nil
}

// decodeUp translates params up from the connection's revision, then decodes them.
func (c *conn) decodeUp(m message, v any) *Error {
	c.mu.Lock()
	rev := c.revision
	c.mu.Unlock()
	if rev != api.Revision && len(m.Params) > 0 {
		p, err := upParams(rev, m.Method, m.Params)
		if err != nil {
			return appErr(api.ErrorKindInvalid, "params could not be translated: %v", err)
		}
		m.Params = p
	}
	return decodeParams(m, v)
}

const (
	hintNoDevices = "%s needs pairing, which this build does not have yet; the devices capability is absent"
	hintNoActions = "%s needs the holder link, which this hab is running without"
	hintNoChat    = "%s needs the transcript reader, which this hab is running without"
	hintNoPeers   = "%s needs the peer list, which this hab is running without; the mesh capability is absent"
	hintNoUsage   = "%s needs the usage readings, which this hab is running without; the usage capability is absent"
)

// later names the stage that builds each method this build defines but does not serve yet
// (spec 3.9). Each answers unavailable with a hint that names it.
var later = map[string]string{
	"session.interrupt": "Part 4 stage 4 (harness adapters: state and input)",
}

// actionErr turns an Actions error into an application error.
func actionErr(method string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return appErr(api.ErrorKindUnavailable, "%s: %v", method, err)
}

// sizeCheck answers a size method that cannot run: no size service, or no pane.
func (c *conn) sizeCheck(method, pane string) *Error {
	if c.s.cfg.Size == nil {
		return appErr(api.ErrorKindUnavailable, hintNoActions, method)
	}
	if pane == "" {
		return appErr(api.ErrorKindInvalid, "%s needs a pane", method)
	}
	return nil
}

func (c *conn) dispatch(m message, seq uint64) (any, *Error) {
	s := api.Seq{Seq: seq}
	switch m.Method {
	case "hab.hello":
		return nil, appErr(api.ErrorKindInvalid, "hab.hello was already sent on this connection")
	case "hab.ping":
		return api.Empty{Seq: s}, nil
	case "rpc.discover":
		return json.RawMessage(api.SchemaJSON), nil
	case "structure.get":
		var p api.StructureGetParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		return api.StructureResult{Seq: s, Structure: c.s.cfg.Source.Snapshot(c.s.cfg.Host)}, nil
	case "data.ticket":
		var p api.TicketParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if c.s.cfg.Data == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
		}
		if p.Pane == "" {
			return nil, appErr(api.ErrorKindInvalid, "data.ticket needs a pane")
		}
		tok, ttl, err := c.s.cfg.Data.Ticket(c.clientID, p.Pane, p.Observe)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.Ticket{Seq: s, Ticket: tok, ExpiresInMS: int(ttl / time.Millisecond)}, nil
	case "pane.take", "pane.request", "pane.disown":
		var p api.PaneRef
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if e := c.sizeCheck(m.Method, p.Pane); e != nil {
			return nil, e
		}
		var owner string
		var err error
		switch m.Method {
		case "pane.take":
			owner, err = c.s.cfg.Size.Take(c.clientID, p.Pane)
		case "pane.request":
			owner, err = c.s.cfg.Size.Request(c.clientID, p.Pane)
		default:
			owner, err = c.s.cfg.Size.Disown(c.clientID, p.Pane)
		}
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.OwnerResult{Seq: s, Owner: owner}, nil
	case "pane.set_desired_size":
		var p api.DesiredSizeParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if e := c.sizeCheck(m.Method, p.Pane); e != nil {
			return nil, e
		}
		applied, err := c.s.cfg.Size.SetDesired(c.clientID, p.Pane, p.Cols, p.Rows)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.DesiredSizeResult{Seq: s, Applied: applied}, nil
	case "pane.redraw":
		var p api.RedrawParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if e := c.sizeCheck(m.Method, p.Pane); e != nil {
			return nil, e
		}
		if err := c.s.cfg.Size.Redraw(c.clientID, p.Pane, p.Mode); err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.Empty{Seq: s}, nil
	case "push.register", "push.register_activity", "push.unregister", "session.focus", "notice.raise":
		return c.dispatchPush(m, s)
	case "devices.pair", "devices.list", "devices.revoke":
		return nil, appErr(api.ErrorKindUnavailable, hintNoDevices, m.Method)
	case "usage.list":
		var p api.UsageListParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if c.s.cfg.Usage == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoUsage, m.Method)
		}
		res := c.s.cfg.Usage.List()
		res.Seq = s
		return api.NonNil(res), nil
	case "catalog.get", "catalog.put", "catalog.list":
		return c.dispatchCatalog(m, s)
	case "settings.get", "settings.set":
		return c.dispatchSettings(m, s)
	case "capcom.open", "capcom.attach", "capcom.detach", "capcom.pending", "capcom.assign":
		return c.dispatchCapcom(m, s)
	case "session.resume":
		return c.dispatchResume(m, s)
	case "session.adopt", "session.outside":
		return c.dispatchAdopt(m, s)
	case "peers.list":
		var p api.PeersListParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if c.s.cfg.Peers == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoPeers, m.Method)
		}
		res := c.s.cfg.Peers.List()
		res.Seq = s
		return api.NonNil(res), nil
	case "peers.accept":
		var p api.PeersAcceptParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if c.s.cfg.Peers == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoPeers, m.Method)
		}
		if p.Label == "" {
			return nil, appErr(api.ErrorKindInvalid, "peers.accept needs a label")
		}
		res, err := c.s.cfg.Peers.Accept(p.Label)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		res.Seq = s
		return res, nil
	case "session.create":
		var p api.SessionCreateParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if e := checkKey(m.Method, p.IdempotencyKey); e != nil {
			return nil, e
		}
		if c.s.cfg.Actions == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
		defer cancel()
		if c.peer != nil {
			ctx = WithCallerPeer(ctx, *c.peer) // report_to names a session on that hab
		}
		made, e := c.s.createIdem(ctx, p, func(pane string) {
			if c.s.cfg.Size != nil {
				c.s.cfg.Size.Created(c.clientID, pane) // the creator owns the new pane (spec 6.5)
			}
		})
		if e != nil {
			return nil, e
		}
		made.Seq = s
		return made, nil
	case "command.list":
		var p api.CommandListParams
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		switch {
		case c.s.cfg.Commands == nil:
			return nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
		case p.Pane == "":
			return nil, appErr(api.ErrorKindInvalid, "command.list needs a pane")
		case p.Limit < 0 || p.Limit > 1000:
			return nil, appErr(api.ErrorKindInvalid, "command.list limit must be 0 to 1000")
		}
		if p.Limit == 0 {
			p.Limit = 100
		}
		res, err := c.s.cfg.Commands.CommandList(p)
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		if res.Commands == nil {
			res.Commands = []api.Command{}
		}
		res.Seq = s
		return res, nil
	case "session.rename", "session.close":
		rec, ok := c.s.cfg.Actions.(SessionRecords)
		if !ok {
			return nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
		}
		var err error
		if m.Method == "session.rename" {
			var p api.SessionRenameParams
			if e := c.decodeUp(m, &p); e != nil {
				return nil, e
			}
			if p.Session == "" || p.Name == "" {
				return nil, appErr(api.ErrorKindInvalid, "session.rename needs a session and a name")
			}
			err = rec.RenameSession(p.Session, p.Name)
		} else {
			var p api.SessionRef
			if e := c.decodeUp(m, &p); e != nil {
				return nil, e
			}
			if p.Session == "" {
				return nil, appErr(api.ErrorKindInvalid, "session.close needs a session")
			}
			err = rec.CloseSession(p.Session)
		}
		if err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.Empty{Seq: s}, nil
	case "pane.close":
		var p api.PaneRef
		if e := c.decodeUp(m, &p); e != nil {
			return nil, e
		}
		if p.Pane == "" {
			return nil, appErr(api.ErrorKindInvalid, "pane.close needs a pane")
		}
		if c.s.cfg.Actions == nil {
			return nil, appErr(api.ErrorKindUnavailable, hintNoActions, m.Method)
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.s.cfg.ActionTimeout)
		defer cancel()
		if err := c.s.cfg.Actions.ClosePane(ctx, p.Pane); err != nil {
			return nil, actionErr(m.Method, err)
		}
		return api.Empty{Seq: s}, nil
	}
	if stage, ok := later[m.Method]; ok {
		return nil, appErr(api.ErrorKindUnavailable, "%s is not built yet; %s builds it", m.Method, stage)
	}
	return nil, protoErr(codeMethodNotFound, "method %q does not exist", m.Method)
}
