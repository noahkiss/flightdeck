package gffclient

import (
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// The config's earlier name for session mode and its action still load, each with one problem
// that names the new spelling (gff-client.md 8.4).
func TestSessionModeAlias(t *testing.T) {
	p := writeConfig(t, "[keys.prefix]\nq = \"detach\"\n[keys.normal]\n\"ctrl-a\" = \"prefix\"\n")
	s, problems, err := LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		p + ":1: [keys.prefix]: the mode is now session; write [keys.session]",
		p + `:4: [keys.normal] ctrl-a: action "prefix" is now "session-mode"`,
	} {
		if !hasProblem(problems, want) {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
	if len(problems) != 2 {
		t.Errorf("problems %q, want 2", problems)
	}
	if a, b := s.Keymap.lookup(ModeSession, "q"), s.Keymap.lookup(ModeNormal, "ctrl-a"); a != actDetach || b != actSession {
		t.Errorf("session q = %q, normal ctrl-a = %q", a, b)
	}
}

// Alt keys in the config: alt-<character>, case kept; [ and O, which start the arrows'
// sequences, refused; normal and locked mode take them as they take ctrl-<key>.
func TestAltKeyConfig(t *testing.T) {
	for in, want := range map[string]string{"alt-h": "alt-h", "ALT-x": "alt-x", "alt-H": "alt-H", "alt-;": "alt-;"} {
		if got, err := ParseKey(in); err != nil || got != want {
			t.Errorf("ParseKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"alt-[", "alt-O", "alt-hh", "alt- ", "alt-é"} {
		if got, err := ParseKey(in); err == nil {
			t.Errorf("ParseKey(%q) = %q, want an error", in, got)
		}
	}
	p := writeConfig(t, "[keys.normal]\n\"alt-z\" = \"zoom\"\n\"alt-h\" = \"none\"\n[keys.locked]\n\"alt-u\" = \"unlock\"\n")
	s, problems, _ := LoadSettings(p)
	if len(problems) != 0 {
		t.Errorf("problems %q", problems)
	}
	if a, b, c := s.Keymap.lookup(ModeNormal, "alt-z"), s.Keymap.lookup(ModeNormal, "alt-h"), s.Keymap.lookup(ModeLocked, "alt-u"); a != actZoom || b != "" || c != actUnlock {
		t.Errorf("alt-z = %q, alt-h = %q, locked alt-u = %q", a, b, c)
	}
	if evs := newKeyReader(s.Keymap).feed([]byte("\x1bh\x1bz")); len(evs) != 2 || string(evs[0].data) != "\x1bh" || evs[1].act != actZoom {
		t.Errorf("unbound alt-h and bound alt-z: %q", evs)
	}
}

// Alt+h and Alt+l move focus along side-by-side views and fall to the previous or next tab at
// the edge; Alt+j and Alt+k move along stacked views and stop at the edge; Alt+i and Alt+o
// move the tab, and stop at the edge (zellij's MoveFocusOrTab, MoveFocus and MoveTab).
func TestAltFocusAndTabMoves(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := barClient(t, 160, 30, &now)
	c.debounce = &fakeTimer{c: make(chan time.Time, 1)}
	c.ui.mode = modePanes
	hs := c.hosts[c.first]
	a := &tab{name: "1", split: SplitColumns}
	for _, id := range []string{"x", "y", "z"} {
		// Exited views open no stream when their tab shows.
		a.views = append(a.views, &view{spec: PaneSpec{ID: id}, h: hs, exited: &api.ExitedFrame{}})
	}
	b, d := &tab{name: "2"}, &tab{name: "3"}
	c.tabs, c.cur = []*tab{a, b, d}, 0
	type state struct{ cur, focus int }
	at := func() state { return state{c.cur, a.focus} }
	for _, step := range []struct {
		keys  string
		split Split
		want  state
	}{
		{"\x1bl", SplitColumns, state{0, 1}},
		{"\x1bl\x1bl", SplitColumns, state{1, 2}}, // past the last view: the next tab
		{"\x1bh", SplitColumns, state{0, 2}},      // a tab with no view: the previous tab
		{"\x1bk\x1bj", SplitColumns, state{0, 2}}, // up and down do nothing side by side
		{"\x1bk", SplitRows, state{0, 1}},
		{"\x1bj\x1bj", SplitRows, state{0, 2}}, // down stops at the last view
		{"\x1bh", SplitRows, state{2, 2}},      // stacked: left goes to the previous tab, wrapping
		{"\x1b[108;3u", SplitRows, state{0, 2}},
	} {
		a.split = step.split
		if err := c.input([]byte(step.keys)); err != nil {
			t.Fatal(err)
		}
		if got := at(); got != step.want {
			t.Errorf("after %q: tab %d focus %d, want %+v", step.keys, got.cur, got.focus, step.want)
		}
	}
	order := func() string {
		s := ""
		for _, t := range c.tabs {
			s += t.name
		}
		return s
	}
	for _, step := range []struct {
		keys, order string
		cur         int
	}{
		{"\x1bo", "213", 1},
		{"\x1bo\x1bo", "231", 2}, // the last place: no further
		{"\x1bi", "213", 1},
		{"\x1b[105;3u", "123", 0},
		{"\x1bi", "123", 0},
	} {
		if err := c.input([]byte(step.keys)); err != nil {
			t.Fatal(err)
		}
		if o := order(); o != step.order || c.cur != step.cur || c.tab() != a {
			t.Errorf("after %q: tabs %s, current %d; want %s, %d, the moved tab current", step.keys, o, c.cur, step.order, step.cur)
		}
	}
	// A zoomed tab lays out again for the newly focused view.
	a.split, a.zoom, a.focus = SplitColumns, true, 0
	c.canvas.valid = true
	if err := c.input([]byte("\x1bl")); err != nil {
		t.Fatal(err)
	}
	if a.focus != 1 || c.canvas.valid {
		t.Errorf("zoomed Alt+l: focus %d, canvas valid %v; want 1 and a repaint", a.focus, c.canvas.valid)
	}

	// While the sidebar, the board or a prompt has the keys, the Alt keys change nothing. One
	// key at a time, so a move and its reverse cannot cancel out. Focus is on the middle view,
	// so Alt+l and Alt+h would each move it; the tab is first, so Alt+o would move it.
	a.zoom, a.focus = false, 1
	for _, m := range []uiMode{modeSidebar, modeBoard, modePrompt} {
		for _, key := range []string{"\x1bl", "\x1bh", "\x1bo"} {
			c.ui.mode = m
			before, o := at(), order()
			if err := c.input([]byte(key)); err != nil {
				t.Fatal(err)
			}
			if at() != before || order() != o {
				t.Errorf("mode %v: %q moved focus or tabs: %+v %s, was %+v %s", m, key, at(), order(), before, o)
			}
		}
	}
}

// A kitty Alt key cut between two reads is held and read whole, never passed to the pane. The
// cuts are where only an Alt key's code matches: no Ctrl leader's code starts 105 or is 104.
func TestAltKeySplitRead(t *testing.T) {
	for _, tc := range []struct {
		chunks   []string
		act, key string
	}{
		{[]string{"\x1b[105;", "3u"}, actMoveTabLeft, "alt-i"},
		{[]string{"\x1b[104", ";3u"}, actFocusLeft, "alt-h"},
	} {
		k := newKeyReader(DefaultKeymap())
		var evs []keyEvent
		for _, chunk := range tc.chunks {
			evs = append(evs, k.feed([]byte(chunk))...)
		}
		if len(evs) != 1 || evs[0].act != tc.act || evs[0].key != tc.key {
			t.Errorf("split %q: %q, want %s", tc.chunks, evs, tc.key)
		}
	}
}

// A send binding types its own key: a Ctrl key's byte, an Alt key as ESC and the character.
func TestKeyBytes(t *testing.T) {
	for k, want := range map[string]string{"ctrl-p": "\x10", `ctrl-\`: "\x1c", "alt-s": "\x1bs", "alt-H": "\x1bH", "x": "", "esc": ""} {
		if got := string(keyBytes(k)); got != want {
			t.Errorf("keyBytes(%q) = %q, want %q", k, got, want)
		}
	}
}
