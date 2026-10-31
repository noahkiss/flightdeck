package vt

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Fake is a Parser for tests that must run without cgo. It keeps a plain grid of runes: it moves the
// cursor, wraps, scrolls into a scrollback, switches to the alternate screen, and counts wide
// characters as two cells. It ignores colours, styles, titles and most modes. Its only snapshot
// encoding is EncodingVT, which holds the text and the cursor position alone.
type Fake struct{}

var _ Parser = Fake{}

// New returns an empty fake terminal.
func (Fake) New(cfg Config) (Terminal, error) {
	if cfg.Cols <= 0 || cfg.Rows <= 0 {
		return nil, errSize
	}
	t := &fakeTerm{cols: cfg.Cols, rows: cfg.Rows}
	t.primary = newGrid(cfg.Cols, cfg.Rows)
	t.alt = newGrid(cfg.Cols, cfg.Rows)
	return t, nil
}

// Restore replays an EncodingVT snapshot into a new fake terminal.
func (f Fake) Restore(enc Encoding, snapshot []byte, cfg Config) (Terminal, error) {
	if enc != EncodingVT {
		return nil, ErrEncoding
	}
	t, err := f.New(cfg)
	if err != nil {
		return nil, err
	}
	_, _ = t.Write(snapshot)
	return t, nil
}

// Encodings returns EncodingVT only.
func (Fake) Encodings() []Encoding { return []Encoding{EncodingVT} }

type errString string

func (e errString) Error() string { return string(e) }

const errSize = errString("vt: cols and rows must be positive")

type parseState int

const (
	stGround parseState = iota
	stEsc
	stEscCharset // ESC ( and friends: one more byte names the charset
	stCSI
	stOSC
	stOSCEsc
)

type fakeTerm struct {
	cols, rows   int
	primary, alt [][]rune // 0 marks the second cell of a wide character; ' ' is blank
	scrollback   [][]rune
	altActive    bool
	x, y         int
	savedX       int
	savedY       int
	pendingWrap  bool
	state        parseState
	csi          []byte
	partial      []byte // an incomplete UTF-8 sequence carried to the next Write
	closed       bool
}

func newGrid(cols, rows int) [][]rune {
	g := make([][]rune, rows)
	for i := range g {
		g[i] = blankRow(cols)
	}
	return g
}

func blankRow(cols int) []rune {
	r := make([]rune, cols)
	for i := range r {
		r[i] = ' '
	}
	return r
}

func (t *fakeTerm) grid() [][]rune {
	if t.altActive {
		return t.alt
	}
	return t.primary
}

func (t *fakeTerm) Write(p []byte) (int, error) {
	if t.closed {
		return 0, ErrClosed
	}
	n := len(p)
	if len(t.partial) > 0 {
		p = append(t.partial, p...)
		t.partial = nil
	}
	for len(p) > 0 {
		b := p[0]
		if t.state != stGround || b < utf8.RuneSelf {
			t.byte(b)
			p = p[1:]
			continue
		}
		if !utf8.FullRune(p) {
			t.partial = append([]byte(nil), p...)
			break
		}
		r, size := utf8.DecodeRune(p)
		t.print(r)
		p = p[size:]
	}
	return n, nil
}

func (t *fakeTerm) byte(b byte) {
	switch t.state {
	case stEsc:
		t.state = stGround
		switch b {
		case '[':
			t.state, t.csi = stCSI, t.csi[:0]
		case ']':
			t.state = stOSC
		case '(', ')', '*', '+':
			t.state = stEscCharset
		case '7':
			t.savedX, t.savedY = t.x, t.y
		case '8':
			t.x, t.y, t.pendingWrap = t.savedX, t.savedY, false
		}
		return
	case stEscCharset:
		t.state = stGround
		return
	case stCSI:
		if b >= 0x40 && b <= 0x7e {
			t.state = stGround
			t.dispatchCSI(b)
			return
		}
		t.csi = append(t.csi, b)
		return
	case stOSC:
		switch b {
		case 0x07:
			t.state = stGround
		case 0x1b:
			t.state = stOSCEsc
		}
		return
	case stOSCEsc:
		t.state = stGround // ESC \ ends the string; any other byte is dropped with it
		return
	}
	switch b {
	case 0x1b:
		t.state = stEsc
	case '\r':
		t.x, t.pendingWrap = 0, false
	case '\n', 0x0b, 0x0c:
		t.lineFeed()
	case '\b':
		if t.x > 0 {
			t.x--
		}
		t.pendingWrap = false
	case '\t':
		t.x = min((t.x/8+1)*8, t.cols-1)
	default:
		if b >= 0x20 && b < 0x7f {
			t.print(rune(b))
		}
	}
}

func (t *fakeTerm) lineFeed() {
	t.pendingWrap = false
	if t.y < t.rows-1 {
		t.y++
		return
	}
	g := t.grid()
	if !t.altActive {
		t.scrollback = append(t.scrollback, g[0])
	}
	copy(g, g[1:])
	g[len(g)-1] = blankRow(t.cols)
}

func (t *fakeTerm) print(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return
	}
	if t.pendingWrap || t.x+w > t.cols {
		t.x = 0
		t.lineFeed()
	}
	row := t.grid()[t.y]
	row[t.x] = r
	if w == 2 {
		row[t.x+1] = 0
	}
	t.x += w
	if t.x >= t.cols {
		t.x, t.pendingWrap = t.cols-1, true
	}
}

func (t *fakeTerm) dispatchCSI(final byte) {
	params := string(t.csi)
	private := strings.HasPrefix(params, "?")
	params = strings.TrimPrefix(params, "?")
	nums := []int{}
	for _, f := range strings.Split(params, ";") {
		v, _ := strconv.Atoi(f)
		nums = append(nums, v)
	}
	arg := func(i, def int) int {
		if i < len(nums) && nums[i] > 0 {
			return nums[i]
		}
		return def
	}
	t.pendingWrap = false
	switch {
	case private && (final == 'h' || final == 'l'):
		for _, m := range nums {
			if m == 1049 || m == 1047 || m == 47 {
				t.setAlt(final == 'h', m == 1049)
			}
		}
	case private:
	case final == 'H' || final == 'f':
		t.y = min(arg(0, 1), t.rows) - 1
		t.x = min(arg(1, 1), t.cols) - 1
	case final == 'A':
		t.y = max(t.y-arg(0, 1), 0)
	case final == 'B':
		t.y = min(t.y+arg(0, 1), t.rows-1)
	case final == 'C':
		t.x = min(t.x+arg(0, 1), t.cols-1)
	case final == 'D':
		t.x = max(t.x-arg(0, 1), 0)
	case final == 'G':
		t.x = min(arg(0, 1), t.cols) - 1
	case final == 'J':
		t.eraseDisplay(arg(0, 0))
	case final == 'K':
		t.eraseLine(arg(0, 0))
	}
}

func (t *fakeTerm) setAlt(on, saveCursor bool) {
	if on == t.altActive {
		return
	}
	if on {
		if saveCursor {
			t.savedX, t.savedY = t.x, t.y
		}
		t.alt = newGrid(t.cols, t.rows)
		t.altActive = true
		return
	}
	t.altActive = false
	if saveCursor {
		t.x, t.y = t.savedX, t.savedY
	}
}

func (t *fakeTerm) eraseDisplay(mode int) {
	g := t.grid()
	switch mode {
	case 0:
		t.eraseLine(0)
		for y := t.y + 1; y < t.rows; y++ {
			g[y] = blankRow(t.cols)
		}
	case 1:
		t.eraseLine(1)
		for y := 0; y < t.y; y++ {
			g[y] = blankRow(t.cols)
		}
	case 2:
		for y := range g {
			g[y] = blankRow(t.cols)
		}
	case 3:
		t.scrollback = nil
	}
}

func (t *fakeTerm) eraseLine(mode int) {
	row := t.grid()[t.y]
	from, to := t.x, t.cols
	switch mode {
	case 1:
		from, to = 0, t.x+1
	case 2:
		from = 0
	}
	for x := from; x < to; x++ {
		row[x] = ' '
	}
}

func (t *fakeTerm) Resize(cols, rows int) error {
	if t.closed {
		return ErrClosed
	}
	if cols <= 0 || rows <= 0 {
		return errSize
	}
	fit := func(g [][]rune, keepScrollback bool) [][]rune {
		for len(g) > rows {
			if keepScrollback {
				t.scrollback = append(t.scrollback, g[0])
			}
			g = g[1:]
		}
		for len(g) < rows {
			g = append(g, blankRow(cols))
		}
		for i, row := range g {
			nr := blankRow(cols)
			copy(nr, row)
			if cols < len(row) && nr[cols-1] != 0 && runeWidth(nr[cols-1]) == 2 {
				nr[cols-1] = ' '
			}
			g[i] = nr
		}
		return g
	}
	shift := max(len(t.grid())-rows, 0)
	t.primary = fit(t.primary, true)
	t.alt = fit(t.alt, false)
	t.cols, t.rows = cols, rows
	t.y = min(max(t.y-shift, 0), rows-1)
	t.x = min(t.x, cols-1)
	t.pendingWrap = false
	return nil
}

func (t *fakeTerm) Size() (int, int) { return t.cols, t.rows }

func (t *fakeTerm) lines() []string {
	var rows [][]rune
	if !t.altActive {
		rows = append(rows, t.scrollback...)
	}
	return rowText(append(rows, t.grid()...))
}

// rowText turns grid rows into lines, with trailing spaces and trailing blank lines removed.
func rowText(rows [][]rune) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var b strings.Builder
		for _, r := range row {
			if r != 0 {
				b.WriteRune(r)
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (t *fakeTerm) PlainText() (string, error) {
	if t.closed {
		return "", ErrClosed
	}
	return strings.Join(t.lines(), "\n"), nil
}

func (t *fakeTerm) Snapshot(enc Encoding, opts SnapshotOptions) ([]byte, error) {
	if t.closed {
		return nil, ErrClosed
	}
	if enc != EncodingVT {
		return nil, ErrEncoding
	}
	sb := t.scrollback
	if opts.Scrollback >= 0 && opts.Scrollback < len(sb) {
		sb = sb[len(sb)-opts.Scrollback:]
	}
	// The primary screen goes first, so leaving the alternate screen after a restore shows it.
	var b strings.Builder
	b.WriteString(strings.Join(rowText(append(append([][]rune{}, sb...), t.primary...)), "\r\n"))
	if t.altActive {
		b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
		b.WriteString(strings.Join(rowText(t.alt), "\r\n"))
	}
	// The cursor goes last, so bytes written after a restore land where they would have.
	b.WriteString("\x1b[" + strconv.Itoa(t.y+1) + ";" + strconv.Itoa(t.x+1) + "H")
	return []byte(b.String()), nil
}

func (t *fakeTerm) Close() error {
	t.closed = true
	t.primary, t.alt, t.scrollback = nil, nil, nil
	return nil
}

// runeWidth returns the cells a rune takes: 0 for combining marks and joiners, 2 for East Asian
// wide characters and most emoji, 1 otherwise. It is a short table, good enough for test fixtures.
func runeWidth(r rune) int {
	switch {
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case (r >= 0x300 && r <= 0x36f) || r == 0x200d || (r >= 0xfe00 && r <= 0xfe0f):
		return 0
	case (r >= 0x1100 && r <= 0x115f) || (r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe30 && r <= 0xfe4f) || (r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f300 && r <= 0x1f64f) || (r >= 0x1f680 && r <= 0x1f6ff) ||
		(r >= 0x1f900 && r <= 0x1f9ff) || (r >= 0x20000 && r <= 0x3fffd):
		return 2
	}
	return 1
}
