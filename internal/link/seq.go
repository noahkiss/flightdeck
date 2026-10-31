package link

// seqScanner follows just enough of the VT parser state machine to tell whether a byte stream
// ends at a point where a parser is in its ground state: not inside an escape sequence, a
// control string or a UTF-8 character. A checkpoint is only taken at such a point, because a
// snapshot cannot carry a half-parsed sequence: restoring one and feeding the rest prints the
// rest as text.
//
// It is deliberately small. It knows the shapes of ESC, CSI, OSC and the other control strings
// (DCS, SOS, PM, APC), CAN and SUB, and UTF-8 lead and continuation bytes. C1 controls as single
// bytes are not recognised: in a UTF-8 stream those bytes are continuation bytes.
type seqScanner struct {
	state seqState
	need  int  // UTF-8 continuation bytes still to come, in seqUTF8
	osc   bool // the control string is an OSC, which BEL also ends
}

type seqState uint8

const (
	seqGround    seqState = iota
	seqUTF8               // inside a multi-byte UTF-8 character
	seqEsc                // after ESC
	seqEscInter           // after ESC and one or more intermediate bytes
	seqCSI                // inside a CSI sequence
	seqString             // inside OSC, DCS, SOS, PM or APC
	seqStringEsc          // after ESC inside a control string: ST if a backslash follows
)

// ground reports whether the bytes so far end in the ground state.
func (s *seqScanner) ground() bool { return s.state == seqGround }

// reset puts the scanner in the ground state, as after a gap or a restore.
func (s *seqScanner) reset() { *s = seqScanner{} }

func (s *seqScanner) write(p []byte) {
	for _, b := range p {
		s.step(b)
	}
}

func (s *seqScanner) step(b byte) {
	if b == 0x18 || b == 0x1a { // CAN and SUB abort any sequence
		s.reset()
		return
	}
	switch s.state {
	case seqGround:
		s.ground1(b)
	case seqUTF8:
		if b >= 0x80 && b <= 0xbf {
			if s.need--; s.need == 0 {
				s.state = seqGround
			}
			return
		}
		s.state = seqGround // a broken character ends; the byte starts afresh
		s.ground1(b)
	case seqEsc:
		s.esc(b)
	case seqEscInter:
		switch {
		case b == 0x1b:
			s.state = seqEsc
		case b < 0x20, b >= 0x20 && b <= 0x2f:
		default:
			s.state = seqGround
		}
	case seqCSI:
		switch {
		case b == 0x1b:
			s.state = seqEsc
		case b >= 0x40 && b <= 0x7e:
			s.state = seqGround
		}
	case seqString:
		switch {
		case b == 0x1b:
			s.state = seqStringEsc
		case b == 0x07 && s.osc:
			s.state = seqGround
		}
	case seqStringEsc:
		if b == '\\' {
			s.state = seqGround
			return
		}
		s.esc(b) // the ESC aborted the string and starts a new sequence
	}
}

func (s *seqScanner) ground1(b byte) {
	switch {
	case b == 0x1b:
		s.state = seqEsc
	case b >= 0xc2 && b <= 0xdf:
		s.state, s.need = seqUTF8, 1
	case b >= 0xe0 && b <= 0xef:
		s.state, s.need = seqUTF8, 2
	case b >= 0xf0 && b <= 0xf4:
		s.state, s.need = seqUTF8, 3
	}
}

// esc handles the byte after an ESC.
func (s *seqScanner) esc(b byte) {
	switch {
	case b == 0x1b:
		s.state = seqEsc
	case b == '[':
		s.state = seqCSI
	case b == ']':
		s.state, s.osc = seqString, true
	case b == 'P', b == 'X', b == '^', b == '_':
		s.state, s.osc = seqString, false
	case b >= 0x20 && b <= 0x2f:
		s.state = seqEscInter
	case b < 0x20:
		s.state = seqEsc // a C0 control inside an escape runs and the escape goes on
	default:
		s.state = seqGround
	}
}
