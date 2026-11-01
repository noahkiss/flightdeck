package gffclient

import (
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"

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
	ev := func(data string, cmd rune) keyEvent {
		if data == "" {
			return keyEvent{cmd: cmd}
		}
		return keyEvent{data: []byte(data)}
	}
	for _, tc := range []struct {
		name   string
		chunks []string
		want   []keyEvent
	}{
		{"plain", []string{"abc"}, []keyEvent{ev("abc", 0)}},
		{"command", []string{"ab\x1czcd"}, []keyEvent{ev("ab", 0), ev("", 'z'), ev("cd", 0)}},
		{"prefix twice sends it", []string{"\x1c\x1c"}, []keyEvent{ev("\x1c", 0)}},
		{"split after prefix", []string{"a\x1c", "o"}, []keyEvent{ev("a", 0), ev("", 'o')}},
		{"esc cancels", []string{"\x1c\x1bx"}, []keyEvent{ev("x", 0)}},
		{"kitty prefix", []string{"\x1b[92;5ur"}, []keyEvent{ev("", 'r')}},
		{"kitty prefix split", []string{"q\x1b[9", "2;5", ":1u", "\x1b[100u"}, []keyEvent{ev("q", 0), ev("", 'd')}},
		{"kitty other key passes", []string{"\x1b[97;5u"}, []keyEvent{ev("\x1b[97;5u", 0)}},
		{"kitty prefix twice", []string{"\x1b[92;5u\x1b[92;5u"}, []keyEvent{ev("\x1c", 0)}},
		// A pane that asked for event types (kitty flag 2): the prefix's release is not the
		// command and never reaches the pane; nor does a modifier key alone.
		{"kitty release skipped", []string{"\x1b[92;5u\x1b[92;5:3uo"}, []keyEvent{ev("", 'o')}},
		{"kitty release and modifier", []string{"\x1b[92;5:1u", "\x1b[57442;5:3u\x1b[92;5:3u", "\x1b[57441;2u\x1b[122;1:1u"},
			[]keyEvent{ev("", 'z')}},
		{"kitty caps lock on", []string{"\x1b[92;69ud"}, []keyEvent{ev("", 'd')}},
		{"lone release passes", []string{"a\x1b[97;1:3u"}, []keyEvent{ev("a\x1b[97;1:3u", 0)}},
		// vim turns on modifyOtherKeys 2 (CSI >4;2m): Ctrl+\ comes as CSI 27;5;92~.
		{"modifyOtherKeys prefix", []string{"\x1b[27;5;92~o"}, []keyEvent{ev("", 'o')}},
		{"modifyOtherKeys prefix split", []string{"x\x1b[2", "7;5;9", "2~", "\x1b[27;5;92~"}, []keyEvent{ev("x", 0), ev("\x1c", 0)}},
		{"modifyOtherKeys other key passes", []string{"\x1b[27;5;99~"}, []keyEvent{ev("\x1b[27;5;99~", 0)}},
		{"lone esc passes at once", []string{"\x1b"}, []keyEvent{ev("\x1b", 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKeyReader(DefaultPrefix)
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
	c := &client{cfg: Config{Parser: vt.Fake{}, Logf: t.Logf}, pass: -1, carry: -1, modes: map[int]bool{},
		keys: newKeyReader(DefaultPrefix), w: w, h: h}
	c.canvas.resize(w, h)
	for _, id := range ids {
		c.views = append(c.views, &view{spec: PaneSpec{ID: id, Label: id}, stream: &habconn.PaneStream{}, cols: w, rows: h})
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
	v := c.views[0]
	c.frame(snapshotFrame(v, "hi"))
	c.render()
	if c.pass != 0 {
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
		v := c.views[0]
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
