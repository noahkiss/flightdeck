package gffclient

import (
	"bytes"
	"strconv"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// hcell is one cell of the composed host screen.
type hcell struct {
	text   string // "" draws a space
	wide   bool
	spacer bool // the second half of a wide cell: never drawn
	style  vt.Style
}

// canvas is the composed host screen, and the screen last written to the host. Writing sends
// only the cells that changed (gff-client.md 3.2).
type canvas struct {
	w, h       int
	next, prev []hcell
	valid      bool // prev holds what the host shows
	cur        vt.Style
	curValid   bool
}

func (c *canvas) resize(w, h int) {
	if w == c.w && h == c.h && c.next != nil {
		return
	}
	c.w, c.h = w, h
	c.next = make([]hcell, w*h)
	c.prev = make([]hcell, w*h)
	c.valid = false
}

func (c *canvas) clear() { clear(c.next) }

func (c *canvas) set(x, y int, hc hcell) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	c.next[y*c.w+x] = hc
}

// text writes s from x on row y within width w, in style st, padding with spaces. A control
// character (C0, DEL, C1) draws as U+FFFD, so no text reaches the host terminal as a control,
// whoever wrote it.
func (c *canvas) text(x, y, w int, s string, st vt.Style) {
	i := 0
	for _, r := range s {
		if i >= w {
			break
		}
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			r = '\uFFFD'
		}
		c.set(x+i, y, hcell{text: string(r), style: st})
		i++
	}
	for ; i < w; i++ {
		c.set(x+i, y, hcell{style: st})
	}
}

// pane draws a pane's screen into r. When the pane is larger than r, the shown part follows
// the cursor; when smaller, the rest is filled dim (gff-client.md 10.1, not the owner).
func (c *canvas) pane(s *vt.Screen, r rect, dim bool) {
	ox := clampOrigin(s.CursorX, s.Cols, r.w)
	oy := clampOrigin(s.CursorY, s.Rows, r.h)
	fill := hcell{text: "·", style: vt.Style{Faint: true}}
	for y := range r.h {
		sy := oy + y
		for x := 0; x < r.w; x++ {
			sx := ox + x
			if sx >= s.Cols || sy >= s.Rows {
				c.set(r.x+x, r.y+y, fill)
				continue
			}
			sc := s.Cells[sy*s.Cols+sx]
			st := sc.Style
			if dim {
				st.Faint = true
			}
			hc := hcell{text: sc.Text, style: st}
			switch {
			case sc.Spacer:
				hc = hcell{style: st} // the first column cut a wide cell in half
			case sc.Wide && x+1 >= r.w:
				hc = hcell{style: st} // a wide cell cut by the right edge
			case sc.Wide:
				hc.wide = true
				c.set(r.x+x, r.y+y, hc)
				c.set(r.x+x+1, r.y+y, hcell{spacer: true, style: st})
				x++
				continue
			}
			c.set(r.x+x, r.y+y, hc)
		}
	}
}

// clampOrigin returns the first pane cell shown in a view n cells long, keeping the cursor in
// view.
func clampOrigin(cursor, size, n int) int {
	if size <= n {
		return 0
	}
	return min(max(cursor-n+1, 0), size-n)
}

// flush appends to out the bytes that bring the host from prev to next, and makes prev next.
func (c *canvas) flush(out *bytes.Buffer) {
	if !c.valid {
		out.WriteString("\x1b[0m\x1b[H\x1b[2J")
		c.curValid = false
		for i := range c.prev {
			c.prev[i] = hcell{}
		}
	}
	atX, atY := -1, -1
	for y := range c.h {
		for x := 0; x < c.w; x++ {
			i := y*c.w + x
			n, p := c.next[i], c.prev[i]
			if c.valid && n == p && !(x > 0 && c.prev[i-1].wide != c.next[i-1].wide) {
				continue
			}
			if !c.valid && n == (hcell{}) {
				continue // the clear left it blank
			}
			if n.spacer {
				continue
			}
			if atX != x || atY != y {
				out.WriteString("\x1b[")
				out.WriteString(strconv.Itoa(y + 1))
				out.WriteByte(';')
				out.WriteString(strconv.Itoa(x + 1))
				out.WriteByte('H')
			}
			if !c.curValid || c.cur != n.style {
				writeSGR(out, n.style)
				c.cur, c.curValid = n.style, true
			}
			if n.text == "" {
				out.WriteByte(' ')
			} else {
				out.WriteString(n.text)
			}
			atX, atY = x+1, y
			if n.wide {
				atX = -1 // the host may give it another width: place the next cell
			}
		}
	}
	copy(c.prev, c.next)
	c.valid = true
}

// writeSGR writes one SGR sequence that sets style st from a reset.
func writeSGR(out *bytes.Buffer, st vt.Style) {
	out.WriteString("\x1b[0")
	flag := func(on bool, code string) {
		if on {
			out.WriteByte(';')
			out.WriteString(code)
		}
	}
	flag(st.Bold, "1")
	flag(st.Faint, "2")
	flag(st.Italic, "3")
	switch st.Underline {
	case vt.UnderlineNone:
	case vt.UnderlineSingle:
		out.WriteString(";4")
	default:
		out.WriteString(";4:" + strconv.Itoa(int(st.Underline)))
	}
	flag(st.Blink, "5")
	flag(st.Inverse, "7")
	flag(st.Invisible, "8")
	flag(st.Strike, "9")
	flag(st.Overline, "53")
	writeColor(out, st.Fg, 30, 90, "38")
	writeColor(out, st.Bg, 40, 100, "48")
	if st.UnderlineColor.Kind != vt.ColorDefault {
		writeColor(out, st.UnderlineColor, -1, -1, "58")
	}
	out.WriteByte('m')
}

// writeColor writes a colour: the 16 base colours in their short forms when base >= 0, else
// the 256-colour or true-colour form under ext.
func writeColor(out *bytes.Buffer, c vt.Color, base, bright int, ext string) {
	switch c.Kind {
	case vt.ColorPalette:
		switch {
		case base >= 0 && c.Index < 8:
			out.WriteString(";" + strconv.Itoa(base+int(c.Index)))
		case base >= 0 && c.Index < 16:
			out.WriteString(";" + strconv.Itoa(bright+int(c.Index)-8))
		default:
			out.WriteString(";" + ext + ";5;" + strconv.Itoa(int(c.Index)))
		}
	case vt.ColorRGB:
		out.WriteString(";" + ext + ";2;" + strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B)))
	}
}
