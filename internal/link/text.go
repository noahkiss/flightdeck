package link

import (
	"github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/panetext"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// Text is a pane's screen and scrollback as plain text, read at one moment (hab-logic-api.md
// 3.11).
type Text struct {
	// Screen is one string per screen row, top first, without trailing spaces (ScreenRows).
	Screen []string
	// Scrollback is the scrollback rows from just above the screen, oldest first, with every
	// escape sequence and control character removed.
	Scrollback      []string
	ScrollbackTotal int
	Cols, Rows      int
	// CursorCol and CursorRow are the cursor's 0-based cell.
	CursorCol, CursorRow int
	// Alternate is true while the program shows the alternate screen.
	Alternate bool
	// Offset is the pane offset the text was read at.
	Offset uint64
}

// ReadText reads pane u's screen and up to scrollback rows of its scrollback (at most
// MaxScrollbackPage) as plain text, under the pane's parser lock, so the screen, the rows and
// the offset agree. It reads the parser that serves snapshots; it keeps no screen of its own.
func (l *Link) ReadText(u holderproto.UUID, scrollback int) (Text, error) {
	p := l.pane(u)
	if p == nil {
		return Text{}, ErrNoPane
	}
	var s vt.Screen
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.term == nil {
		return Text{}, ErrClosed
	}
	if err := p.term.ReadScreen(&s); err != nil {
		return Text{}, err
	}
	pg, err := p.term.Scrollback(0, min(max(scrollback, 0), MaxScrollbackPage))
	if err != nil {
		return Text{}, err
	}
	t := Text{Screen: ScreenRows(&s), ScrollbackTotal: pg.Total, Cols: s.Cols, Rows: s.Rows,
		CursorCol: s.CursorX, CursorRow: s.CursorY, Offset: p.off}
	for _, m := range []int{1049, 1047, 47} {
		if set, _ := p.term.Mode(m, false); set {
			t.Alternate = true
		}
	}
	if pg.Rows > 0 {
		t.Scrollback = panetext.Text(pg.VT)
	}
	return t, nil
}
