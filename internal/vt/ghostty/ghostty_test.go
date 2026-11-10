//go:build ghostty

package ghostty

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/vttest"
)

func TestParser(t *testing.T) {
	vttest.Run(t, Parser{}, "fixture.golden")
}

// A ghostty snapshot keeps colours and styles: the VT rendering of the restored terminal matches
// the original byte for byte.
func TestGhosttySnapshotKeepsStyles(t *testing.T) {
	cfg := vt.Config{Cols: vttest.FixtureCols, Rows: vttest.FixtureRows}
	p := Parser{}
	term, err := p.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.Write(vttest.Fixture(t))
	want, err := term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(want, []byte("38;2;10;20;30")) {
		t.Fatalf("VT snapshot lost the truecolour run: %q", want)
	}
	snap, err := term.Snapshot(vt.EncodingGhostty, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := p.Restore(vt.EncodingGhostty, snap, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("VT rendering differs after ghostty restore\n got %q\nwant %q", got, want)
	}
}

// ReadScreen keeps palette colours as indexes, true colours as RGB, and attributes.
func TestReadScreenStyles(t *testing.T) {
	term, err := Parser{}.New(vt.Config{Cols: 20, Rows: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.Write([]byte("\x1b[1;31ma\x1b[0;38;2;10;20;30;4mb\x1b[0mc\x1b[?25l"))
	var s vt.Screen
	if err := term.ReadScreen(&s); err != nil {
		t.Fatal(err)
	}
	a, b, c := s.Cells[0].Style, s.Cells[1].Style, s.Cells[2].Style
	if !a.Bold || a.Fg != (vt.Color{Kind: vt.ColorPalette, Index: 1}) {
		t.Errorf("a style = %+v, want bold palette 1", a)
	}
	if b.Bold || b.Underline != vt.UnderlineSingle || b.Fg != (vt.Color{Kind: vt.ColorRGB, R: 10, G: 20, B: 30}) {
		t.Errorf("b style = %+v, want underlined rgb 10,20,30", b)
	}
	if c != (vt.Style{}) {
		t.Errorf("c style = %+v, want default", c)
	}
	if s.CursorVisible {
		t.Error("cursor visible after DECTCEM reset")
	}
}

// A restored terminal keeps writing into the bottom page it restored, so restarts cost no
// memory: a terminal snapshotted, restored and written to 60 times holds the same pages and
// the same scrollback as one that parsed the same bytes. Unpatched libghostty restores that
// page full, every cycle leaves one more mostly empty page behind, and at this cap of ten
// pages the chained terminal drops scrollback the reference keeps (the nightly restart loop's
// failure). third_party/ghostty/patches/0001 fixes it.
func TestRestoreLeavesRoom(t *testing.T) {
	cfg := vt.Config{Cols: 80, Rows: 24, ScrollbackBytes: 4 << 20}
	p := Parser{}
	ref, err := p.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	cur, err := p.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cur.Close() }()
	line := 0
	for cycle := range 60 {
		var b bytes.Buffer
		for range 7 {
			line++
			fmt.Fprintf(&b, "\x1b[3%dmline %d\x1b[0m \x1b[1m世界\x1b[0m\r\n", line%8, line)
		}
		ref.Write(b.Bytes())
		cur.Write(b.Bytes())
		snap, err := cur.Snapshot(vt.EncodingGhostty, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
		if err != nil {
			t.Fatal(err)
		}
		next, err := p.Restore(vt.EncodingGhostty, snap, cfg)
		if err != nil {
			t.Fatal(err)
		}
		cur.Close()
		cur = next
		want, _, _ := ref.(*terminal).MemoryUsage()
		got, _, _ := cur.(*terminal).MemoryUsage()
		if got != want {
			t.Fatalf("cycle %d: the restored terminal holds %d bytes of pages, the reference %d", cycle, got, want)
		}
	}
	wantText, _ := ref.PlainText()
	gotText, _ := cur.PlainText()
	if gotText != wantText {
		t.Fatalf("after 60 restores the text differs: %d lines, the reference %d",
			strings.Count(gotText, "\n"), strings.Count(wantText, "\n"))
	}
}
