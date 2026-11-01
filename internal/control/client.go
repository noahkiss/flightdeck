package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// ErrClosed is returned by calls on a client whose connection has closed.
var ErrClosed = errors.New("control: connection closed")

// Client speaks the control protocol over any byte stream: the Unix socket, or the stdio of
// `gff-hab connect` run through ssh. It implements api.Caller, so api.Client{Caller: c} gives
// typed methods.
type Client struct {
	rw io.ReadWriteCloser

	wmu      sync.Mutex
	w        *bufio.Writer
	lastSend time.Time

	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan message
	err     error // why the connection ended
	reason  api.ClosingReason

	notes    chan Notification
	queue    []Notification
	qmu      sync.Mutex
	qwake    chan struct{}
	done     chan struct{}
	stopPing chan struct{}
	stopOnce sync.Once
}

// Dial connects to the control socket at path.
func Dial(path string) (*Client, error) {
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return NewClient(nc), nil
}

// NewClient starts a client on rw. The client owns rw and closes it.
func NewClient(rw io.ReadWriteCloser) *Client {
	c := &Client{
		rw:       rw,
		w:        bufio.NewWriter(rw),
		pending:  map[string]chan message{},
		notes:    make(chan Notification),
		qwake:    make(chan struct{}, 1),
		done:     make(chan struct{}),
		stopPing: make(chan struct{}),
	}
	go c.readLoop()
	go c.pump()
	return c
}

// Notifications delivers every notification in the order the server sent it, without loss. The
// channel closes after the connection ends and every notification was delivered.
func (c *Client) Notifications() <-chan Notification { return c.notes }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err says why the connection ended: nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// ClosingReason is the reason from the server's closing notification, if one arrived.
func (c *Client) ClosingReason() api.ClosingReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// Close ends the connection.
func (c *Client) Close() error {
	c.stopOnce.Do(func() { close(c.stopPing) })
	return c.rw.Close()
}

// KeepAlive sends hab.ping whenever the client has sent nothing for interval (spec 3.5), until
// the connection ends.
func (c *Client) KeepAlive(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval / 2)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-c.stopPing:
				return
			case <-t.C:
				c.wmu.Lock()
				idle := time.Since(c.lastSend) >= interval
				c.wmu.Unlock()
				if idle {
					ctx, cancel := context.WithTimeout(context.Background(), interval)
					_ = c.Call(ctx, "hab.ping", api.PingParams{}, nil)
					cancel()
				}
			}
		}
	}()
}

// Hello sends hab.hello with this build's revision range.
func (c *Client) Hello(ctx context.Context, client api.ClientInfo, label, reconnectToken string) (api.Welcome, error) {
	return api.Client{Caller: c}.HabHello(ctx, api.HelloParams{
		Revision:       api.Revision,
		MinRevision:    api.MinRevision,
		Client:         client,
		Label:          label,
		ReconnectToken: reconnectToken,
	})
}

// Call sends one request and decodes its result into result (which may be nil, or a
// *json.RawMessage to keep it raw). An error result is an *Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if params == nil {
		params = struct{}{}
	}
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := strconv.FormatUint(c.nextID, 10)
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	b, err := json.Marshal(message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: p})
	if err != nil {
		return err
	}
	err = c.write(ctx, b)
	if err != nil {
		return fmt.Errorf("control: send %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return c.Err()
		}
		if m.Error != nil {
			return &Error{Code: m.Error.Code, Message: m.Error.Message, Data: m.Error.Data}
		}
		if result == nil {
			return nil
		}
		if raw, ok := result.(*json.RawMessage); ok {
			*raw = append((*raw)[:0], m.Result...)
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}

// WriteTimeout bounds one request write when the call's context has no earlier deadline.
const WriteTimeout = 30 * time.Second

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

// write sends one request line. A write that outlasts the context's deadline (or WriteTimeout)
// ends the connection: a half-written line would corrupt the stream anyway.
func (c *Client) write(ctx context.Context, b []byte) error {
	// A context already over must not reach the socket: a deadline in the past fails the
	// write at once as an i/o timeout, and that would end the connection for every call.
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(WriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if wd, ok := c.rw.(writeDeadliner); ok && wd.SetWriteDeadline(deadline) == nil {
		defer wd.SetWriteDeadline(time.Time{})
	} else {
		t := time.AfterFunc(time.Until(deadline), func() { c.rw.Close() })
		defer t.Stop()
	}
	c.w.Write(b)
	c.w.WriteByte('\n')
	err := c.w.Flush()
	c.lastSend = time.Now()
	if err != nil {
		c.rw.Close()
		c.w.Reset(c.rw) // drop the unsent bytes; the connection is over
	}
	return err
}

// readLine reads one line up to MaxMessage bytes. The server's backlog limit is smaller, so no
// message it can deliver is longer.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > MaxMessage {
			return nil, fmt.Errorf("control: a message from the server is longer than %d bytes", MaxMessage)
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func (c *Client) readLoop() {
	br := bufio.NewReaderSize(c.rw, 64<<10)
	var err error
	for {
		var line []byte
		line, err = readLine(br)
		if len(line) > 0 && line[0] == '{' {
			var m message
			if jerr := json.Unmarshal(line, &m); jerr != nil {
				err = fmt.Errorf("control: bad message from server: %w", jerr)
				break
			}
			c.dispatch(m)
		}
		if err != nil {
			break
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		err = ErrClosed
	}
	c.mu.Lock()
	if c.reason != "" {
		err = fmt.Errorf("%w: server sent closing (%s)", ErrClosed, c.reason)
	}
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.rw.Close()
	close(c.done)
	c.signalPump()
}

func (c *Client) dispatch(m message) {
	if m.Method != "" {
		if m.Method == "closing" {
			var cl api.Closing
			if json.Unmarshal(m.Params, &cl) == nil {
				c.mu.Lock()
				c.reason = cl.Reason
				c.mu.Unlock()
			}
		}
		c.qmu.Lock()
		c.queue = append(c.queue, Notification{Method: m.Method, Params: m.Params})
		c.qmu.Unlock()
		c.signalPump()
		return
	}
	c.mu.Lock()
	ch := c.pending[string(m.ID)]
	c.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (c *Client) signalPump() {
	select {
	case c.qwake <- struct{}{}:
	default:
	}
}

// pump moves notifications from the unbounded queue to the channel, so a slow reader of
// Notifications never stalls responses.
func (c *Client) pump() {
	defer close(c.notes)
	for {
		c.qmu.Lock()
		q := c.queue
		c.queue = nil
		c.qmu.Unlock()
		for _, n := range q {
			c.notes <- n
		}
		if len(q) > 0 {
			continue
		}
		select {
		case <-c.qwake:
		case <-c.done:
			c.qmu.Lock()
			q := c.queue
			c.queue = nil
			c.qmu.Unlock()
			for _, n := range q {
				c.notes <- n
			}
			return
		}
	}
}
