package control

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
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

	mu       sync.Mutex
	queue    [][]byte
	queued   int  // bytes in queue
	closing  bool // nothing more is queued; the writer drains, then closes
	seq      uint64
	revision int
	sub      *filter    // nil until events.subscribe
	seeding  bool       // a subscribe is taking its snapshot; events wait in pending
	pending  []pubEvent // events that arrived while seeding, in order
}

func newConn(s *Server, nc net.Conn) *conn {
	return &conn{
		s:    s,
		nc:   nc,
		br:   bufio.NewReaderSize(nc, 64<<10),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

// --- Outgoing ---

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// enqueueLocked queues one message. Past the backlog limit it drops the queue, queues a final
// closing {reason: event_backlog} and stops the connection (spec 3.5). Callers hold c.mu.
func (c *conn) enqueueLocked(b []byte) {
	if c.closing {
		return
	}
	if c.queued+len(b) > c.s.cfg.BacklogLimit {
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
	var buf bytes.Buffer
	for {
		c.mu.Lock()
		batch, closing := c.queue, c.closing
		c.queue, c.queued = nil, 0
		c.mu.Unlock()
		if len(batch) > 0 {
			buf.Reset()
			for _, b := range batch {
				buf.Write(b)
			}
			if _, err := c.nc.Write(buf.Bytes()); err != nil {
				c.mu.Lock()
				c.closing = true
				c.queue = nil
				c.mu.Unlock()
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
				c.queue = append(c.queue, notification("heartbeat", api.Heartbeat{Seq: c.seq}))
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

// deliverLocked gives an in-scope event the next seq and queues it.
func (c *conn) deliverLocked(pe pubEvent) {
	if !c.sub.pass(pe.ev) {
		return
	}
	e := api.Event{Seq: c.seq + 1, Type: pe.ev.Type, ID: pe.ev.ID, Data: pe.data}
	rev := pe.ev.Revision
	e.Revision = &rev
	e, ok, err := downEvent(c.revision, e)
	if err != nil {
		c.s.cfg.Logf("translate event %s: %v", e.Type, err)
		return
	}
	if !ok {
		return
	}
	c.seq++
	c.enqueueLocked(notification("event", e))
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
	seq := c.currentSeq()
	result, e := c.dispatch(m, seq)
	c.reply(m, seq, result, e)
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
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revision = chosen
	c.enqueueLocked(c.encodeReply(m, 0, w, nil))
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
	hintNoHolder  = "%s needs the holder link, which this build does not have yet"
	hintNoDevices = "%s needs pairing, which this build does not have yet; the devices capability is absent"
)

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
	case "pane.input", "data.ticket", "pane.take", "pane.request", "pane.disown",
		"pane.set_desired_size", "pane.redraw":
		return nil, appErr(api.ErrorKindUnavailable, hintNoHolder, m.Method)
	case "devices.pair", "devices.list", "devices.revoke":
		return nil, appErr(api.ErrorKindUnavailable, hintNoDevices, m.Method)
	}
	return nil, protoErr(codeMethodNotFound, "method %q does not exist", m.Method)
}
