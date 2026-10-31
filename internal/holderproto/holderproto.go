// Package holderproto is the wire protocol between gff-hab (the logic process) and
// gff-hab-holder (the process that owns the PTYs).
//
// A connection carries frames. Each frame is a 10-byte header (kind, pane handle, payload length)
// and a payload. Payloads are compact big-endian binary. The protocol version is negotiated once
// per connection, and a Conn keeps the version it negotiated, so one gff-hab can hold connections
// to several holders at different versions at once (a draining old holder beside the current one).
//
// docs/protocol/holder.md describes every frame byte by byte. This package is pure Go: the holder
// must build with CGO_ENABLED=0.
package holderproto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
)

// Version is a protocol version. It is separate from a build id.
type Version uint16

// Version1 is the first protocol version.
const Version1 Version = 1

// Kind identifies a frame type.
type Kind uint16

// Handle is the small number a holder gives a pane. Zero means "no pane": the frame concerns the
// connection, or the pane has no handle yet (spawn).
type Handle uint32

// HeaderSize is the length of a frame header in bytes.
const HeaderSize = 10

// MaxPayload is the largest payload a receiver accepts. A larger length is a protocol violation
// and ends the connection, because the receiver cannot tell a corrupt length from a real one.
const MaxPayload = 16 << 20

// ErrTooLarge means a frame header announced a payload over MaxPayload.
var ErrTooLarge = errors.New("holderproto: payload over the size limit")

// ErrNotInVersion means a frame kind is not part of the connection's negotiated version, or the
// version is not negotiated yet. The frame was not sent.
var ErrNotInVersion = errors.New("holderproto: frame kind not in the negotiated version")

// Frame is one protocol message.
type Frame interface {
	// Kind returns the frame's kind.
	Kind() Kind
	// Pane returns the pane handle for the header, or 0.
	Pane() Handle
	// MarshalPayload returns the payload as version v lays it out.
	MarshalPayload(v Version) []byte
}

// DecodeFunc builds a frame of one kind from its header pane handle and payload, as version v
// lays it out. It must ignore bytes after the last field it knows.
type DecodeFunc func(v Version, pane Handle, payload []byte) (Frame, error)

type entry struct {
	since  Version
	decode DecodeFunc
}

// Registry is the set of frame kinds and the version range one side speaks. V1 returns the real
// protocol. Tests build a later version by changing a copy.
type Registry struct {
	Min, Max Version
	kinds    map[Kind]entry
}

// NewRegistry returns an empty registry for the range min..max.
func NewRegistry(min, max Version) *Registry {
	return &Registry{Min: min, Max: max, kinds: map[Kind]entry{}}
}

// Register adds or replaces a frame kind. since is the first version that carries the kind; 0
// means the kind is part of the handshake and is legal before negotiation.
func (r *Registry) Register(k Kind, since Version, dec DecodeFunc) {
	r.kinds[k] = entry{since: since, decode: dec}
}

// Clone returns an independent copy.
func (r *Registry) Clone() *Registry {
	c := NewRegistry(r.Min, r.Max)
	for k, e := range r.kinds {
		c.kinds[k] = e
	}
	return c
}

// Kinds returns every registered kind in ascending order.
func (r *Registry) Kinds() []Kind {
	ks := make([]Kind, 0, len(r.kinds))
	for k := range r.kinds {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// Since returns the first version that carries kind k, and whether k is registered.
func (r *Registry) Since(k Kind) (Version, bool) {
	e, ok := r.kinds[k]
	return e.since, ok
}

// Range returns the registry's version range.
func (r *Registry) Range() Range { return Range{r.Min, r.Max} }

// AppendFrame appends the header and payload of f, encoded at version v, to b.
func AppendFrame(b []byte, f Frame, v Version) ([]byte, error) {
	p := f.MarshalPayload(v)
	if len(p) > MaxPayload {
		return b, ErrTooLarge
	}
	b = binary.BigEndian.AppendUint16(b, uint16(f.Kind()))
	b = binary.BigEndian.AppendUint32(b, uint32(f.Pane()))
	b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
	return append(b, p...), nil
}

// DecodeFrame decodes the first frame in b at version v and returns it with the number of bytes
// it used. A frame whose kind is unknown, or not part of version v, comes back as a nil Frame and
// a nil error with its length, so the caller can skip it. Fewer bytes than a whole frame give
// io.ErrUnexpectedEOF.
func (r *Registry) DecodeFrame(v Version, b []byte) (Frame, int, error) {
	if len(b) < HeaderSize {
		return nil, 0, io.ErrUnexpectedEOF
	}
	kind := Kind(binary.BigEndian.Uint16(b[0:2]))
	pane := Handle(binary.BigEndian.Uint32(b[2:6]))
	n := binary.BigEndian.Uint32(b[6:10])
	if n > MaxPayload {
		return nil, 0, ErrTooLarge
	}
	end := HeaderSize + int(n)
	if len(b) < end {
		return nil, 0, io.ErrUnexpectedEOF
	}
	e, ok := r.kinds[kind]
	if !ok || e.since > v {
		return nil, end, nil
	}
	p := append([]byte{}, b[HeaderSize:end]...)
	f, err := e.decode(v, pane, p)
	if err != nil {
		return nil, 0, fmt.Errorf("holderproto: kind %#04x: %w", uint16(kind), err)
	}
	return f, end, nil
}

// Conn reads and writes frames on one connection. WriteFrame is safe for concurrent use;
// ReadFrame must be called from one goroutine at a time.
type Conn struct {
	reg     *Registry
	r       *bufio.Reader
	w       io.Writer
	wmu     sync.Mutex
	version atomic.Uint32
	skipped atomic.Uint64
	hdr     [HeaderSize]byte
}

// NewConn wraps rw. The connection starts with no version: only handshake frames pass until
// Hello or AcceptHello negotiates one.
func NewConn(rw io.ReadWriter, reg *Registry) *Conn {
	return &Conn{reg: reg, r: bufio.NewReaderSize(rw, 64<<10), w: rw}
}

// Version returns the negotiated version, or 0 before negotiation.
func (c *Conn) Version() Version { return Version(c.version.Load()) }

// Registry returns the registry the connection decodes with.
func (c *Conn) Registry() *Registry { return c.reg }

// Skipped returns how many frames ReadFrame skipped because their kind was unknown or newer than
// the negotiated version.
func (c *Conn) Skipped() uint64 { return c.skipped.Load() }

func (c *Conn) setVersion(v Version) { c.version.Store(uint32(v)) }

// legal reports whether kind k may cross this connection now.
func (c *Conn) legal(k Kind) (entry, bool) {
	e, ok := c.reg.kinds[k]
	if !ok {
		return e, false
	}
	return e, e.since <= c.Version()
}

// WriteFrame encodes f at the connection's version and writes it in one Write call.
func (c *Conn) WriteFrame(f Frame) error {
	if _, ok := c.legal(f.Kind()); !ok {
		return fmt.Errorf("%w: kind %#04x at version %d", ErrNotInVersion, uint16(f.Kind()), c.Version())
	}
	b, err := AppendFrame(make([]byte, 0, HeaderSize+64), f, c.Version())
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// ReadFrame returns the next frame. It skips, by length, every frame whose kind is unknown or not
// part of the negotiated version. A malformed payload of a known kind is an error, and the caller
// should drop the connection.
func (c *Conn) ReadFrame() (Frame, error) {
	for {
		if _, err := io.ReadFull(c.r, c.hdr[:]); err != nil {
			return nil, err
		}
		kind := Kind(binary.BigEndian.Uint16(c.hdr[0:2]))
		pane := Handle(binary.BigEndian.Uint32(c.hdr[2:6]))
		n := binary.BigEndian.Uint32(c.hdr[6:10])
		if n > MaxPayload {
			return nil, ErrTooLarge
		}
		e, ok := c.legal(kind)
		if !ok {
			if _, err := c.r.Discard(int(n)); err != nil {
				return nil, noEOF(err)
			}
			c.skipped.Add(1)
			continue
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.r, p); err != nil {
			return nil, noEOF(err)
		}
		f, err := e.decode(c.Version(), pane, p)
		if err != nil {
			return nil, fmt.Errorf("holderproto: kind %#04x: %w", uint16(kind), err)
		}
		return f, nil
	}
}

// noEOF turns an end of stream inside a frame into io.ErrUnexpectedEOF.
func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
