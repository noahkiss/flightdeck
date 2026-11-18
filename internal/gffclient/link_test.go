package gffclient

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// hostLinks reads the bytes gff wrote to the host as a terminal would for OSC 8: it follows
// cursor moves (CUP) and the open link, and returns each printed cell's link as "id;uri", or ""
// for none, keyed by "x,y". It also returns the link still open at the end.
func hostLinks(t *testing.T, out []byte) (map[string]string, string) {
	t.Helper()
	cells := map[string]string{}
	link := ""
	x, y := 0, 0
	for len(out) > 0 {
		switch {
		case bytes.HasPrefix(out, []byte("\x1b]8;")):
			end := bytes.Index(out, []byte("\x1b\\"))
			if end < 0 {
				t.Fatalf("an OSC 8 without its ST: %q", out)
			}
			params, uri, ok := strings.Cut(string(out[4:end]), ";")
			if !ok {
				t.Fatalf("OSC 8 %q: no URI field", out[:end])
			}
			link = ""
			if uri != "" {
				link = strings.TrimPrefix(params, "id=") + ";" + uri
			}
			out = out[end+2:]
		case bytes.HasPrefix(out, []byte("\x1b[")):
			i := 2
			for i < len(out) && (out[i] < 0x40 || out[i] > 0x7e) {
				i++
			}
			if out[i] == 'H' {
				row, col, _ := strings.Cut(string(out[2:i]), ";")
				y, _ = strconv.Atoi(row)
				x, _ = strconv.Atoi(col)
				x, y = max(x-1, 0), max(y-1, 0)
			}
			out = out[i+1:]
		case out[0] == 0x1b:
			t.Fatalf("unexpected escape %q", out)
		default:
			r := []rune(string(out[:min(len(out), 4)]))[0]
			cells[strconv.Itoa(x)+","+strconv.Itoa(y)] = link
			x++
			out = out[len(string(r)):]
		}
	}
	return cells, link
}

// The canvas carries each pane cell's OSC 8 link to the host: it opens the link with an id before
// the first linked cell of a run and closes it before the next cell that is not in it, at a line
// end, at a pane's edge, before a cursor move and at the end of a frame. The same URI in two panes
// gets two ids, so a terminal never joins them on hover.
func TestCanvasLinks(t *testing.T) {
	const uri = "https://example.com/a"
	pane := func(rows ...string) *vt.Screen {
		var s vt.Screen
		s.Reset(4, len(rows))
		for y, r := range rows {
			for x, ch := range r {
				c := vt.Cell{Text: string(ch)}
				if ch >= 'A' && ch <= 'Z' {
					c.Link = uri
				}
				s.Cells[y*4+x] = c
			}
		}
		return &s
	}
	var c canvas
	c.resize(9, 2)
	// Upper case cells are linked: a run then plain text, a run to the pane's right edge, and the
	// other pane's link at its own left edge, right after the separator.
	c.pane(pane("ABcd", "EFGH"), rect{0, 0, 4, 2}, false, "left")
	for y := range 2 {
		c.set(4, y, hcell{text: "│"})
	}
	c.pane(pane("wxYZ", "IJkl"), rect{5, 0, 4, 2}, false, "right")
	var out bytes.Buffer
	c.flush(&out)
	cells, open := hostLinks(t, out.Bytes())
	if open != "" {
		t.Errorf("the frame ends with the link %q open", open)
	}
	left, right := paneLink("left", uri), paneLink("right", uri)
	if left.id == right.id || !strings.HasPrefix(left.id, "gff-") {
		t.Fatalf("ids %q and %q: want two gff ids", left.id, right.id)
	}
	l, r := left.id+";"+uri, right.id+";"+uri
	want := map[string]string{
		"0,0": l, "1,0": l, "2,0": "", "3,0": "", "4,0": "", "5,0": "", "6,0": "", "7,0": r, "8,0": r,
		"0,1": l, "1,1": l, "2,1": l, "3,1": l, "4,1": "", "5,1": r, "6,1": r, "7,1": "", "8,1": "",
	}
	for k, w := range want {
		if got, ok := cells[k]; !ok || got != w {
			t.Errorf("cell %s: link %q (written %v), want %q", k, got, ok, w)
		}
	}
	if !strings.Contains(out.String(), "\x1b]8;id="+left.id+";"+uri+"\x1b\\AB\x1b]8;;\x1b\\c") {
		t.Errorf("the first run is not wrapped in its link: %q", out.String())
	}

	// A later frame that changes one linked cell writes that cell inside its link, and closes it.
	out.Reset()
	c.clear()
	c.pane(pane("AXcd", "EFGH"), rect{0, 0, 4, 2}, false, "left")
	for y := range 2 {
		c.set(4, y, hcell{text: "│"})
	}
	c.pane(pane("wxYZ", "IJkl"), rect{5, 0, 4, 2}, false, "right")
	c.flush(&out)
	if got, w := out.String(), "\x1b[1;2H\x1b]8;id="+left.id+";"+uri+"\x1b\\X\x1b]8;;\x1b\\"; got != w {
		t.Errorf("one changed linked cell wrote %q, want %q", got, w)
	}

	// A URI with a control byte is no link.
	if l := paneLink("p", "https://x\x1b]2;evil"); l != (hlink{}) {
		t.Errorf("a URI with ESC gave %+v", l)
	}
	if l := paneLink("p", strings.Repeat("a", maxLinkBytes+1)); l != (hlink{}) {
		t.Errorf("a long URI gave a link")
	}
}
