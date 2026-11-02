package gffclient

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// hasProblem reports whether a problem contains want.
func hasProblem(problems []string, want string) bool {
	return slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) })
}

// No config traps the user in a mode (gff-client.md 8.4): a mode the keymap reaches with no
// way back gets the default one, and a problem says so.
func TestKeymapNoTraps(t *testing.T) {
	// Locked with no unlock key: Ctrl+G unlocks again.
	p := writeConfig(t, "[keys.locked]\n\"ctrl-g\" = \"none\"\n")
	s, problems, err := LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if !hasProblem(problems, "no key leaves locked mode; Ctrl+G now does") || s.Keymap.lookup(ModeLocked, "ctrl-g") != actUnlock {
		t.Errorf("locked with no unlock: %q, ctrl-g = %q", problems, s.Keymap.lookup(ModeLocked, "ctrl-g"))
	}
	if evs := newKeyReader(s.Keymap).feed([]byte("\x07\x07\x0fr")); len(evs) != 4 || evs[3].act != actRedraw {
		t.Errorf("no way out of locked mode: %q", evs)
	}

	// Pane mode with Esc and Enter both bound to commands: Esc goes back again.
	p = writeConfig(t, "[keys.pane]\nesc = \"zoom\"\nenter = \"split\"\n")
	s, problems, _ = LoadSettings(p)
	if !hasProblem(problems, "no key leaves pane mode; Esc now does") {
		t.Errorf("pane with Esc and Enter bound: %q", problems)
	}
	if evs := newKeyReader(s.Keymap).feed([]byte("\x10\x1bx")); !reflect.DeepEqual(evs, []keyEvent{
		{act: actPaneMode, key: "ctrl-p"}, {act: actBack, key: "esc"}, {data: []byte("x")}}) {
		t.Errorf("no way out of pane mode: %q", evs)
	}

	// clear-defaults with a lock key and no locked table, and no sidebar keys.
	p = writeConfig(t, "[keys]\nclear-defaults = true\n[keys.normal]\n\"ctrl-g\" = \"lock\"\n")
	s, problems, _ = LoadSettings(p)
	for _, want := range []string{"no key leaves locked mode", "no key leaves sidebar mode", "no mode binds detach"} {
		if !hasProblem(problems, want) {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
	if s.Keymap.lookup(ModeSidebar, "esc") != actBack {
		t.Error("the sidebar got no way back")
	}

	// A mode the keymap does not reach is no trap, and the shipped keymap has none.
	p = writeConfig(t, "[keys]\nclear-defaults = true\n[keys.normal]\n\"ctrl-a\" = \"session-mode\"\n[keys.session]\nd = \"detach\"\n[keys.sidebar]\nq = \"back\"\n")
	if _, problems, _ = LoadSettings(p); len(problems) != 0 {
		t.Errorf("problems for modes not reached: %q", problems)
	}
	if problems := DefaultKeymap().exits("x"); len(problems) != 0 {
		t.Errorf("default keymap: %q", problems)
	}
}

// Esc and Enter leave pane and tab mode while the mode does not bind them, as 8.4's table says.
func TestEscLeavesWhenUnbound(t *testing.T) {
	p := writeConfig(t, "[keys.pane]\nesc = \"none\"\n[keys.tab]\nenter = \"none\"\n")
	s, problems, _ := LoadSettings(p)
	if len(problems) != 0 {
		t.Errorf("problems %q", problems)
	}
	evs := newKeyReader(s.Keymap).feed([]byte("\x10\x1bx\x14\rx"))
	want := []keyEvent{{act: actPaneMode, key: "ctrl-p"}, {data: []byte("x")},
		{act: actTabMode, key: "ctrl-t"}, {data: []byte("x")}}
	if !reflect.DeepEqual(evs, want) {
		t.Errorf("got %q, want %q", evs, want)
	}
}

// The key that entered a command mode, pressed again, sends itself, so a moved prefix and its
// send key cannot drift apart (S3); the old prefix sends nothing.
func TestPrefixSendFollowsTheKey(t *testing.T) {
	p := writeConfig(t, "[keys.normal]\n\"ctrl-o\" = \"none\"\n\"ctrl-a\" = \"session-mode\"\n")
	s, problems, _ := LoadSettings(p)
	if len(problems) != 0 {
		t.Errorf("problems %q", problems)
	}
	evs := newKeyReader(s.Keymap).feed([]byte("\x01\x01\x01\x0fx"))
	want := []keyEvent{{act: actSession, key: "ctrl-a"}, {act: actSend, key: "ctrl-a"}, {act: actSession, key: "ctrl-a"},
		{data: []byte("x")}}
	if !reflect.DeepEqual(evs, want) {
		t.Errorf("got %q, want %q", evs, want)
	}

	// -prefix ctrl-a over a config that binds ctrl-a to zoom: the flag wins, and says so.
	s, _, _ = LoadSettings(writeConfig(t, "[keys.normal]\n\"ctrl-a\" = \"zoom\"\n"))
	problems = s.Keymap.SetPrefix("ctrl-a")
	if !hasProblem(problems, `-prefix ctrl-a replaces [keys.normal] ctrl-a = "zoom"`) {
		t.Errorf("problems %q", problems)
	}
	if a, o := s.Keymap.lookup(ModeNormal, "ctrl-a"), s.Keymap.lookup(ModeNormal, "ctrl-o"); a != actSession || o != "" {
		t.Errorf("normal ctrl-a = %q, ctrl-o = %q", a, o)
	}
	if h := barHints(s.Keymap, ModeNormal); h != "Ctrl+G lock  Ctrl+P pane  Ctrl+T tab  Ctrl+A session  Alt+h/l/j/k move focus  Alt+i/o move tab" {
		t.Errorf("normal hints %q", h)
	}
}

// A value of the wrong type is a problem on its line, and the rest of its table and the file
// still load (S4).
func TestConfigValueTypes(t *testing.T) {
	p := writeConfig(t, `[keys.session]
z = 1
q = "detach"

[bar]
max_hints = "x"
clock = "15:04:05"
usage = "no"
info = "cpu"
colour = "red"
`)
	s, problems, err := LoadSettings(p)
	if err != nil {
		t.Fatalf("one bad value failed the file: %v", err)
	}
	for _, want := range []string{
		p + `:2: [keys.session] z: want an action in quotes`,
		p + `:6: [bar] max_hints: want a number`,
		p + `:8: [bar] usage: want true or false`,
		p + `:9: [bar] info: want a list`,
		p + `:10: [bar] colour: unknown key`,
	} {
		if !hasProblem(problems, want) {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
	if s.Keymap.lookup(ModeSession, "q") != actDetach {
		t.Error("q = detach beside a bad line was lost")
	}
	if s.Bar.Clock != "15:04:05" || s.Bar.MaxHints != 0 || !s.Bar.Usage || len(s.Bar.Info) != len(infoItems) {
		t.Errorf("bar %+v", s.Bar)
	}

	// [keys] with a misspelt defaults is an unknown key, not a mode.
	if _, problems, _ := LoadSettings(writeConfig(t, "[keys]\ndefault = false\n")); !hasProblem(problems, ":2: [keys] default: unknown key") {
		t.Errorf("default typo: %q", problems)
	}
}

// Problems keep their line when a table header has a comment after it.
func TestKeyLineHeaderComment(t *testing.T) {
	p := writeConfig(t, "[keys.session] # mine\nk = \"launch\"\n[ bar ]   \nclock = 1\n")
	_, problems, _ := LoadSettings(p)
	for _, want := range []string{p + `:2: [keys.session] k: unknown action`, p + `:4: [bar] clock: want`} {
		if !hasProblem(problems, want) {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
}

// Two spellings of one key are a problem, and the later line wins, whatever order the TOML
// reader gives them in.
func TestConfigLaterLineWins(t *testing.T) {
	for range 20 {
		p := writeConfig(t, "[keys.session]\nescape = \"zoom\"\nesc = \"split\"\nreturn = \"fit\"\nenter = \"redraw\"\n")
		s, problems, _ := LoadSettings(p)
		if !hasProblem(problems, p+":3: [keys.session] esc: the key is bound twice; this later line wins") ||
			!hasProblem(problems, p+":5: [keys.session] enter: the key is bound twice") {
			t.Fatalf("problems %q", problems)
		}
		if a, b := s.Keymap.lookup(ModeSession, "esc"), s.Keymap.lookup(ModeSession, "enter"); a != actSplit || b != actRedraw {
			t.Fatalf("esc = %q, enter = %q: the earlier line won", a, b)
		}
	}
}

// Locked mode takes only unlock, and send never shows on the bar.
func TestConfigLockedAndSend(t *testing.T) {
	p := writeConfig(t, "[keys.locked]\n\"ctrl-a\" = \"zoom\"\n[keys.session]\n\"ctrl-a\" = \"send\"\n")
	s, problems, _ := LoadSettings(p)
	if !hasProblem(problems, p+`:2: [keys.locked] ctrl-a: action "zoom" does not work in locked mode`) {
		t.Errorf("problems %q", problems)
	}
	if s.Keymap.lookup(ModeLocked, "ctrl-a") != "" {
		t.Error("locked mode took zoom")
	}
	if s.Keymap.lookup(ModeSession, "ctrl-a") != actSend {
		t.Fatal("send not bound")
	}
	for _, h := range s.Keymap.hints(ModeSession) {
		if h.label == actionLabels[actSend] {
			t.Errorf("send shows on the bar: %v", h)
		}
	}
}
