// Package vt defines the terminal parser that gff-hab and gff keep for each pane.
//
// A Terminal takes the pane's output bytes in order and keeps the screen, the scrollback and the
// modes. gff-hab reads a snapshot from it when a client opens a data stream (spec 5.5) and when it
// writes a checkpoint (spec 2.3). gff reads its cells to compose split panes (gff-client.md 3.4).
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
	// because libghostty's snapshot format has no bound. Terminal.Scrollback reads the older
	// lines a bounded snapshot leaves out.
	Scrollback int
}

// ScrollbackPage is a run of scrollback rows, oldest first. Rows are counted from the newest
// scrollback row (the one just above the top of the screen) back towards the oldest.
type ScrollbackPage struct {
	// VT holds the rows as VT bytes: each row's text with its colours and styles, rows joined by
	// CRLF, then an SGR reset. It holds no cursor move, mode or screen switch, so a client can
	// draw it into its own scrollback view.
	VT []byte
	// Rows is the number of rows in VT. It is less than asked for when the scrollback ends.
	Rows int
	// Total is the number of scrollback rows the terminal holds.
	Total int
}

// Terminal parses one pane's output. A Terminal is not safe for concurrent use.
type Terminal interface {
	// Write feeds pane output to the parser. It always consumes all of p.
	Write(p []byte) (int, error)
	// Resize changes the screen size.
	Resize(cols, rows int) error
	// Size returns the screen size.
	Size() (cols, rows int)
	// SetScrollbackBytes changes the scrollback cap. Lowering it may drop the oldest scrollback at
	// once. Zero or less leaves the cap as it is.
	SetScrollbackBytes(n int) error
	// Snapshot encodes the terminal state in enc. A snapshot never contains a terminal query
	// and never sets the palette (spec 5.5).
	Snapshot(enc Encoding, opts SnapshotOptions) ([]byte, error)
	// Scrollback returns older scrollback rows than a bounded snapshot carries; see
	// ScrollbackPage. skip is the number of the newest scrollback rows to pass over, and n the
	// most rows to return. On the alternate screen it reads the primary screen's scrollback.
	Scrollback(skip, n int) (ScrollbackPage, error)
	// Cursor returns the cursor's 0-based column and row on the active screen. gff-hab answers
	// cursor position queries from it (spec 5.6).
	Cursor() (col, row int)
	// Mode reports whether mode n is set: an ANSI mode when ansi, else a DEC private mode. known
	// is false for a mode the parser does not track.
	Mode(n int, ansi bool) (set, known bool)
	// KeyboardFlags returns the keyboard protocol flags the program set (CSI > u), 0 for none.
	KeyboardFlags() int
	// ScrollView moves the view delta rows into the primary screen's scrollback (negative:
	// older rows, up; positive: back down) and returns how many rows above the bottom the view
	// is now; 0 is the live screen. Output that arrives while the view is up leaves it on the
	// same rows. On the alternate screen it moves nothing and returns 0. ReadScreen reads the
	// view, with no cursor while it is up (gff-client.md 5.7).
	ScrollView(delta int) int
	// ReadScreen fills dst with the active screen's cells and the cursor, for a client that
	// draws the screen into part of its own (gff's composed view, gff-client.md 3.2). dst's
	// slices are reused when they are large enough.
	ReadScreen(dst *Screen) error
	// PlainText returns the active screen with its scrollback as text, one line per row, with
	// trailing spaces and trailing blank lines removed. Tests compare it with golden files.
	PlainText() (string, error)
	// Close frees the terminal. Close is safe to call more than once.
	Close() error
}

// ColorKind says how a Color is given.
type ColorKind uint8

const (
	ColorDefault ColorKind = iota // the terminal's default foreground or background
	ColorPalette                  // an index into the 256-colour palette
	ColorRGB                      // a true colour
)

// Color is a cell colour as the program set it. A palette colour stays an index, so a client
// draws it in its own terminal's theme.
type Color struct {
	Kind    ColorKind
	Index   uint8 // for ColorPalette
	R, G, B uint8 // for ColorRGB
}

// Underline styles, as SGR 4:n sets them.
const (
	UnderlineNone uint8 = iota
	UnderlineSingle
	UnderlineDouble
	UnderlineCurly
	UnderlineDotted
	UnderlineDashed
)

// Style is a cell's colours and attributes.
type Style struct {
	Fg, Bg, UnderlineColor Color
	Underline              uint8 // one of the Underline constants
	Bold, Faint, Italic    bool
	Blink, Inverse         bool
	Invisible, Strike      bool
	Overline               bool
}

// Cell is one screen cell.
type Cell struct {
	// Text is the cell's grapheme cluster as UTF-8; empty for a blank cell.
	Text string
	// Wide marks a character that takes this cell and the next. Spacer marks that next cell,
	// which a client skips when it draws.
	Wide, Spacer bool
	Style        Style
}

// Screen is the active screen as cells, row by row: the cell at column x of row y is
// Cells[y*Cols+x].
type Screen struct {
	Cols, Rows int
	Cells      []Cell
	// CursorX and CursorY are the cursor's 0-based cell; CursorVisible is false when the
	// program hid it (DECTCEM).
	CursorX, CursorY int
	CursorVisible    bool
}

// Reset sizes s for cols x rows and blanks every cell.
func (s *Screen) Reset(cols, rows int) {
	n := cols * rows
	if cap(s.Cells) < n {
		s.Cells = make([]Cell, n)
	}
	s.Cells = s.Cells[:n]
	clear(s.Cells)
	s.Cols, s.Rows = cols, rows
	s.CursorX, s.CursorY, s.CursorVisible = 0, 0, true
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
