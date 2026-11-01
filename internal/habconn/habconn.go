// Package habconn is the client side of one hab: the shared connection library that the gff
// terminal client uses, and that the app reaches through its own SSH (docs/specs/gff-client.md
// 11, 15.2).
//
// It owns exactly one hab connection: the control connection (hello, the seq and revision
// rules, liveness, reconnect with the token, a fresh snapshot on a new epoch) and one data
// stream per pane (ticket, snapshot, bytes, resume by holder offset, credit). Merging several
// hosts sits on top of it and is not here.
//
// It speaks the protocol over a byte-stream seam, Transport. A Transport opens one byte stream
// per connection: one for the control connection and one per data stream, each starting with
// its own hello (docs/specs/hab-logic-api.md 4.1, 4.2 option A). The package ships a Unix socket
// transport, a command transport that speaks over a command's stdio, and an SSH transport that
// runs the system ssh with `gff-hab connect` on the far side.
package habconn

import (
	"context"
	"errors"
	"io"
)

// Kind says what a stream will carry. The hab tells them apart by the first byte, so a
// transport may ignore it; it is there for transports that name or limit their channels.
type Kind int

const (
	KindControl Kind = iota + 1 // the control connection: JSON-RPC lines
	KindData                    // one data stream: frames
)

func (k Kind) String() string {
	switch k {
	case KindControl:
		return "control"
	case KindData:
		return "data"
	}
	return "unknown"
}

// Transport opens byte streams to one hab. Each Dial returns a new stream that reaches the
// hab's control socket, as `gff-hab connect` does. ctx bounds the dial only, never the life of
// the stream. Close on the stream must unblock a pending Read.
type Transport interface {
	Dial(ctx context.Context, kind Kind) (io.ReadWriteCloser, error)
}

// Errors the package returns.
var (
	// ErrClosed: the Conn or PaneStream was closed by its owner.
	ErrClosed = errors.New("habconn: closed")
	// ErrOffline: no control connection is up. Calls are refused, never queued: a delayed
	// keystroke could approve something the owner no longer wants (gff-client.md 11.6).
	ErrOffline = errors.New("habconn: hab is offline")
	// ErrPaneGone: the hab says the pane does not exist (reject not_found).
	ErrPaneGone = errors.New("habconn: pane is gone")
)

// DeviceChangedError: the hab presented a device id other than the pinned one. The Conn stops;
// only the owner may accept the new id (transport.md 4.1, gff-client.md 11.4).
type DeviceChangedError struct {
	Pinned, Got string
}

func (e *DeviceChangedError) Error() string {
	return "habconn: the hab's device id changed from " + e.Pinned + " to " + e.Got +
		"; accept the new id on purpose to connect"
}

// stderrer is a stream that keeps the tail of its command's standard error, such as ssh's
// own messages. Errors from a failed hello carry it.
type stderrer interface {
	Stderr() string
}
