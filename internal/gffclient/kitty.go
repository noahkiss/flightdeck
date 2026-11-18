package gffclient

// The kitty keyboard protocol in the composed view (gff-client.md 3.4, 8.2). gff asks the host
// whether it has the protocol once at start: CSI ? u, then a primary device attributes query
// (CSI c), as the protocol's spec describes. A host with the protocol answers the first; every
// terminal answers the second, which ends the detection. When the host has the protocol, gff
// pushes flags 1 and 4 (disambiguate, alternate keys) on the composed screen and pops them
// before it leaves that screen. While they are on, the host sends Esc, Ctrl, Alt and the keypad
// as CSI u key events, with the shifted key; gff decodes them and encodes each one again for
// where it goes: legacy bytes for a pane that never pushed kitty flags and for gff's own
// sidebar, board and prompts, and the pane's own kitty flags, through its parser's encoder, for
// a pane that did. A host that never answers gets no push, and gff reads its keys as before.

import (
	"bytes"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// Host sequences of the kitty keyboard protocol.
const (
	// kittyQuery asks for the current flags, then for the primary device attributes. A host
	// with the protocol answers CSI ? flags u first; every host answers CSI ? … c.
	kittyQuery = "\x1b[?u\x1b[c"
	kittyPush  = "\x1b[>5u" // push flags 1 and 4: disambiguate escape codes, report alternate keys
	kittyPop   = "\x1b[<u"  // pop one entry
)

// kittyDetectWait bounds the detection for a host that answers neither query. kittyDrainWait
// bounds how long gff, as it exits, waits for the answers still to come.
const (
	kittyDetectWait = 2 * time.Second
	kittyDrainWait  = 300 * time.Millisecond
)

// kittyDetecting reports whether the detection still waits for the host's answers.
func (c *client) kittyDetecting() bool {
	return c.kittyPending && c.cfg.Now().Sub(c.kittyAsked) < kittyDetectWait
}

// takeKittyReplies removes the host's answers to kittyQuery from p while the detection runs,
// and returns the rest. A program never sees one: the pass-through filter drops a pane's own
// CSI ? u and CSI c (3.4), so these answers are gff's. A bracketed paste is never searched. A
// read that ends inside a possible answer is held for the next. The DA1 answer ends the
// detection; after it gff takes nothing.
func (c *client) takeKittyReplies(p []byte) []byte {
	if len(c.kittyHeld) > 0 {
		p = append(c.kittyHeld, p...)
		c.kittyHeld = nil
	}
	if !c.kittyDetecting() {
		c.kittyPending = false
		return p
	}
	var out []byte
	for i := 0; i < len(p); {
		if !c.kittyPending {
			return append(out, p[i:]...)
		}
		if c.kittyPaste {
			j := bytes.Index(p[i:], pasteEnd)
			if j < 0 {
				return append(out, p[i:]...)
			}
			out = append(out, p[i:i+j+len(pasteEnd)]...)
			i += j + len(pasteEnd)
			c.kittyPaste = false
			continue
		}
		if bytes.HasPrefix(p[i:], pasteStart) {
			out = append(out, pasteStart...)
			i += len(pasteStart)
			c.kittyPaste = true
			continue
		}
		if p[i] != 0x1b {
			out = append(out, p[i])
			i++
			continue
		}
		n, state, final := kittyReply(p[i:])
		switch state {
		case replyWhole:
			if final == 'u' && !c.hostKitty {
				c.hostKitty = true
				c.cfg.Logf("the host terminal has the kitty keyboard protocol")
			}
			if final == 'c' {
				c.kittyPending = false
			}
			i += n
		case replyPartial:
			c.kittyHeld = append([]byte(nil), p[i:]...)
			return out
		default:
			out = append(out, p[i])
			i++
		}
	}
	return out
}

// drainKittyAnswer reads the host's input as gff exits until the detection's answers have come,
// or kittyDrainWait passes, so a late answer never lands in the shell gff leaves.
func (c *client) drainKittyAnswer(input <-chan []byte) {
	if !c.kittyDetecting() {
		return
	}
	t := time.NewTimer(kittyDrainWait)
	defer t.Stop()
	for c.kittyPending {
		select {
		case p := <-input:
			c.takeKittyReplies(p) // gff is leaving: the rest of the read is dropped
		case <-t.C:
			return
		}
	}
}

type replyState int

const (
	replyNo replyState = iota
	replyWhole
	replyPartial
)

// kittyReply reads an answer to kittyQuery at the start of p: ESC [ ? digits u, or the device
// attributes ESC [ ? digits and semicolons c. final is u or c.
func kittyReply(p []byte) (int, replyState, byte) {
	const head = "\x1b[?"
	const most = 64
	if len(p) < len(head) {
		if bytes.HasPrefix([]byte(head), p) {
			return 0, replyPartial, 0
		}
		return 0, replyNo, 0
	}
	if string(p[:len(head)]) != head {
		return 0, replyNo, 0
	}
	semi := false
	for i := len(head); i < len(p) && i < most; i++ {
		switch c := p[i]; {
		case c >= '0' && c <= '9':
		case c == ';':
			semi = true
		case c == 'u' && i > len(head) && !semi:
			return i + 1, replyWhole, 'u'
		case c == 'c' && i > len(head):
			return i + 1, replyWhole, 'c'
		default:
			return 0, replyNo, 0
		}
	}
	if len(p) < most {
		return 0, replyPartial, 0
	}
	return 0, replyNo, 0
}

// keyCodec is what a reader of keys wants: key encodes a decoded CSI u event; csi, when set,
// rewrites the other CSI sequences the host sends (cursor and function keys); all asks for
// every key as an escape code (kitty flag 8).
type keyCodec struct {
	key func(vt.Key) []byte
	csi func([]byte) []byte
	all bool
}

// kittyIn re-encodes host input read while gff's kitty flags are on the host. It keeps a
// bracketed paste whole and holds a key sequence that a read cut.
type kittyIn struct {
	paste bool
	held  []byte
}

// translate turns p into bytes for a reader that takes keys from codec. With codec.all, text
// and Enter, Tab and Backspace become key events too; else they pass as they came.
func (x *kittyIn) translate(p []byte, codec keyCodec) []byte {
	if len(x.held) > 0 {
		p = append(x.held, p...)
		x.held = nil
	}
	var out []byte
	for i := 0; i < len(p); {
		if x.paste {
			j := bytes.Index(p[i:], pasteEnd)
			if j < 0 {
				return append(out, p[i:]...)
			}
			out = append(out, p[i:i+j+len(pasteEnd)]...)
			i += j + len(pasteEnd)
			x.paste = false
			continue
		}
		if bytes.HasPrefix(p[i:], pasteStart) {
			out = append(out, pasteStart...)
			i += len(pasteStart)
			x.paste = true
			continue
		}
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '[' {
			n, complete := csiEnd(p[i:])
			switch {
			case n < 0:
				out = append(out, p[i])
				i++
			case !complete && len(p)-i < maxKeySeq:
				x.held = append([]byte(nil), p[i:]...)
				return out
			case !complete:
				out = append(out, p[i:]...)
				i = len(p)
			default:
				out = append(out, codec.seq(p[i:i+n])...)
				i += n
			}
			continue
		}
		if !codec.all {
			out = append(out, p[i])
			i++
			continue
		}
		if !utf8.FullRune(p[i:]) {
			x.held = append([]byte(nil), p[i:]...)
			return out
		}
		r, n := utf8.DecodeRune(p[i:])
		if k, ok := textKey(r); ok {
			out = append(out, codec.key(k)...)
		} else {
			out = append(out, p[i:i+n]...)
		}
		i += n
	}
	return out
}

// seq re-encodes one complete CSI sequence: a kitty key event through key, any other through
// csi, or as it came.
func (codec keyCodec) seq(s []byte) []byte {
	if s[len(s)-1] == 'u' && s[2] >= '0' && s[2] <= '9' {
		if k, ok := decodeKeyFull(s); ok {
			return codec.key(kittyToKey(k))
		}
		return s
	}
	if codec.csi != nil {
		return codec.csi(s)
	}
	return s
}

// kittyToKey turns a decoded CSI u event into a vt.Key. A key that types text with no Ctrl,
// Alt or other modifier but Shift and the locks gets its text: the event's own text field, or
// the shifted key, or the key itself.
func kittyToKey(k kittyKey) vt.Key {
	key := vt.Key{Code: k.code, Shifted: k.shifted, Mods: max(k.mods-1, 0), Event: k.event}
	if key.Mods&^(1|64|128) != 0 || k.code < 0x20 || k.code == 0x7f || k.code >= 57344 {
		return key
	}
	switch {
	case k.text > 0:
		key.Text = string(rune(k.text))
	case key.Mods&1 != 0 && k.shifted > 0:
		key.Text = string(rune(k.shifted))
	case key.Mods&1 != 0 && k.code >= 'a' && k.code <= 'z':
		key.Text = string(rune(k.code - 'a' + 'A'))
	default:
		key.Text = string(rune(k.code))
	}
	return key
}

// textKey makes a key event of a character the host sent as text, for a reader that wants
// every key as an escape code. An upper case ASCII letter is Shift and the letter; any other
// character is its own key, since gff cannot know the keyboard layout. Control characters other
// than Enter, Tab and Backspace pass as they came.
func textKey(r rune) (vt.Key, bool) {
	switch {
	case r == '\r':
		return vt.Key{Code: 13}, true
	case r == '\t':
		return vt.Key{Code: 9}, true
	case r == 0x7f:
		return vt.Key{Code: 127}, true
	case r < 0x20 || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError:
		return vt.Key{}, false
	case r >= 'A' && r <= 'Z':
		return vt.Key{Code: int(r - 'A' + 'a'), Mods: 1, Text: string(r)}, true
	}
	return vt.Key{Code: int(r), Text: string(r)}, true
}

// legacyModes are a legacy reader's key modes: cursor keys (DECCKM) and keypad (DECKPAM) in
// application mode.
type legacyModes struct {
	appCursor, appKeypad bool
}

// xtermMods is the modifier parameter xterm writes in CSI 1;m X and CSI n;m ~: 1 plus Shift 1,
// Alt 2, Ctrl 4 and Meta 8 (kitty's Super). 0 means no modifier.
func xtermMods(kitty int) int {
	if m := kitty & 15; m != 0 {
		return m + 1
	}
	return 0
}

// cursorSeq is a cursor-style key (final A to D, H, F, E, or P to S for F1 to F4) as xterm
// sends it: SS3 alone, CSI alone, or CSI 1;m with a modifier.
func cursorSeq(final byte, mods int, ss3 bool) []byte {
	switch {
	case mods != 0:
		return []byte("\x1b[1;" + strconv.Itoa(mods) + string(final))
	case ss3:
		return []byte{0x1b, 'O', final}
	}
	return []byte{0x1b, '[', final}
}

// tildeSeq is a CSI n ~ key with xterm's modifier parameter.
func tildeSeq(n int, mods int) []byte {
	if mods != 0 {
		return []byte("\x1b[" + strconv.Itoa(n) + ";" + strconv.Itoa(mods) + "~")
	}
	return []byte("\x1b[" + strconv.Itoa(n) + "~")
}

// legacyCSI rewrites a cursor or function key the host sent in kitty's form into xterm's. With
// kitty flags on, the host ignores DECCKM and sends F1 to F4 as CSI P, Q, 13 ~ and S (CSI R
// would read as a cursor position report): unmodified cursor keys become SS3 under DECCKM, F1
// to F4 SS3 P to S, and a modified F3 CSI 1;m R. Everything else passes as it came.
func legacyCSI(s []byte, m legacyModes) []byte {
	final := s[len(s)-1]
	body := string(s[2 : len(s)-1])
	switch final {
	case 'A', 'B', 'C', 'D', 'H', 'F':
		if (body == "" || body == "1") && m.appCursor {
			return []byte{0x1b, 'O', final}
		}
	case 'P', 'Q', 'S':
		if body == "" || body == "1" {
			return []byte{0x1b, 'O', final}
		}
	case '~':
		if body == "13" {
			return []byte("\x1bOR")
		}
		if mods, ok := bytes.CutPrefix([]byte(body), []byte("13;")); ok {
			return []byte("\x1b[1;" + string(mods) + "R")
		}
	}
	return s
}

// legacyKey encodes k as a terminal without the kitty protocol sends it, the way xterm does
// with no modifyOtherKeys: control bytes for Ctrl over the shifted key, an ESC before an Alt
// key, CSI Z for Shift+Tab, and the keypad by its modes and modifiers. It returns nil for a
// release, a modifier key alone, and a key legacy encoding has no bytes for.
func legacyKey(k vt.Key, m legacyModes) []byte {
	if k.Event == 3 {
		return nil
	}
	alt := k.Mods&2 != 0
	ctrl := k.Mods&4 != 0
	shift := k.Mods&1 != 0
	if k.Code >= 57344 {
		return keypadKey(k, m)
	}
	var b []byte
	switch code := k.Code; {
	case code == 27:
		b = []byte{0x1b}
	case code == 13:
		b = []byte{'\r'}
	case code == 9 && shift:
		b = []byte("\x1b[Z")
	case code == 9:
		b = []byte{'\t'}
	case code == 127 && ctrl:
		b = []byte{0x08}
	case code == 127:
		b = []byte{0x7f}
	default:
		r := rune(code)
		switch {
		case shift && k.Shifted > 0:
			r = rune(k.Shifted)
		case r >= 'a' && r <= 'z' && shift != (k.Mods&64 != 0): // Shift or Caps Lock, not both
			r -= 'a' - 'A'
		}
		if ctrl {
			if c, ok := ctrlLegacy(int(r)); ok {
				b = []byte{c}
				break
			}
		}
		switch {
		case k.Text != "":
			b = []byte(k.Text)
		case r >= 0x20:
			b = utf8.AppendRune(nil, r)
		default:
			return nil
		}
	}
	if alt {
		b = append([]byte{0x1b}, b...)
	}
	return b
}

// ctrlLegacy is the byte a legacy terminal sends for Ctrl and the character c, as xterm maps
// it: Ctrl+@, Ctrl+Space and Ctrl+2 NUL, Ctrl+a to z 1 to 26, Ctrl+[ ESC, Ctrl+_ and Ctrl+/ 0x1f,
// Ctrl+? 0x7f, and on. Another ASCII character sends itself. A character beyond ASCII has no
// control form: false.
func ctrlLegacy(c int) (byte, bool) {
	switch {
	case c >= 'a' && c <= 'z':
		return byte(c - 'a' + 1), true
	case c >= 'A' && c <= 'Z':
		return byte(c - 'A' + 1), true
	}
	switch c {
	case '@', ' ', '2':
		return 0, true
	case '[', '3':
		return 0x1b, true
	case '\\', '4':
		return 0x1c, true
	case ']', '5':
		return 0x1d, true
	case '^', '6':
		return 0x1e, true
	case '_', '7', '/':
		return 0x1f, true
	case '8', '?':
		return 0x7f, true
	}
	if c > 0x20 && c < 0x7f {
		return byte(c), true
	}
	return 0, false
}

// keypadKey encodes a keypad key, or another key kitty numbers from 57344, as xterm sends it.
// The keypad's arrows, Home, End and Begin are cursor keys, its Page, Insert and Delete keys
// CSI n ~ keys, each with xterm's modifier parameter. Its digits and operators send their
// character (Enter CR), with ESC before them for Alt, or in application keypad mode their SS3
// form. Other codes (media keys, modifiers alone) send nothing.
func keypadKey(k vt.Key, m legacyModes) []byte {
	mods := xtermMods(k.Mods)
	if f, ok := keypadCursor[k.Code]; ok {
		return cursorSeq(f, mods, m.appCursor)
	}
	if n, ok := keypadTilde[k.Code]; ok {
		return tildeSeq(n, mods)
	}
	t, ok := keypadText[k.Code]
	if !ok {
		return nil
	}
	if m.appKeypad {
		return []byte{0x1b, 'O', t.app}
	}
	b := []byte{t.char}
	if k.Mods&2 != 0 {
		b = append([]byte{0x1b}, b...)
	}
	return b
}

// The keypad by kitty code: cursor keys by their final, CSI n ~ keys by n, and text keys by
// their character and their application-mode SS3 final.
var (
	keypadCursor = map[int]byte{57417: 'D', 57418: 'C', 57419: 'A', 57420: 'B', 57423: 'H', 57424: 'F', 57427: 'E'}
	keypadTilde  = map[int]int{57421: 5, 57422: 6, 57425: 2, 57426: 3}
	keypadText   = map[int]struct{ char, app byte }{
		57399: {'0', 'p'}, 57400: {'1', 'q'}, 57401: {'2', 'r'}, 57402: {'3', 's'}, 57403: {'4', 't'},
		57404: {'5', 'u'}, 57405: {'6', 'v'}, 57406: {'7', 'w'}, 57407: {'8', 'x'}, 57408: {'9', 'y'},
		57409: {'.', 'n'}, 57410: {'/', 'o'}, 57411: {'*', 'j'}, 57412: {'-', 'm'}, 57413: {'+', 'k'},
		57414: {'\r', 'M'}, 57415: {'=', 'X'}, 57416: {',', 'l'},
	}
)

// kittyActive reports whether host input comes with gff's kitty flags on.
func (c *client) kittyActive() bool { return c.kittyOn && c.composed }

// syncKitty pushes gff's flags on the composed screen once the host has the protocol.
func (c *client) syncKitty() {
	if c.hostKitty && c.composed && !c.kittyOn {
		c.out.WriteString(kittyPush)
		c.kittyOn = true
	}
}

// popKitty pops gff's flags before the host leaves the composed screen, and drops what the
// re-encoder held: input after it comes in another encoding.
func (c *client) popKitty() {
	if c.kittyOn {
		c.out.WriteString(kittyPop)
		c.kittyOn = false
	}
	c.kin = kittyIn{}
}

// legacyCodec encodes keys for a legacy reader with modes m.
func legacyCodec(m legacyModes) keyCodec {
	return keyCodec{
		key: func(k vt.Key) []byte { return legacyKey(k, m) },
		csi: func(s []byte) []byte { return legacyCSI(s, m) },
	}
}

// paneKeys re-encodes host input for view v while gff's kitty flags are on: through the pane's
// own encoder when its program pushed kitty flags, else as legacy bytes by the pane's modes.
func (c *client) paneKeys(v *view, p []byte) []byte {
	if !c.kittyActive() {
		return p
	}
	var m legacyModes
	if v != nil && v.term != nil {
		if v.live() {
			if f := v.term.KeyboardFlags(); f != 0 {
				if e, ok := v.term.(vt.KeyEncoder); ok {
					return c.kin.translate(p, keyCodec{key: e.EncodeKey, all: f&8 != 0})
				}
			}
		}
		m.appCursor, _ = v.term.Mode(1, false)
		m.appKeypad, _ = v.term.Mode(66, false)
	}
	return c.kin.translate(p, legacyCodec(m))
}

// uiKeys re-encodes host input for gff's own sidebar, board and prompts, which read legacy keys.
func (c *client) uiKeys(p []byte) []byte {
	if !c.kittyActive() {
		return p
	}
	return c.kin.translate(p, legacyCodec(legacyModes{}))
}
