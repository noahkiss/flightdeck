package gffclient

import (
	"reflect"
	"strings"
	"testing"
)

// barHints is mode m's hints as the bar writes them with room for all.
func barHints(km *Keymap, m Mode) string {
	var all []string
	for _, h := range km.hints(m) {
		all = append(all, h.full())
	}
	return strings.Join(all, "  ")
}

// The default keymap is modes with Ctrl leaders, as zellij's (gff-client.md 8.4, 15.5): the
// leaders enter their modes from normal and from each other's modes, each mode binds its
// actions, a focus or tab move keeps pane or tab mode and any other command ends it, and the
// bar lists each mode's keys.
func TestDefaultKeymap(t *testing.T) {
	km := DefaultKeymap()
	for _, tc := range []struct {
		mode Mode
		hint string
	}{
		{ModeNormal, "Ctrl+G lock  Ctrl+P pane  Ctrl+T tab  Ctrl+O session  Alt+h/l/j/k move focus  Alt+i/o move tab"},
		{ModeLocked, "Ctrl+G unlock"},
		{ModeSession, "w sidebar  g board  b bars  ! warnings  r redraw  n nudge  f fit  d detach  Esc back  " +
			"Ctrl+G lock  Ctrl+P pane  Ctrl+T tab  Alt+h/l/j/k move focus  Alt+i/o move tab"},
		{ModePane, "p/Tab focus  f zoom  s split  x close view  Esc/Enter back  Ctrl+G lock  Ctrl+T tab  Ctrl+O session  Alt+h/l/j/k move focus  Alt+i/o move tab"},
		{ModeTab, "n new tab  h/←/k/↑ previous tab  l/→/j/↓ next tab  1-9 go to tab  Esc/Enter back  " +
			"Ctrl+G lock  Ctrl+P pane  Ctrl+O session  Alt+h/l/j/k move focus  Alt+i/o move tab"},
	} {
		if got := barHints(km, tc.mode); got != tc.hint {
			t.Errorf("%s mode bar:\n got %q\nwant %q", tc.mode, got, tc.hint)
		}
	}
	// The keys the owner's zellij setup leaves to programs stay with them: Ctrl+N and Ctrl+H
	// (resize and move modes gff does not have), Ctrl+S, Ctrl+B, Ctrl+Q, and Alt+F and Alt+B for
	// the shell's word moves.
	for _, k := range []string{"ctrl-s", "ctrl-b", "ctrl-q", "ctrl-n", "alt-f", "alt-b"} {
		if a := km.lookup(ModeNormal, k); a != "" {
			t.Errorf("normal mode takes %s for %s", k, a)
		}
	}
	if p := km.exits("x"); len(p) != 0 {
		t.Errorf("default keymap problems %q", p)
	}

	d := func(s string) keyEvent { return keyEvent{data: []byte(s)} }
	a := func(act, key string) keyEvent { return keyEvent{act: act, key: key} }
	for _, tc := range []struct {
		name string
		in   string
		want []keyEvent
		mode Mode
	}{
		{"focus keeps pane mode", "\x10p\t", []keyEvent{a(actPaneMode, "ctrl-p"), a(actFocusNext, "p"), a(actFocusNext, "tab")}, ModePane},
		{"zoom ends pane mode", "\x10fx", []keyEvent{a(actPaneMode, "ctrl-p"), a(actZoom, "f"), d("x")}, ModeNormal},
		{"close view ends pane mode", "\x10xs", []keyEvent{a(actPaneMode, "ctrl-p"), a(actCloseView, "x"), d("s")}, ModeNormal},
		{"tab moves keep tab mode", "\x14l\x1b[Dhkj", []keyEvent{a(actTabMode, "ctrl-t"), a(actNextTab, "l"), a(actPrevTab, "left"),
			a(actPrevTab, "h"), a(actPrevTab, "k"), a(actNextTab, "j")}, ModeTab},
		{"new tab ends tab mode", "\x14nx", []keyEvent{a(actTabMode, "ctrl-t"), a(actNewTab, "n"), d("x")}, ModeNormal},
		{"a digit ends tab mode", "\x147x", []keyEvent{a(actTabMode, "ctrl-t"), a(actTabN+"7", "7"), d("x")}, ModeNormal},
		{"session mode takes one key", "\x0fdx", []keyEvent{a(actSession, "ctrl-o"), a(actDetach, "d"), d("x")}, ModeNormal},
		{"leaders switch modes", "\x10\x14\x0f\x10", []keyEvent{a(actPaneMode, "ctrl-p"), a(actTabMode, "ctrl-t"),
			a(actSession, "ctrl-o"), a(actPaneMode, "ctrl-p")}, ModePane},
		{"lock from pane mode", "\x10\x07x\x10\x07", []keyEvent{a(actPaneMode, "ctrl-p"), a(actLock, "ctrl-g"), d("x\x10"),
			a(actUnlock, "ctrl-g")}, ModeNormal},
		{"alt keys in normal mode", "\x1bh\x1bl\x1bj\x1bk\x1bi\x1bo", []keyEvent{a(actFocusLeft, "alt-h"), a(actFocusRight, "alt-l"),
			a(actFocusDown, "alt-j"), a(actFocusUp, "alt-k"), a(actMoveTabLeft, "alt-i"), a(actMoveTabRight, "alt-o")}, ModeNormal},
		{"alt-f and alt-b reach the shell", "\x1bf\x1bb\x1b", []keyEvent{d("\x1bf\x1bb\x1b")}, ModeNormal},
		{"kitty and modifyOtherKeys alt", "\x1b[104;3u\x1b[27;3;108~", []keyEvent{a(actFocusLeft, "alt-h"), a(actFocusRight, "alt-l")}, ModeNormal},
		{"kitty alt release dropped", "\x1b[104;3u\x1b[104;3:3ua", []keyEvent{a(actFocusLeft, "alt-h"), d("a")}, ModeNormal},
		{"kitty alt shift passes", "\x1b[104;4u", []keyEvent{d("\x1b[104;4u")}, ModeNormal},
		{"alt keys keep pane mode", "\x10\x1bl\x1b[107;3ux", []keyEvent{a(actPaneMode, "ctrl-p"), a(actFocusRight, "alt-l"),
			a(actFocusUp, "alt-k"), a(actCloseView, "x")}, ModeNormal},
		{"an unbound alt key in pane mode is Esc", "\x10\x1bfx", []keyEvent{a(actPaneMode, "ctrl-p"), a(actBack, "esc"), d("fx")}, ModeNormal},
		{"kitty alt f in tab mode is no key", "\x14\x1b[102;3u", []keyEvent{a(actTabMode, "ctrl-t")}, ModeTab},
		{"locked passes alt keys", "\x07\x1bh\x1b[104;3u\x07", []keyEvent{a(actLock, "ctrl-g"), d("\x1bh\x1b[104;3u"), a(actUnlock, "ctrl-g")}, ModeNormal},
		{"a leader twice sends it", "\x10\x10\x14\x14\x0f\x0fx", []keyEvent{a(actPaneMode, "ctrl-p"), a(actSend, "ctrl-p"),
			a(actTabMode, "ctrl-t"), a(actSend, "ctrl-t"), a(actSession, "ctrl-o"), a(actSend, "ctrl-o"), d("x")}, ModeNormal},
	} {
		k := newKeyReader(km)
		if got := k.feed([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) || k.mode != tc.mode {
			t.Errorf("%s: got %q in %s mode, want %q in %s mode", tc.name, got, k.mode, tc.want, tc.mode)
		}
	}
}

// [keys.shared] binds a key in normal, session, pane and tab mode at once, but a mode key not
// in the mode it enters; a mode's own table wins over it. clear-defaults in [keys] or in a
// mode's table drops the shipped bindings there, as zellij's clear-defaults; [keys.shared]
// still applies to a cleared mode.
func TestKeysSharedAndClear(t *testing.T) {
	p := writeConfig(t, `[keys.pane]
"ctrl-a" = "back"
clear-defaults = true
z = "zoom"

[keys.shared]
"ctrl-y" = "tab-mode"
"ctrl-a" = "pane-mode"
"ctrl-p" = "none"
a = "zoom"
"ctrl-x" = "open"
clear-defaults = true
`)
	s, problems, err := LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		p + `:10: [keys.shared] a: [keys.shared] takes only ctrl-<key>`,
		p + `:11: [keys.shared] ctrl-x: action "open" does not work in [keys.shared]`,
		p + `:12: [keys.shared] clear-defaults: unknown key`,
	} {
		if !hasProblem(problems, want) {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
	if len(problems) != 3 {
		t.Errorf("problems %q, want 3", problems)
	}
	km := s.Keymap
	for _, m := range []Mode{ModeNormal, ModeSession, ModeTab} {
		if a := km.lookup(m, "ctrl-a"); a != actPaneMode {
			t.Errorf("%s ctrl-a = %q, want pane-mode", m, a)
		}
		if a := km.lookup(m, "ctrl-p"); a != "" {
			t.Errorf("%s ctrl-p = %q, want unbound", m, a)
		}
	}
	if a := km.lookup(ModeTab, "ctrl-y"); a != "" {
		t.Errorf("tab ctrl-y = %q: a mode key bound in its own mode", a)
	}
	// Pane mode: cleared, then the shared keys, then its own table, whose ctrl-a = back wins.
	if got := barHints(km, ModePane); got != "Ctrl+Y tab  Ctrl+A back  z zoom" {
		t.Errorf("pane bar %q", got)
	}
	if got := barHints(km, ModeNormal); got != "Ctrl+G lock  Ctrl+T/Y tab  Ctrl+O session  Alt+h/l/j/k move focus  Alt+i/o move tab  Ctrl+A pane" {
		t.Errorf("normal bar %q", got)
	}
	// Ctrl+Y enters tab mode and, pressed again there, sends itself; Ctrl+A enters pane mode;
	// Ctrl+P reaches the program.
	evs := newKeyReader(km).feed([]byte("\x19\x19\x01z\x10"))
	want := []keyEvent{{act: actTabMode, key: "ctrl-y"}, {act: actSend, key: "ctrl-y"}, {act: actPaneMode, key: "ctrl-a"},
		{act: actZoom, key: "z"}, {data: []byte("\x10")}}
	if !reflect.DeepEqual(evs, want) {
		t.Errorf("got %q, want %q", evs, want)
	}

	// [keys] clear-defaults drops every shipped key; the old spelling says what to write.
	s, problems, _ = LoadSettings(writeConfig(t, "[keys]\nclear-defaults = true\n[keys.shared]\n\"ctrl-a\" = \"session-mode\"\n[keys.session]\nd = \"detach\"\n[keys.sidebar]\nesc = \"back\"\n"))
	if len(problems) != 0 {
		t.Errorf("problems %q", problems)
	}
	got := barHints(s.Keymap, ModeNormal) + " | " + barHints(s.Keymap, ModeSession) + " | " + barHints(s.Keymap, ModeTab)
	if got != "Ctrl+A session | d detach | Ctrl+A session" {
		t.Errorf("cleared keymap %q", got)
	}
	s, problems, _ = LoadSettings(writeConfig(t, "[keys]\ndefaults = false\n"))
	if !hasProblem(problems, ":2: [keys] defaults: unknown key; write clear-defaults = true") || s.Keymap.lookup(ModeNormal, "ctrl-p") != actPaneMode {
		t.Errorf("old defaults switch: %q", problems)
	}
	if _, problems, _ = LoadSettings(writeConfig(t, "[keys.tab]\nclear-defaults = \"yes\"\n")); !hasProblem(problems, ":2: [keys.tab] clear-defaults: want true or false") {
		t.Errorf("clear-defaults not a bool: %q", problems)
	}
}
