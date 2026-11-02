package gffclient

import (
	"bytes"
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

// The bracketed paste markers (DECSET 2004). A host sends them around a paste while gff
// mirrors the mode for the focused pane.
var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// keyReader splits host input into pane bytes and actions, following the keymap's modes
// (gff-client.md 8.1). In normal and locked mode it watches for the mode's Ctrl keys and passes
// the rest; in session, pane and tab mode every key is a command key. It knows a Ctrl key in its
// legacy form (one control byte), in the kitty keyboard protocol's form (CSI code;5u) and in
// xterm's modifyOtherKeys form (CSI 27;5;code~). A host sends those forms while a pane in the
// pass-through view has that protocol or mode on.
//
// A bracketed paste is never keys: it goes to the pane whole, markers and all, in any mode. A
// paste that comes in a command mode ends the mode first.
type keyReader struct {
	km     *Keymap
	mode   Mode
	chords map[byte]bool // the Ctrl keys of the current chord mode
	alts   map[byte]bool // the Alt keys of the current chord mode, by their character
	held   []byte        // the start of a possible key sequence, cut at a read boundary
	entry  string        // the key that entered the current command mode
	paste  bool          // inside a bracketed paste
	// taken lists the keys whose press the reader took, by key id (commandKey). A host in
	// kitty's event mode sends each one's release later, perhaps in another mode; the reader
	// drops it there too.
	taken map[int]bool
}

// maxTaken bounds taken: a host that sends no releases never empties it.
const maxTaken = 32

func newKeyReader(km *Keymap) *keyReader {
	k := &keyReader{km: km, taken: map[int]bool{}}
	k.setMode(ModeNormal)
	return k
}

func (k *keyReader) setMode(m Mode) {
	k.mode = m
	if m.chord() {
		k.chords = k.km.chords(m)
		k.alts = k.km.alts(m)
	}
}

// take records that the reader took the press of key id.
func (k *keyReader) take(id int) {
	if id == 0 {
		return
	}
	if len(k.taken) >= maxTaken {
		clear(k.taken)
	}
	k.taken[id] = true
}

// keySkip is commandKey's name for a key event that is no command: a key release, or a
// modifier key alone. The reader still waits for the command.
const keySkip = "\x00skip"

// act turns a key name into an event and moves the mode.
func (k *keyReader) act(name string) (keyEvent, bool) {
	a := k.km.lookup(k.mode, name)
	if a == "" && k.mode.command() && name == k.entry && k.entry != "" {
		a = actSend // the key that entered the mode, pressed again, sends itself
	}
	next := k.mode
	switch {
	case a == actSession:
		next = ModeSession
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
	case k.mode.sticky() && a != "" && !stays(a):
		next = ModeNormal // as zellij: a command ends pane or tab mode, a focus or tab move does not
	case k.mode == ModeSession:
		next = ModeNormal // one key, then back; an unbound key cancels
	}
	if next != k.mode && next.command() {
		k.entry = name
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
		start = end
	}
	hold := func(i int) []keyEvent {
		flush(i)
		k.held = append([]byte(nil), p[i:]...)
		return evs
	}
	for i := 0; i < len(p); {
		if k.paste {
			j := bytes.Index(p[i:], pasteEnd)
			if j < 0 {
				return hold(len(p) - partialTail(p[i:], pasteEnd))
			}
			i += j + len(pasteEnd)
			k.paste = false
			continue
		}
		if bytes.HasPrefix(p[i:], pasteStart) {
			if !k.mode.chord() {
				k.setMode(ModeNormal) // a paste ends a command mode; it goes to the pane
				start = i
			}
			k.paste = true
			i += len(pasteStart)
			continue
		}
		if k.mode.command() {
			if c, ok := altByte(p[i:]); ok {
				// ESC and a character in one read: one Alt key, bound or not, as its kitty
				// form is. A legacy key sends no release, so the reader takes nothing.
				if ev, ok := k.act(altName(c)); ok {
					evs = append(evs, ev)
				}
				i += 2
				start = i
				continue
			}
			name, id, n, ok := commandKey(p[i:])
			if !ok {
				return hold(i)
			}
			i += n
			start = i
			if name == keySkip {
				delete(k.taken, id)
				continue
			}
			k.take(id)
			if ev, ok := k.act(name); ok {
				evs = append(evs, ev)
			}
			continue
		}
		if k.chords[p[i]] {
			flush(i)
			k.take(chordCode(p[i]))
			if ev, ok := k.act(ctrlName(p[i])); ok {
				evs = append(evs, ev)
			}
			i++
			start = i
			continue
		}
		if c, ok := altByte(p[i:]); ok && k.alts[c] {
			flush(i) // a legacy key sends no release: nothing to take
			if ev, ok := k.act(altName(c)); ok {
				evs = append(evs, ev)
			}
			i += 2
			start = i
			continue
		}
		if p[i] == 0x1b {
			n, name, id, m := k.chordSeq(p[i:])
			switch m {
			case seqChord:
				flush(i)
				k.take(id)
				if ev, ok := k.act(name); ok {
					evs = append(evs, ev)
				}
				i += n
				start = i
				continue
			case seqChordRelease:
				// The key's press never reached the pane, so its release does not either.
				flush(i)
				i += n
				start = i
				continue
			case seqPartial:
				return hold(i)
			}
		}
		i++
	}
	flush(len(p))
	return evs
}

// partialTail is the length of the longest end of p that starts marker without being all of
// it: what to hold when a read ends inside the marker.
func partialTail(p, marker []byte) int {
	for n := min(len(p), len(marker)-1); n > 0; n-- {
		if bytes.HasPrefix(marker, p[len(p)-n:]) {
			return n
		}
	}
	return 0
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

// altByte reads an Alt key in its legacy form at the start of p: ESC and one printable ASCII
// character other than [ and O, which start CSI and SS3 sequences.
func altByte(p []byte) (byte, bool) {
	if len(p) < 2 || p[0] != 0x1b {
		return 0, false
	}
	c := p[1]
	return c, c > 0x20 && c < 0x7f && c != '[' && c != 'O'
}

// altName is the canonical name of the Alt key of character c: alt-h.
func altName(c byte) string { return "alt-" + string(rune(c)) }

// chordSeq matches a sequence at the start of p, which starts with ESC: one of the mode's Ctrl
// or Alt keys in the kitty form CSI code;5u or code;3u (an event type may follow the
// modifiers: :1 press, :2 repeat, :3 release) or the modifyOtherKeys form CSI 27;5;code~ or
// 27;3;code~; or the release of a key whose press the reader took. Caps Lock and Num Lock in
// the modifiers do not matter. It returns the key's name and kitty id. The start of a
// bracketed paste marker is partial.
func (k *keyReader) chordSeq(p []byte) (int, string, int, seqMatch) {
	if len(p) < 2 || p[1] != '[' {
		return 0, "", 0, seqNo // a lone Esc is a key of its own; it is never held
	}
	n, complete := csiEnd(p)
	if n < 0 {
		return 0, "", 0, seqNo
	}
	if !complete {
		if len(p) >= maxKeySeq {
			return 0, "", 0, seqNo
		}
		if bytes.HasPrefix(pasteStart, p) {
			return 0, "", 0, seqPartial
		}
		f0, rest, more := strings.Cut(string(p[2:]), ";")
		f0, _, _ = strings.Cut(f0, ":")
		if strings.ContainsAny(rest, "<=>?") {
			return 0, "", 0, seqNo
		}
		var codes []string
		if len(k.chords)+len(k.alts) > 0 {
			codes = append(codes, "27")
		}
		for b := range k.chords {
			codes = append(codes, strconv.Itoa(chordCode(b)))
		}
		for c := range k.alts {
			codes = append(codes, strconv.Itoa(int(c)))
		}
		for id := range k.taken {
			if id > 0 {
				codes = append(codes, strconv.Itoa(id))
			} else {
				codes = append(codes, "1") // an arrow's release: CSI 1;1:3A
			}
		}
		for _, code := range codes {
			if (!more && strings.HasPrefix(code, f0)) || (more && f0 == code) {
				return 0, "", 0, seqPartial
			}
		}
		return 0, "", 0, seqNo
	}
	if id, release := keyRelease(p[:n]); release && k.taken[id] {
		delete(k.taken, id)
		return n, "", 0, seqChordRelease
	}
	if len(k.chords)+len(k.alts) == 0 {
		return 0, "", 0, seqNo
	}
	code, mods, event, ok := decodeKey(p[:n])
	if !ok {
		return 0, "", 0, seqNo
	}
	name := ""
	switch {
	case ctrlOnly(mods):
		for b := range k.chords {
			if chordCode(b) == code {
				name = ctrlName(b)
			}
		}
	case altOnly(mods):
		if code > 0 && code < 0x80 && k.alts[byte(code)] {
			name = altName(byte(code))
		}
	}
	switch {
	case name == "":
		return 0, "", 0, seqNo
	case event == 3:
		return n, name, code, seqChordRelease
	}
	return n, name, code, seqChord
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

// keyRelease reads a complete CSI sequence as a kitty key event. id names the key, as
// commandKey does, or is 0; release is true for a release event (:3).
func keyRelease(seq []byte) (id int, release bool) {
	final := seq[len(seq)-1]
	body := string(seq[2 : len(seq)-1])
	if body == "" || strings.ContainsAny(body[:1], "<=>?") {
		return 0, false
	}
	fields := strings.Split(body, ";")
	if len(fields) < 2 {
		return 0, false
	}
	_, e, _ := strings.Cut(fields[1], ":")
	switch {
	case final == 'u':
		c, _, _ := strings.Cut(fields[0], ":")
		if id, _ = strconv.Atoi(c); id <= 0 {
			return 0, false
		}
	case final >= 'A' && final <= 'D' && fields[0] == "1":
		id = -int(final)
	default:
		return 0, false
	}
	return id, e == "3"
}

// decodeKey reads a complete key sequence: kitty's CSI code[:alternates][;mods[:event][;text]]u,
// or modifyOtherKeys' CSI 27;mods;code~. mods is 1 plus the modifier bits; event is 1 for a
// press, 2 for a repeat and 3 for a release.
func decodeKey(seq []byte) (code, mods, event int, ok bool) {
	k, ok := decodeKeyFull(seq)
	return k.code, k.mods, k.event, ok
}

// kittyKey is one decoded key sequence. shifted is the kitty shifted alternate key and text the
// first code point of the text field, each 0 when absent.
type kittyKey struct {
	code, shifted, text, mods, event int
}

func decodeKeyFull(seq []byte) (kittyKey, bool) {
	final := seq[len(seq)-1]
	body := string(seq[2 : len(seq)-1])
	if body == "" || strings.ContainsAny(body[:1], "<=>?") {
		return kittyKey{}, false
	}
	fields := strings.Split(body, ";")
	k := kittyKey{mods: 1, event: 1}
	var err error
	switch final {
	case 'u':
		alts := strings.Split(fields[0], ":")
		if k.code, err = strconv.Atoi(alts[0]); err != nil {
			return kittyKey{}, false
		}
		if len(alts) > 1 && alts[1] != "" {
			k.shifted, _ = strconv.Atoi(alts[1])
		}
		if len(fields) > 1 && fields[1] != "" {
			m, e, hasEvent := strings.Cut(fields[1], ":")
			if k.mods, err = strconv.Atoi(m); err != nil {
				return kittyKey{}, false
			}
			if hasEvent {
				if k.event, err = strconv.Atoi(e); err != nil {
					return kittyKey{}, false
				}
			}
		}
		if len(fields) > 2 && fields[2] != "" {
			t, _, _ := strings.Cut(fields[2], ":")
			k.text, _ = strconv.Atoi(t)
		}
	case '~':
		if len(fields) != 3 || fields[0] != "27" {
			return kittyKey{}, false
		}
		if k.mods, err = strconv.Atoi(fields[1]); err != nil {
			return kittyKey{}, false
		}
		if k.code, err = strconv.Atoi(fields[2]); err != nil {
			return kittyKey{}, false
		}
	default:
		return kittyKey{}, false
	}
	return k, true
}

// ctrlOnly reports whether mods is Ctrl and no other modifier, Caps Lock and Num Lock aside.
func ctrlOnly(mods int) bool { return (mods-1)&^(64|128) == 4 }

// altOnly reports whether mods is Alt and no other modifier, Caps Lock and Num Lock aside.
func altOnly(mods int) bool { return (mods-1)&^(64|128) == 2 }

// Kitty's code points for keys that are no command alone: Shift, Ctrl, Alt, Super, Hyper,
// Meta (left and right), ISO Level 3 (AltGr) and Level 5 shift; and the lock keys.
func modifierKey(code int) bool {
	return (code >= 57441 && code <= 57454) || (code >= 57358 && code <= 57360)
}

// commandKey reads one command key: one character, or a kitty or modifyOtherKeys key
// sequence, and names it as the keymap does (ParseKey). id names the physical key, for
// matching its release later: its kitty code, or an arrow's final negated. ok is false when
// p ends inside a key sequence. The name is "" for a key it cannot name, and keySkip for a key
// release or a modifier key alone.
func commandKey(p []byte) (name string, id, n int, ok bool) {
	if len(p) == 0 {
		return "", 0, 0, false
	}
	if p[0] != 0x1b {
		if !utf8.FullRune(p) {
			return "", 0, 0, false
		}
		r, n := utf8.DecodeRune(p)
		return runeName(r), runeID(r), n, true
	}
	if len(p) == 1 || (p[1] != '[' && p[1] != 'O') {
		return "esc", 0x1b, 1, true // a lone Esc
	}
	if p[1] == 'O' { // SS3: an arrow in cursor-key application mode
		if len(p) < 3 {
			return "", 0, 0, false
		}
		return arrowName(p[2]), -int(p[2]), 3, true
	}
	n, complete := csiEnd(p)
	switch {
	case n < 0:
		return "esc", 0x1b, 1, true
	case !complete:
		if len(p) >= maxKeySeq {
			return "", 0, len(p), true
		}
		return "", 0, 0, false
	}
	if f := p[n-1]; f >= 'A' && f <= 'D' {
		id, release := keyRelease(p[:n])
		if release {
			return keySkip, id, n, true
		}
		return arrowName(f), -int(f), n, true
	}
	k, ok := decodeKeyFull(p[:n])
	if !ok {
		return "", 0, n, true
	}
	id = k.code
	if k.event == 3 || modifierKey(k.code) {
		return keySkip, id, n, true // a release; Shift, Ctrl, Alt, Super and the like alone; a lock key
	}
	code := k.code
	switch mods := k.mods - 1; {
	case mods&2 != 0:
		// Alt: alt-<character> alone; Alt with Ctrl or Shift names no key.
		if mods&(4|1) == 0 && code > 0x20 && code < 0x7f && code != '[' && code != 'O' {
			return altName(byte(code)), id, n, true
		}
		return "", id, n, true
	case mods&4 != 0:
		if code >= 0x40 && code < 0x80 {
			code &= 0x1f // a Ctrl key: its legacy byte
		}
	case mods&1 != 0:
		// Shift: the shifted key, from the alternate or the text; a letter upper-cased; else no
		// name. Never the unshifted key: Shift+1 is not 1.
		switch {
		case k.shifted > 0:
			code = k.shifted
		case k.text > 0:
			code = k.text
		case code >= 'a' && code <= 'z':
			code -= 'a' - 'A'
		case code < 0x20 || code == 0x7f:
			// Shift+Tab, Shift+Enter and the like: the key itself.
		default:
			return "", id, n, true
		}
	}
	return runeName(rune(code)), id, n, true
}

// runeID is the kitty key id of a key typed as one character: its code point, or the code of
// its Ctrl key.
func runeID(r rune) int {
	switch {
	case r == 0x08:
		return 0x7f
	case r == '\n':
		return '\r'
	case r == '\r' || r == '\t' || r == 0x1b || r == 0x7f:
	case r >= 0 && r < 0x20:
		return chordCode(byte(r))
	}
	return int(r)
}

func arrowName(f byte) string {
	switch f {
	case 'A':
		return "up"
	case 'B':
		return "down"
	case 'C':
		return "right"
	case 'D':
		return "left"
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
