// Package progstatus reads program status reports (OSC 7501) from a pane's output
// (docs/specs/harness-adapters.md, Claude Code). A program such as Claude Code reports what it
// is doing as
//
//	ESC ] 7501 ; key=value:key=value ST
//
// where ST is ESC \ or BEL. The query form, ESC ] 7501 ; ? ST, is a terminal query: package
// termq finds it and gff-hab answers it once. This package reads only reports.
//
// The Scanner follows a byte stream that arrives in chunks of any size. It never changes the
// stream: reports stay in the bytes the clients get, and a terminal ignores an OSC it does not
// know.
package progstatus

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits from the protocol. A report that breaks one is discarded whole.
const (
	MaxSequence = 4096 // ESC ] through ST
	maxKey      = 16
	maxMsg      = 2048 // decoded bytes
	maxTitle    = 192  // decoded bytes
	maxApp      = 32
	maxID       = 128
	maxSegment  = 32
	maxDepth    = 8
)

// Report is one status report. Only State is required.
type Report struct {
	// State is idle, working, done, blocked, error or clear.
	State string
	// ID names a record below the program's root record; empty is the root record.
	ID string
	// Kind is permission, question or auth, with blocked; empty otherwise.
	Kind string
	// App is the program's stable name, such as claude; empty when the report has none.
	App string
	// Progress is 0 to 100 with working or blocked, or -1 when indeterminate.
	Progress int
	// Title and Msg are the decoded free text, with no control characters.
	Title, Msg string
}

var states = map[string]bool{"idle": true, "working": true, "done": true, "blocked": true, "error": true, "clear": true}
var kinds = map[string]bool{"permission": true, "question": true, "auth": true}

// Parse reads the body of a report: the bytes after "7501;" and before ST. ok is false when the
// report is discarded: no recognised state, a broken limit, bad base64, a control character in
// decoded text, or a malformed id. A malformed pair is skipped; an unknown key is ignored; a
// repeated key takes its last value.
func Parse(body []byte) (r Report, ok bool) {
	if len(body) > MaxSequence {
		return Report{}, false
	}
	r.Progress = -1
	var title, msg, progress, kind, app string
	for _, pair := range strings.Split(string(body), ":") {
		k, v, found := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !found || k == "" || len(k) > maxKey || !isKey(k) || !isValue(v) {
			continue // a malformed pair
		}
		switch k {
		case "state":
			r.State = v
		case "id":
			r.ID = v
		case "kind":
			kind = v
		case "progress":
			progress = v
		case "app":
			app = v
		case "title":
			title = v
		case "msg":
			msg = v
		}
	}
	if !states[r.State] {
		return Report{}, false
	}
	if r.ID != "" && !validID(r.ID) {
		return Report{}, false
	}
	if kinds[kind] && r.State == "blocked" {
		r.Kind = kind
	}
	if n, err := strconv.Atoi(progress); err == nil && n >= 0 && n <= 100 && (r.State == "working" || r.State == "blocked") {
		r.Progress = n
	}
	if len(app) <= maxApp && validSegment(app) {
		r.App = app
	}
	var good bool
	if r.Title, good = text(title, maxTitle); !good {
		return Report{}, false
	}
	if r.Msg, good = text(msg, maxMsg); !good {
		return Report{}, false
	}
	return r, true
}

func isKey(k string) bool {
	for i := 0; i < len(k); i++ {
		if k[i] < 'a' || k[i] > 'z' {
			return false
		}
	}
	return true
}

func isValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			strings.IndexByte("_.,+/=-", c) >= 0
		if !ok {
			return false
		}
	}
	return true
}

func validSegment(s string) bool {
	if s == "" || len(s) > maxSegment {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("_.+-", c) >= 0) {
			return false
		}
	}
	return true
}

func validID(id string) bool {
	if len(id) > maxID {
		return false
	}
	segs := strings.Split(id, "/")
	if len(segs) > maxDepth {
		return false
	}
	for _, s := range segs {
		if !validSegment(s) {
			return false
		}
	}
	return true
}

// text decodes standard base64, padding optional, and refuses invalid UTF-8 and control
// characters.
func text(v string, max int) (string, bool) {
	if v == "" {
		return "", true
	}
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(v, "="))
	if err != nil || len(b) > max || !utf8.Valid(b) {
		return "", false
	}
	for _, r := range string(b) {
		if r < 0x20 || r >= 0x7f && r <= 0x9f {
			return "", false
		}
	}
	return string(b), true
}

type state uint8

const (
	stGround  state = iota
	stEsc           // after ESC
	stNum           // after ESC ], reading the OSC number
	stBody          // inside an OSC 7501 body
	stBodyEsc       // ESC inside the body
	stSkip          // inside another OSC, or a report too long: wait for its end
	stSkipEsc       // ESC inside it
)

// Scanner finds the reports in an output stream. The zero value is ready to use.
type Scanner struct {
	st  state
	buf []byte // the OSC number, then the body
	n   int    // bytes of the current sequence, from ESC
}

// Reset drops a partial sequence, as after a gap in the stream.
func (s *Scanner) Reset() {
	s.st, s.buf, s.n = stGround, s.buf[:0], 0
}

// Feed scans p and calls fn with each valid report, in order. A discarded report calls nothing.
func (s *Scanner) Feed(p []byte, fn func(Report)) {
	for _, b := range p {
		if s.st != stGround {
			s.n++
		}
		switch s.st {
		case stGround:
			if b == 0x1b {
				s.st, s.n = stEsc, 1
			}
		case stEsc:
			if b == ']' {
				s.st, s.buf = stNum, s.buf[:0]
			} else if b != 0x1b {
				s.st = stGround
			} else {
				s.n = 1
			}
		case stNum:
			switch {
			case b >= '0' && b <= '9' && len(s.buf) < 5:
				s.buf = append(s.buf, b)
			case b == ';' && string(s.buf) == "7501":
				s.st, s.buf = stBody, s.buf[:0]
			default:
				s.skip(b)
			}
		case stBody:
			switch {
			case b == 0x07:
				s.end(fn)
			case b == 0x1b:
				s.st = stBodyEsc
			case b == 0x18 || b == 0x1a: // CAN and SUB abort the sequence
				s.st = stGround
			case s.n > MaxSequence:
				s.st = stSkip
			default:
				s.buf = append(s.buf, b)
			}
		case stBodyEsc:
			if b == '\\' {
				s.end(fn)
			} else {
				s.restart(b) // the ESC ended the string and starts a new sequence
			}
		case stSkip:
			s.skip(b)
		case stSkipEsc:
			if b == '\\' {
				s.st = stGround
			} else {
				s.restart(b)
			}
		}
	}
}

// skip follows an OSC that is not a report until its end.
func (s *Scanner) skip(b byte) {
	switch b {
	case 0x07, 0x18, 0x1a:
		s.st = stGround
	case 0x1b:
		s.st = stSkipEsc
	default:
		s.st = stSkip
	}
}

// restart treats b as the byte after a fresh ESC.
func (s *Scanner) restart(b byte) {
	s.st, s.n = stEsc, 1
	s.Feed([]byte{b}, nil)
}

func (s *Scanner) end(fn func(Report)) {
	body := s.buf
	s.st, s.buf = stGround, s.buf[:0]
	if fn == nil || s.n > MaxSequence {
		return
	}
	if r, ok := Parse(bytes.Clone(body)); ok {
		fn(r)
	}
}
