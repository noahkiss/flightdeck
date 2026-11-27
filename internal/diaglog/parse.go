package diaglog

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Line is one structured line read back.
type Line struct {
	T     time.Time
	Lvl   string
	Proc  string
	Epoch string
	Comp  string
	Msg   string
	Trace string
	// Attrs are the other keys, in the line's order.
	Attrs []KV
}

// KV is one key and value of a Line. Num marks a number or a Boolean, written bare.
type KV struct {
	Key string
	Val string
	Num bool
}

// Parse reads a structured line. ok is false for a line that is not a JSON object with a time
// and a message, such as a text line of an older build.
func Parse(s string) (l Line, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return Line{}, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return Line{}, false
	}
	var t, msg bool
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Line{}, false
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return Line{}, false
		}
		val, num := rawValue(raw)
		switch key {
		case KeyT:
			tt, err := time.Parse(time.RFC3339Nano, val)
			if err != nil {
				return Line{}, false
			}
			l.T, t = tt, true
		case KeyLvl:
			l.Lvl = val
		case KeyProc:
			l.Proc = val
		case KeyEpoch:
			l.Epoch = val
		case KeyComp:
			l.Comp = val
		case KeyMsg:
			l.Msg, msg = val, true
		case KeyTrace:
			l.Trace = val
		default:
			l.Attrs = append(l.Attrs, KV{Key: key, Val: val, Num: num})
		}
	}
	return l, t && msg
}

// rawValue is a JSON value as text: a string's contents, else the value as written.
func rawValue(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, false
		}
	}
	return string(raw), true
}

// Get returns the value of key, and whether the line has it.
func (l Line) Get(key string) (string, bool) {
	for _, kv := range l.Attrs {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return "", false
}

// Text is the line for a reader, with no time: a legacy line's own text; else the component,
// the message and each value as key=value, quoted when it holds a space or a quote. A level
// other than info comes first.
func (l Line) Text() string {
	var b strings.Builder
	if l.Lvl != "" && l.Lvl != "info" {
		b.WriteString(l.Lvl)
		b.WriteByte(' ')
	}
	if l.Msg == MsgLegacy {
		text, _ := l.Get(KeyText)
		b.WriteString(text)
		return b.String()
	}
	if l.Comp != "" {
		b.WriteString(l.Comp)
		b.WriteString(": ")
	}
	b.WriteString(l.Msg)
	for _, kv := range l.Attrs {
		b.WriteByte(' ')
		b.WriteString(kv.Key)
		b.WriteByte('=')
		if kv.Num || (kv.Val != "" && !strings.ContainsAny(kv.Val, " \t\n\"=")) {
			b.WriteString(kv.Val)
		} else {
			b.WriteString(strconv.Quote(kv.Val))
		}
	}
	if l.Trace != "" {
		b.WriteString(", trace ")
		b.WriteString(l.Trace)
	}
	return b.String()
}
