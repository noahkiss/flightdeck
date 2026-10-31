package holderproto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrShortPayload means a payload ended before a field it must carry at its version.
var ErrShortPayload = errors.New("holderproto: payload too short")

// Enc appends fields to a payload in the protocol's byte layout. All integers are big-endian.
// Frame types use it in MarshalPayload; it is exported so that a test variant of a frame, or a
// later version's frame defined outside this file, encodes the same way.
type Enc struct{ b []byte }

// Bytes returns the payload written so far.
func (e *Enc) Bytes() []byte { return e.b }

// U8 appends one byte.
func (e *Enc) U8(v uint8) { e.b = append(e.b, v) }

// U16 appends a big-endian uint16.
func (e *Enc) U16(v uint16) { e.b = binary.BigEndian.AppendUint16(e.b, v) }

// U32 appends a big-endian uint32.
func (e *Enc) U32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }

// U64 appends a big-endian uint64.
func (e *Enc) U64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }

// I32 appends a big-endian two's complement int32.
func (e *Enc) I32(v int32) { e.U32(uint32(v)) }

// Bool appends 1 for true and 0 for false.
func (e *Enc) Bool(v bool) {
	if v {
		e.U8(1)
	} else {
		e.U8(0)
	}
}

// Raw appends bytes with no length prefix. Only the last field of a payload may be raw.
func (e *Enc) Raw(p []byte) { e.b = append(e.b, p...) }

// Blob appends a uint32 length, then the bytes.
func (e *Enc) Blob(p []byte) {
	e.U32(uint32(len(p)))
	e.b = append(e.b, p...)
}

// String appends a uint32 length, then the UTF-8 bytes.
func (e *Enc) String(s string) {
	e.U32(uint32(len(s)))
	e.b = append(e.b, s...)
}

// Strings appends a uint32 count, then each string as String does.
func (e *Enc) Strings(list []string) {
	e.U32(uint32(len(list)))
	for _, s := range list {
		e.String(s)
	}
}

// UUID appends 16 bytes.
func (e *Enc) UUID(u UUID) { e.b = append(e.b, u[:]...) }

// Dec reads fields from a payload. The first failure sticks: later reads return zero values, and
// Err reports the failure. Bytes left after the last known field are ignored; that is how an
// older receiver ignores fields a newer sender appended.
type Dec struct {
	b   []byte
	err error
}

// NewDec returns a decoder over p.
func NewDec(p []byte) *Dec { return &Dec{b: p} }

// Err returns the first failure, or nil.
func (d *Dec) Err() error { return d.err }

// Len returns the number of unread bytes.
func (d *Dec) Len() int { return len(d.b) }

func (d *Dec) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || len(d.b) < n {
		d.err = ErrShortPayload
		d.b = nil
		return nil
	}
	p := d.b[:n]
	d.b = d.b[n:]
	return p
}

// U8 reads one byte.
func (d *Dec) U8() uint8 {
	p := d.take(1)
	if p == nil {
		return 0
	}
	return p[0]
}

// U16 reads a big-endian uint16.
func (d *Dec) U16() uint16 {
	p := d.take(2)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint16(p)
}

// U32 reads a big-endian uint32.
func (d *Dec) U32() uint32 {
	p := d.take(4)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint32(p)
}

// U64 reads a big-endian uint64.
func (d *Dec) U64() uint64 {
	p := d.take(8)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint64(p)
}

// I32 reads a big-endian two's complement int32.
func (d *Dec) I32() int32 { return int32(d.U32()) }

// Bool reads one byte; any value other than 0 is true.
func (d *Dec) Bool() bool { return d.U8() != 0 }

// Rest returns every unread byte as a copy and leaves the decoder empty.
func (d *Dec) Rest() []byte {
	if d.err != nil {
		return nil
	}
	p := append([]byte{}, d.b...)
	d.b = nil
	return p
}

// Blob reads a uint32 length, then that many bytes, as a copy.
func (d *Dec) Blob() []byte {
	n := d.U32()
	p := d.take(int(n))
	if p == nil {
		return nil
	}
	return append([]byte{}, p...)
}

// String reads a uint32 length, then that many bytes.
func (d *Dec) String() string {
	n := d.U32()
	return string(d.take(int(n)))
}

// Strings reads a uint32 count, then that many strings. The count is checked against the bytes
// left, so a corrupt count cannot force a huge allocation.
func (d *Dec) Strings() []string {
	n := d.U32()
	if d.err != nil {
		return nil
	}
	if uint64(n)*4 > uint64(len(d.b)) {
		d.err = ErrShortPayload
		return nil
	}
	list := make([]string, 0, n)
	for i := uint32(0); i < n && d.err == nil; i++ {
		list = append(list, d.String())
	}
	if d.err != nil {
		return nil
	}
	return list
}

// UUID reads 16 bytes.
func (d *Dec) UUID() UUID {
	var u UUID
	copy(u[:], d.take(16))
	return u
}

// UUID is a pane uuid in its 16-byte binary form.
type UUID [16]byte

// String formats the uuid in the usual 8-4-4-4-12 hex form.
func (u UUID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
