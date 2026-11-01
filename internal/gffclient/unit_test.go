package gffclient

import (
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// The pass-through filter drops every sequence the host would answer, and the palette, working
// directory and window settings, from any split of the bytes; everything else passes byte for
// byte.
func TestPassFilter(t *testing.T) {
	kept := "plain \x1b[1;31mred\x1b[0m \x1b]0;title\x07 \x1b]52;c;aGk=\x07 \x1b[?1049h\x1b[22;0t\x1b[2 q" +
		"\x1b[?2004h\x1b[>1u\x1b[!p\x1bPq#0;2;0;0;0#0!10~\x1b\\ \x1b(B\x1b7\x1b8 ü"
	dropped := []string{
		"\x1b[6n", "\x1b[?6n", "\x1b[5n", "\x1b[c", "\x1b[>c", "\x1b[=c", "\x1b[>q", "\x1b[?1$p", "\x1b[4$p",
		"\x1b[?u", "\x1b[14t", "\x1b[16t", "\x1b[18t", "\x1b[8;50;100t", "\x1b[1\x088t", "\x1b[x",
		"\x1b]52;c;?\x07", "\x1b]11;?\x1b\\", "\x1b]4;1;?\x07", "\x1b]4;1;rgb:ff/00/00\x07", "\x1b]7;file:///x\x07",
		"\x1b]104\x07", "\x1b]10;#ffffff\x07", "\x1bP$qm\x1b\\", "\x1bP+q544e\x1b\\", "\x1b_Ga=q;AAAA\x1b\\",
		"\x1b[?1;1;0S", "\x1b[?4m",
	}
	var in, want strings.Builder
	for _, d := range dropped {
		in.WriteString(d + "|")
		want.WriteString("|")
	}
	in.WriteString(kept)
	want.WriteString(kept)
	for _, step := range []int{len(in.String()), 1, 2, 3, 7} {
		var f passFilter
		var out bytes.Buffer
		b := []byte(in.String())
		for i := 0; i < len(b); i += step {
			f.write(&out, b[i:min(i+step, len(b))])
		}
		if out.String() != want.String() {
			t.Errorf("step %d:\n got %q\nwant %q", step, out.String(), want.String())
		}
	}
}

// A long control string streams once the filter knows it is not one it drops.
func TestPassFilterLongString(t *testing.T) {
	long := "\x1b]52;c;" + strings.Repeat("QUFB", 2000) + "\x07"
	var f passFilter
	var out bytes.Buffer
	f.write(&out, []byte(long[:3000]))
	if out.Len() == 0 {
		t.Fatal("a long clipboard write was held back")
	}
	f.write(&out, []byte(long[3000:]+"after"))
	if out.String() != long+"after" {
		t.Errorf("long string changed: %d bytes out, want %d", out.Len(), len(long)+5)
	}
	out.Reset()
	f.write(&out, []byte("\x1b_G"+strings.Repeat("A", 5000)+"\x1b\\ok"))
	if out.String() != "ok" {
		t.Errorf("long kitty graphics command passed: %q", out.String()[:min(out.Len(), 40)])
	}
}

func TestKeyReader(t *testing.T) {
	d := func(data string) keyEvent { return keyEvent{data: []byte(data)} }
	a := func(act, key string) keyEvent { return keyEvent{act: act, key: key} }
	pre := a(actPrefix, `ctrl-\`)
	for _, tc := range []struct {
		name   string
		chunks []string
		want   []keyEvent
	}{
		{"plain", []string{"abc"}, []keyEvent{d("abc")}},
		{"command", []string{"ab\x1czcd"}, []keyEvent{d("ab"), pre, a(actZoom, "z"), d("cd")}},
		{"prefix twice sends it", []string{"\x1c\x1c"}, []keyEvent{pre, a(actSend, `ctrl-\`)}},
		{"split after prefix", []string{"a\x1c", "o"}, []keyEvent{d("a"), pre, a(actFocusNext, "o")}},
		{"tab after prefix", []string{"\x1c\t"}, []keyEvent{pre, a(actFocusNext, "tab")}},
		{"esc cancels", []string{"\x1c\x1bx"}, []keyEvent{pre, a(actBack, "esc"), d("x")}},
		{"unbound key cancels", []string{"\x1c\x01x"}, []keyEvent{pre, d("x")}},
		{"kitty prefix", []string{"\x1b[92;5ur"}, []keyEvent{pre, a(actRedraw, "r")}},
		{"kitty prefix split", []string{"q\x1b[9", "2;5", ":1u", "\x1b[100u"}, []keyEvent{d("q"), pre, a(actDetach, "d")}},
		{"kitty other key passes", []string{"\x1b[97;5u"}, []keyEvent{d("\x1b[97;5u")}},
		{"kitty prefix twice", []string{"\x1b[92;5u\x1b[92;5u"}, []keyEvent{pre, a(actSend, `ctrl-\`)}},
		// A pane that asked for event types (kitty flag 2): the prefix's release is not the
		// command and never reaches the pane; nor does a modifier key alone.
		{"kitty release skipped", []string{"\x1b[92;5u\x1b[92;5:3uo"}, []keyEvent{pre, a(actFocusNext, "o")}},
		{"kitty release and modifier", []string{"\x1b[92;5:1u", "\x1b[57442;5:3u\x1b[92;5:3u", "\x1b[57441;2u\x1b[122;1:1u"},
			[]keyEvent{pre, a(actZoom, "z")}},
		{"kitty caps lock on", []string{"\x1b[92;69ud"}, []keyEvent{pre, a(actDetach, "d")}},
		{"lone release passes", []string{"a\x1b[97;1:3u"}, []keyEvent{d("a\x1b[97;1:3u")}},
		// vim turns on modifyOtherKeys 2 (CSI >4;2m): Ctrl+\ comes as CSI 27;5;92~.
		{"modifyOtherKeys prefix", []string{"\x1b[27;5;92~o"}, []keyEvent{pre, a(actFocusNext, "o")}},
		{"modifyOtherKeys prefix split", []string{"x\x1b[2", "7;5;9", "2~", "\x1b[27;5;92~"}, []keyEvent{d("x"), pre, a(actSend, `ctrl-\`)}},
		{"modifyOtherKeys other key passes", []string{"\x1b[27;5;99~"}, []keyEvent{d("\x1b[27;5;99~")}},
		{"lone esc passes at once", []string{"\x1b"}, []keyEvent{d("\x1b")}},
		// Pane mode stays until Esc or Enter; keys it does not bind go nowhere.
		{"pane mode stays", []string{"\x1cpzqo\rab"}, []keyEvent{pre, a(actPaneMode, "p"), a(actZoom, "z"),
			a(actFocusNext, "o"), a(actBack, "enter"), d("ab")}},
		{"tab mode arrows and digits", []string{"\x1ct3\x1b[A]\x1b"}, []keyEvent{pre, a(actTabMode, "t"), a(actTabN+"3", "3"),
			a(actNextTab, "]"), a(actBack, "esc")}},
		// Locked: the prefix reaches the pane; only Ctrl+G, the unlock key, does not.
		{"locked passes the prefix", []string{"\x1cl\x1cz\x07\x1cz"}, []keyEvent{pre, a(actLock, "l"), d("\x1cz"),
			a(actUnlock, "ctrl-g"), pre, a(actZoom, "z")}},
		{"kitty unlock", []string{"\x1cl\x1b[103;5u\x1cz"}, []keyEvent{pre, a(actLock, "l"), a(actUnlock, "ctrl-g"), pre, a(actZoom, "z")}},

		// A bracketed paste goes to the pane whole, with its markers, in any mode, and is never
		// keys: in tab mode "xc12" closes no view and makes no tab; after the prefix the start
		// marker is not the one key; a prefix or unlock byte inside it does not act.
		{"paste in tab mode", []string{"\x1ct\x1b[200~xc12\x1b[201~x"}, []keyEvent{pre, a(actTabMode, "t"),
			d("\x1b[200~xc12\x1b[201~x")}},
		{"paste after the prefix", []string{"\x1c\x1b[200~hello\x1b[201~"}, []keyEvent{pre, d("\x1b[200~hello\x1b[201~")}},
		{"prefix inside a paste", []string{"a\x1b[200~b\x1cz\x1b[201~\x1cz"}, []keyEvent{d("a\x1b[200~b\x1cz\x1b[201~"), pre,
			a(actZoom, "z")}},
		{"unlock inside a paste", []string{"\x1cl\x1b[200~\x07\x1b[201~\x07"}, []keyEvent{pre, a(actLock, "l"),
			d("\x1b[200~\x07\x1b[201~"), a(actUnlock, "ctrl-g")}},
		{"paste split across reads", []string{"\x1b[20", "0~a\x1c", "b\x1b[2", "01~\x1cz"}, []keyEvent{d("\x1b[200~a\x1c"),
			d("b"), d("\x1b[201~"), pre, a(actZoom, "z")}},

		// Kitty's report-all-keys mode: Shift+1 is "!" from the shifted key or the text field,
		// never "1"; with neither, it names no key, so the prefix cancels.
		{"kitty shift alternate", []string{"\x1c\x1b[49:33;2u"}, []keyEvent{pre, a(actWarnings, "!")}},
		{"kitty shift text", []string{"\x1c\x1b[49;2;33u"}, []keyEvent{pre, a(actWarnings, "!")}},
		{"kitty shift no alternate", []string{"\x1c\x1b[49;2ux"}, []keyEvent{pre, d("x")}},
		{"kitty shift letter", []string{"\x1c\x1b[122;2ux"}, []keyEvent{pre, d("x")}},

		// The release of a key the reader took never reaches the pane, in whatever mode it comes.
		{"kitty unlock release", []string{"\x1cl\x1b[103;5u\x1b[103;5:3ua"}, []keyEvent{pre, a(actLock, "l"),
			a(actUnlock, "ctrl-g"), d("a")}},
		{"command key release after the prefix", []string{"\x1b[92;5uz\x1b[122;1:3ua"}, []keyEvent{pre, a(actZoom, "z"), d("a")}},
		{"prefix twice, release in normal mode", []string{"\x1b[92;5u\x1b[92;5u\x1b[92;5:3ua"}, []keyEvent{pre,
			a(actSend, `ctrl-\`), d("a")}},
		{"arrow release after pane mode", []string{"\x1cp\x1b[1;1:1A\x1b", "\x1b[1;1:3Aa"}, []keyEvent{pre,
			a(actPaneMode, "p"), a(actBack, "esc"), d("a")}},
		{"other release passes", []string{"\x1b[120;1:3u"}, []keyEvent{d("\x1b[120;1:3u")}},

		// Modifier keys alone are no command: right Ctrl, and AltGr on a European layout.
		{"kitty right ctrl", []string{"\x1c\x1b[57448;5uz"}, []keyEvent{pre, a(actZoom, "z")}},
		{"kitty altgr", []string{"\x1c\x1b[57453u]"}, []keyEvent{pre, a(actNextTab, "]")}},
		{"kitty level 5 shift", []string{"\x1c\x1b[57454u]"}, []keyEvent{pre, a(actNextTab, "]")}},
		// Ctrl+Shift+\ is not the prefix.
		{"kitty ctrl shift passes", []string{"\x1b[92;6u"}, []keyEvent{d("\x1b[92;6u")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKeyReader(DefaultKeymap())
			var got []keyEvent
			for _, c := range tc.chunks {
				got = append(got, k.feed([]byte(c))...)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := ParsePrefix("ctrl-m"); err == nil {
		t.Error("ctrl-m (Enter) accepted as the prefix")
	}
	if p, err := ParsePrefix("ctrl-a"); err != nil || p != 1 {
		t.Errorf("ctrl-a = %d, %v", p, err)
	}
}

// testClient is a client with no connection, for driving frame and render by hand. Its views
// have a zero PaneStream: no data connection opens, so a snapshot keeps the view's size.
func testClient(t *testing.T, w, h int, ids ...string) *client {
	hs := &hostState{label: "hab", online: true}
	c := &client{cfg: Config{Parser: vt.Fake{}, Logf: t.Logf, Now: time.Now}, hosts: map[string]*hostState{"hab": hs},
		first: "hab", tabs: []*tab{{name: "1"}}, modes: map[int]bool{}, keys: newKeyReader(DefaultKeymap()), w: w, h: h}
	c.canvas.resize(w, h)
	for _, id := range ids {
		c.tabs[0].views = append(c.tabs[0].views, &view{spec: PaneSpec{ID: id, Label: id}, h: hs, stream: &habconn.PaneStream{}, cols: w, rows: h})
	}
	return c
}

func snapshotFrame(v *view, data string) paneFrame {
	return paneFrame{v: v, f: habconn.Frame{Kind: api.FrameSnapshot, Encoding: string(vt.EncodingVT), Data: []byte(data)}}
}

func bytesFrame(v *view, data string) paneFrame {
	return paneFrame{v: v, f: habconn.Frame{Kind: api.FrameBytes, Data: []byte(data)}}
}

// A new snapshot for the pass-through pane, at a size that no longer fits (a data connection
// opened again after another device resized the pane), sets back the modes the pane set on the
// host before the composed view starts.
func TestNewSnapshotResetsModes(t *testing.T) {
	c := testClient(t, 80, 24, "p")
	v := c.tab().views[0]
	c.frame(snapshotFrame(v, "hi"))
	c.render()
	if c.pass != v {
		t.Fatal("the pane does not pass through")
	}
	c.frame(bytesFrame(v, "\x1b[?1000h\x1b[>4;2m"))
	c.out.Reset()
	v.cols, v.rows = 60, 20 // the new connection's welcome
	c.frame(snapshotFrame(v, "hi"))
	c.render()
	out := c.out.String()
	reset, compose := strings.Index(out, resetPaneModes), strings.Index(out, enterCompose)
	if compose < 0 || reset < 0 || reset > compose {
		t.Errorf("composing after a new snapshot without the mode reset first: %q", out)
	}
}

// The reset covers the modes tmux sets back: a synchronized update, modifyOtherKeys, and the
// G0 charset.
func TestResetPaneModes(t *testing.T) {
	for _, s := range []string{"\x1b[?2026l", "\x1b[>4m", "\x1b(B", "\x0f", "\x1b[?1000l", "\x1b[=0;1u"} {
		if !strings.Contains(resetPaneModes, s) {
			t.Errorf("resetPaneModes lacks %q", s)
		}
	}
	if !strings.HasPrefix(resetPaneModes, "\x1b[?2026l") {
		t.Error("resetPaneModes does not end a synchronized update first")
	}
}

// modeTerm reports the private modes in on, whatever its terminal says.
type modeTerm struct {
	vt.Terminal
	on map[int]bool
}

func (m modeTerm) Mode(n int, ansi bool) (bool, bool) { return !ansi && m.on[n], true }

// Leaving pass-through, gff takes the host off the alternate screen the way the pane went on:
// 1049, 1047 or 47.
func TestRestoreLeavesAltScreen(t *testing.T) {
	for _, m := range []int{1049, 1047, 47} {
		c := testClient(t, 80, 24, "p")
		v := c.tab().views[0]
		c.frame(snapshotFrame(v, "hi"))
		c.render()
		v.term = modeTerm{Terminal: v.term, on: map[int]bool{m: true}}
		c.out.Reset()
		c.restoreHost()
		if want := "\x1b[?" + strconv.Itoa(m) + "l"; !strings.HasSuffix(c.out.String(), want) {
			t.Errorf("mode %d: restore wrote %q, want it to end with %q", m, c.out.String(), want)
		}
	}
}

func TestArrange(t *testing.T) {
	a := arrange(2, 0, SplitColumns, false, 100, 30)
	if len(a.slots) != 2 || a.slots[0].content != (rect{0, 1, 50, 29}) || a.slots[1].content != (rect{51, 1, 49, 29}) ||
		len(a.seps) != 1 || a.seps[0] != (rect{50, 0, 1, 30}) {
		t.Errorf("columns: %+v", a)
	}
	a = arrange(2, 1, SplitRows, false, 80, 25)
	if a.slots[0].content != (rect{0, 1, 80, 12}) || a.slots[1].frame != (rect{0, 13, 80, 1}) || a.slots[1].wantRows != 11 {
		t.Errorf("rows: %+v", a)
	}
	a = arrange(3, 2, SplitColumns, true, 80, 24)
	if len(a.slots) != 1 || a.slots[0].view != 2 || a.slots[0].wantCols != 80 || a.slots[0].wantRows != 24 {
		t.Errorf("zoom: %+v", a)
	}
}

// The canvas writes only the cells that changed, and clips a wide cell at a view's edge.
func TestCanvas(t *testing.T) {
	var s vt.Screen
	s.Reset(4, 1)
	s.Cells[0] = vt.Cell{Text: "a"}
	s.Cells[2] = vt.Cell{Text: "界", Wide: true}
	s.Cells[3] = vt.Cell{Spacer: true}
	var c canvas
	c.resize(3, 1)
	c.pane(&s, rect{0, 0, 3, 1}, false)
	var out bytes.Buffer
	c.flush(&out)
	if !strings.Contains(out.String(), "a") || strings.Contains(out.String(), "界") {
		t.Errorf("first frame %q: want a, and the cut wide cell left out", out.String())
	}
	out.Reset()
	c.clear()
	c.pane(&s, rect{0, 0, 3, 1}, false)
	c.flush(&out)
	if out.Len() != 0 {
		t.Errorf("an unchanged frame wrote %q", out.String())
	}
	s.Cells[0].Text = "b"
	c.clear()
	c.pane(&s, rect{0, 0, 3, 1}, false)
	c.flush(&out)
	if got := out.String(); got != "\x1b[1;1Hb" {
		t.Errorf("one changed cell wrote %q", got)
	}
}
