package gffclient

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultPrefix is the prefix key: Ctrl+\. Shells and harnesses rarely bind it, and it is
// none of the keys gff-client.md 8.1 leaves to programs. Pressing it twice sends it to the pane.
const DefaultPrefix byte = 0x1c

// ParsePrefix reads a prefix key written as ctrl-<key>, for a key from @ a-z \ ] ^ _, except
// the keys that are Backspace, Tab, Enter and Esc.
func ParsePrefix(s string) (byte, error) {
	k, ok := strings.CutPrefix(strings.ToLower(s), "ctrl-")
	if !ok || len(k) != 1 {
		return 0, fmt.Errorf("prefix %q: want ctrl-<key>, such as ctrl-\\ or ctrl-a", s)
	}
	c := k[0]
	switch c {
	case 'h', 'i', 'j', 'm', '[':
		return 0, fmt.Errorf("prefix %q: that key is Backspace, Tab, Enter or Esc", s)
	}
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 1, nil
	case c == '@' || (c >= '[' && c <= '_'):
		return c & 0x1f, nil
	}
	return 0, fmt.Errorf("prefix %q: want ctrl- and one of @ a-z [ \\ ] ^ _", s)
}

// Commands after the prefix (gff-client.md 5.2, 10.1). The full keymap with modes is a later
// stage (8.1); this is the client core's small set.
const (
	cmdFocusNext = 'o' // focus the next pane; Tab too
	cmdZoom      = 'z' // the focused pane alone, and back
	cmdSplit     = 's' // switch side by side and stacked
	cmdRedraw    = 'r' // pane.redraw snapshot
	cmdNudge     = 'n' // pane.redraw nudge
	cmdFit       = 'f' // pane.take
	cmdDetach    = 'd' // quit gff; programs keep running
)

// keyEvent is what the key reader makes of host input: bytes for the focused pane, or a
// command.
type keyEvent struct {
	data []byte
	cmd  rune
}

// keyReader splits host input into pane bytes and prefix commands. It knows the prefix in its
// legacy form (one control byte), in the kitty keyboard protocol's form (CSI code;5u) and in
// xterm's modifyOtherKeys form (CSI 27;5;code~). A host sends those forms while a pane in the
// pass-through view has that protocol or mode on.
type keyReader struct {
	prefix  byte
	code    int    // the prefix key's code point in the kitty and modifyOtherKeys forms
	pending bool   // the prefix came; the next key is a command
	held    []byte // the start of a possible prefix sequence or command key, cut at a read boundary
}

func newKeyReader(prefix byte) *keyReader {
	code := prefix | 0x60 // ctrl-a is 'a', ctrl-\ is '\'
	if prefix == 0 {
		code = '@'
	} else if prefix >= 0x1b {
		code = prefix | 0x40
	}
	return &keyReader{prefix: prefix, code: int(code)}
}

// keySkip is commandKey's result for a key event that is no command: a key release, or a
// modifier key alone. The reader still waits for the command.
const keySkip rune = -2

// feed reads p and returns its events in order.
func (k *keyReader) feed(p []byte) []keyEvent {
	if len(k.held) > 0 {
		p = append(k.held, p...)
		k.held = nil
	}
	var evs []keyEvent
	start := 0
	flush := func(end int) {
		if end > start {
			evs = append(evs, keyEvent{data: append([]byte(nil), p[start:end]...)})
		}
	}
	for i := 0; i < len(p); {
		if k.pending {
			r, n, ok := commandKey(p[i:])
			if !ok {
				k.held = append([]byte(nil), p[i:]...)
				return evs
			}
			i += n
			start = i
			if r == keySkip {
				continue
			}
			k.pending = false
			switch {
			case r == rune(k.prefix):
				evs = append(evs, keyEvent{data: []byte{k.prefix}})
			case r == 0x1b || r < 0:
			default:
				evs = append(evs, keyEvent{cmd: r})
			}
			continue
		}
		if p[i] == k.prefix {
			flush(i)
			k.pending = true
			i++
			start = i
			continue
		}
		if p[i] == 0x1b {
			n, m := k.prefixSeq(p[i:])
			switch m {
			case seqPrefix:
				flush(i)
				k.pending = true
				i += n
				start = i
				continue
			case seqPrefixRelease:
				// The prefix's press never reached the pane, so its release does not either.
				flush(i)
				i += n
				start = i
				continue
			case seqPartial:
				flush(i)
				k.held = append([]byte(nil), p[i:]...)
				return evs
			}
		}
		i++
	}
	flush(len(p))
	return evs
}

type seqMatch int

const (
	seqNo seqMatch = iota
	seqPrefix
	seqPrefixRelease
	seqPartial
)

// maxKeySeq bounds a key sequence the reader holds across reads.
const maxKeySeq = 32

// prefixSeq matches the prefix as a key sequence at the start of p, which starts with ESC: the
// kitty form CSI code;5u (an event type may follow the 5: :1 press, :2 repeat, :3 release) or
// the modifyOtherKeys form CSI 27;5;code~. Caps Lock and Num Lock in the modifiers do not
// matter.
func (k *keyReader) prefixSeq(p []byte) (int, seqMatch) {
	if len(p) < 2 || p[1] != '[' {
		return 0, seqNo // a lone Esc is a key of its own; it is never held
	}
	n, complete := csiEnd(p)
	if n < 0 {
		return 0, seqNo
	}
	if !complete {
		if len(p) >= maxKeySeq {
			return 0, seqNo
		}
		f0, rest, more := strings.Cut(string(p[2:]), ";")
		f0, _, _ = strings.Cut(f0, ":")
		code := strconv.Itoa(k.code)
		switch {
		case !more && (strings.HasPrefix(code, f0) || strings.HasPrefix("27", f0)):
			return 0, seqPartial
		case more && (f0 == code || f0 == "27") && !strings.ContainsAny(rest, "<=>?"):
			return 0, seqPartial
		}
		return 0, seqNo
	}
	code, mods, event, ok := decodeKey(p[:n])
	if !ok || code != k.code || !ctrlOnly(mods) {
		return 0, seqNo
	}
	if event == 3 {
		return n, seqPrefixRelease
	}
	return n, seqPrefix
}

// csiEnd finds the end of the CSI sequence at the start of p (ESC [ ...). It returns the
// sequence's length and true, or len(p) and false when p ends inside it, or -1 when a byte
// that cannot be in a CSI comes first.
func csiEnd(p []byte) (int, bool) {
	for i := 2; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 0x40 && c <= 0x7e:
			return i + 1, true
		case c < 0x20 || c > 0x7e:
			return -1, false
		}
	}
	return len(p), false
}

// decodeKey reads a complete key sequence: kitty's CSI code[:alternates][;mods[:event][;text]]u,
// or modifyOtherKeys' CSI 27;mods;code~. mods is 1 plus the modifier bits; event is 1 for a
// press, 2 for a repeat and 3 for a release.
func decodeKey(seq []byte) (code, mods, event int, ok bool) {
	final := seq[len(seq)-1]
	body := string(seq[2 : len(seq)-1])
	if body == "" || strings.ContainsAny(body[:1], "<=>?") {
		return 0, 0, 0, false
	}
	fields := strings.Split(body, ";")
	mods, event = 1, 1
	var err error
	switch final {
	case 'u':
		c, _, _ := strings.Cut(fields[0], ":")
		if code, err = strconv.Atoi(c); err != nil {
			return 0, 0, 0, false
		}
		if len(fields) > 1 && fields[1] != "" {
			m, e, hasEvent := strings.Cut(fields[1], ":")
			if mods, err = strconv.Atoi(m); err != nil {
				return 0, 0, 0, false
			}
			if hasEvent {
				if event, err = strconv.Atoi(e); err != nil {
					return 0, 0, 0, false
				}
			}
		}
	case '~':
		if len(fields) != 3 || fields[0] != "27" {
			return 0, 0, 0, false
		}
		if mods, err = strconv.Atoi(fields[1]); err != nil {
			return 0, 0, 0, false
		}
		if code, err = strconv.Atoi(fields[2]); err != nil {
			return 0, 0, 0, false
		}
	default:
		return 0, 0, 0, false
	}
	return code, mods, event, true
}

// ctrlOnly reports whether mods is Ctrl and no other modifier, Caps Lock and Num Lock aside.
func ctrlOnly(mods int) bool { return (mods-1)&^(64|128) == 4 }

// commandKey reads the key after the prefix: one byte, or a kitty or modifyOtherKeys key
// sequence. ok is false when p ends inside a CSI. r is negative for a key it cannot name, such
// as an arrow, and keySkip for a key release or a modifier key alone.
func commandKey(p []byte) (r rune, n int, ok bool) {
	if len(p) == 0 {
		return 0, 0, false
	}
	if p[0] != 0x1b {
		return rune(p[0]), 1, true
	}
	if len(p) == 1 || p[1] != '[' {
		return 0x1b, 1, true // a lone Esc cancels
	}
	n, complete := csiEnd(p)
	switch {
	case n < 0:
		return 0x1b, 1, true
	case !complete:
		if len(p) >= maxKeySeq {
			return -1, len(p), true
		}
		return 0, 0, false
	}
	code, mods, event, ok := decodeKey(p[:n])
	switch {
	case !ok:
		return -1, n, true
	case event == 3, code >= 57441 && code <= 57452, code >= 57358 && code <= 57360:
		return keySkip, n, true // a release; Shift, Ctrl, Alt, Super and the like alone; a lock key
	}
	if (mods-1)&4 != 0 && code >= 0x40 && code < 0x80 {
		code &= 0x1f // a Ctrl key: its legacy byte
	}
	return rune(code), n, true
}
