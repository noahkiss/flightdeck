package holderclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// Holder is one connection to one holder, at the protocol version negotiated with it.
type Holder struct {
	c       *Client
	nc      net.Conn
	pc      *hp.Conn
	welcome *hp.Welcome
	sock    fileID // the socket file this connection was dialled at
	done    chan struct{}
	byeC    chan struct{} // closed when the holder's bye is read: it will answer nothing more

	mu       sync.Mutex
	entry    holderreg.Entry
	draining bool
	bye      bool
	err      error
	closed   bool
	token    uint32
	waits    map[uint32]chan hp.Frame
	inputs   map[uint32]*InputCall // input frames in flight, by token
	panes    map[hp.Handle]*pane

	sendMu sync.Mutex // input frames go out in the order of their offsets
}

// pane is the client's record of one pane on a holder.
type pane struct {
	info  hp.PaneInfo
	input uint64 // the input offset the next input frame carries
	// gen counts the input failures that moved input back to the holder's count. An input frame
	// carries the gen it was sent in; only the first failure of a gen resyncs (see track).
	gen uint64
}

// Pane names a pane and the holder it lives on.
type Pane struct {
	UUID   hp.UUID
	Handle hp.Handle
	Holder *Holder
}

// Instance returns the holder's instance id.
func (h *Holder) Instance() string { return h.welcome.Instance }

// Build returns the holder's build id, from its welcome.
func (h *Holder) Build() string { return h.welcome.Build }

// Version returns the protocol version negotiated with this holder.
func (h *Holder) Version() hp.Version { return h.pc.Version() }

// Welcome returns the holder's welcome.
func (h *Holder) Welcome() *hp.Welcome { return h.welcome }

// Entry returns the holder's registry entry as last read.
func (h *Holder) Entry() holderreg.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entry
}

// Draining reports whether the holder is draining: its welcome or registry file said so, or this
// client sent it drain.
func (h *Holder) Draining() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining
}

// Closed reports whether the connection has ended.
func (h *Holder) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Done is closed when the connection ends.
func (h *Holder) Done() <-chan struct{} { return h.done }

// Err returns why the connection ended: nil while it is open and after a bye.
func (h *Holder) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *Holder) sameHolder(e holderreg.Entry) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && h.entry.Pid == e.Pid && h.entry.Started.Equal(e.Started)
}

// nextToken returns a fresh request token. 0 is never used: it cannot be told from an event.
func (h *Holder) nextToken() uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token++
	if h.token == 0 {
		h.token++
	}
	return h.token
}

// readLoop reads frames until the connection ends. A frame whose token has a waiting request
// goes to that request; every other frame is an event.
func (h *Holder) readLoop() {
	for {
		f, err := h.pc.ReadFrame()
		if err != nil {
			h.mu.Lock()
			bye := h.bye
			h.mu.Unlock()
			if bye {
				err = nil
			}
			h.close(err)
			return
		}
		h.track(f)
		tok := f.Header().Token
		h.mu.Lock()
		w := h.waits[tok]
		h.mu.Unlock()
		if tok != 0 && w != nil {
			select {
			case w <- f:
			case <-h.done:
			}
			continue
		}
		h.c.emit(h, Event{Holder: h, Frame: f})
	}
}

// track keeps the client's pane records in step with what the holder says.
func (h *Holder) track(f hp.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch f := f.(type) {
	case *hp.Spawned:
		if h.panes[f.Handle] == nil {
			h.panes[f.Handle] = &pane{info: hp.PaneInfo{UUID: f.UUID, Handle: f.Handle, Pid: f.Pid}}
		}
	case *hp.Exited:
		if p := h.panes[f.Handle]; p != nil {
			p.info.Exited, p.info.Exit = true, f.Status
		}
	case *hp.Error:
		// An input frame refused with an offset leaves the holder's count where it was, so every
		// input frame sent after it fails on its offset. The first failure of a gen moves the
		// next offset back to the holder's count; the later ones are aborted. The reader sees
		// the errors in the order the holder sent them, so the first one is always the cause.
		ic := h.inputs[f.Token]
		if ic == nil || f.Ref != hp.KindInput {
			break
		}
		p := h.panes[ic.hd]
		if p == nil || p.gen != ic.gen {
			ic.aborted = f.Code == hp.CodeInputOffset
			break
		}
		if f.Code == hp.CodeInputOffset || f.Code == hp.CodeQueueFull {
			p.input = f.Offset
			p.gen++
		}
	case *hp.Bye:
		if !h.bye {
			h.bye = true
			close(h.byeC)
		}
	}
}

// close ends the connection once. A nil err means a clean end (after bye).
func (h *Holder) close(err error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.err = err
	h.panes = map[hp.Handle]*pane{}
	h.mu.Unlock()
	h.nc.Close()
	close(h.done)
	h.c.forget(h)
	// The end event must not be lost, but nobody may be reading any more: deliver it from its
	// own goroutine, which gives up once the client is closed. Every frame event before it was
	// delivered already.
	ev := Event{Holder: h, Err: err}
	select {
	case h.c.events <- ev:
	default:
		go func() {
			select {
			case h.c.events <- ev:
			case <-h.c.quit:
			}
		}()
	}
}

// call sends the request mk builds with a fresh token, and returns every frame answering it up
// to and including the first one for which last returns true. An error frame for the token ends
// the call with a *RequestError.
func (h *Holder) call(ctx context.Context, mk func(tok uint32) hp.Frame, last func(hp.Frame) bool) ([]hp.Frame, error) {
	tok := h.nextToken()
	w := make(chan hp.Frame, 16)
	h.mu.Lock()
	if h.closed || h.bye {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	h.waits[tok] = w
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.waits, tok)
		h.mu.Unlock()
	}()
	if err := h.pc.WriteFrame(mk(tok)); err != nil {
		return nil, h.writeFailed(err)
	}
	var got []hp.Frame
	// handle takes one reply; done reports whether the call is over.
	handle := func(f hp.Frame) (done bool, err error) {
		if e, ok := f.(*hp.Error); ok {
			return true, &RequestError{Ref: e.Ref, Code: e.Code, Offset: e.Offset, Message: e.Message}
		}
		got = append(got, f)
		return last(f), nil
	}
	for {
		select {
		case f := <-w:
			if done, err := handle(f); done {
				return got, err
			}
		case <-h.byeC:
			// The reader delivers frames in order, so every reply sent before the bye is
			// already queued. Take those, then end: the holder answers nothing after its bye.
			for {
				select {
				case f := <-w:
					if done, err := handle(f); done {
						return got, err
					}
				default:
					return got, ErrClosed
				}
			}
		case <-h.done:
			return got, ErrClosed
		case <-ctx.Done():
			return got, ctx.Err()
		}
	}
}

// do sends a request that has no success reply, then a ping. The holder handles a connection's
// frames in order, so the pong means the request was handled; an error before it means the
// request failed.
func (h *Holder) do(ctx context.Context, mk func(tok uint32) hp.Frame) error {
	tok := h.nextToken()
	w := make(chan hp.Frame, 4)
	h.mu.Lock()
	if h.closed || h.bye {
		h.mu.Unlock()
		return ErrClosed
	}
	h.waits[tok] = w
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.waits, tok)
		h.mu.Unlock()
	}()
	if err := h.pc.WriteFrame(mk(tok)); err != nil {
		return h.writeFailed(err)
	}
	_, err := h.call(ctx, func(t uint32) hp.Frame { return &hp.Ping{Token: t} }, isPong)
	select {
	case f := <-w:
		if e, ok := f.(*hp.Error); ok {
			return &RequestError{Ref: e.Ref, Code: e.Code, Offset: e.Offset, Message: e.Message}
		}
	default:
	}
	return err
}

// writeFailGrace is how long a failed write waits for the reader to see the holder's end.
const writeFailGrace = time.Second

// writeFailed turns a failed request write into the error the caller sees. A holder that sent
// bye and exited (the last release on a draining holder, or a drain of an empty one) makes the
// next write fail with a broken pipe, possibly before the reader has read the bye. That is the
// connection's normal end, so it is ErrClosed, as it would be had the reader been first. The
// reader gets a short grace to catch up. Any other write failure leaves a frame half written, so
// the connection ends with that error.
func (h *Holder) writeFailed(err error) error {
	ended := func() (closed, bye bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.closed, h.bye
	}
	closed, bye := ended()
	if !closed && !bye {
		select {
		case <-h.done:
		case <-time.After(writeFailGrace):
		}
		closed, bye = ended()
	}
	if closed || bye {
		if bye {
			h.close(nil)
		}
		return ErrClosed
	}
	h.close(err)
	return err
}

func isPong(f hp.Frame) bool { _, ok := f.(*hp.Pong); return ok }

// Ping checks that the holder answers.
func (h *Holder) Ping(ctx context.Context) error {
	_, err := h.call(ctx, func(t uint32) hp.Frame { return &hp.Ping{Token: t} }, isPong)
	return err
}

// Spawn starts a pane on this holder. A draining holder refuses with code draining.
func (h *Holder) Spawn(ctx context.Context, s hp.Spawn) (*Pane, error) {
	fs, err := h.call(ctx, func(tok uint32) hp.Frame {
		s.Token = tok
		return &s
	}, func(hp.Frame) bool { return true })
	if err != nil {
		return nil, err
	}
	switch f := fs[0].(type) {
	case *hp.Spawned:
		return &Pane{UUID: f.UUID, Handle: f.Handle, Holder: h}, nil
	case *hp.SpawnFailed:
		return nil, &RequestError{Ref: hp.KindSpawn, Code: f.Code, Message: f.Message}
	default:
		return nil, fmt.Errorf("holderclient: unexpected answer to spawn: %s", f.Header().Kind)
	}
}

// Replay sends an open replay from offset from, which subscribes the pane: live output events
// follow from the returned end (protocol section 6). It returns the replayed bytes and the
// replay end.
func (h *Holder) Replay(ctx context.Context, hd hp.Handle, from uint64) ([]byte, *hp.ReplayEnd, error) {
	fs, err := h.call(ctx, func(tok uint32) hp.Frame {
		return &hp.Replay{Handle: hd, Token: tok, From: from}
	}, func(f hp.Frame) bool { _, ok := f.(*hp.ReplayEnd); return ok })
	if err != nil {
		return nil, nil, err
	}
	var data []byte
	for _, f := range fs {
		if d, ok := f.(*hp.ReplayData); ok {
			data = append(data, d.Data...)
		}
	}
	return data, fs[len(fs)-1].(*hp.ReplayEnd), nil
}

// ReplayRange sends a bounded replay of [from, to). It does not change the live stream
// (protocol section 6). It returns the replayed bytes and the replay end, which says what range
// the holder really sent and whether part of it had left the ring.
func (h *Holder) ReplayRange(ctx context.Context, hd hp.Handle, from, to uint64) ([]byte, *hp.ReplayEnd, error) {
	fs, err := h.call(ctx, func(tok uint32) hp.Frame {
		return &hp.Replay{Handle: hd, Token: tok, From: from, HasTo: true, To: to}
	}, func(f hp.Frame) bool { _, ok := f.(*hp.ReplayEnd); return ok })
	if err != nil {
		return nil, nil, err
	}
	var data []byte
	for _, f := range fs {
		if d, ok := f.(*hp.ReplayData); ok {
			data = append(data, d.Data...)
		}
	}
	return data, fs[len(fs)-1].(*hp.ReplayEnd), nil
}

// Input writes data to the pane's PTY and waits until the holder took it. After a wrong offset
// (another gff-hab wrote meanwhile) it resends once at the offset the holder expects.
func (h *Holder) Input(ctx context.Context, hd hp.Handle, data []byte) error {
	for try := 0; ; try++ {
		ic, err := h.SendInput(hd, data)
		if err == nil {
			err = ic.Wait(ctx)
		}
		var re *RequestError
		if try == 0 && (errors.Is(err, ErrInputAborted) || errors.As(err, &re) && re.Code == hp.CodeInputOffset) {
			continue
		}
		return err
	}
}

// InputCall is an input frame in flight; Wait returns its result.
type InputCall struct {
	h         *Holder
	hd        hp.Handle
	tok, ping uint32
	gen       uint64
	w         chan hp.Frame
	aborted   bool // set by the reader, under h.mu: an earlier input frame of the gen failed
}

// SendInput writes data to the pane's PTY without waiting for the holder's answer, so a caller
// may send the next input at once. Input frames reach the program in the order SendInput is
// called. A frame refused for a full queue or a wrong offset makes every frame sent after it, up
// to the next SendInput after the refusal is read, fail with ErrInputAborted; none of those
// reached the program.
//
// The holder answers in order with its output, so under heavy output Wait returns well after
// the input reached the program.
func (h *Holder) SendInput(hd hp.Handle, data []byte) (*InputCall, error) {
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	ic := &InputCall{h: h, hd: hd, tok: h.nextToken(), ping: h.nextToken(), w: make(chan hp.Frame, 4)}
	h.mu.Lock()
	if h.closed || h.bye {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	p := h.panes[hd]
	if p == nil {
		h.mu.Unlock()
		return nil, &RequestError{Ref: hp.KindInput, Code: hp.CodeNoPane, Message: "no such pane"}
	}
	off := p.input
	ic.gen = p.gen
	p.input += uint64(len(data))
	h.waits[ic.tok], h.waits[ic.ping] = ic.w, ic.w
	h.inputs[ic.tok] = ic
	h.mu.Unlock()
	err := h.pc.WriteFrame(&hp.Input{Handle: hd, Token: ic.tok, Offset: off, Data: data})
	if err == nil {
		err = h.pc.WriteFrame(&hp.Ping{Token: ic.ping})
	}
	if err != nil {
		ic.forget()
		return nil, h.writeFailed(err)
	}
	return ic, nil
}

func (ic *InputCall) forget() {
	h := ic.h
	h.mu.Lock()
	delete(h.waits, ic.tok)
	delete(h.waits, ic.ping)
	delete(h.inputs, ic.tok)
	h.mu.Unlock()
}

// Wait waits for the holder's answer to the input frame: nil once the holder took it, a
// *RequestError when it refused it, ErrInputAborted when an earlier input frame failed. When ctx
// ends first, the input may still reach the program.
func (ic *InputCall) Wait(ctx context.Context) error {
	defer ic.forget()
	h := ic.h
	// handle takes one answer; done reports whether the call is over. The holder sends an
	// error for the input before the pong.
	handle := func(f hp.Frame) (done bool, err error) {
		e, ok := f.(*hp.Error)
		switch {
		case ok && e.Token == ic.tok:
			h.mu.Lock()
			aborted := ic.aborted
			h.mu.Unlock()
			if aborted {
				return true, ErrInputAborted
			}
			return true, &RequestError{Ref: e.Ref, Code: e.Code, Offset: e.Offset, Message: e.Message}
		case ok:
			return true, &RequestError{Ref: e.Ref, Code: e.Code, Offset: e.Offset, Message: e.Message}
		}
		return isPong(f), nil
	}
	for {
		select {
		case f := <-ic.w:
			if done, err := handle(f); done {
				return err
			}
		case <-h.byeC:
			for {
				select {
				case f := <-ic.w:
					if done, err := handle(f); done {
						return err
					}
				default:
					return ErrClosed
				}
			}
		case <-h.done:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Resize sets a pane's size in cells.
func (h *Holder) Resize(ctx context.Context, hd hp.Handle, cols, rows uint16) error {
	return h.do(ctx, func(tok uint32) hp.Frame { return &hp.Resize{Handle: hd, Token: tok, Cols: cols, Rows: rows} })
}

// Release drops a pane (spec 2.7). On a draining holder, releasing the last pane ends the
// holder: it sends bye and exits.
func (h *Holder) Release(ctx context.Context, hd hp.Handle) error {
	err := h.do(ctx, func(tok uint32) hp.Frame { return &hp.Release{Handle: hd, Token: tok} })
	if err == nil || errors.Is(err, ErrClosed) {
		h.mu.Lock()
		delete(h.panes, hd)
		h.mu.Unlock()
	}
	if errors.Is(err, ErrClosed) && h.Err() == nil {
		return nil // the release ended a draining holder before its pong
	}
	return err
}

// Drain tells the holder to take no new panes and to exit after its last pane is released
// (spec 4.3). When the holder has no panes, it exits at once.
func (h *Holder) Drain(ctx context.Context) error {
	err := h.do(ctx, func(tok uint32) hp.Frame { return &hp.Drain{Token: tok} })
	if err == nil || (errors.Is(err, ErrClosed) && h.Err() == nil) {
		h.mu.Lock()
		h.draining = true
		h.mu.Unlock()
		return nil
	}
	return err
}

// Send writes a frame the caller built, at this connection's version. Use it for requests the
// client has no method for; answers with a token arrive as events. Take the token from Token, so
// it never collides with a request of the client's own.
func (h *Holder) Send(f hp.Frame) error { return h.pc.WriteFrame(f) }

// Token returns a fresh request token for a frame sent with Send. Answers to it arrive as
// events, in order with the holder's other events.
func (h *Holder) Token() uint32 { return h.nextToken() }
