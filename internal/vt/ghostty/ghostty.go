//go:build ghostty

// Package ghostty implements vt.Parser with libghostty-vt through cgo.
//
// It is the only package in the module that needs cgo, and it builds only with the ghostty build
// tag. Run `make libghostty` first; the Makefile sets PKG_CONFIG_PATH so cgo finds the library.
package ghostty

import (
	"fmt"

	"github.com/noahkiss/flightdeck/internal/vt"
	"go.mitchellh.com/libghostty"
)

// Parser makes libghostty terminals.
type Parser struct{}

var _ vt.Parser = Parser{}

// New returns an empty terminal.
func (Parser) New(cfg vt.Config) (vt.Terminal, error) {
	if err := checkSize(cfg.Cols, cfg.Rows); err != nil {
		return nil, err
	}
	opts := []libghostty.TerminalOption{libghostty.WithSize(uint16(cfg.Cols), uint16(cfg.Rows))}
	if cfg.ScrollbackBytes > 0 {
		opts = append(opts, libghostty.WithMaxScrollbackBytes(uint(cfg.ScrollbackBytes)))
	}
	t, err := libghostty.NewTerminal(opts...)
	if err != nil {
		return nil, fmt.Errorf("ghostty: new terminal: %w", err)
	}
	return &terminal{t: t}, nil
}

// Restore rebuilds a terminal from a snapshot. An EncodingGhostty snapshot carries its own size
// and modes; an EncodingVT snapshot is replayed into a new terminal of cfg's size.
func (p Parser) Restore(enc vt.Encoding, snapshot []byte, cfg vt.Config) (vt.Terminal, error) {
	switch enc {
	case vt.EncodingVT:
		t, err := p.New(cfg)
		if err != nil {
			return nil, err
		}
		_, _ = t.Write(snapshot)
		return t, nil
	case vt.EncodingGhostty:
		d, err := libghostty.NewSnapshotDecoderBytesCopy(snapshot)
		if err != nil {
			return nil, fmt.Errorf("ghostty: snapshot decoder: %w", err)
		}
		defer d.Close()
		t, err := d.Decode()
		if err != nil {
			return nil, fmt.Errorf("ghostty: decode snapshot: %w", err)
		}
		if cfg.ScrollbackBytes > 0 {
			limit := uint(cfg.ScrollbackBytes)
			if err := t.SetScrollbackMaxBytes(&limit); err != nil {
				t.Close()
				return nil, fmt.Errorf("ghostty: scrollback limit: %w", err)
			}
		}
		return &terminal{t: t}, nil
	}
	return nil, vt.ErrEncoding
}

// Encodings returns EncodingVT, the baseline, and EncodingGhostty.
func (Parser) Encodings() []vt.Encoding {
	return []vt.Encoding{vt.EncodingVT, vt.EncodingGhostty}
}

func checkSize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 0xffff || rows > 0xffff {
		return fmt.Errorf("ghostty: bad size %dx%d", cols, rows)
	}
	return nil
}

type terminal struct {
	t *libghostty.Terminal
}

func (t *terminal) Write(p []byte) (int, error) {
	if t.t == nil {
		return 0, vt.ErrClosed
	}
	t.t.VTWrite(p)
	return len(p), nil
}

func (t *terminal) Resize(cols, rows int) error {
	if t.t == nil {
		return vt.ErrClosed
	}
	if err := checkSize(cols, rows); err != nil {
		return err
	}
	return t.t.Resize(uint16(cols), uint16(rows), 0, 0)
}

func (t *terminal) Size() (int, int) {
	if t.t == nil {
		return 0, 0
	}
	c, _ := t.t.Cols()
	r, _ := t.t.Rows()
	return int(c), int(r)
}

func (t *terminal) Snapshot(enc vt.Encoding, opts vt.SnapshotOptions) ([]byte, error) {
	if t.t == nil {
		return nil, vt.ErrClosed
	}
	switch enc {
	case vt.EncodingVT:
		return t.snapshotVT(opts.Scrollback)
	case vt.EncodingGhostty:
		return t.t.Snapshot()
	}
	return nil, vt.ErrEncoding
}

// snapshotVT formats the terminal as VT bytes. The formatter writes only the active screen, so
// on the alternate screen it formats a copy that has left the alternate screen first: the
// snapshot then carries the primary screen, then switches to the alternate screen and draws it.
func (t *terminal) snapshotVT(scrollback int) ([]byte, error) {
	screen, err := t.t.ActiveScreen()
	if err != nil {
		return nil, fmt.Errorf("ghostty: active screen: %w", err)
	}
	active, err := t.formatVT(scrollback)
	if err != nil || screen != libghostty.ScreenAlternate {
		return active, err
	}
	raw, err := t.t.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("ghostty: copy for primary screen: %w", err)
	}
	cols, rows := t.Size()
	cp, err := Parser{}.Restore(vt.EncodingGhostty, raw, vt.Config{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	defer cp.Close()
	cp.Write([]byte("\x1b[?1049l"))
	primary, err := cp.(*terminal).formatVT(scrollback)
	if err != nil {
		return nil, err
	}
	out := append(primary, "\x1b[?1049h\x1b[H\x1b[2J"...)
	return append(out, active...), nil
}

func (t *terminal) formatVT(scrollback int) ([]byte, error) {
	sel, err := t.scrollbackSelection(scrollback)
	if err != nil {
		return nil, err
	}
	// No palette and no working directory: on a pass-through client those would recolour the
	// user's own terminal and rename its tab.
	return t.format(libghostty.FormatterFormatVT,
		libghostty.WithFormatterSelection(sel),
		libghostty.WithFormatterTrim(false),
		libghostty.WithFormatterExtraModes(true),
		libghostty.WithFormatterExtraScrollingRegion(true),
		libghostty.WithFormatterExtraTabstops(true),
		libghostty.WithFormatterExtraKeyboard(true),
		libghostty.WithFormatterExtraCursor(true),
		libghostty.WithFormatterExtraStyle(true),
		libghostty.WithFormatterExtraHyperlink(true),
		libghostty.WithFormatterExtraProtection(true),
		libghostty.WithFormatterExtraKittyKeyboard(true),
		libghostty.WithFormatterExtraCharsets(true),
	)
}

// scrollbackSelection returns a selection that keeps the last n scrollback lines and the whole
// screen, or nil to keep everything.
func (t *terminal) scrollbackSelection(n int) (*libghostty.Selection, error) {
	if n < 0 {
		return nil, nil
	}
	sb, err := t.t.ScrollbackRows()
	if err != nil {
		return nil, err
	}
	if uint(n) >= sb {
		return nil, nil
	}
	cols, rows := t.Size()
	start, err := t.t.GridRef(libghostty.Point{Tag: libghostty.PointTagScreen, X: 0, Y: uint32(sb - uint(n))})
	if err != nil {
		return nil, fmt.Errorf("ghostty: scrollback start: %w", err)
	}
	end, err := t.t.GridRef(libghostty.Point{Tag: libghostty.PointTagScreen, X: uint16(cols - 1), Y: uint32(sb) + uint32(rows) - 1})
	if err != nil {
		return nil, fmt.Errorf("ghostty: scrollback end: %w", err)
	}
	return &libghostty.Selection{Start: *start, End: *end}, nil
}

func (t *terminal) PlainText() (string, error) {
	if t.t == nil {
		return "", vt.ErrClosed
	}
	b, err := t.format(libghostty.FormatterFormatPlain, libghostty.WithFormatterTrim(true))
	return string(b), err
}

func (t *terminal) format(f libghostty.FormatterFormat, opts ...libghostty.FormatterOption) ([]byte, error) {
	fm, err := libghostty.NewFormatter(t.t, append([]libghostty.FormatterOption{libghostty.WithFormatterFormat(f)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("ghostty: formatter: %w", err)
	}
	defer fm.Close()
	return fm.Format()
}

// MemoryUsage reports the resident bytes the terminal's pages hold, for measurement.
func (t *terminal) MemoryUsage() (resident, virtual uint64, err error) {
	u, err := t.t.MemoryUsage()
	if err != nil {
		return 0, 0, err
	}
	tot := u.Total()
	return uint64(tot.ResidentBytes), uint64(tot.VirtualBytes), nil
}

func (t *terminal) Close() error {
	if t.t != nil {
		t.t.Close()
		t.t = nil
	}
	return nil
}
