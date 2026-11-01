package gffclient

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

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

// keyEvent is what the key reader makes of host input: bytes for the focused pane (or the
// sidebar, board or a prompt), or an action of the keymap.
type keyEvent struct {
	data []byte
	act  string
	key  string // the key that made act, by its canonical name
}

// keyReader splits host input into pane bytes and actions, following the keymap's modes
// (gff-client.md 8.1). In normal and locked mode it watches for the mode's Ctrl keys and passes
// the rest; in prefix, pane and tab mode every key is a command key. It knows a Ctrl key in its
// legacy form (one control byte), in the kitty keyboard protocol's form (CSI code;5u) and in
// xterm's modifyOtherKeys form (CSI 27;5;code~). A host sends those forms while a pane in the
// pass-through view has that protocol or mode on.
type keyReader struct {
	km     *Keymap
	mode   Mode
	chords map[byte]bool // the Ctrl keys of the current chord mode
	held   []byte        // the start of a possible key sequence, cut at a read boundary
}

func newKeyReader(km *Keymap) *keyReader {
	k := &keyReader{km: km}
	k.setMode(ModeNormal)
	return k
}

func (k *keyReader) setMode(m Mode) {
	k.mode = m
	if m.chord() {
		k.chords = k.km.chords(m)
	}
}

// keySkip is commandKey's name for a key event that is no command: a key release, or a
// modifier key alone. The reader still waits for the command.
const keySkip = "\x00skip"

// act turns a key name into an event and moves the mode.
func (k *keyReader) act(name string) (keyEvent, bool) {
	a := k.km.lookup(k.mode, name)
	next := k.mode
	switch {
	case a == actPrefix:
		next = ModePrefix
	case a == actPaneMode:
		next = ModePane
	case a == actTabMode:
		next = ModeTab
	case a == actLock:
		next = ModeLocked
	case a == actUnlock, a == actBack:
		next = ModeNormal
	case a == "" && k.mode.sticky() && (name == "esc" || name == "enter"):
		next = ModeNormal // a way out of a mode always works
	case k.mode == ModePrefix:
		next = ModeNormal // one key, then back; an unbound key cancels
	}
	k.setMode(next)
	if a == "" {
		return keyEvent{}, false
	}
	return keyEvent{act: a, key: name}, true
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
		if k.mode.command() {
			name, n, ok := commandKey(p[i:])
			if !ok {
				k.held = append([]byte(nil), p[i:]...)
				return evs
			}
			i += n
			start = i
			if name == keySkip {
				continue
			}
			if ev, ok := k.act(name); ok {
				evs = append(evs, ev)
			}
			continue
		}
		if k.chords[p[i]] {
			flush(i)
			if ev, ok := k.act(ctrlName(p[i])); ok {
				evs = append(evs, ev)
			}
			i++
			start = i
			continue
		}
		if p[i] == 0x1b {
			n, b, m := k.chordSeq(p[i:])
			switch m {
			case seqChord:
				flush(i)
				if ev, ok := k.act(ctrlName(b)); ok {
					evs = append(evs, ev)
				}
				i += n
				start = i
				continue
			case seqChordRelease:
				// The chord's press never reached the pane, so its release does not either.
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
	seqChord
	seqChordRelease
	seqPartial
)

// maxKeySeq bounds a key sequence the reader holds across reads.
const maxKeySeq = 32

// chordCode is the code point of a Ctrl byte in the kitty and modifyOtherKeys forms: ctrl-a is
// 'a', ctrl-\ is '\'.
func chordCode(b byte) int {
	switch {
	case b == 0:
		return '@'
	case b >= 0x1b:
		return int(b | 0x40)
	}
	return int(b | 0x60)
}

// chordSeq matches one of the mode's Ctrl keys as a key sequence at the start of p, which
// starts with ESC: the kitty form CSI code;5u (an event type may follow the 5: :1 press, :2
// repeat, :3 release) or the modifyOtherKeys form CSI 27;5;code~. Caps Lock and Num Lock in
// the modifiers do not matter. It returns the Ctrl byte it matched.
func (k *keyReader) chordSeq(p []byte) (int, byte, seqMatch) {
	if len(p) < 2 || p[1] != '[' || len(k.chords) == 0 {
		return 0, 0, seqNo // a lone Esc is a key of its own; it is never held
	}
	n, complete := csiEnd(p)
	if n < 0 {
		return 0, 0, seqNo
	}
	if !complete {
		if len(p) >= maxKeySeq {
			return 0, 0, seqNo
		}
		f0, rest, more := strings.Cut(string(p[2:]), ";")
		f0, _, _ = strings.Cut(f0, ":")
		for b := range k.chords {
			code := strconv.Itoa(chordCode(b))
			switch {
			case !more && (strings.HasPrefix(code, f0) || strings.HasPrefix("27", f0)):
				return 0, 0, seqPartial
			case more && (f0 == code || f0 == "27") && !strings.ContainsAny(rest, "<=>?"):
				return 0, 0, seqPartial
			}
		}
		return 0, 0, seqNo
	}
	code, mods, event, ok := decodeKey(p[:n])
	if !ok || !ctrlOnly(mods) {
		return 0, 0, seqNo
	}
	for b := range k.chords {
		if chordCode(b) != code {
			continue
		}
		if event == 3 {
			return n, b, seqChordRelease
		}
		return n, b, seqChord
	}
	return 0, 0, seqNo
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

// commandKey reads one command key: one character, or a kitty or modifyOtherKeys key
// sequence, and names it as the keymap does (ParseKey). ok is false when p ends inside a key
// sequence. The name is "" for a key it cannot name, and keySkip for a key release or a
// modifier key alone.
func commandKey(p []byte) (name string, n int, ok bool) {
	if len(p) == 0 {
		return "", 0, false
	}
	if p[0] != 0x1b {
		if !utf8.FullRune(p) {
			return "", 0, false
		}
		r, n := utf8.DecodeRune(p)
		return runeName(r), n, true
	}
	if len(p) == 1 || (p[1] != '[' && p[1] != 'O') {
		return "esc", 1, true // a lone Esc
	}
	if p[1] == 'O' { // SS3: an arrow in cursor-key application mode
		if len(p) < 3 {
			return "", 0, false
		}
		return arrowName(p[2]), 3, true
	}
	n, complete := csiEnd(p)
	switch {
	case n < 0:
		return "esc", 1, true
	case !complete:
		if len(p) >= maxKeySeq {
			return "", len(p), true
		}
		return "", 0, false
	}
	if f := p[n-1]; f >= 'A' && f <= 'D' {
		return arrowName(f), n, true
	}
	code, mods, event, ok := decodeKey(p[:n])
	switch {
	case !ok:
		return "", n, true
	case event == 3, code >= 57441 && code <= 57452, code >= 57358 && code <= 57360:
		return keySkip, n, true // a release; Shift, Ctrl, Alt, Super and the like alone; a lock key
	}
	if (mods-1)&4 != 0 && code >= 0x40 && code < 0x80 {
		code &= 0x1f // a Ctrl key: its legacy byte
	}
	return runeName(rune(code)), n, true
}

func arrowName(f byte) string {
	switch f {
	case 'A':
		return "up"
	case 'B':
		return "down"
	}
	return ""
}

// runeName names one key by its character or control byte.
func runeName(r rune) string {
	switch {
	case r == '\r' || r == '\n':
		return "enter"
	case r == '\t':
		return "tab"
	case r == 0x1b:
		return "esc"
	case r == 0x7f || r == 0x08:
		return "backspace"
	case r >= 0 && r < 0x20:
		return ctrlName(byte(r))
	case r == utf8.RuneError:
		return ""
	}
	return string(r)
}
