package datastream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/noahkiss/flightdeck/api"
)

// Msg is one frame a client read, decoded.
type Msg struct {
	Kind api.FrameKind
	// Offset is the offset a snapshot covers, or the offset of a bytes frame's first byte.
	Offset uint64
	// Encoding names a snapshot's format.
	Encoding string
	// Data is a snapshot's screen bytes or a bytes frame's program output.
	Data   []byte
	Resize *api.ResizeFrame
	Exited *api.ExitedFrame
	// Cost is the credit the frame used, the length of Data; the client returns it once it
	// applied the frame.
	Cost int
}

// End returns the offset after the frame: what a client stores to resume.
func (m Msg) End() uint64 {
	if m.Kind == api.FrameBytes {
		return m.Offset + uint64(len(m.Data))
	}
	return m.Offset
}

// RejectError is a reject frame.
type RejectError struct{ api.DataReject }

func (e *RejectError) Error() string {
	return fmt.Sprintf("data connection refused (%s): %s", e.Kind, e.Hint)
}

// Client is the client side of one data connection.
type Client struct {
	rw  io.ReadWriteCloser
	br  *bufio.Reader
	wmu sync.Mutex
}

// Open sends the hello on rw and reads the welcome. A reject comes back as *RejectError.
func Open(rw io.ReadWriteCloser, hello api.DataHello) (*Client, api.DataWelcome, error) {
	c := &Client{rw: rw, br: bufio.NewReaderSize(rw, 64<<10)}
	p, _ := json.Marshal(hello)
	if err := c.write(api.FrameHello, p); err != nil {
		return nil, api.DataWelcome{}, err
	}
	f, err := ReadFrame(c.br)
	if err != nil {
		return nil, api.DataWelcome{}, err
	}
	switch f.Kind {
	case api.FrameWelcome:
		var w api.DataWelcome
		if err := json.Unmarshal(f.Payload, &w); err != nil {
			return nil, w, err
		}
		return c, w, nil
	case api.FrameReject:
		var r RejectError
		if err := json.Unmarshal(f.Payload, &r.DataReject); err != nil {
			return nil, api.DataWelcome{}, err
		}
		return nil, api.DataWelcome{}, &r
	}
	return nil, api.DataWelcome{}, fmt.Errorf("%w: kind %d before the welcome", ErrFrame, f.Kind)
}

func (c *Client) write(k api.FrameKind, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(AppendFrame(nil, k, p))
	return err
}

// Next reads the next frame. It returns io.EOF when the hab closed the stream.
func (c *Client) Next() (Msg, error) {
	f, err := ReadFrame(c.br)
	if err != nil {
		return Msg{}, err
	}
	m := Msg{Kind: f.Kind}
	switch f.Kind {
	case api.FrameSnapshot:
		m.Offset, m.Encoding, m.Data, err = ParseSnapshot(f.Payload)
		m.Cost = len(m.Data)
	case api.FrameBytes:
		m.Offset, m.Data, err = ParseBytes(f.Payload)
		m.Cost = len(m.Data)
	case api.FrameResize:
		m.Resize = &api.ResizeFrame{}
		err = json.Unmarshal(f.Payload, m.Resize)
	case api.FrameExited:
		m.Exited = &api.ExitedFrame{}
		err = json.Unmarshal(f.Payload, m.Exited)
	}
	return m, err
}

// Credit returns n bytes of credit to the hab.
func (c *Client) Credit(n int) error {
	p, _ := json.Marshal(api.CreditFrame{Bytes: uint64(n)})
	return c.write(api.FrameCredit, p)
}

// Close closes the connection.
func (c *Client) Close() error { return c.rw.Close() }
