package gffclient

import (
	"bytes"
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
// legacy form (one control byte) and in the kitty keyboard protocol's form (CSI code;5u),
// which a host sends when a pane in the pass-through view enabled that protocol.
type keyReader struct {
	prefix  byte
	kitty   []byte // CSI <code>;5 — the kitty form's start, without the final u or event type
	pending bool   // the prefix came; the next key is a command
	held    []byte // the start of a possible kitty prefix cut at a read boundary
}

func newKeyReader(prefix byte) *keyReader {
	code := prefix | 0x60 // ctrl-a is 'a', ctrl-\ is '\'
	if prefix == 0 {
		code = '@'
	} else if prefix >= 0x1b {
		code = prefix | 0x40
	}
	return &keyReader{prefix: prefix, kitty: []byte("\x1b[" + strconv.Itoa(int(code)) + ";5")}
}

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
			k.pending = false
			i += n
			start = i
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
			n, state := k.kittyPrefix(p[i:])
			switch state {
			case kittyYes:
				flush(i)
				k.pending = true
				i += n
				start = i
				continue
			case kittyPartial:
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

type kittyMatch int

const (
	kittyNo kittyMatch = iota
	kittyYes
	kittyPartial
)

// kittyPrefix matches the prefix in kitty form at the start of p: CSI code;5u, or with an
// event type, CSI code;5:1u (press) or :2u (repeat). A release (:3u) is not the prefix.
func (k *keyReader) kittyPrefix(p []byte) (int, kittyMatch) {
	n := min(len(p), len(k.kitty))
	if !bytes.Equal(p[:n], k.kitty[:n]) {
		return 0, kittyNo
	}
	if len(p) <= len(k.kitty) {
		return 0, kittyPartial
	}
	rest := p[len(k.kitty):]
	for _, tail := range []string{"u", ":1u", ":2u"} {
		if bytes.HasPrefix(rest, []byte(tail)) {
			return len(k.kitty) + len(tail), kittyYes
		}
		if len(rest) < len(tail) && strings.HasPrefix(tail, string(rest)) {
			return 0, kittyPartial
		}
	}
	return 0, kittyNo
}

// commandKey reads the key after the prefix: one byte, or a kitty CSI code[;mods]u key. ok is
// false when p ends inside a CSI. r is negative for a key it cannot name, such as an arrow.
func commandKey(p []byte) (r rune, n int, ok bool) {
	if len(p) == 0 {
		return 0, 0, false
	}
	if p[0] != 0x1b {
		return rune(p[0]), 1, true
	}
	if len(p) == 1 {
		return 0x1b, 1, true // a lone Esc cancels
	}
	if p[1] != '[' {
		return 0x1b, 1, true
	}
	for i := 2; i < len(p); i++ {
		c := p[i]
		if c >= 0x40 && c <= 0x7e {
			if c != 'u' {
				return -1, i + 1, true
			}
			code, mods, _ := strings.Cut(string(p[2:i]), ";")
			cp, err := strconv.Atoi(code)
			if err != nil {
				return -1, i + 1, true
			}
			if m, _, _ := strings.Cut(mods, ":"); m == "5" && cp >= 0x40 && cp < 0x80 {
				cp &= 0x1f // a kitty-encoded Ctrl key: its legacy byte
			}
			return rune(cp), i + 1, true
		}
	}
	return 0, 0, false
}
