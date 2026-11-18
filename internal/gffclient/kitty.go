package gffclient

// The kitty keyboard protocol in the composed view (gff-client.md 8.2). gff asks the host
// whether it has the protocol (CSI ? u) once at start. When the host answers, gff pushes
// flag 1 (disambiguate) on the composed screen and pops it before it leaves that screen, as
// zellij does. While the flag is on, the host sends Esc, Ctrl, Alt and the keypad as CSI u key
// events; gff decodes them and encodes each one again for where it goes: legacy bytes for a
// pane that never pushed kitty flags and for gff's own sidebar, board and prompts, and the
// pane's own kitty flags, through its parser's encoder, for a pane that did. A host that never
// answers gets no push, and gff reads its keys as before.

import (
	"bytes"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// Host sequences of the kitty keyboard protocol.
const (
	kittyQuery = "\x1b[?u"  // ask for the current flags; a host with the protocol answers CSI ? flags u
	kittyPush  = "\x1b[>1u" // push flag 1, disambiguate escape codes
	kittyPop   = "\x1b[<u"  // pop one entry
)

// kittyReplyWait is how long after its query gff holds a read that ends inside a possible
// answer. A complete answer is taken whenever it comes.
const kittyReplyWait = time.Second

// takeKittyReplies removes the host's answers to kittyQuery from p and returns the rest. A
// program never sees one: the pass-through filter drops a pane's own CSI ? u (3.4), so every
// answer on the host's input is gff's. Shortly after the query, a read that ends inside a
// possible answer is held for the next.
func (c *client) takeKittyReplies(p []byte) []byte {
	if len(c.kittyHeld) > 0 {
		p = append(c.kittyHeld, p...)
		c.kittyHeld = nil
	}
	if !bytes.Contains(p, []byte("\x1b[")) {
		return p
	}
	var out []byte
	for i := 0; i < len(p); {
		j := bytes.Index(p[i:], []byte("\x1b["))
		if j < 0 {
			out = append(out, p[i:]...)
			break
		}
		out = append(out, p[i:i+j]...)
		i += j
		n, state := kittyReply(p[i:])
		switch state {
		case replyWhole:
			if !c.hostKitty {
				c.hostKitty = true
				c.cfg.Logf("the host terminal has the kitty keyboard protocol")
			}
			i += n
		case replyPartial:
			if c.cfg.Now().Sub(c.kittyAsked) < kittyReplyWait {
				c.kittyHeld = append([]byte(nil), p[i:]...)
				return out
			}
			fallthrough
		default:
			out = append(out, p[i:i+2]...)
			i += 2
		}
	}
	return out
}

type replyState int

const (
	replyNo replyState = iota
	replyWhole
	replyPartial
)

// kittyReply reads an answer to kittyQuery at the start of p: ESC [ ? digits u.
func kittyReply(p []byte) (int, replyState) {
	const head = "\x1b[?"
	if len(p) < len(head) {
		if bytes.HasPrefix([]byte(head), p) {
			return 0, replyPartial
		}
		return 0, replyNo
	}
	if string(p[:len(head)]) != head {
		return 0, replyNo
	}
	for i := len(head); i < len(p) && i < 16; i++ {
		switch c := p[i]; {
		case c >= '0' && c <= '9':
		case c == 'u' && i > len(head):
			return i + 1, replyWhole
		default:
			return 0, replyNo
		}
	}
	if len(p) < 16 {
		return 0, replyPartial
	}
	return 0, replyNo
}

// kittyIn re-encodes host input read while gff's kitty flag is on the host. It keeps a
// bracketed paste whole and holds a key sequence that a read cut.
type kittyIn struct {
	paste bool
	held  []byte
}

// translate turns p into bytes for a reader that takes keys from enc. With all, the reader
// asked for every key as an escape code (kitty flag 8), so text and Enter, Tab and Backspace
// become key events too; else they pass as they came, as do the legacy sequences the host
// still sends with flag 1 (arrows, Home, F keys), which read the same in both encodings.
func (x *kittyIn) translate(p []byte, enc func(vt.Key) []byte, all bool) []byte {
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
				out = append(out, x.seq(p[i:i+n], enc)...)
				i += n
			}
			continue
		}
		if !all {
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
			out = append(out, enc(k)...)
		} else {
			out = append(out, p[i:i+n]...)
		}
		i += n
	}
	return out
}

// seq re-encodes one complete CSI sequence: a kitty key event through enc, anything else as
// it came.
func (x *kittyIn) seq(s []byte, enc func(vt.Key) []byte) []byte {
	if s[len(s)-1] != 'u' || s[2] < '0' || s[2] > '9' {
		return s
	}
	k, ok := decodeKeyFull(s)
	if !ok {
		return s
	}
	return enc(kittyToKey(k))
}

// kittyToKey turns a decoded CSI u event into a vt.Key. A key that types text with no Ctrl,
// Alt or other modifier but Shift and the locks gets its text: the event's own text field, or
// the shifted key, or the key itself.
func kittyToKey(k kittyKey) vt.Key {
	key := vt.Key{Code: k.code, Mods: max(k.mods-1, 0), Event: k.event}
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

// legacyKey encodes k as a terminal without the kitty protocol sends it, the way xterm does
// with no modifyOtherKeys: control bytes for Ctrl, an ESC before an Alt key, CSI Z for
// Shift+Tab. appCursor picks the SS3 form of the keypad's arrows (DECCKM). It returns nil for a
// release, a modifier key alone, and a key legacy encoding has no bytes for.
func legacyKey(k vt.Key, appCursor bool) []byte {
	if k.Event == 3 {
		return nil
	}
	alt := k.Mods&2 != 0
	ctrl := k.Mods&4 != 0
	shift := k.Mods&1 != 0
	var b []byte
	switch code := k.Code; {
	case code == 27:
		b = []byte{0x1b}
	case code == 13 || code == 57414:
		b = []byte{'\r'}
	case code == 9 && shift:
		b = []byte("\x1b[Z")
	case code == 9:
		b = []byte{'\t'}
	case code == 127 && ctrl:
		b = []byte{0x08}
	case code == 127:
		b = []byte{0x7f}
	case code >= 57344:
		s, ok := keypadLegacy[code]
		if !ok {
			return nil
		}
		if len(s) == 1 {
			b = []byte(s)
			break
		}
		if appCursor && s[2] >= 'A' && s[2] <= 'H' {
			s = "\x1bO" + s[2:]
		}
		b = []byte(s)
	case ctrl:
		c, ok := ctrlLegacy(code)
		if !ok {
			return nil
		}
		b = []byte{c}
	case k.Text != "":
		b = []byte(k.Text)
	case code >= 0x20 && code < 57344:
		r := rune(code)
		if shift && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		b = utf8.AppendRune(nil, r)
	default:
		return nil
	}
	if alt {
		b = append([]byte{0x1b}, b...)
	}
	return b
}

// ctrlLegacy is the byte a legacy terminal sends for Ctrl and the key with code point c, as
// xterm maps it: Ctrl+@ and Ctrl+Space and Ctrl+2 NUL, Ctrl+a to z 1 to 26, Ctrl+[ ESC, and on.
// A key with no control form sends itself.
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

// keypadLegacy is what the keypad's keys send in a legacy terminal with the keypad in numeric
// mode, by kitty code: their character, or the arrow and editing keys' sequences.
var keypadLegacy = map[int]string{
	57399: "0", 57400: "1", 57401: "2", 57402: "3", 57403: "4", 57404: "5", 57405: "6",
	57406: "7", 57407: "8", 57408: "9", 57409: ".", 57410: "/", 57411: "*", 57412: "-",
	57413: "+", 57415: "=", 57416: ",",
	57417: "\x1b[D", 57418: "\x1b[C", 57419: "\x1b[A", 57420: "\x1b[B",
	57421: "\x1b[5~", 57422: "\x1b[6~", 57423: "\x1b[H", 57424: "\x1b[F",
	57425: "\x1b[2~", 57426: "\x1b[3~", 57427: "\x1b[E",
}

// kittyActive reports whether host input comes with gff's kitty flag on.
func (c *client) kittyActive() bool { return c.kittyOn && c.composed }

// syncKitty pushes gff's flag on the composed screen once the host has the protocol.
func (c *client) syncKitty() {
	if c.hostKitty && c.composed && !c.kittyOn {
		c.out.WriteString(kittyPush)
		c.kittyOn = true
	}
}

// popKitty pops gff's flag before the host leaves the composed screen.
func (c *client) popKitty() {
	if c.kittyOn {
		c.out.WriteString(kittyPop)
		c.kittyOn = false
	}
}

// paneKeys re-encodes host input for view v while gff's kitty flag is on: through the pane's
// own encoder when its program pushed kitty flags, else as legacy bytes.
func (c *client) paneKeys(v *view, p []byte) []byte {
	if !c.kittyActive() {
		return p
	}
	if v != nil && v.term != nil && v.live() {
		if f := v.term.KeyboardFlags(); f != 0 {
			if e, ok := v.term.(vt.KeyEncoder); ok {
				return c.kin.translate(p, e.EncodeKey, f&8 != 0)
			}
		}
	}
	appCursor := false
	if v != nil && v.term != nil {
		appCursor, _ = v.term.Mode(1, false)
	}
	return c.kin.translate(p, func(k vt.Key) []byte { return legacyKey(k, appCursor) }, false)
}

// uiKeys re-encodes host input for gff's own sidebar, board and prompts, which read legacy keys.
func (c *client) uiKeys(p []byte) []byte {
	if !c.kittyActive() {
		return p
	}
	return c.kin.translate(p, func(k vt.Key) []byte { return legacyKey(k, false) }, false)
}
