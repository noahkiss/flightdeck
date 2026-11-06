package gffclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/vt"
)

func TestParseMouse(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    mouseEvent
		n       int
		partial bool
	}{
		{"\x1b[<64;10;5M", mouseEvent{b: 64, x: 10, y: 5}, 11, false},
		{"\x1b[<0;1;2mrest", mouseEvent{b: 0, x: 1, y: 2, release: true}, 9, false},
		{"\x1b[<65;3", mouseEvent{}, 0, true},
		{"\x1b[", mouseEvent{}, 0, true},
		{"\x1b", mouseEvent{}, 0, false}, // a lone Esc is a key
		{"\x1b[<6x", mouseEvent{}, 0, false},
		{"\x1b[A", mouseEvent{}, 0, false},
		{"ab", mouseEvent{}, 0, false},
	} {
		e, n, partial := parseMouse([]byte(c.in))
		if e != c.want || n != c.n || partial != c.partial {
			t.Errorf("%q: %+v %d %v, want %+v %d %v", c.in, e, n, partial, c.want, c.n, c.partial)
		}
	}
	for _, c := range []struct {
		in             string
		delta, n       int
		bottom, partly bool
	}{
		{"\x1b[5;2~", -9, 6, false, false},
		{"\x1b[6;2~x", 9, 6, false, false},
		{"\x1b[1;2F", 0, 6, true, false},
		{"\x1b[5;", 0, 0, false, true},
		{"\x1b[5~", 0, 0, false, false},
	} {
		d, bottom, n, partial := scrollKey([]byte(c.in), 9)
		if d != c.delta || bottom != c.bottom || n != c.n || partial != c.partly {
			t.Errorf("%q: %d %v %d %v", c.in, d, bottom, n, partial)
		}
	}
}

// TestWheelScrollsLocally: with the real holder and gff-hab, the wheel over a view scrolls its
// local scrollback and sends the program nothing; typed keys reach the program and the view
// stays scrolled; the wheel down brings it back. Shift+PageUp scrolls a page and Shift+End
// goes back to the bottom. Over a program that asked for the mouse, the wheel goes to it as a
// report at the pane's own cell; on an alternate screen with no mouse mode, as arrow keys.
func TestWheelScrollsLocally(t *testing.T) {
	h := startHab(t)
	other := h.other()
	dir := shortDir(t)
	typed, mouse := filepath.Join(dir, "typed"), filepath.Join(dir, "mouse")
	pane := spawn(t, other, `read -r x; seq 1 100; stty raw -echo; exec cat > "`+typed+`"`, 80, 24)
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, h.sock)
	hst.waitScreen("the session", func(s string) bool { return line(s, " sh ") > 0 })
	hst.typ("\r")
	hst.waitScreen("its view", func(s string) bool { return strings.Contains(s, " sh · one") })
	var empty api.Empty
	call(t, other, "pane.input", api.InputParams{Pane: pane, Data: []byte("\n")}, &empty)
	hst.waitScreen("the numbers", func(s string) bool { return line(s, "100") > 0 })
	waitUntil(t, "cat running", func() bool { _, err := os.Stat(typed); return err == nil })

	// A cell in the pane's content: right of the sidebar, under the bars and the frame row.
	at := "60;10"
	hst.typ("\x1b[<64;" + at + "M")
	hst.waitScreen("three rows up", func(s string) bool { return strings.Contains(s, "scrolled 3") && line(s, "100") < 0 })
	before := hst.screen()
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(typed); len(b) != 0 {
		t.Fatalf("the wheel reached the program: %q", b)
	}
	hst.typ("zz")
	waitUntil(t, "the typed keys at the program", func() bool { b, _ := os.ReadFile(typed); return string(b) == "zz" })
	if s := hst.screen(); !strings.Contains(s, "scrolled 3") || line(s, "100") >= 0 || paneText(s) != paneText(before) {
		t.Fatalf("typing moved the view:\n%s", s)
	}
	hst.typ("\x1b[<65;" + at + "M")
	hst.waitScreen("back at the bottom", func(s string) bool { return !strings.Contains(s, "scrolled") && line(s, "100") > 0 })
	hst.typ("\x1b[5;2~")
	hst.waitScreen("a page up", func(s string) bool { return strings.Contains(s, "scrolled 23") && line(s, "100") < 0 })
	hst.typ("\x1b[1;2F")
	hst.waitScreen("Shift+End: the bottom", func(s string) bool { return !strings.Contains(s, "scrolled") && line(s, "100") > 0 })
	if b, _ := os.ReadFile(typed); string(b) != "zz" {
		t.Fatalf("scroll keys reached the program: %q", b)
	}

	// Pasted control sequences are data, even when the entire paste arrives in one read.
	paste := "\x1b[200~\x1b[<64;60;10M\x1b[5;2~\x1b[201~"
	hst.typ(paste)
	waitUntil(t, "paste at the program", func() bool { b, _ := os.ReadFile(typed); return string(b) == "zz"+paste })
	if strings.Contains(hst.screen(), "scrolled") {
		t.Fatal("pasted mouse report scrolled the view")
	}

	// A program that asks for the mouse gets the wheel; one on the alternate screen with no
	// mouse mode gets arrow keys.
	call(t, other, "pane.close", api.PaneRef{Pane: pane}, &empty)
	hst.waitScreen("the first program ended", func(s string) bool { return strings.Contains(s, "exited") || strings.Contains(s, "gone") })
	hst.typ("\x10x") // close its view
	hst.waitScreen("the empty tab", func(s string) bool { return strings.Contains(s, "empty tab") })
	hst.typ(cmdKey + "w")
	mp := spawn(t, other, `read -r x; stty raw -echo; printf '\033[?1049h\033[?1000h\033[?1006hmouse-on'; exec cat > "`+mouse+`"`, 80, 24)
	hst.waitScreen("the mouse program's row", func(s string) bool { return line(s, "sh-2") > 0 })
	hst.typ("/sh-2\r\r")
	hst.waitScreen("its view", func(s string) bool { return strings.Contains(s, " sh-2 · one") })
	call(t, other, "pane.input", api.InputParams{Pane: mp, Data: []byte("\n")}, &empty)
	hst.waitScreen("mouse on", func(s string) bool { return strings.Contains(s, "mouse-on") })
	hst.typ("\x1b[<64;" + at + "M")
	waitUntil(t, "the wheel at the program", func() bool { b, _ := os.ReadFile(mouse); return strings.HasPrefix(string(b), "\x1b[<64;") })
	b, _ := os.ReadFile(mouse)
	if f := strings.Split(strings.TrimSuffix(strings.TrimPrefix(string(b), "\x1b[<"), "M"), ";"); len(f) != 3 || f[1] == "60" || f[2] == "10" {
		t.Fatalf("report %q: want the pane's own cell, not the host's", b)
	}
	if strings.Contains(hst.screen(), "scrolled") {
		t.Fatal("the view scrolled under a mouse program")
	}

	arrows := filepath.Join(dir, "arrows")
	hst.typ("\x10x")
	hst.waitScreen("the empty tab again", func(s string) bool { return strings.Contains(s, "empty tab") })
	hst.typ(cmdKey + "w")
	ap := spawn(t, other, `read -r x; stty raw -echo; printf '\033[?1049halt-on'; exec cat > "`+arrows+`"`, 80, 24)
	hst.typ("/\x7f\x7f\x7f\x7fsh-3\r") // the filter still holds sh-2: replace it
	hst.waitScreen("the third program's row", func(s string) bool { return line(s, "sh-3") > 0 })
	hst.typ("\r")
	hst.waitScreen("its view", func(s string) bool { return strings.Contains(s, " sh-3 · one") })
	call(t, other, "pane.input", api.InputParams{Pane: ap, Data: []byte("\n")}, &empty)
	hst.waitScreen("the alternate screen", func(s string) bool { return strings.Contains(s, "alt-on") })
	hst.typ("\x1b[<64;" + at + "M")
	waitUntil(t, "three arrow keys at the program", func() bool { b, _ := os.ReadFile(arrows); return string(b) == "\x1b[A\x1b[A\x1b[A" })
}

// paneText is the screen without its top row (the clock) and bottom row (system info).
func paneText(s string) string {
	l := strings.Split(s, "\n")
	if len(l) < 3 {
		return s
	}
	return strings.Join(l[1:len(l)-1], "\n")
}

// TestScrollTracksOutput keeps Shift+End tied to the bottom as output adds rows.
func TestScrollTracksOutput(t *testing.T) {
	term, err := (vt.Fake{}).New(vt.Config{Cols: 20, Rows: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.Write([]byte("1\r\n2\r\n3\r\n4\r\n5"))
	v := &view{term: term}
	c := &client{}
	c.scrollView(v, -1)
	if v.scroll != 1 {
		t.Fatalf("scroll = %d", v.scroll)
	}
	c.frame(bytesFrame(v, "\r\n6\r\n7"))
	if v.scroll != 3 {
		t.Fatalf("scroll after output = %d, want 3", v.scroll)
	}
	c.scrollView(v, v.scroll)
	if v.scroll != 0 {
		t.Fatalf("scroll at bottom = %d", v.scroll)
	}
}
