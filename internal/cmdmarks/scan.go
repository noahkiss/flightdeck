// Package cmdmarks finds the OSC 133 prompt marks in a pane's output and keeps the pane's list
// of shell commands (docs/specs/hab-logic-api.md 2.5).
//
// A Scanner follows the byte stream, from any read split, and reports each well-formed mark with
// its absolute offsets. A Tracker applies the marks to a per-pane state machine and keeps a
// bounded list of commands: where each one's prompt, typed line and output fall, how it ended,
// and when. Neither needs a VT parser: the scanner knows only how an OSC string starts and ends,
// which is how a terminal finds its end too.
package cmdmarks

import "bytes"

// MaxPayload is the most payload bytes of one OSC 133 the scanner holds. A longer mark is cut
// there: the rest is followed to its end and not held, and the mark says Truncated.
const MaxPayload = 4 << 10

// Mark is one well-formed OSC 133 mark.
type Mark struct {
	// Kind is 'A' (prompt start), 'B' (command start), 'C' (output start) or 'D' (command end).
	Kind byte
	// Start is the offset of the mark's ESC; End the offset just after its terminator.
	Start, End uint64
	// Params are the ;-separated fields after the letter.
	Params []string
	// Truncated is true when the payload was longer than MaxPayload: the last param is cut.
	Truncated bool
}

type scanState uint8

const (
	stGround scanState = iota
	stEsc              // after ESC
	stOSC              // inside an OSC string
	stOSCEsc           // after ESC inside an OSC string: ST when a backslash follows
)

var prefix = []byte("133;")

// Scanner finds OSC 133 marks in a byte stream fed in any splits.
type Scanner struct {
	off   uint64 // offset after the last byte fed
	st    scanState
	start uint64 // offset of the ESC that began the current sequence
	buf   []byte // the OSC payload held so far; at most MaxPayload bytes
	drop  bool   // the current OSC is not a mark (another OSC, or malformed)
	cut   bool   // the current OSC went past MaxPayload; the rest is not held
}

// NewScanner returns a scanner for a stream whose next byte is at offset off.
func NewScanner(off uint64) *Scanner { return &Scanner{off: off} }

// Reset drops any held candidate and restarts at offset off, as after a gap.
func (s *Scanner) Reset(off uint64) {
	s.off, s.st, s.drop, s.cut = off, stGround, false, false
	s.buf = s.buf[:0]
}

// Offset returns the offset after the last byte fed.
func (s *Scanner) Offset() uint64 { return s.off }

// Idle reports whether the scanner holds no partial sequence.
func (s *Scanner) Idle() bool { return s.st == stGround }

// Feed scans p, which starts at Offset, and calls emit for each mark that ends inside p.
func (s *Scanner) Feed(p []byte, emit func(Mark)) {
	for i, b := range p {
		at := s.off + uint64(i)
		if b == 0x18 || b == 0x1a { // CAN and SUB abort any sequence
			s.st = stGround
			continue
		}
		switch s.st {
		case stGround:
			if b == 0x1b {
				s.st, s.start = stEsc, at
			}
		case stEsc:
			s.esc(b, at)
		case stOSC:
			switch b {
			case 0x07:
				s.finish(at+1, emit)
			case 0x1b:
				s.st = stOSCEsc
			default:
				s.hold(b)
			}
		case stOSCEsc:
			if b == '\\' {
				s.finish(at+1, emit)
				continue
			}
			// The ESC aborted the OSC and starts a new sequence.
			s.st, s.start = stEsc, at-1
			s.esc(b, at)
		}
	}
	s.off += uint64(len(p))
}

// esc handles the byte after an ESC at s.start.
func (s *Scanner) esc(b byte, at uint64) {
	switch {
	case b == ']':
		s.st, s.drop, s.cut = stOSC, false, false
		s.buf = s.buf[:0]
	case b == 0x1b:
		s.start = at // a new escape
	case b < 0x20:
		// A C0 control inside an escape runs, and the escape goes on.
	default:
		s.st = stGround
	}
}

// hold keeps one payload byte while the OSC may still be a mark.
func (s *Scanner) hold(b byte) {
	if s.drop {
		return
	}
	if len(s.buf) >= MaxPayload {
		// The held bytes start "133;": keep them and cut the rest. A byte that makes the
		// payload malformed still drops the mark.
		s.cut = true
		if b < 0x20 || b == 0x7f {
			s.drop = true
			s.buf = s.buf[:0]
		}
		return
	}
	s.buf = append(s.buf, b)
	if n := len(s.buf); n <= len(prefix) && s.buf[n-1] != prefix[n-1] {
		s.drop = true // another OSC: follow it to its end, hold nothing
		s.buf = s.buf[:0]
	}
}

// finish ends the OSC at end and emits it when it is a well-formed mark.
func (s *Scanner) finish(end uint64, emit func(Mark)) {
	s.st = stGround
	if s.drop {
		return
	}
	if m, ok := parse(s.buf); ok {
		m.Start, m.End, m.Truncated = s.start, end, s.cut
		emit(m)
	}
	s.buf = s.buf[:0]
}

// parse reads an OSC payload as a mark: "133;" then A to D, then nothing or ";" and params.
func parse(p []byte) (Mark, bool) {
	if len(p) < len(prefix)+1 || !bytes.HasPrefix(p, prefix) {
		return Mark{}, false
	}
	for _, b := range p {
		if b < 0x20 || b == 0x7f {
			return Mark{}, false
		}
	}
	k := p[len(prefix)]
	if k < 'A' || k > 'D' {
		return Mark{}, false
	}
	rest := p[len(prefix)+1:]
	m := Mark{Kind: k}
	if len(rest) == 0 {
		return m, true
	}
	if rest[0] != ';' {
		return Mark{}, false
	}
	for f := range bytes.SplitSeq(rest[1:], []byte{';'}) {
		m.Params = append(m.Params, string(f))
	}
	return m, true
}
