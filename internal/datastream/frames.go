// Package datastream serves gff-hab's data connections (docs/specs/hab-logic-api.md 5): one
// connection per visible pane per client, carrying a snapshot, then the program's output, each
// framed with the holder's absolute offsets.
//
//   - A client gets a single-use ticket from data.ticket on its control connection, then opens a
//     data connection to the same socket and sends a hello frame with the ticket and, to
//     resume, the offset it holds.
//   - gff-hab answers welcome or reject, then sends a snapshot frame (unless the resume needs
//     none), bytes frames, resize frames and, when the program ends, an exited frame.
//   - The client returns credit as it applies bytes. A stream never has more uncredited bytes
//     in flight than its credit, and a client too far behind gets a fresh snapshot instead of
//     the backlog (5.7).
//   - Terminal queries are answered by the holder link and left out of every bytes frame (5.6).
//     Each bytes frame covers one contiguous run of holder offsets, so a removed query shows as
//     a skip between two frames, never inside one.
//
// The package also holds a client, which the tests and the terminal client use.
package datastream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/noahkiss/flightdeck/api"
)

// MaxPayload bounds one frame's payload. A snapshot-ghostty of a full 10 MiB scrollback is about
// 4.6 MiB (spec 2.3).
const MaxPayload = 64 << 20

// maxBytesFrame is the most program output in one bytes frame.
const maxBytesFrame = 64 << 10

// ErrFrame means a frame broke the framing rules.
var ErrFrame = errors.New("datastream: malformed frame")

// Frame is one frame on a data connection: a kind byte, a 4-byte big-endian payload length, and
// the payload (spec 5.3).
type Frame struct {
	Kind    api.FrameKind
	Payload []byte
}

// AppendFrame appends a frame to b.
func AppendFrame(b []byte, k api.FrameKind, payload []byte) []byte {
	b = append(b, byte(k))
	b = binary.BigEndian.AppendUint32(b, uint32(len(payload)))
	return append(b, payload...)
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxPayload {
		return Frame{}, fmt.Errorf("%w: payload of %d bytes", ErrFrame, n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Kind: api.FrameKind(h[0]), Payload: p}, nil
}

// SnapshotPayload builds a snapshot frame's payload: the 8-byte big-endian offset it covers, a
// 1-byte encoding name length, the encoding name, then the screen bytes.
func SnapshotPayload(off uint64, enc string, screen []byte) []byte {
	b := make([]byte, 0, 9+len(enc)+len(screen))
	b = binary.BigEndian.AppendUint64(b, off)
	b = append(b, byte(len(enc)))
	b = append(b, enc...)
	return append(b, screen...)
}

// ParseSnapshot reads a snapshot frame's payload.
func ParseSnapshot(p []byte) (off uint64, enc string, screen []byte, err error) {
	if len(p) < 9 || len(p) < 9+int(p[8]) {
		return 0, "", nil, fmt.Errorf("%w: short snapshot", ErrFrame)
	}
	n := int(p[8])
	return binary.BigEndian.Uint64(p), string(p[9 : 9+n]), p[9+n:], nil
}

// BytesPayload builds a bytes frame's payload: the 8-byte big-endian offset of the first byte,
// then the program output.
func BytesPayload(off uint64, data []byte) []byte {
	b := make([]byte, 0, 8+len(data))
	b = binary.BigEndian.AppendUint64(b, off)
	return append(b, data...)
}

// ParseBytes reads a bytes frame's payload.
func ParseBytes(p []byte) (off uint64, data []byte, err error) {
	if len(p) < 8 {
		return 0, nil, fmt.Errorf("%w: short bytes frame", ErrFrame)
	}
	return binary.BigEndian.Uint64(p), p[8:], nil
}
