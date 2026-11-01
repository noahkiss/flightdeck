package gffclient

import (
	"bytes"
	"strconv"
)

// passFilter cleans a pane's output before the pass-through view writes it to the host
// terminal (gff-client.md 3.2, 3.3; hab-logic-api.md 5.6, 5.8).
//
// It drops two kinds of sequence:
//
//   - Anything the host terminal would answer. gff-hab answers the queries it knows and removes
//     them; the ones it leaves to clients (pixel size reports, clipboard reads, a query with a
//     control byte inside, a query cut by a gap) and any other query must not reach the host,
//     because the host's answer would arrive on gff's input and go to the program as typing.
//   - Settings that would change the host terminal beyond the pane: the palette and the special
//     colours (OSC 4, 5, 10 to 19 and their resets), the working directory (OSC 7), window
//     moves and resizes (CSI t), and kitty graphics (APC), whose commands the host answers.
//
// Everything else passes byte for byte. A sequence is held until it is complete, across
// writes; a control string longer than maxHeld cannot be a query, so it streams once the
// filter knows it is not one it drops.
type passFilter struct {
	st   fState
	held []byte
	drop bool // in a long control string being dropped
	pass bool // in a long control string being passed on
}

type fState uint8

const (
	fGround fState = iota
	fEsc
	fCSI
	fOSC    // OSC body
	fOSCEsc // ESC inside an OSC
	fDCS    // DCS, APC, PM or SOS body
	fDCSEsc // ESC inside one
)

// maxHeld bounds a held control string. Queries are short; a longer string streams.
const maxHeld = 1024

func (f *passFilter) reset() { *f = passFilter{} }

// write filters p and appends what passes to out.
func (f *passFilter) write(out *bytes.Buffer, p []byte) {
	for _, b := range p {
		f.byte(out, b)
	}
}

func (f *passFilter) byte(out *bytes.Buffer, b byte) {
	switch f.st {
	case fGround:
		if b == 0x1b {
			f.st, f.held = fEsc, append(f.held[:0], b)
			return
		}
		out.WriteByte(b)
	case fEsc:
		f.held = append(f.held, b)
		switch b {
		case '[':
			f.st = fCSI
		case ']':
			f.st = fOSC
		case 'P', '_', '^', 'X':
			f.st = fDCS
		case 0x1b:
			out.Write(f.held[:len(f.held)-1])
			f.held = f.held[:1]
		default:
			if b >= 0x20 && b < 0x30 { // an intermediate: ESC ( B and friends
				return
			}
			f.flush(out)
		}
	case fCSI:
		switch {
		case b == 0x1b:
			out.Write(f.held) // an ESC aborts the CSI; the host drops it too
			f.st, f.held = fEsc, append(f.held[:0], b)
		case b == 0x18 || b == 0x1a: // CAN, SUB cancel it
			f.held = append(f.held, b)
			f.flush(out)
		case b >= 0x40 && b <= 0x7e:
			f.held = append(f.held, b)
			if csiDropped(f.held) {
				f.discard()
			} else {
				f.flush(out)
			}
		default:
			f.held = append(f.held, b)
			if len(f.held) > maxHeld {
				f.flush(out)
			}
		}
	case fOSC, fDCS:
		switch {
		case b == 0x07 && f.st == fOSC:
			f.endString(out, []byte{b})
		case b == 0x1b:
			if f.st == fOSC {
				f.st = fOSCEsc
			} else {
				f.st = fDCSEsc
			}
		case b == 0x18 || b == 0x1a: // CAN, SUB cancel the string
			f.endString(out, []byte{b})
		default:
			f.hold(out, b)
		}
	case fOSCEsc, fDCSEsc:
		if b == '\\' {
			f.endString(out, []byte{0x1b, b})
			return
		}
		// ESC and another byte: the ESC ended the string and starts a new sequence.
		f.endString(out, nil)
		f.byte(out, 0x1b)
		f.byte(out, b)
	}
}

// hold adds b to a control string, or streams it once the string is known to be long.
func (f *passFilter) hold(out *bytes.Buffer, b byte) {
	switch {
	case f.drop:
	case f.pass:
		out.WriteByte(b)
	default:
		f.held = append(f.held, b)
		if len(f.held) > maxHeld {
			if stringDropped(f.held, false) {
				f.drop, f.held = true, f.held[:0]
			} else {
				out.Write(f.held)
				f.pass, f.held = true, f.held[:0]
			}
		}
	}
}

// endString ends a control string with its terminator, tail (nil when a new ESC ended it).
func (f *passFilter) endString(out *bytes.Buffer, tail []byte) {
	switch {
	case f.drop:
	case f.pass:
		out.Write(tail)
	default:
		f.held = append(f.held, tail...)
		if !stringDropped(f.held, true) {
			out.Write(f.held)
		}
	}
	f.st, f.held, f.drop, f.pass = fGround, f.held[:0], false, false
}

func (f *passFilter) flush(out *bytes.Buffer) {
	out.Write(f.held)
	f.discard()
}

func (f *passFilter) discard() { f.st, f.held = fGround, f.held[:0] }

// csiDropped reports whether a complete CSI sequence (ESC [ ... final) is one the host would
// answer, or one that would move or resize the host window.
func csiDropped(seq []byte) bool {
	final := seq[len(seq)-1]
	body := seq[2 : len(seq)-1]
	var prefix byte
	if len(body) > 0 && body[0] >= 0x3c && body[0] <= 0x3f {
		prefix, body = body[0], body[1:]
	}
	var params, inter []byte
	for i, c := range body {
		if c >= 0x20 && c <= 0x2f {
			params, inter = body[:i], body[i:]
			break
		}
	}
	if inter == nil {
		params = body
	}
	switch final {
	case 'n': // device status, cursor position and every other report request
		return true
	case 'c': // device attributes, primary, secondary and tertiary
		return len(inter) == 0
	case 't': // window operations: reports, moves and resizes; the title stack stays
		if prefix != 0 || len(inter) != 0 {
			return false
		}
		n, _ := strconv.Atoi(firstParam(params))
		return n != 22 && n != 23
	case 'q': // XTVERSION
		return prefix == '>' && len(inter) == 0
	case 'p': // DECRQM, ANSI and private
		return bytes.Equal(inter, []byte("$"))
	case 'u': // kitty keyboard flags query
		return prefix == '?'
	case 'x': // DECREQTPARM
		return prefix == 0 && len(inter) == 0
	case 'S': // XTSMGRAPHICS
		return prefix == '?'
	case 'm': // XTQMODKEYS
		return prefix == '?'
	case 'y': // DECRQCRA checksum
		return bytes.Equal(inter, []byte("*"))
	case 'w': // DECRQPSR
		return bytes.Equal(inter, []byte("$"))
	}
	return false
}

// firstParam returns the first parameter, without control bytes a program put inside.
func firstParam(p []byte) string {
	if i := bytes.IndexByte(p, ';'); i >= 0 {
		p = p[:i]
	}
	var b []byte
	for _, c := range p {
		if c >= '0' && c <= '9' {
			b = append(b, c)
		}
	}
	return string(b)
}

// stringDropped reports whether a control string (from its ESC) is dropped. complete says
// whether seq holds the whole string; a long string is judged by its start.
func stringDropped(seq []byte, complete bool) bool {
	switch seq[1] {
	case ']':
		body := seq[2:]
		if complete {
			body = bytes.TrimSuffix(bytes.TrimSuffix(body, []byte{0x07}), []byte("\x1b\\"))
		}
		num, rest, _ := bytes.Cut(body, []byte(";"))
		n, err := strconv.Atoi(string(num))
		if err != nil {
			return true // not a well-formed OSC: nothing a host should act on
		}
		switch {
		case n == 4 || n == 5 || n == 7 || (n >= 10 && n <= 19) ||
			n == 104 || n == 105 || (n >= 110 && n <= 119):
			return true
		}
		if complete {
			for _, field := range bytes.Split(rest, []byte(";")) {
				if bytes.Equal(field, []byte("?")) {
					return true // a query: OSC 52 clipboard read and the like
				}
			}
		}
		return false
	case 'P':
		body := seq[2:]
		return bytes.HasPrefix(body, []byte("$q")) || bytes.HasPrefix(body, []byte("+q")) ||
			bytes.HasPrefix(body, []byte("tmux;"))
	case '_':
		return true // APC: kitty graphics, which the host answers
	}
	return false
}
