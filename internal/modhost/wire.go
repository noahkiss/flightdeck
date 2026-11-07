package modhost

// The pipe between gff-hab and the module host process (modules.md 7.4): length-prefixed JSON
// frames on the child's stdin and stdout. Both sides send requests and answer the other's.

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

// maxFrame bounds one frame: a bundle (1 MiB of sources) with its state, or an exec result
// (two 1 MiB streams, base64 in JSON), fit with room.
const maxFrame = 16 << 20

// frame is one message. A request has ID and Op; its answer has Re (the request's ID) and Body
// or Err. Each side numbers its own requests, so an answer always names a request of the side
// that reads it. Mod is the module a request is about: the parent's handle, not the module's id.
type frame struct {
	ID   uint64          `json:"id,omitempty"`
	Re   uint64          `json:"re,omitempty"`
	Op   string          `json:"op,omitempty"`
	Mod  uint64          `json:"mod,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	Err  *wireError      `json:"err,omitempty"`
}

// Ops, parent to child.
const (
	opLoad    = "load"    // body wireLoad; answer wireLoaded
	opRefresh = "refresh" // body wireCall; answer wireDone
	opRender  = "render"  // body wireCall; answer wireDone with the tree
	opAct     = "act"     // body wireCall; answer wireDone
	opUnload  = "unload"  // no body; answer empty
	opPing    = "ping"    // no body; answer wirePong
)

// Ops, child to parent: the backends of one module (modules.md 7.3).
const (
	opHabGet    = "hab.get"    // body wireHab; answer the result
	opHabAct    = "hab.act"    // body wireHab; answer the result
	opExec      = "exec"       // body wireExec; answer ExecResult
	opFetch     = "fetch"      // body wireFetch; answer FetchResponse
	opStateLoad = "state.load" // no body; answer the state
	opStateSave = "state.save" // body the state; answer empty
	opLog       = "log"        // body the line as a JSON string; never answered
)

// opCancel, either way: the request ID names ends its context. Never answered.
const opCancel = "cancel"

type wireLoad struct {
	Dir      string            `json:"dir"`
	Manifest *modules.Manifest `json:"manifest"`
	Sources  map[string]string `json:"sources"`
	Settings map[string]any    `json:"settings,omitempty"`
	Limits   Limits            `json:"limits"`
	// Which backends the parent has: a missing one throws in the module, as in process.
	Hab, Exec, Fetch, State, Log bool
}

type wireLoaded struct {
	Exported []string `json:"exported"`
}

type wireCall struct {
	Call Call            `json:"call"`
	Name string          `json:"name,omitempty"` // act
	Args json.RawMessage `json:"args,omitempty"` // act
}

type wireDone struct {
	Tree   json.RawMessage `json:"tree,omitempty"`
	Growth int64           `json:"growth"`
}

type wirePong struct {
	// Mem is the child's own count of the memory Go holds from the system, the resident size
	// where the parent cannot read one.
	Mem uint64 `json:"mem"`
}

type wireHab struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	// Deadline is the child call's deadline, so the parent's backend gets the same one.
	Deadline time.Time `json:"deadline"`
}

type wireExec struct {
	Req      ExecRequest `json:"req"`
	Deadline time.Time   `json:"deadline"`
}

type wireFetch struct {
	Req      FetchRequest `json:"req"`
	Deadline time.Time    `json:"deadline"`
}

// wireError carries the package's typed errors across, so errors.As and errors.Is work on the
// parent's side as they do in process.
type wireError struct {
	Kind     string           `json:"kind"`
	Message  string           `json:"message,omitempty"`
	Func     string           `json:"func,omitempty"`
	Name     string           `json:"name,omitempty"`
	Stack    string           `json:"stack,omitempty"`
	Limit    int64            `json:"limit,omitempty"`
	Stuck    bool             `json:"stuck,omitempty"`
	Path     string           `json:"path,omitempty"`
	Number   int64            `json:"number,omitempty"`
	Number2  int64            `json:"number2,omitempty"`
	Problems modules.Problems `json:"problems,omitempty"`
	// Runtime marks a backend panic whose value was a runtime.Error: the child panics with one
	// too, so goja treats it as it would in process.
	Runtime bool `json:"runtime,omitempty"`
}

// Error kinds.
const (
	kindTimeout      = "timeout"
	kindInternal     = "internal"
	kindMemory       = "memory"
	kindScript       = "script"
	kindTree         = "tree"
	kindClientTooOld = "client_too_old"
	kindBundle       = "bundle"
	kindNewerBlocks  = "newer_blocks"
	kindNewerBridge  = "newer_bridge"
	kindProblems     = "problems"
	kindStopped      = "stopped"
	kindHeapPressure = "heap_pressure"
	kindNoAct        = "no_act"
	kindClientAction = "client_action"
	kindCanceled     = "canceled"
	kindDeadline     = "deadline"
	kindPanic        = "panic" // a backend panicked in the parent
	kindOther        = "other"
)

func encodeError(err error) *wireError {
	if err == nil {
		return nil
	}
	var (
		te  *TimeoutError
		ie  *InternalError
		me  *MemoryError
		se  *ScriptError
		tre *TreeError
		cto *ClientTooOld
		be  *BundleError
		nbl *NewerBlocks
		nbr *modules.NewerBridge
		ps  modules.Problems
	)
	switch {
	case errors.As(err, &te):
		return &wireError{Kind: kindTimeout, Func: te.Func, Limit: int64(te.Limit), Stuck: te.Stuck}
	case errors.As(err, &ie):
		return &wireError{Kind: kindInternal, Func: ie.Func, Message: ie.Value, Stack: ie.Stack}
	case errors.As(err, &me):
		return &wireError{Kind: kindMemory, Func: me.Func, Limit: int64(me.Limit)}
	case errors.As(err, &se):
		return &wireError{Kind: kindScript, Func: se.Func, Name: se.Name, Message: se.Message, Stack: se.Stack}
	case errors.As(err, &tre):
		return &wireError{Kind: kindTree, Problems: tre.Problems}
	case errors.As(err, &cto):
		return &wireError{Kind: kindClientTooOld, Number: int64(cto.Vocabulary), Number2: cto.Blocks}
	case errors.As(err, &be):
		return &wireError{Kind: kindBundle, Path: be.Path, Message: be.Message}
	case errors.As(err, &nbl):
		return &wireError{Kind: kindNewerBlocks, Number: nbl.Blocks}
	case errors.As(err, &nbr):
		return &wireError{Kind: kindNewerBridge, Number: nbr.Bridge}
	case errors.As(err, &ps):
		return &wireError{Kind: kindProblems, Problems: ps}
	case errors.Is(err, ErrStopped):
		return &wireError{Kind: kindStopped}
	case errors.Is(err, ErrHeapPressure):
		return &wireError{Kind: kindHeapPressure, Message: err.Error()}
	case errors.Is(err, ErrNoAct):
		return &wireError{Kind: kindNoAct}
	case errors.Is(err, ErrClientAction):
		return &wireError{Kind: kindClientAction}
	case errors.Is(err, context.Canceled):
		return &wireError{Kind: kindCanceled}
	case errors.Is(err, context.DeadlineExceeded):
		return &wireError{Kind: kindDeadline}
	}
	return &wireError{Kind: kindOther, Message: err.Error()}
}

// heapPressureError keeps the child's message while errors.Is still finds ErrHeapPressure.
type heapPressureError struct{ msg string }

func (e *heapPressureError) Error() string { return e.msg }
func (e *heapPressureError) Unwrap() error { return ErrHeapPressure }

func (w *wireError) decode() error {
	if w == nil {
		return nil
	}
	switch w.Kind {
	case kindTimeout:
		return &TimeoutError{Func: w.Func, Limit: time.Duration(w.Limit), Stuck: w.Stuck}
	case kindInternal:
		return &InternalError{Func: w.Func, Value: w.Message, Stack: w.Stack}
	case kindMemory:
		return &MemoryError{Func: w.Func, Limit: uint64(w.Limit)}
	case kindScript:
		return &ScriptError{Func: w.Func, Name: w.Name, Message: w.Message, Stack: w.Stack}
	case kindTree:
		return &TreeError{Problems: w.Problems}
	case kindClientTooOld:
		return &ClientTooOld{Vocabulary: int(w.Number), Blocks: w.Number2}
	case kindBundle:
		return &BundleError{Path: w.Path, Message: w.Message}
	case kindNewerBlocks:
		return &NewerBlocks{Blocks: w.Number}
	case kindNewerBridge:
		return &modules.NewerBridge{Bridge: w.Number}
	case kindProblems:
		return w.Problems
	case kindStopped:
		return ErrStopped
	case kindHeapPressure:
		return &heapPressureError{msg: w.Message}
	case kindNoAct:
		return ErrNoAct
	case kindClientAction:
		return ErrClientAction
	case kindCanceled:
		return context.Canceled
	case kindDeadline:
		return context.DeadlineExceeded
	}
	return errors.New(w.Message)
}

// conn is one end of the pipe: a frame writer shared by every goroutine, and the requests this
// side sent that wait for an answer.
type conn struct {
	wmu sync.Mutex
	w   *bufio.Writer

	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan frame
	closed  bool
}

func newConn(w io.Writer) *conn {
	return &conn{w: bufio.NewWriter(w), pending: map[uint64]chan frame{}}
}

// errFrameTooBig is a message over maxFrame; nothing was written.
var errFrameTooBig = errors.New("module host message too large")

func (c *conn) send(f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(data) > maxFrame {
		return fmt.Errorf("%w: %d bytes, the limit is %d", errFrameTooBig, len(data), maxFrame)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	if _, err := c.w.Write(n[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(data); err != nil {
		return err
	}
	return c.w.Flush()
}

// start registers a request and returns its ID and the channel its answer arrives on. The
// channel is closed, with no answer, when the connection ends.
func (c *conn) start() (uint64, chan frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, false
	}
	c.next++
	ch := make(chan frame, 1)
	c.pending[c.next] = ch
	return c.next, ch, true
}

// forget drops a request whose answer nobody waits for any more.
func (c *conn) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// answered hands an answer to its request. An answer to a request already forgotten is dropped.
func (c *conn) answered(f frame) {
	c.mu.Lock()
	ch := c.pending[f.Re]
	delete(c.pending, f.Re)
	c.mu.Unlock()
	if ch != nil {
		ch <- f
	}
}

// end closes every waiting request's channel; later starts fail.
func (c *conn) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

func (c *conn) reply(re uint64, body any, err error) error {
	f := frame{Re: re, Err: encodeError(err)}
	if err == nil && body != nil {
		data, merr := json.Marshal(body)
		if merr != nil {
			f.Err = encodeError(merr)
		} else {
			f.Body = data
		}
	}
	return c.send(f)
}

func readFrame(r *bufio.Reader) (frame, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return frame{}, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > maxFrame {
		return frame{}, fmt.Errorf("module host message of %d bytes is over the limit of %d", size, maxFrame)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return frame{}, fmt.Errorf("bad module host message: %w", err)
	}
	return f, nil
}

// marshalBody encodes a request's body. It fails for a value JSON cannot hold: a caller's bad
// RawMessage, or NaN in settings.
func marshalBody(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("module host message: %w", err)
	}
	return data, nil
}
