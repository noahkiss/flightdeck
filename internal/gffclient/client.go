// Package gffclient is the core of gff, the terminal client: it shows one or more panes of one
// hab in the terminal it runs in (docs/specs/gff-client.md 3, 5.2, 8.2, 10).
//
// One pane that fits the host terminal passes its bytes straight through. Otherwise each pane is
// parsed by a terminal parser (internal/vt: libghostty in release builds) and its cells are
// composed into the host screen, where only changed cells are written. Input goes to the
// focused pane as pane.input; the hab hands size ownership to the client that types. The client
// asks for each pane's size as its view changes, and never answers a terminal query.
//
// The connection is internal/habconn; merging several habs, tabs, the sidebar and the bars are
// later stages.
package gffclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// DefaultSizeDebounce is how long the host must stay one size before gff asks the hab for the
// panes' new sizes. Repainting from the parsers happens at once; only the request waits.
const DefaultSizeDebounce = 300 * time.Millisecond

// PaneSpec is one pane to show.
type PaneSpec struct {
	ID    string
	Label string // shown in the pane's frame; default the pane id
	// Observe opens an observer stream (hab-logic-api.md 6.4): it never sends a size, never
	// takes ownership and never sends input.
	Observe bool
}

// Timer is the debounce timer; tests replace it.
type Timer interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

// Config is what Run needs. Conn, Parser, Panes, In, Out and Size are required.
type Config struct {
	Conn   *habconn.Conn
	Parser vt.Parser
	Panes  []PaneSpec
	Split  Split
	// Prefix is the prefix key. Zero: DefaultPrefix.
	Prefix byte

	// The host terminal: its input (already in raw mode), its output, its size, and a signal
	// on each size change.
	In      io.Reader
	Out     io.Writer
	Size    func() (cols, rows int)
	Resized <-chan struct{}

	// SizeDebounce: DefaultSizeDebounce when zero.
	SizeDebounce time.Duration
	// NewTimer makes the debounce timer, stopped. Nil: a real timer.
	NewTimer func() Timer
	Logf     func(format string, args ...any)
}

// ErrDetached is Run's result when the owner detaches with the prefix and d.
var ErrDetached = errors.New("gff: detached; the programs keep running")

// view is one pane on screen.
type view struct {
	spec   PaneSpec
	stream *habconn.PaneStream
	term   vt.Terminal
	in     *inputSender

	cols, rows int
	owner      string
	opens      int // the stream's data connections when the last snapshot came
	exited     *api.ExitedFrame
	ended      error // the stream ended for good
	note       string

	filter passFilter
	screen vt.Screen
	dirty  bool

	sentCols, sentRows int // the last size asked for
}

func (v *view) live() bool { return v.exited == nil && v.ended == nil && v.term != nil }

type paneFrame struct {
	v     *view
	f     habconn.Frame
	ended bool
	err   error
}

type client struct {
	cfg     Config
	views   []*view
	focus   int
	split   Split
	zoom    bool
	offline bool
	me      string // this client's id on the hab

	w, h     int
	canvas   canvas
	pass     int // the view passing bytes through, or -1
	composed bool
	modes    map[int]bool // modes mirrored on the host while composed
	out      bytes.Buffer
	keys     *keyReader
	ops      *opQueue
	notes    chan paneNote
	debounce Timer
	// What the host shows besides the canvas, so an unchanged screen writes nothing.
	cursorX, cursorY int
	cursorOn         bool
}

type paneNote struct{ pane, msg string }

// Run shows the panes until the owner detaches (ErrDetached), every pane has ended (nil), ctx
// ends, or the connection fails for good. It leaves the host terminal as it found it, except
// for what the panes printed while passing through.
func Run(ctx context.Context, cfg Config) (err error) {
	if cfg.Conn == nil || cfg.Parser == nil || len(cfg.Panes) == 0 || cfg.In == nil || cfg.Out == nil || cfg.Size == nil {
		return errors.New("gffclient: Conn, Parser, Panes, In, Out and Size are required")
	}
	if cfg.Prefix == 0 {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.SizeDebounce <= 0 {
		cfg.SizeDebounce = DefaultSizeDebounce
	}
	if cfg.NewTimer == nil {
		cfg.NewTimer = newRealTimer
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	c := &client{cfg: cfg, split: cfg.Split, pass: -1, modes: map[int]bool{}, keys: newKeyReader(cfg.Prefix),
		notes: make(chan paneNote, 64), debounce: cfg.NewTimer()}
	defer func() {
		cancel()
		c.debounce.Stop()
		for _, v := range c.views {
			v.stream.Close()
		}
		wg.Wait()
		for _, v := range c.views {
			if v.term != nil {
				v.term.Close()
			}
		}
	}()
	if w, ok := cfg.Conn.Welcome(); ok {
		c.me = w.ClientID
	}
	c.ops = newOpQueue(cfg.Conn, func(pane, msg string) { c.postNote(pane, msg) })
	wg.Add(1)
	go func() { defer wg.Done(); c.ops.run(ctx) }()

	frames := make(chan paneFrame)
	for _, spec := range cfg.Panes {
		if spec.Label == "" {
			spec.Label = spec.ID
		}
		s, err := cfg.Conn.OpenPane(ctx, spec.ID, habconn.PaneOptions{Observe: spec.Observe})
		if err != nil {
			return fmt.Errorf("open pane %s: %w", spec.ID, err)
		}
		w := s.Welcome()
		v := &view{spec: spec, stream: s, cols: w.Cols, rows: w.Rows, owner: w.Owner}
		c.views = append(c.views, v)
		pane := spec.ID
		v.in = newInputSender(cfg.Conn, pane, func(msg string) { c.postNote(pane, msg) })
		wg.Add(2)
		go func() { defer wg.Done(); v.in.run(ctx) }()
		go func() {
			defer wg.Done()
			for f := range s.Frames() {
				select {
				case frames <- paneFrame{v: v, f: f}:
				case <-ctx.Done():
					return
				}
			}
			select {
			case frames <- paneFrame{v: v, ended: true, err: s.Err()}:
			case <-ctx.Done():
			}
		}()
	}

	input := make(chan []byte)
	inputErr := make(chan error, 1)
	go func() { // not waited for: a terminal read cannot be interrupted
		buf := make([]byte, 32<<10)
		for {
			n, err := cfg.In.Read(buf)
			if n > 0 {
				select {
				case input <- append([]byte(nil), buf[:n]...):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				inputErr <- err
				return
			}
		}
	}()

	c.w, c.h = c.hostSize()
	c.canvas.resize(c.w, c.h)
	c.sendSizes()
	defer func() {
		c.restoreHost()
		cfg.Out.Write(c.out.Bytes())
	}()

	updates := cfg.Conn.Updates()
	for {
		c.render()
		if c.out.Len() > 0 {
			if _, err := cfg.Out.Write(c.out.Bytes()); err != nil {
				return fmt.Errorf("write to the terminal: %w", err)
			}
			c.out.Reset()
		}
		if done, err := c.allEnded(); done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pf := <-frames:
			c.frame(pf)
			// Apply what else is ready before drawing once.
			for range 64 {
				select {
				case pf := <-frames:
					c.frame(pf)
					continue
				default:
				}
				break
			}
		case p := <-input:
			if err := c.input(p); err != nil {
				return err
			}
		case err := <-inputErr:
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read the terminal: %w", err)
		case <-cfg.Resized:
			w, h := c.hostSize()
			if w != c.w || h != c.h {
				c.w, c.h = w, h
				c.canvas.resize(w, h)
				c.layoutChanged()
			}
		case <-c.debounce.C():
			c.sendSizes()
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if err := c.update(u); err != nil {
				return err
			}
		case n := <-c.notes:
			for _, v := range c.views {
				if v.spec.ID == n.pane {
					v.note = n.msg
					v.dirty = true
				}
			}
		}
	}
}

func (c *client) postNote(pane, msg string) {
	select {
	case c.notes <- paneNote{pane, msg}:
	default:
	}
}

func (c *client) hostSize() (int, int) {
	w, h := c.cfg.Size()
	if w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// allEnded reports whether every pane has exited or gone, with the reason to return.
func (c *client) allEnded() (bool, error) {
	for _, v := range c.views {
		if v.exited == nil && v.ended == nil {
			return false, nil
		}
	}
	for _, v := range c.views {
		if v.ended != nil && !errors.Is(v.ended, io.EOF) && v.exited == nil {
			return true, fmt.Errorf("pane %s: %w", v.spec.ID, v.ended)
		}
	}
	return true, nil
}

// frame applies one frame of a pane's stream.
func (c *client) frame(pf paneFrame) {
	v := pf.v
	if pf.ended {
		v.ended = pf.err
		if v.ended == nil {
			v.ended = io.EOF
		}
		v.dirty = true
		return
	}
	f := pf.f
	switch f.Kind {
	case api.FrameSnapshot:
		cols, rows := v.cols, v.rows
		if n := v.stream.Opens(); n != v.opens {
			// The first snapshot on a new data connection: its welcome has the size.
			v.opens = n
			w := v.stream.Welcome()
			cols, rows, v.owner = w.Cols, w.Rows, w.Owner
		}
		if v.term != nil {
			v.term.Close()
		}
		cfg := vt.Config{Cols: max(cols, 1), Rows: max(rows, 1)}
		t, err := c.cfg.Parser.Restore(vt.Encoding(f.Encoding), f.Data, cfg)
		if err != nil {
			// An encoding this parser cannot read: the bytes still draw in a new terminal.
			if t, err = c.cfg.Parser.New(cfg); err == nil {
				t.Write(f.Data)
			}
		}
		if err != nil {
			c.cfg.Logf("pane %s: parser: %v", v.spec.ID, err)
			v.ended = err
			return
		}
		v.term, v.cols, v.rows = t, cfg.Cols, cfg.Rows
		v.filter.reset()
		if c.pass >= 0 && c.views[c.pass] == v {
			c.pass = -1 // pass through again from the new screen
		}
	case api.FrameBytes:
		if v.term == nil {
			return
		}
		v.term.Write(f.Data)
		start := c.out.Len()
		v.filter.write(&c.out, f.Data)
		if c.pass < 0 || c.views[c.pass] != v {
			c.out.Truncate(start) // only the pass-through view reaches the host
		}
	case api.FrameResize:
		if r := f.Resize; r != nil {
			v.owner = r.Owner
			if v.term != nil && r.Cols > 0 && r.Rows > 0 {
				v.term.Resize(r.Cols, r.Rows)
				v.cols, v.rows = r.Cols, r.Rows
			}
		}
	case api.FrameExited:
		v.exited = f.Exited
		if v.exited == nil {
			v.exited = &api.ExitedFrame{}
		}
	}
	v.dirty = true
}

// input handles host input.
func (c *client) input(p []byte) error {
	for _, ev := range c.keys.feed(p) {
		if ev.cmd == 0 {
			c.typeKeys(ev.data)
			continue
		}
		if err := c.command(ev.cmd); err != nil {
			return err
		}
	}
	return nil
}

// typeKeys sends bytes to the focused pane.
func (c *client) typeKeys(p []byte) {
	v := c.focused()
	switch {
	case v.spec.Observe:
		v.note, v.dirty = "observer: input off", true
	case !v.live():
	case c.offline:
		v.note, v.dirty = "offline: input dropped", true
	default:
		v.in.send(p)
	}
}

func (c *client) focused() *view { return c.views[c.focus] }

func (c *client) command(r rune) error {
	v := c.focused()
	switch r {
	case cmdDetach:
		return ErrDetached
	case cmdFocusNext, '\t':
		c.focus = (c.focus + 1) % len(c.views)
		if c.zoom {
			c.layoutChanged()
		}
	case cmdZoom:
		if len(c.views) > 1 {
			c.zoom = !c.zoom
			c.layoutChanged()
		}
	case cmdSplit:
		if c.split == SplitColumns {
			c.split = SplitRows
		} else {
			c.split = SplitColumns
		}
		c.layoutChanged()
	case cmdRedraw, cmdNudge:
		mode := api.RedrawModeSnapshot
		if r == cmdNudge {
			if v.spec.Observe {
				v.note, v.dirty = "observer: no nudge", true
				return nil
			}
			mode = api.RedrawModeNudge
		}
		c.ops.push(op{kind: opRedraw, pane: v.spec.ID, mode: mode})
		c.canvas.valid = false
		c.pass = -1
	case cmdFit:
		if v.spec.Observe {
			v.note, v.dirty = "observer: cannot fit", true
			return nil
		}
		c.ops.push(op{kind: opTake, pane: v.spec.ID})
	}
	return nil
}

// layoutChanged repaints at once and asks for the new sizes after the debounce.
func (c *client) layoutChanged() {
	c.canvas.valid = false
	for _, v := range c.views {
		v.dirty = true
	}
	c.debounce.Reset(c.cfg.SizeDebounce)
}

// sendSizes asks for each shown pane's size where it changed. Hidden and observer views ask
// for nothing (hab-logic-api.md 6.4).
func (c *client) sendSizes() {
	a := arrange(len(c.views), c.focus, c.split, c.zoom, c.w, c.h)
	for _, s := range a.slots {
		v := c.views[s.view]
		if v.spec.Observe || (v.sentCols == s.wantCols && v.sentRows == s.wantRows) {
			continue
		}
		v.sentCols, v.sentRows = s.wantCols, s.wantRows
		c.ops.push(op{kind: opSize, pane: v.spec.ID, cols: s.wantCols, rows: s.wantRows})
	}
}

// update handles one connection update.
func (c *client) update(u habconn.Update) error {
	switch u.Kind {
	case habconn.UpdateOnline:
		c.offline = false
		c.me = u.Welcome.ClientID
		if u.NewEpoch {
			// A new hab process keeps no desired sizes and no owners: ask again.
			for _, v := range c.views {
				v.sentCols, v.sentRows = 0, 0
			}
			c.sendSizes()
		}
		for _, v := range c.views {
			v.dirty = true
		}
	case habconn.UpdateOffline:
		c.offline = true
		for _, v := range c.views {
			v.dirty = true
		}
	case habconn.UpdateFailed:
		return fmt.Errorf("hab connection: %w", u.Err)
	}
	return nil
}

// Host terminal sequences.
const (
	// Modes a pane may have set on the host while passing through, set back to the defaults.
	resetPaneModes = "\x1b[0m\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" +
		"\x1b[?1004l\x1b[?2004l\x1b[?1l\x1b>\x1b[=0;1u\x1b[4l\x1b[?6l\x1b[r\x1b[?7h\x1b[ q\x1b[?25h"
	enterCompose = "\x1b[?1049h\x1b[?7l\x1b[?25l"
	leaveCompose = "\x1b[0m\x1b[?7h\x1b[?25h\x1b[?1049l"
)

// mirrored are the focused pane's input modes that the composed view sets on the host, so
// the host encodes keys and pastes the way the program asked (gff-client.md 8.2): cursor keys,
// keypad, bracketed paste.
var mirrored = []struct {
	mode    int
	on, off string
}{
	{1, "\x1b[?1h", "\x1b[?1l"},
	{66, "\x1b=", "\x1b>"},
	{2004, "\x1b[?2004h", "\x1b[?2004l"},
}

// render brings the host terminal up to date: pass-through when one pane fits it, else a
// composed screen.
func (c *client) render() {
	a := arrange(len(c.views), c.focus, c.split, c.zoom, c.w, c.h)
	if len(a.slots) == 1 {
		i := a.slots[0].view
		v := c.views[i]
		if v.term == nil && v.ended == nil {
			return // wait for the first snapshot: it may pass straight through
		}
		if v.live() && !c.offline && v.cols == c.w && v.rows == c.h {
			if c.pass != i {
				c.enterPass(i)
			}
			return
		}
	}
	if c.pass >= 0 || !c.composed {
		if c.pass >= 0 {
			c.out.WriteString(resetPaneModes)
			if !c.composed {
				c.out.WriteString("\x1b[H\x1b[2J")
			}
		}
		c.out.WriteString(enterCompose)
		c.pass, c.composed = -1, true
		c.canvas.valid = false
		c.cursorOn = false
		clear(c.modes)
	}
	c.compose(a)
}

// enterPass makes view i pass its bytes through: the host leaves the composed screen and
// gets the pane's screen and modes from the local parser, then the pane's bytes as they come.
func (c *client) enterPass(i int) {
	v := c.views[i]
	if c.composed {
		c.out.WriteString(resetMirrored(c.modes))
		c.out.WriteString(leaveCompose)
		c.composed = false
	}
	c.out.WriteString(resetPaneModes)
	c.out.WriteString("\x1b[H\x1b[2J")
	snap, err := v.term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: 0})
	if err != nil {
		c.cfg.Logf("pane %s: snapshot: %v", v.spec.ID, err)
	}
	// A snapshot holds no query (hab-logic-api.md 5.5); the filter is a cheap guard all the same.
	var f passFilter
	f.write(&c.out, snap)
	c.pass = i
}

func resetMirrored(modes map[int]bool) string {
	var b bytes.Buffer
	for _, m := range mirrored {
		if modes[m.mode] {
			b.WriteString(m.off)
		}
	}
	clear(modes)
	return b.String()
}

// restoreHost leaves the host terminal as gff found it.
func (c *client) restoreHost() {
	switch {
	case c.composed:
		c.out.WriteString(resetMirrored(c.modes))
		c.out.WriteString(leaveCompose)
	case c.pass >= 0:
		v := c.views[c.pass]
		alt := false
		if v.term != nil {
			alt, _ = v.term.Mode(1049, false)
		}
		c.out.WriteString(resetPaneModes)
		if alt {
			c.out.WriteString("\x1b[?1049l")
		} else {
			c.out.WriteString("\x1b[" + strconv.Itoa(c.h) + ";1H\r\n")
		}
	}
	c.composed, c.pass = false, -1
}

// compose draws every shown pane, its frame row and the separators, and writes the changes.
func (c *client) compose(a arrangement) {
	c.canvas.clear()
	sepStyle := vt.Style{Faint: true}
	for _, r := range a.seps {
		for y := r.y; y < r.y+r.h; y++ {
			c.canvas.set(r.x, y, hcell{text: "│", style: sepStyle})
		}
	}
	var cursorX, cursorY int
	cursorOn := false
	for _, s := range a.slots {
		v := c.views[s.view]
		focused := s.view == c.focus
		if v.term != nil {
			if v.dirty || v.screen.Cells == nil {
				if err := v.term.ReadScreen(&v.screen); err != nil {
					c.cfg.Logf("pane %s: read screen: %v", v.spec.ID, err)
				}
				v.dirty = false
			}
			c.canvas.pane(&v.screen, s.content, c.offline || !v.live())
			if focused && v.live() && v.screen.CursorVisible {
				x := v.screen.CursorX - clampOrigin(v.screen.CursorX, v.screen.Cols, s.content.w)
				y := v.screen.CursorY - clampOrigin(v.screen.CursorY, v.screen.Rows, s.content.h)
				if x >= 0 && y >= 0 && x < s.content.w && y < s.content.h {
					cursorX, cursorY, cursorOn = s.content.x+x, s.content.y+y, true
				}
			}
		}
		st := vt.Style{Inverse: true, Faint: !focused, Bold: focused}
		c.canvas.text(s.frame.x, s.frame.y, s.frame.w, c.frameText(v, s), st)
	}
	start := c.out.Len()
	c.out.WriteString("\x1b[?25l")
	mark := c.out.Len()
	c.canvas.flush(&c.out)
	changed := c.out.Len() > mark
	// The focused pane's input modes, mirrored on the host.
	fv := c.focused()
	for _, m := range mirrored {
		want := false
		if fv.term != nil && fv.live() {
			want, _ = fv.term.Mode(m.mode, false)
		}
		if want != c.modes[m.mode] {
			if want {
				c.out.WriteString(m.on)
			} else {
				c.out.WriteString(m.off)
			}
			c.modes[m.mode] = want
			changed = true
		}
	}
	if !changed && cursorOn == c.cursorOn && cursorX == c.cursorX && cursorY == c.cursorY {
		c.out.Truncate(start)
		return
	}
	c.cursorX, c.cursorY, c.cursorOn = cursorX, cursorY, cursorOn
	if cursorOn {
		c.out.WriteString("\x1b[" + strconv.Itoa(cursorY+1) + ";" + strconv.Itoa(cursorX+1) + "H\x1b[?25h")
	}
}

// frameText is a pane's frame row: its label and a short note (gff-client.md 3.1).
func (c *client) frameText(v *view, s slot) string {
	t := " " + v.spec.Label
	var notes []string
	switch {
	case v.exited != nil:
		notes = append(notes, "exited"+exitText(v.exited))
	case v.ended != nil:
		notes = append(notes, "gone")
	case c.offline:
		notes = append(notes, "offline")
	}
	if v.spec.Observe {
		notes = append(notes, "observer")
	}
	if v.term != nil && (v.cols != s.content.w || v.rows != s.content.h) {
		if v.owner != "" && v.owner != c.me {
			notes = append(notes, "owner "+short(v.owner)+" "+strconv.Itoa(v.cols)+"x"+strconv.Itoa(v.rows))
		} else {
			notes = append(notes, strconv.Itoa(v.cols)+"x"+strconv.Itoa(v.rows))
		}
	}
	if v.note != "" {
		notes = append(notes, v.note)
	}
	for _, n := range notes {
		t += " · " + n
	}
	return t + " "
}

func exitText(e *api.ExitedFrame) string {
	switch {
	case e.Code != nil:
		return " " + strconv.Itoa(*e.Code)
	case e.Signal != "":
		return " " + e.Signal
	}
	return ""
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

type realTimer struct{ t *time.Timer }

func newRealTimer() Timer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return realTimer{t}
}

func (r realTimer) C() <-chan time.Time   { return r.t.C }
func (r realTimer) Reset(d time.Duration) { r.t.Reset(d) }
func (r realTimer) Stop()                 { r.t.Stop() }
