//go:build ghostty

package ghostty

import (
	"github.com/noahkiss/flightdeck/internal/vt"
	"go.mitchellh.com/libghostty"
)

var _ vt.KeyEncoder = (*terminal)(nil)

// EncodeKey encodes k with libghostty's key encoder, set from the terminal's own modes and
// keyboard flags. gff calls it for a program that pushed kitty flags; libghostty's legacy
// encoding differs from xterm's for a few keys (it sends Ctrl+I as CSI 105;5u), so gff encodes
// legacy keys itself.
func (t *terminal) EncodeKey(k vt.Key) []byte {
	if t.t == nil {
		return nil
	}
	if t.enc == nil {
		enc, err := libghostty.NewKeyEncoder()
		if err != nil {
			return nil
		}
		ev, err := libghostty.NewKeyEvent()
		if err != nil {
			enc.Close()
			return nil
		}
		t.enc, t.ev = enc, ev
	}
	// SetFromTerminal resets option-as-alt; Alt is always Alt here, as gff decoded it.
	if t.enc.SetFromTerminal(t.t) != nil || t.enc.SetOptionAsAlt(libghostty.OptionAsAltTrue) != nil {
		return nil
	}
	key, cp := ghosttyKey(k.Code)
	action := libghostty.KeyActionPress
	switch k.Event {
	case 2:
		action = libghostty.KeyActionRepeat
	case 3:
		action = libghostty.KeyActionRelease
	}
	mods := ghosttyMods(k.Mods)
	var consumed libghostty.Mods
	if k.Text != "" && mods&libghostty.ModShift != 0 {
		consumed = libghostty.ModShift // Shift made the text, as a keyboard layout does
	}
	if t.ev.SetAction(action) != nil || t.ev.SetKey(key) != nil || t.ev.SetMods(mods) != nil ||
		t.ev.SetConsumedMods(consumed) != nil || t.ev.SetUTF8(k.Text) != nil ||
		t.ev.SetUnshiftedCodepoint(cp) != nil {
		return nil
	}
	b, err := t.enc.Encode(t.ev)
	if err != nil {
		return nil
	}
	return b
}

// ghosttyMods turns kitty's modifier bits into libghostty's.
func ghosttyMods(m int) libghostty.Mods {
	var g libghostty.Mods
	for _, b := range []struct {
		kitty int
		mod   libghostty.Mods
	}{
		{1, libghostty.ModShift}, {2, libghostty.ModAlt}, {4, libghostty.ModCtrl}, {8, libghostty.ModSuper},
		{64, libghostty.ModCapsLock}, {128, libghostty.ModNumLock},
	} {
		if m&b.kitty != 0 {
			g |= b.mod
		}
	}
	return g
}

// ghosttyKey returns libghostty's physical key for a kitty key code, and the unshifted code point
// the encoder writes for it (0 for a key with none). A code point with no key of its own on a US
// keyboard, such as é, is an unidentified key with that code point.
func ghosttyKey(code int) (libghostty.Key, rune) {
	switch {
	case code >= 'a' && code <= 'z':
		return libghostty.KeyA + libghostty.Key(code-'a'), rune(code)
	case code >= 'A' && code <= 'Z':
		return libghostty.KeyA + libghostty.Key(code-'A'), rune(code - 'A' + 'a')
	case code >= '1' && code <= '9':
		return libghostty.KeyDigit1 + libghostty.Key(code-'1'), rune(code)
	}
	if k, ok := textKeys[code]; ok {
		return k, rune(code)
	}
	if k, ok := funcKeys[code]; ok {
		return k, 0
	}
	if code >= 0x20 && code < 57344 {
		return libghostty.KeyUnidentified, rune(code)
	}
	return libghostty.KeyUnidentified, 0
}

// textKeys are the keys that type a character, other than letters and 1 to 9.
var textKeys = map[int]libghostty.Key{
	'0': libghostty.KeyDigit0, ' ': libghostty.KeySpace, '`': libghostty.KeyBackquote,
	'\\': libghostty.KeyBackslash, '[': libghostty.KeyBracketLeft, ']': libghostty.KeyBracketRight,
	',': libghostty.KeyComma, '=': libghostty.KeyEqual, '-': libghostty.KeyMinus,
	'.': libghostty.KeyPeriod, '\'': libghostty.KeyQuote, ';': libghostty.KeySemicolon,
	'/': libghostty.KeySlash,
}

// funcKeys are the keys with no text, by kitty code: Esc, Enter, Tab and Backspace by their
// control code, the rest by kitty's numbers from 57344.
var funcKeys = map[int]libghostty.Key{
	27: libghostty.KeyEscape, 13: libghostty.KeyEnter, 9: libghostty.KeyTab, 127: libghostty.KeyBackspace,
	57348: libghostty.KeyInsert, 57349: libghostty.KeyDelete, 57350: libghostty.KeyArrowLeft,
	57351: libghostty.KeyArrowRight, 57352: libghostty.KeyArrowUp, 57353: libghostty.KeyArrowDown,
	57354: libghostty.KeyPageUp, 57355: libghostty.KeyPageDown, 57356: libghostty.KeyHome,
	57357: libghostty.KeyEnd, 57358: libghostty.KeyCapsLock, 57359: libghostty.KeyScrollLock,
	57360: libghostty.KeyNumLock, 57361: libghostty.KeyPrintScreen, 57362: libghostty.KeyPause,
	57363: libghostty.KeyContextMenu,
	57364: libghostty.KeyF1, 57365: libghostty.KeyF2, 57366: libghostty.KeyF3, 57367: libghostty.KeyF4,
	57368: libghostty.KeyF5, 57369: libghostty.KeyF6, 57370: libghostty.KeyF7, 57371: libghostty.KeyF8,
	57372: libghostty.KeyF9, 57373: libghostty.KeyF10, 57374: libghostty.KeyF11, 57375: libghostty.KeyF12,
	57376: libghostty.KeyF13, 57377: libghostty.KeyF14, 57378: libghostty.KeyF15, 57379: libghostty.KeyF16,
	57380: libghostty.KeyF17, 57381: libghostty.KeyF18, 57382: libghostty.KeyF19, 57383: libghostty.KeyF20,
	57384: libghostty.KeyF21, 57385: libghostty.KeyF22, 57386: libghostty.KeyF23, 57387: libghostty.KeyF24,
	57388: libghostty.KeyF25,
	57399: libghostty.KeyNumpad0, 57400: libghostty.KeyNumpad1, 57401: libghostty.KeyNumpad2,
	57402: libghostty.KeyNumpad3, 57403: libghostty.KeyNumpad4, 57404: libghostty.KeyNumpad5,
	57405: libghostty.KeyNumpad6, 57406: libghostty.KeyNumpad7, 57407: libghostty.KeyNumpad8,
	57408: libghostty.KeyNumpad9, 57409: libghostty.KeyNumpadDecimal, 57410: libghostty.KeyNumpadDivide,
	57411: libghostty.KeyNumpadMultiply, 57412: libghostty.KeyNumpadSubtract, 57413: libghostty.KeyNumpadAdd,
	57414: libghostty.KeyNumpadEnter, 57415: libghostty.KeyNumpadEqual, 57416: libghostty.KeyNumpadSeparator,
	57417: libghostty.KeyNumpadLeft, 57418: libghostty.KeyNumpadRight, 57419: libghostty.KeyNumpadUp,
	57420: libghostty.KeyNumpadDown, 57421: libghostty.KeyNumpadPageUp, 57422: libghostty.KeyNumpadPageDown,
	57423: libghostty.KeyNumpadHome, 57424: libghostty.KeyNumpadEnd, 57425: libghostty.KeyNumpadInsert,
	57426: libghostty.KeyNumpadDelete, 57427: libghostty.KeyNumpadBegin,
	57441: libghostty.KeyShiftLeft, 57442: libghostty.KeyControlLeft, 57443: libghostty.KeyAltLeft,
	57444: libghostty.KeyMetaLeft, 57447: libghostty.KeyShiftRight, 57448: libghostty.KeyControlRight,
	57449: libghostty.KeyAltRight, 57450: libghostty.KeyMetaRight,
}
