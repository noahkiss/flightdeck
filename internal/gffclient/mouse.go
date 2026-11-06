package gffclient

// Scrolling and the mouse in the composed view (gff-client.md 5.7). While composed, gff asks
// the host for mouse reports (SGR, mode 1006). The wheel over a view scrolls its local
// scrollback, sending nothing to the hab; over a program that asked for the mouse it goes to
// the program, and on an alternate screen with no mouse mode it is arrow keys. A click focuses
// the view under it. Shift+PageUp, Shift+PageDown and Shift+End scroll the focused view by keys.

import (
	"bytes"
	"strconv"
)

// wheelRows is how many rows one wheel notch scrolls, as zellij's default.
const wheelRows = 3

// Host keys that scroll the focused view in normal mode (xterm's Shift forms).
var (
	keyPageUp   = []byte("\x1b[5;2~")
	keyPageDown = []byte("\x1b[6;2~")
	keyEnd      = []byte("\x1b[1;2F")
	sgrMouse    = []byte("\x1b[<")
)

// mouseEvent is one SGR mouse report: the button code and its 1-based cell.
type mouseEvent struct {
	b, x, y int
	release bool
}

func (e mouseEvent) wheel() bool { return e.b&64 != 0 }

// parseMouse reads an SGR mouse report at the start of p, ESC [ < b ; x ; y then M or m. n is
// its length; partial reports that p ends inside one; n 0 and not partial means p does not
// start with one.
func parseMouse(p []byte) (e mouseEvent, n int, partial bool) {
	if !bytes.HasPrefix(p, sgrMouse) {
		// A lone ESC at the end of a read is Esc, never the start of a report.
		return e, 0, len(p) >= 2 && bytes.HasPrefix(sgrMouse, p)
	}
	var f [3]int
	k := 0
	for i := len(sgrMouse); i < len(p) && i < 32; i++ {
		c := p[i]
		switch {
		case c >= '0' && c <= '9':
			f[k] = f[k]*10 + int(c-'0')
			if f[k] > 1<<16 {
				return e, 0, false
			}
		case c == ';' && k < 2:
			k++
		case (c == 'M' || c == 'm') && k == 2:
			return mouseEvent{b: f[0], x: f[1], y: f[2], release: c == 'm'}, i + 1, false
		default:
			return e, 0, false
		}
	}
	return e, 0, len(p) < 32
}

// scrollKey reads one of the scroll keys at the start of p: the rows it moves (negative: up),
// bottom for Shift+End, its length, and whether p ends inside one.
func scrollKey(p []byte, page int) (delta int, bottom bool, n int, partial bool) {
	for _, k := range []struct {
		seq    []byte
		delta  int
		bottom bool
	}{{keyPageUp, -page, false}, {keyPageDown, page, false}, {keyEnd, 0, true}} {
		if bytes.HasPrefix(p, k.seq) {
			return k.delta, k.bottom, len(k.seq), false
		}
		if len(p) >= 2 && len(p) < len(k.seq) && bytes.HasPrefix(k.seq, p) {
			partial = true
		}
	}
	return 0, false, 0, partial
}

// takesMouse reports whether gff reads mouse reports and scroll keys: while composed, and not
// while a pane passes its bytes through (they are that pane's).
func (c *client) takesMouse() bool { return c.pass == nil && c.composed }

// input handles host input: mouse reports and scroll keys while composed, the rest through the
// key reader.
func (c *client) input(p []byte) error {
	if len(c.mouseHeld) > 0 {
		p = append(c.mouseHeld, p...)
		c.mouseHeld = nil
	}
	start := 0
	for i := 0; i < len(p); i++ {
		// Let the key reader enter or leave paste mode before inspecting the rest of
		// this read. Pasted text can contain mouse reports and scroll key sequences.
		marker := pasteStart
		if c.keys.paste {
			marker = pasteEnd
		}
		if len(c.keys.held) == 0 && bytes.HasPrefix(p[i:], marker) {
			end := i + len(marker)
			if err := c.keyInput(p[start:end]); err != nil {
				return err
			}
			i = end - 1
			start = end
			continue
		}
		if p[i] != 0x1b || !c.takesMouse() || c.keys.paste || len(c.keys.held) > 0 {
			continue
		}
		if e, n, partial := parseMouse(p[i:]); n > 0 || partial {
			if err := c.keyInput(p[start:i]); err != nil {
				return err
			}
			if partial {
				c.mouseHeld = append([]byte(nil), p[i:]...)
				return nil
			}
			c.mouse(e)
			i += n - 1
			start = i + 1
			continue
		}
		if c.keys.mode != ModeNormal || !c.panesFocused() {
			continue
		}
		v := c.focused()
		page := 1
		if v != nil {
			page = max(v.rows-1, 1)
		}
		if delta, bottom, n, partial := scrollKey(p[i:], page); n > 0 || partial {
			if err := c.keyInput(p[start:i]); err != nil {
				return err
			}
			if partial {
				c.mouseHeld = append([]byte(nil), p[i:]...)
				return nil
			}
			if v != nil && v.term != nil {
				if bottom {
					delta = v.scroll
				}
				c.scrollView(v, delta)
			}
			i += n - 1
			start = i + 1
		}
	}
	return c.keyInput(p[start:])
}

// keyInput feeds host input to the key reader and acts on what it makes.
func (c *client) keyInput(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	for _, ev := range c.keys.feed(p) {
		if ev.act == "" {
			if c.ws && c.ui.mode != modePanes {
				c.ui.keys(ev.data)
				continue
			}
			c.typeKeys(ev.data)
			continue
		}
		if err := c.command(ev); err != nil {
			return err
		}
	}
	return nil
}

// scrollView moves v's view delta rows (negative: up into the scrollback).
func (c *client) scrollView(v *view, delta int) {
	if v.term == nil || delta == 0 {
		return
	}
	v.scroll = v.term.ScrollView(delta)
	v.dirty = true
}

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// mouse handles one mouse report.
func (c *client) mouse(e mouseEvent) {
	x, y := e.x-1, e.y-1
	step := wheelRows
	if e.b&1 == 0 {
		step = -step // button 64: wheel up
	}
	if c.ws {
		u := &c.ui
		switch {
		case u.mode == modePrompt:
			return
		case u.mode == modeBoard || u.sidebarShown() && x < min(SidebarWidth, c.w-1) && y >= u.top() && y < c.h-1:
			if e.wheel() {
				u.move(step / wheelRows)
			}
			return
		}
	}
	t := c.tab()
	a := c.arrangement()
	for _, s := range a.slots {
		if !s.box().has(x, y) {
			continue
		}
		v := t.views[s.view]
		if !e.wheel() && !e.release && e.b&32 == 0 && e.b&3 == 0 && (s.view != t.focus || c.ws && c.ui.mode != modePanes) {
			// A click focuses the view under it, and gives the panes the keys.
			t.focus = s.view
			if c.ws {
				c.ui.mode = modePanes
				c.ui.focusChanged()
			}
			c.layoutChanged()
		}
		if v.term == nil || !v.live() {
			return
		}
		cx, cy := x-s.content.x, y-s.content.y
		if wantsMouse(v) {
			if s.content.has(x, y) && !v.spec.Observe && v.h.online {
				v.in.send(encodeMouse(v, e, cx+1, cy+1))
			}
			return
		}
		if !e.wheel() {
			return
		}
		if altScreen(v) {
			if v.spec.Observe || !v.h.online {
				return
			}
			key := "A"
			if step > 0 {
				key = "B"
			}
			prefix := "\x1b["
			if on, _ := v.term.Mode(1, false); on {
				prefix = "\x1bO" // cursor keys mode: the application form
			}
			v.in.send(bytes.Repeat([]byte(prefix+key), wheelRows))
			return
		}
		c.scrollView(v, step)
		return
	}
}

// wantsMouse reports whether v's program asked for mouse reports.
func wantsMouse(v *view) bool {
	for _, m := range []int{1000, 1002, 1003} {
		if on, _ := v.term.Mode(m, false); on {
			return true
		}
	}
	return false
}

// altScreen reports whether v's program is on the alternate screen.
func altScreen(v *view) bool {
	for _, m := range []int{1049, 1047, 47} {
		if on, _ := v.term.Mode(m, false); on {
			return true
		}
	}
	return false
}

// encodeMouse writes e for v's program at its cell x, y (1-based): SGR when it asked for mode
// 1006, else the legacy form (cells past 223 cannot be sent and clamp).
func encodeMouse(v *view, e mouseEvent, x, y int) []byte {
	if on, _ := v.term.Mode(1006, false); on {
		end := "M"
		if e.release {
			end = "m"
		}
		return []byte("\x1b[<" + strconv.Itoa(e.b) + ";" + strconv.Itoa(x) + ";" + strconv.Itoa(y) + end)
	}
	b := e.b
	if e.release && !e.wheel() {
		b = b&^3 | 3
	}
	return []byte{0x1b, '[', 'M', byte(32 + min(b, 223)), byte(32 + min(x, 223)), byte(32 + min(y, 223))}
}
