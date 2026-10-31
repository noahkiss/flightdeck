// Package termq finds terminal queries in a pane's output and answers them
// (docs/specs/hab-logic-api.md 5.6).
//
// gff-hab answers each query once, from the pane's parser, and removes it from the bytes it sends
// to clients, so no client answers it a second time. Two parts of gff-hab use this package:
//
//   - The holder link runs one Scanner per pane over the whole output stream and answers the
//     queries in live bytes.
//   - Each data stream runs its own Scanner and leaves the queries out of its bytes frames.
//
// Both must agree on what a query is, from any point a stream may start. They do, because every
// query starts with ESC, and an ESC ends any escape sequence or control string a terminal parser
// is in. So a Scanner that holds nothing back is in the same state as a new one, and a stream
// may restart at any offset where its Scanner held nothing back (a frame boundary).
//
// The Scanner is deliberately small. C1 controls as single bytes are not recognised: in a UTF-8
// stream those bytes are continuation bytes.
package termq

import (
	"bytes"
	"fmt"
	"strconv"
)

// Kind names a query this package answers.
type Kind int

const (
	DSR        Kind = iota + 1 // CSI 5 n: device status
	CPR                        // CSI 6 n: cursor position
	DECXCPR                    // CSI ? 6 n: extended cursor position
	DA1                        // CSI c, CSI 0 c: primary device attributes
	DA2                        // CSI > c: secondary device attributes
	DA3                        // CSI = c: tertiary device attributes
	XTVERSION                  // CSI > q: terminal name and version
	DECRQM                     // CSI ? Ps $ p, CSI Ps $ p: mode query
	WinSize                    // CSI 18 t: text area size in cells
	KittyKeys                  // CSI ? u: keyboard protocol flags
	OSCColor                   // OSC 10, 11 or 12 with ?: foreground, background, cursor colour
	OSCPalette                 // OSC 4 with ?: palette entries
	DECRQSS                    // DCS $ q ... ST: setting query
	XTGETTCAP                  // DCS + q ... ST: termcap query
)

// Query is one query found in the output.
type Query struct {
	Kind Kind
	// Mode and ANSI name the mode of a DECRQM. ANSI is false for a DEC private mode.
	Mode int
	ANSI bool
	// Colors lists the OSC numbers asked (10, 11, 12) for OSCColor, or palette indexes for
	// OSCPalette.
	Colors []int
	// ST is true when an OSC ended with ESC \ rather than BEL; the answer ends the same way.
	ST bool
}

// Segment is a run of output: text to pass on, or one query.
type Segment struct {
	// Off is the absolute offset of the first byte.
	Off uint64
	// Data is the bytes. For text it may alias the slice given to Feed.
	Data []byte
	// Query is set when the segment is a query.
	Query *Query
}

// Bounds on a held-back candidate. A longer one is passed on as text.
const (
	maxCSI    = 64
	maxString = 512
)

type state uint8

const (
	stGround  state = iota
	stEsc           // after ESC
	stCSI           // inside CSI
	stOSC           // inside an OSC that may be a query
	stOSCEsc        // ESC inside such an OSC
	stDCS           // after ESC P, before the query is known
	stDCSBody       // inside a DCS query
	stDCSEsc        // ESC inside it
)

// Scanner splits an output stream into text and queries. It holds back the bytes of a sequence
// that may still turn out to be a query; they come out with a later Feed.
type Scanner struct {
	off  uint64 // offset after the last byte fed
	st   state
	pend []byte // held bytes, from an ESC
}

// NewScanner returns a scanner for a stream whose next byte is at offset off.
func NewScanner(off uint64) *Scanner { return &Scanner{off: off} }

// Reset drops held bytes and restarts at offset off, as after a gap or a snapshot.
func (s *Scanner) Reset(off uint64) { *s = Scanner{off: off} }

// Offset returns the offset after the last byte fed.
func (s *Scanner) Offset() uint64 { return s.off }

// Held returns the number of bytes held back.
func (s *Scanner) Held() int { return len(s.pend) }

// Feed scans p, which starts at Offset, and calls emit with each complete segment in order.
// A text run may come in several segments.
func (s *Scanner) Feed(p []byte, emit func(Segment)) {
	textStart := -1 // start in p of the text run not yet emitted
	flushText := func(end int) {
		if textStart >= 0 && end > textStart {
			emit(Segment{Off: s.off + uint64(textStart), Data: p[textStart:end]})
		}
		textStart = -1
	}
	// pendText passes on the held bytes as text. They precede p[i], so any text run in p
	// before i has already been emitted.
	pendText := func(i int) {
		if len(s.pend) > 0 {
			d := append([]byte(nil), s.pend...)
			emit(Segment{Off: s.off + uint64(i) - uint64(len(d)), Data: d})
		}
		s.pend = s.pend[:0]
		s.st = stGround
	}
	for i := 0; i < len(p); i++ {
		b := p[i]
		if s.st == stGround {
			if b == 0x1b {
				flushText(i)
				s.pend = append(s.pend[:0], b)
				s.st = stEsc
				continue
			}
			if textStart < 0 {
				textStart = i
			}
			continue
		}
		// After ESC inside a string, anything but a backslash means the string ended at the ESC,
		// and the ESC starts a new sequence.
		if (s.st == stOSCEsc || s.st == stDCSEsc) && b != '\\' {
			emit(Segment{Off: s.off + uint64(i) - uint64(len(s.pend)), Data: append([]byte(nil), s.pend[:len(s.pend)-1]...)})
			s.pend = append(s.pend[:0], 0x1b)
			s.st = stEsc
		}
		// Inside a candidate. ESC starts a new one, except as the start of ST in a string.
		if b == 0x1b && s.st != stOSC && s.st != stDCSBody {
			pendText(i)
			s.pend = append(s.pend, b)
			s.st = stEsc
			continue
		}
		if b == 0x18 || b == 0x1a { // CAN and SUB abort a sequence; they are text
			pendText(i)
			textStart = i
			continue
		}
		s.pend = append(s.pend, b)
		q, done, isText := s.step(b)
		switch {
		case q != nil:
			d := append([]byte(nil), s.pend...)
			emit(Segment{Off: s.off + uint64(i+1) - uint64(len(d)), Data: d, Query: q})
			s.pend = s.pend[:0]
			s.st = stGround
		case isText:
			pendText(i + 1)
		case done:
			s.pend = s.pend[:0]
			s.st = stGround
		}
	}
	if s.st == stGround {
		flushText(len(p))
	}
	s.off += uint64(len(p))
}

// Flush passes on the held bytes as text, as at the end of a stream, and returns to the ground
// state.
func (s *Scanner) Flush(emit func(Segment)) {
	if len(s.pend) > 0 {
		emit(Segment{Off: s.off - uint64(len(s.pend)), Data: append([]byte(nil), s.pend...)})
	}
	s.pend, s.st = s.pend[:0], stGround
}

// step moves the state machine by b, already appended to pend. It returns a query when pend is
// a whole query, or isText when pend cannot be one.
func (s *Scanner) step(b byte) (q *Query, done, isText bool) {
	switch s.st {
	case stEsc:
		switch b {
		case '[':
			s.st = stCSI
		case ']':
			s.st = stOSC
		case 'P':
			s.st = stDCS
		default:
			return nil, false, true
		}
		return nil, false, false
	case stCSI:
		if b < 0x20 || b >= 0x7f || len(s.pend) > maxCSI {
			return nil, false, true // a control inside CSI: not one of ours
		}
		if b >= 0x40 && b <= 0x7e {
			if q := classifyCSI(s.pend[2:]); q != nil {
				return q, true, false
			}
			return nil, false, true
		}
		return nil, false, false
	case stOSC:
		switch {
		case b == 0x07:
			if q := classifyOSC(s.pend[2:len(s.pend)-1], false); q != nil {
				return q, true, false
			}
			return nil, false, true
		case b == 0x1b:
			s.st = stOSCEsc
			return nil, false, false
		case len(s.pend) > maxString:
			return nil, false, true
		}
		// Give up at once on an OSC number that is never a query, so a long title passes.
		body := s.pend[2:]
		if i := bytes.IndexByte(body, ';'); i >= 0 || len(body) > 3 {
			if i < 0 {
				return nil, false, true
			}
			switch string(body[:i]) {
			case "4", "10", "11", "12":
			default:
				return nil, false, true
			}
		} else if b < '0' || b > '9' {
			return nil, false, true
		}
		return nil, false, false
	case stOSCEsc: // b is the backslash of ST
		if q := classifyOSC(s.pend[2:len(s.pend)-2], true); q != nil {
			return q, true, false
		}
		return nil, false, true
	case stDCS:
		body := s.pend[2:]
		if len(body) == 1 {
			if b == '$' || b == '+' {
				return nil, false, false
			}
			return nil, false, true
		}
		if b == 'q' {
			s.st = stDCSBody
			return nil, false, false
		}
		return nil, false, true
	case stDCSBody:
		if b == 0x1b {
			s.st = stDCSEsc
			return nil, false, false
		}
		if len(s.pend) > maxString {
			return nil, false, true
		}
		return nil, false, false
	case stDCSEsc: // b is the backslash of ST
		k := DECRQSS
		if s.pend[2] == '+' {
			k = XTGETTCAP
		}
		return &Query{Kind: k}, true, false
	}
	return nil, false, true
}

// classifyCSI reads the bytes after ESC [ up to and including the final byte.
func classifyCSI(seq []byte) *Query {
	final := seq[len(seq)-1]
	body := seq[:len(seq)-1]
	var prefix byte
	if len(body) > 0 && body[0] >= '<' && body[0] <= '?' {
		prefix, body = body[0], body[1:]
	}
	var inter []byte
	for len(body) > 0 && body[len(body)-1] >= 0x20 && body[len(body)-1] <= 0x2f {
		inter = append([]byte{body[len(body)-1]}, inter...)
		body = body[:len(body)-1]
	}
	params := string(body)
	none := params == "" || params == "0"
	switch {
	case final == 'n' && len(inter) == 0:
		switch {
		case prefix == 0 && params == "5":
			return &Query{Kind: DSR}
		case prefix == 0 && params == "6":
			return &Query{Kind: CPR}
		case prefix == '?' && params == "6":
			return &Query{Kind: DECXCPR}
		}
	case final == 'c' && len(inter) == 0 && none:
		switch prefix {
		case 0:
			return &Query{Kind: DA1}
		case '>':
			return &Query{Kind: DA2}
		case '=':
			return &Query{Kind: DA3}
		}
	case final == 'q' && len(inter) == 0 && prefix == '>' && none:
		return &Query{Kind: XTVERSION}
	case final == 'p' && string(inter) == "$" && (prefix == 0 || prefix == '?'):
		n, err := strconv.Atoi(params)
		if err == nil && n >= 0 {
			return &Query{Kind: DECRQM, Mode: n, ANSI: prefix == 0}
		}
	case final == 't' && len(inter) == 0 && prefix == 0 && params == "18":
		return &Query{Kind: WinSize}
	case final == 'u' && len(inter) == 0 && prefix == '?' && params == "":
		return &Query{Kind: KittyKeys}
	}
	return nil
}

// classifyOSC reads an OSC body between ESC ] and its terminator. A body that sets a colour
// as well as asking is passed on: only a pure query is answered and removed.
func classifyOSC(body []byte, st bool) *Query {
	parts := bytes.Split(body, []byte(";"))
	n, err := strconv.Atoi(string(parts[0]))
	if err != nil {
		return nil
	}
	rest := parts[1:]
	if len(rest) == 0 {
		return nil
	}
	switch n {
	case 10, 11, 12:
		// OSC 10;?;? asks for 10 and 11, and so on, up to 12.
		var cs []int
		for i, p := range rest {
			if string(p) != "?" || n+i > 12 {
				return nil
			}
			cs = append(cs, n+i)
		}
		return &Query{Kind: OSCColor, Colors: cs, ST: st}
	case 4:
		if len(rest)%2 != 0 {
			return nil
		}
		var cs []int
		for i := 0; i < len(rest); i += 2 {
			idx, err := strconv.Atoi(string(rest[i]))
			if err != nil || idx < 0 || idx > 255 || string(rest[i+1]) != "?" {
				return nil
			}
			cs = append(cs, idx)
		}
		return &Query{Kind: OSCPalette, Colors: cs, ST: st}
	}
	return nil
}

// RGB is a colour.
type RGB struct{ R, G, B uint8 }

func (c RGB) xparse() string {
	return fmt.Sprintf("rgb:%02x%02x/%02x%02x/%02x%02x", c.R, c.R, c.G, c.G, c.B, c.B)
}

// Colors are the colours gff-hab reports when a program asks (spec 5.6: the owner client's
// display facts, else a configured default).
type Colors struct {
	Foreground, Background, Cursor RGB
	// Palette holds the 256 indexed colours.
	Palette [256]RGB
}

// DefaultColors is a dark theme with the xterm palette.
func DefaultColors() Colors {
	c := Colors{
		Foreground: RGB{0xe5, 0xe5, 0xe5},
		Background: RGB{0x00, 0x00, 0x00},
		Cursor:     RGB{0xe5, 0xe5, 0xe5},
	}
	base := [16]RGB{
		{0x00, 0x00, 0x00}, {0xcd, 0x00, 0x00}, {0x00, 0xcd, 0x00}, {0xcd, 0xcd, 0x00},
		{0x00, 0x00, 0xee}, {0xcd, 0x00, 0xcd}, {0x00, 0xcd, 0xcd}, {0xe5, 0xe5, 0xe5},
		{0x7f, 0x7f, 0x7f}, {0xff, 0x00, 0x00}, {0x00, 0xff, 0x00}, {0xff, 0xff, 0x00},
		{0x5c, 0x5c, 0xff}, {0xff, 0x00, 0xff}, {0x00, 0xff, 0xff}, {0xff, 0xff, 0xff},
	}
	copy(c.Palette[:], base[:])
	level := func(i int) uint8 {
		if i == 0 {
			return 0
		}
		return uint8(55 + 40*i)
	}
	for i := 0; i < 216; i++ {
		c.Palette[16+i] = RGB{level(i / 36), level(i / 6 % 6), level(i % 6)}
	}
	for i := 0; i < 24; i++ {
		v := uint8(8 + 10*i)
		c.Palette[232+i] = RGB{v, v, v}
	}
	return c
}

// Facts are what an answer is built from: the pane's parser at the moment of the query, and the
// display facts.
type Facts struct {
	// Col and Row are the cursor's 0-based column and row on the active screen.
	Col, Row int
	// Cols and Rows are the screen size.
	Cols, Rows int
	// Mode reports a mode's state; known is false for a mode the parser does not track.
	Mode func(n int, ansi bool) (set, known bool)
	// KeyboardFlags are the keyboard protocol flags the program set (CSI > u).
	KeyboardFlags int
	Colors        Colors
	// Version is gff-hab's build, for XTVERSION.
	Version string
}

// Answer returns the reply to q, as the program reads it on its input.
func Answer(q Query, f Facts) []byte {
	switch q.Kind {
	case DSR:
		return []byte("\x1b[0n")
	case CPR:
		return fmt.Appendf(nil, "\x1b[%d;%dR", f.Row+1, f.Col+1)
	case DECXCPR:
		return fmt.Appendf(nil, "\x1b[?%d;%d;1R", f.Row+1, f.Col+1)
	case DA1:
		return []byte("\x1b[?62;22c") // VT220 with ANSI colour
	case DA2:
		return []byte("\x1b[>1;10;0c")
	case DA3:
		return []byte("\x1bP!|00000000\x1b\\")
	case XTVERSION:
		return fmt.Appendf(nil, "\x1bP>|flightdeck(%s)\x1b\\", f.Version)
	case DECRQM:
		v := 0 // not recognised
		if f.Mode != nil {
			if set, known := f.Mode(q.Mode, q.ANSI); known {
				v = 2
				if set {
					v = 1
				}
			}
		}
		if q.ANSI {
			return fmt.Appendf(nil, "\x1b[%d;%d$y", q.Mode, v)
		}
		return fmt.Appendf(nil, "\x1b[?%d;%d$y", q.Mode, v)
	case WinSize:
		return fmt.Appendf(nil, "\x1b[8;%d;%dt", f.Rows, f.Cols)
	case KittyKeys:
		return fmt.Appendf(nil, "\x1b[?%du", f.KeyboardFlags)
	case OSCColor, OSCPalette:
		end := "\x07"
		if q.ST {
			end = "\x1b\\"
		}
		var out []byte
		for _, n := range q.Colors {
			if q.Kind == OSCPalette {
				out = fmt.Appendf(out, "\x1b]4;%d;%s%s", n, f.Colors.Palette[n].xparse(), end)
				continue
			}
			c := map[int]RGB{10: f.Colors.Foreground, 11: f.Colors.Background, 12: f.Colors.Cursor}[n]
			out = fmt.Appendf(out, "\x1b]%d;%s%s", n, c.xparse(), end)
		}
		return out
	case DECRQSS:
		return []byte("\x1bP0$r\x1b\\") // not a setting this terminal reports
	case XTGETTCAP:
		return []byte("\x1bP0+r\x1b\\") // no such capability
	}
	return nil
}
