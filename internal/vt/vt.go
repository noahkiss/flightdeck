// Package vt defines the terminal parser that gff-hab keeps for each pane.
//
// A Terminal takes the pane's output bytes in order and keeps the screen, the scrollback and the
// modes. gff-hab reads a snapshot from it when a client opens a data stream (spec 5.5) and when it
// writes a checkpoint (spec 2.3).
//
// This package is pure Go. The libghostty implementation lives in internal/vt/ghostty, the only
// package that needs cgo. Fake is a small implementation for tests.
package vt

import "errors"

// Encoding names a snapshot format. The values match the snapshot capability words in the API.
type Encoding string

const (
	// EncodingVT is plain VT bytes that redraw the screen in any terminal.
	EncodingVT Encoding = "snapshot-vt"
	// EncodingGhostty is libghostty's own snapshot format. It keeps more state than EncodingVT.
	EncodingGhostty Encoding = "snapshot-ghostty"
)

// ErrEncoding means the implementation cannot write or read the asked encoding.
var ErrEncoding = errors.New("vt: unsupported snapshot encoding")

// ErrClosed means the terminal was used after Close.
var ErrClosed = errors.New("vt: terminal closed")

// Config sets up a new terminal.
type Config struct {
	Cols, Rows int
	// ScrollbackBytes caps the scrollback. Zero keeps the implementation's default.
	ScrollbackBytes int
}

// AllScrollback asks a snapshot for the whole scrollback.
const AllScrollback = -1

// SnapshotOptions bounds a snapshot.
type SnapshotOptions struct {
	// Scrollback is the number of scrollback lines to include: AllScrollback for all of it, 0
	// for none. It bounds EncodingVT only: EncodingGhostty always carries the whole scrollback,
	// because libghostty's snapshot format has no bound.
	Scrollback int
}

// Terminal parses one pane's output. A Terminal is not safe for concurrent use.
type Terminal interface {
	// Write feeds pane output to the parser. It always consumes all of p.
	Write(p []byte) (int, error)
	// Resize changes the screen size.
	Resize(cols, rows int) error
	// Size returns the screen size.
	Size() (cols, rows int)
	// Snapshot encodes the terminal state in enc. A snapshot never contains a terminal query
	// and never sets the palette (spec 5.5).
	Snapshot(enc Encoding, opts SnapshotOptions) ([]byte, error)
	// PlainText returns the active screen with its scrollback as text, one line per row, with
	// trailing spaces and trailing blank lines removed. Tests compare it with golden files.
	PlainText() (string, error)
	// Close frees the terminal. Close is safe to call more than once.
	Close() error
}

// Parser makes terminals.
type Parser interface {
	// New returns an empty terminal.
	New(cfg Config) (Terminal, error)
	// Restore returns a terminal rebuilt from a snapshot. cfg gives the size for encodings that
	// do not carry one (EncodingVT) and the scrollback cap.
	Restore(enc Encoding, snapshot []byte, cfg Config) (Terminal, error)
	// Encodings lists the encodings that Snapshot and Restore support, baseline first.
	Encodings() []Encoding
}
