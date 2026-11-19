// Package panetext turns a pane's VT bytes into plain text lines (hab-logic-api.md 3.11). One
// streaming filter serves the scrollback rows of pane.read and the new output pane.wait reads,
// so both remove the same sequences. It drops CSI sequences, OSC, DCS, SOS, PM and APC strings,
// other ESC sequences, C0 controls but tab, DEL and C1 controls. It never builds a screen: a
// cursor move is dropped, not applied. Pure Go.
package panetext

import (
	"strings"
	"unicode/utf8"
)

// MaxLine bounds one line. A longer line is handed on in parts of at most MaxLine bytes.
const MaxLine = 64 << 10

type state uint8

const (
	ground state = iota
	esc          // after ESC
	escInter     // ESC, then intermediate bytes
	csi          // ESC [ or C1 CSI: parameters until a final byte
	str          // OSC, DCS, SOS, PM or APC: until BEL or ST
	strEsc       // ESC inside a string: \ ends it
)

// Lines splits a byte stream into text lines. LF ends a line; so does CR, and CR LF counts
// once. The zero value is ready to use. A Lines is not safe for concurrent use.
type Lines struct {
	st    state
	cr    bool   // the last byte ended a line with CR: an LF next is part of that end
	cur   []byte // the open line
	dirty bool   // the open line changed since it was last handed on
}

// Feed reads p. It calls fn for each line p ends (done true), then once for the open line when
// p changed it and it is not empty (done false). Feed stops and returns true as soon as fn
// returns true.
func (l *Lines) Feed(p []byte, fn func(line string, done bool) bool) bool {
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch l.st {
		case esc:
			switch {
			case b == '[':
				l.st = csi
			case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
				l.st = str
			case b == 0x1b:
				// ESC again: start over.
			case b == 0x18 || b == 0x1a:
				l.st = ground
			case b >= 0x20 && b <= 0x2f:
				l.st = escInter
			case b < 0x20:
				// A control inside a sequence is acted on as in ground, and the sequence goes on.
				if l.control(b, fn) {
					return true
				}
			default:
				l.st = ground
			}
			continue
		case escInter:
			switch {
			case b == 0x1b:
				l.st = esc
			case b == 0x18 || b == 0x1a:
				l.st = ground
			case b >= 0x30 && b <= 0x7e:
				l.st = ground
			}
			continue
		case csi:
			switch {
			case b == 0x1b:
				l.st = esc
			case b == 0x18 || b == 0x1a:
				l.st = ground
			case b >= 0x40 && b <= 0x7e:
				l.st = ground
			}
			continue
		case str:
			switch b {
			case 0x07, 0x18, 0x1a:
				l.st = ground
			case 0x1b:
				l.st = strEsc
			}
			continue
		case strEsc:
			if b == '\\' {
				l.st = ground
				continue
			}
			// Not ST: the ESC starts a new sequence, and b is its next byte.
			l.st = esc
			i--
			continue
		}
		// ground
		switch {
		case b == 0x1b:
			l.st = esc
			l.cr = false
		case b < 0x20 || b == 0x7f:
			if l.control(b, fn) {
				return true
			}
		default:
			l.cr = false
			l.cur = append(l.cur, b)
			l.dirty = true
			if len(l.cur) >= MaxLine {
				if l.end(fn) {
					return true
				}
			}
		}
	}
	if l.dirty && len(l.cur) > 0 {
		l.dirty = false
		return fn(clean(l.cur), false)
	}
	return false
}

// control acts on one C0 control byte or DEL.
func (l *Lines) control(b byte, fn func(string, bool) bool) bool {
	switch b {
	case '\n':
		if l.cr {
			l.cr = false
			return false
		}
		return l.end(fn)
	case '\r':
		stop := l.end(fn)
		l.cr = true
		return stop
	case '\t':
		l.cr = false
		l.cur = append(l.cur, '\t')
		l.dirty = true
	default:
		l.cr = false
	}
	return false
}

// end hands on the open line as done and starts a new one.
func (l *Lines) end(fn func(string, bool) bool) bool {
	line := clean(l.cur)
	l.cur, l.dirty = l.cur[:0], false
	return fn(line, true)
}

// Open returns the open line: the text after the last line end.
func (l *Lines) Open() string { return clean(l.cur) }

// clean makes b valid UTF-8 and drops C1 controls (U+0080 to U+009F).
func clean(b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if !strings.ContainsFunc(s, isC1) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isC1(r) {
			return -1
		}
		return r
	}, s)
}

func isC1(r rune) bool { return r >= 0x80 && r <= 0x9f }

// Text returns the lines of vt as plain text, each without trailing spaces: every line it ends,
// then the open line when it is not empty.
func Text(vt []byte) []string {
	var l Lines
	var out []string
	l.Feed(vt, func(line string, done bool) bool {
		if done {
			out = append(out, strings.TrimRight(line, " "))
		}
		return false
	})
	if open := strings.TrimRight(l.Open(), " "); open != "" {
		out = append(out, open)
	}
	return out
}
