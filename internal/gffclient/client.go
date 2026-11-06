// Package gffclient is gff, the terminal client: it shows panes of one or more habs in the
// terminal it runs in (docs/specs/gff-client.md 3 to 7, 8.2, 10).
//
// One pane that fits the host terminal passes its bytes straight through. Otherwise each pane is
// parsed by a terminal parser (internal/vt: libghostty in release builds) and its cells are
// composed into the host screen, where only changed cells are written. Input goes to the
// focused pane as pane.input; the hab hands size ownership to the client that types. The client
// asks for each pane's size as its view changes, and never answers a terminal query.
//
// It runs in one of two ways. The single-hab view (Config.Conn) shows the panes it is given and
// nothing else, and ends when they all end. The workspace (Config.Hosts) connects to every
// configured host at once and adds tabs, a sidebar of every session on every host and a status
// board (workspace.go). Structure comes from the habs; tabs, splits, focus and the sidebar stay
// in the client.
package gffclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// DefaultSizeDebounce is how long the host must stay one size before gff asks the hab for the
// panes' new sizes. Repainting from the parsers happens at once; only the request waits.
const DefaultSizeDebounce = 300 * time.Millisecond

// PaneSpec is one pane to show.
type PaneSpec struct {
	ID    string
	Label string // shown in the pane's frame; default the pane id
	// Host is the host's label in the workspace; empty means the first host.
	Host string
	// Session is the hab session the pane belongs to, when known. A view of a session follows
	// it to the new pane a resume starts (gff-client.md 6.2).
	Session string
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

// Config is what Run needs. Parser, In, Out and Size are required, and one of Conn (with
// Panes) and Hosts.
type Config struct {
	// Conn is the single-hab view: Panes of this hab and nothing else.
	Conn *habconn.Conn
	// Hosts is the workspace: every host, with tabs, the sidebar and the status board. Panes,
	// if any, open in the first tab.
	Hosts  *hosts.Set
	Parser vt.Parser
	Panes  []PaneSpec
	Split  Split
	// Keymap is the keys and their modes (gff-client.md 8). Nil: DefaultKeymap.
	Keymap *Keymap
	// Bar shapes the workspace's bars. Nil: DefaultBar.
	Bar *BarConfig
	// Stats reads this machine's system info for the shortcut bar. Nil: a LocalStats.
	Stats func() (api.HostStats, error)

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
	// Now is the clock for the times the workspace shows. Nil: time.Now.
	Now func() time.Time
	// Tick, when set, replaces the workspace's one-second redraw tick (times in state, bell
	// marks). Tests send on it.
	Tick <-chan time.Time
	Logf func(format string, args ...any)

	// Workspace is the workspace's name, shown beside the clock (gff-client.md 5.5). Empty:
	// none shows.
	Workspace string
	// TitleTemplate sets the host window title. Nil uses DefaultTitleTemplate; empty disables it.
	TitleTemplate *string
	// StatePath is the workspace's file (WorkspacePath): gff restores the tabs from it at start
	// and writes them back on each change (gff-client.md 5.6). Empty: nothing is saved.
	StatePath string
	// StartupTabs are the tabs a new workspace starts with: one with no file yet.
	StartupTabs []StartupTab

	// Modules gives the status board its module cards (gff-client.md 7.6). Nil: no module
	// section; the hab does not serve module trees yet.
	Modules ModuleSource
}

// ErrDetached is Run's result when the owner detaches (session mode, then d).
var ErrDetached = errors.New("gff: detached; the programs keep running")

// hostState is the client's side of one host: its calls queue and whether it is online.
type hostState struct {
	label  string
	conn   *habconn.Conn
	ops    *opQueue
	online bool
	me     string // this client's id on the hab
	gone   bool   // removed from the host list
}

// view is one pane on screen.
type view struct {
	spec   PaneSpec
	h      *hostState
	stream *habconn.PaneStream // nil while the view is hidden or opening
	gen    int                 // raised on each open and close, so stale frames are dropped
	stop   context.CancelFunc  // ends the current stream's goroutine
	in     *inputSender
	quit   context.CancelFunc // ends the view's input sender
	term   vt.Terminal

	cols, rows int
	owner      string
	opens      int // the stream's data connections when the last snapshot came
	exited     *api.ExitedFrame
	ended      error // the stream ended for good
	note       string
	offset     uint64 // after the last applied byte, to resume (gff-client.md 11.3)
	haveOffset bool
	bellUntil  time.Time
	scroll     int // rows the view is up in its scrollback (mouse.go); 0 is the live screen

	filter passFilter
	screen vt.Screen
	dirty  bool

	sentCols, sentRows int // the last size asked for
}

func (v *view) live() bool {
	return v.exited == nil && v.ended == nil && v.term != nil
}

// tab is one client-side page of panes (gff-client.md 5.1). The hab knows nothing of tabs.
type tab struct {
	name     string
	root     *node   // the layout tree (layout.go)
	views    []*view // the views in tree order
	focus    int
	split    Split // how a view added at the end of the tab joins it
	zoom     bool
	activity bool      // a hidden pane printed
	bell     time.Time // a pane rang the bell; marked until then
}

type paneFrame struct {
	v      *view
	gen    int
	opened *habconn.PaneStream
	f      habconn.Frame
	ended  bool
	err    error
}

type client struct {
	cfg   Config
	ctx   context.Context
	wg    *sync.WaitGroup
	ws    bool // the workspace, not the single-hab view
	hosts map[string]*hostState
	first string // the first host's label
	tabs  []*tab
	cur   int
	last  *tab // the tab shown before the current one, for last-tab

	windowTitle string
	titleSaved  bool
	restored    bool   // the workspace file was read (or is new), so writing it loses nothing
	savedBytes  []byte // what the file holds, as last written or read
	saveErr     string // the last write's failure, shown once

	w, h   int
	canvas canvas
	pass   *view // the view passing bytes through, or nil
	// carry is the view whose modes the host may still have from passing through, or nil: from
	// entering pass-through until the modes are set back. A new snapshot, a hidden tab or a
	// closed view ends pass-through but not this.
	carry    *view
	composed bool
	modes    map[int]bool // modes mirrored on the host while composed
	out      bytes.Buffer
	keys     *keyReader
	// mouseHeld is the start of a mouse report or scroll key cut at a read boundary.
	mouseHeld []byte
	notes     chan paneNote
	frames    chan paneFrame
	later     chan func()
	debounce  Timer
	// What the host shows besides the canvas, so an unchanged screen writes nothing.
	cursorX, cursorY int
	cursorOn         bool

	ui workspaceUI
}

type paneNote struct{ host, pane, msg string }

// Run shows panes until the owner detaches (ErrDetached), ctx ends, or, in the single-hab
// view, every pane has ended (nil) or the connection fails for good. It leaves the host
// terminal as it found it, except for what the panes printed while passing through.
func Run(ctx context.Context, cfg Config) (err error) {
	if cfg.Parser == nil || cfg.In == nil || cfg.Out == nil || cfg.Size == nil {
		return errors.New("gffclient: Parser, In, Out and Size are required")
	}
	ws := cfg.Hosts != nil
	switch {
	case !ws && cfg.Conn == nil:
		return errors.New("gffclient: Conn or Hosts is required")
	case !ws && len(cfg.Panes) == 0:
		return errors.New("gffclient: the single-hab view needs Panes")
	}
	if !ws {
		cfg.Hosts = hosts.Wrap("hab", cfg.Conn, cfg.Now)
	}
	if cfg.Keymap == nil {
		cfg.Keymap = DefaultKeymap()
	}
	if cfg.Bar == nil {
		b := DefaultBar()
		cfg.Bar = &b
	}
	if cfg.Stats == nil {
		cfg.Stats = (&LocalStats{Battery: slices.Contains(cfg.Bar.Info, "battery")}).Read
	}
	if cfg.SizeDebounce <= 0 {
		cfg.SizeDebounce = DefaultSizeDebounce
	}
	if cfg.NewTimer == nil {
		cfg.NewTimer = newRealTimer
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	c := &client{cfg: cfg, ctx: ctx, wg: &wg, ws: ws, hosts: map[string]*hostState{}, modes: map[int]bool{},
		keys: newKeyReader(cfg.Keymap), notes: make(chan paneNote, 64), frames: make(chan paneFrame),
		later: make(chan func(), 64), debounce: cfg.NewTimer()}
	c.ui.init(c)
	c.tabs = []*tab{{name: "1", split: cfg.Split}}
	defer func() {
		c.saveWorkspace()
		cancel()
		c.debounce.Stop()
		for _, t := range c.tabs {
			for _, v := range t.views {
				c.closeStream(v)
			}
		}
		wg.Wait()
		for _, t := range c.tabs {
			for _, v := range t.views {
				if v.term != nil {
					v.term.Close()
				}
			}
		}
	}()
	for _, hm := range cfg.Hosts.Hosts() {
		c.host(hm)
	}
	for _, spec := range cfg.Panes {
		hs := c.hosts[spec.Host]
		if spec.Host == "" {
			hs = c.hosts[c.first]
		}
		if hs == nil {
			return fmt.Errorf("pane %s: no host %q", spec.ID, spec.Host)
		}
		c.addView(c.tabs[0], spec, hs)
	}
	if ws && len(cfg.Panes) == 0 && cfg.StatePath != "" {
		existed, writable, err := c.restoreWorkspace()
		if err != nil {
			c.ui.msg = err.Error()
			cfg.Logf("%v", err)
		}
		c.restored = writable
		if !existed && writable {
			c.startupTabs()
		}
		if len(c.tab().views) > 0 {
			c.ui.mode = modePanes
		}
		for _, v := range c.tab().views {
			c.openStream(v)
		}
	}

	// Views open their streams off the loop, so the loop reads keys while they open: the
	// session leader and d end a start that hangs with the host raw, and keys typed meanwhile go to the
	// focused pane (typeKeys sends before the first snapshot).
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
	if ws {
		c.ui.runPending() // startup tabs on hosts already online
	}
	defer func() {
		c.restoreHost()
		cfg.Out.Write(c.out.Bytes())
	}()

	tick := cfg.Tick
	if ws && tick == nil {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		tick = t.C
	}
	updates := cfg.Hosts.Updates()
	var modChanged <-chan struct{}
	if cfg.Modules != nil && ws {
		modChanged = cfg.Modules.Changed()
	}
	for {
		c.render()
		if c.out.Len() > 0 {
			if _, err := cfg.Out.Write(c.out.Bytes()); err != nil {
				return fmt.Errorf("write to the terminal: %w", err)
			}
			c.out.Reset()
		}
		if !ws {
			if done, err := c.allEnded(); done {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pf := <-c.frames:
			c.frame(pf)
			// Apply what else is ready before drawing once.
			for range 64 {
				select {
				case pf := <-c.frames:
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
		case <-tick:
			c.ui.tick()
		case f := <-c.later:
			f()
		case <-modChanged:
			c.ui.changed()
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if err := c.update(u); err != nil {
				return err
			}
		case n := <-c.notes:
			c.eachView(func(_ *tab, v *view) {
				if v.h.label == n.host && v.spec.ID == n.pane {
					v.note = n.msg
					v.dirty = true
				}
			})
		}
	}
}

// host returns the client's state for a host, making it on first sight.
func (c *client) host(hm hosts.Host) *hostState {
	if hs := c.hosts[hm.Label]; hs != nil {
		return hs
	}
	hs := &hostState{label: hm.Label, conn: hm.Conn, online: hm.Online(), me: hm.Welcome.ClientID}
	label := hm.Label
	hs.ops = newOpQueue(hm.Conn, func(pane, msg string) { c.postNote(label, pane, msg) })
	c.wg.Add(1)
	go func() { defer c.wg.Done(); hs.ops.run(c.ctx) }()
	c.hosts[hm.Label] = hs
	if c.first == "" {
		c.first = hm.Label
	}
	return hs
}

// freeTabName is a number for a tab at place pos (from 1): pos, else the next number no tab is
// named.
func (c *client) freeTabName(pos int) string {
	for n := max(pos, 1); ; n++ {
		name := strconv.Itoa(n)
		taken := false
		for _, t := range c.tabs {
			if t.name == name {
				taken = true
			}
		}
		if !taken {
			return name
		}
	}
}

// tab returns the current tab.
func (c *client) tab() *tab { return c.tabs[c.cur] }

func (c *client) eachView(f func(*tab, *view)) {
	for _, t := range c.tabs {
		for _, v := range t.views {
			f(t, v)
		}
	}
}

// addView adds a view of a hab pane to t and, when t is shown, opens its stream.
func (c *client) addView(t *tab, spec PaneSpec, hs *hostState) *view {
	return c.addViewAt(t, spec, hs, nil, placeEnd)
}

// addViewAt adds a view of a hab pane to t against anchor, as how says (layout.go), and, when
// t is shown, opens its stream.
func (c *client) addViewAt(t *tab, spec PaneSpec, hs *hostState, anchor *view, how place) *view {
	if spec.Label == "" {
		spec.Label = spec.ID
	}
	v := &view{spec: spec, h: hs}
	c.startInput(v)
	t.put(v, anchor, how)
	if t == c.tab() {
		c.openStream(v)
	}
	return v
}

// startInput starts the view's input sender for its pane.
func (c *client) startInput(v *view) {
	ictx, quit := context.WithCancel(c.ctx)
	v.quit = quit
	host, pane := v.h.label, v.spec.ID
	v.in = newInputSender(v.h.conn, pane, func(msg string) { c.postNote(host, pane, msg) })
	c.wg.Add(1)
	go func() { defer c.wg.Done(); v.in.run(ictx) }()
}

// repoint makes v show another pane of its session: the pane a resume started (gff-client.md
// 6.2). The view keeps its tab, place and focus; its parser, offset and input sender go with
// the old pane.
func (c *client) repoint(t *tab, v *view, pane string) {
	c.closeStream(v)
	v.quit()
	if v.term != nil {
		v.term.Close()
		v.term = nil
	}
	v.spec.ID = pane
	v.exited, v.ended, v.note, v.owner = nil, nil, "", ""
	v.offset, v.haveOffset, v.opens, v.scroll = 0, false, 0, 0
	v.cols, v.rows, v.sentCols, v.sentRows = 0, 0, 0, 0
	v.filter = passFilter{}
	v.dirty = true
	c.startInput(v)
	if t == c.tab() {
		c.openStream(v)
		c.layoutChanged()
	}
}

// removeView takes a view out of its tab; the program keeps running (gff-client.md 5.2).
func (c *client) removeView(t *tab, v *view) {
	t.take(v)
	c.closeStream(v)
	v.quit()
	if v.term != nil {
		v.term.Close()
		v.term = nil
	}
	if c.pass == v {
		c.pass = nil
	}
	if len(t.views) < 2 {
		t.zoom = false
	}
}

// openStream opens the view's data stream off the loop: it resumes from the view's offset
// when it has one (gff-client.md 11.3). A view of a host that is not online waits: its stream
// opens when the host comes online.
func (c *client) openStream(v *view) {
	if v.stream != nil || v.stop != nil || v.ended != nil || v.exited != nil || !v.h.online {
		return
	}
	v.gen++
	gen := v.gen
	ctx, stop := context.WithCancel(c.ctx)
	v.stop = stop
	v.opens = 0
	opts := habconn.PaneOptions{Observe: v.spec.Observe}
	if v.haveOffset {
		off := v.offset
		opts.Offset = &off
	}
	conn, pane := v.h.conn, v.spec.ID
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		send := func(pf paneFrame) bool {
			select {
			case c.frames <- pf:
				return true
			case <-ctx.Done():
				return false
			}
		}
		s, err := openPane(ctx, conn, pane, opts)
		if err != nil {
			send(paneFrame{v: v, gen: gen, ended: true, err: err})
			return
		}
		if !send(paneFrame{v: v, gen: gen, opened: s}) {
			s.Close()
			return
		}
		for f := range s.Frames() {
			if !send(paneFrame{v: v, gen: gen, f: f}) {
				return
			}
		}
		send(paneFrame{v: v, gen: gen, ended: true, err: s.Err()})
	}()
}

// openPane opens a pane's stream, and gives up when ctx ends even if the open has not
// returned: a hab that hangs must not hold Run's return (the session leader and d while opening). A
// stream that opens after that is closed.
func openPane(ctx context.Context, conn *habconn.Conn, pane string, opts habconn.PaneOptions) (*habconn.PaneStream, error) {
	type result struct {
		s   *habconn.PaneStream
		err error
	}
	done := make(chan result, 1)
	go func() { // not waited for: it ends when the open returns
		s, err := conn.OpenPane(ctx, pane, opts)
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		return r.s, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.s != nil {
				r.s.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// closeStream closes the view's stream and keeps its parser and offset, for a hidden tab.
func (c *client) closeStream(v *view) {
	if v.stop != nil {
		v.stop()
		v.stop = nil
	}
	if v.stream != nil {
		v.stream.Close()
		v.stream = nil
	}
	v.gen++
	if c.pass == v {
		c.pass = nil
	}
}

func (c *client) postNote(host, pane, msg string) {
	select {
	case c.notes <- paneNote{host, pane, msg}:
	default:
	}
}

// run hands f to the loop: results of calls made off the loop.
func (c *client) run(f func()) {
	select {
	case c.later <- f:
	case <-c.ctx.Done():
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
	vs := c.tab().views
	for _, v := range vs {
		if v.exited == nil && v.ended == nil {
			return false, nil
		}
	}
	for _, v := range vs {
		if v.ended != nil && !errors.Is(v.ended, io.EOF) && v.exited == nil {
			return true, fmt.Errorf("pane %s: %w", v.spec.ID, v.ended)
		}
	}
	return true, nil
}

// frame applies one message of a view's stream goroutine.
func (c *client) frame(pf paneFrame) {
	v := pf.v
	if pf.gen != v.gen {
		if pf.opened != nil {
			pf.opened.Close()
		}
		return // a stream closed since
	}
	if pf.opened != nil {
		v.stream = pf.opened
		return
	}
	if pf.ended {
		v.ended = pf.err
		if v.ended == nil {
			v.ended = io.EOF
		}
		v.stop = nil
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
			v.term = nil
			v.ended = err
			return
		}
		v.term, v.cols, v.rows = t, cfg.Cols, cfg.Rows
		v.scroll = 0 // a new screen: the live one
		v.offset, v.haveOffset = f.Offset, true
		v.filter.reset()
		if f.Encoding == string(vt.EncodingVT) {
			var discard bytes.Buffer
			v.filter.write(&discard, f.Data)
		}
		if c.pass == v {
			c.pass = nil // pass through again from the new screen
		}
	case api.FrameBytes:
		if v.term == nil {
			return
		}
		v.term.Write(f.Data)
		if v.scroll > 0 {
			v.scroll = v.term.ScrollView(0)
		}
		v.offset, v.haveOffset = f.Offset+uint64(len(f.Data)), true
		start := c.out.Len()
		v.filter.write(&c.out, f.Data)
		if c.pass != v {
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

// typeKeys sends bytes to the focused pane. Before the pane's first snapshot the keys still
// go, so typeahead as gff starts is kept, as ssh keeps it.
func (c *client) typeKeys(p []byte) {
	v := c.focused()
	switch {
	case v == nil:
	case v.spec.Observe:
		v.note, v.dirty = "observer: input off", true
	case c.ws && c.ui.resumable(v):
		if isEnter(p) {
			c.ui.resumeView(v)
		}
	case v.exited != nil || v.ended != nil:
	case !v.h.online:
		v.note, v.dirty = "offline: input dropped", true
	default:
		v.in.send(p)
	}
}

func (c *client) focused() *view {
	t := c.tab()
	if len(t.views) == 0 {
		return nil
	}
	return t.views[min(t.focus, len(t.views)-1)]
}

// keyBytes is what a send binding types for its key: a Ctrl key's byte, or an Alt key in its
// legacy form, ESC and the character. Other keys type nothing.
func keyBytes(k string) []byte {
	if b, ok := ctrlByte(k); ok {
		return []byte{b}
	}
	if a, ok := strings.CutPrefix(k, "alt-"); ok {
		return []byte("\x1b" + a)
	}
	return nil
}

// panesFocused reports whether the panes have the keys: always outside the workspace, and in
// it unless the sidebar, the board or a prompt has them. Focus and tab moves act only then,
// so they never change what is out of sight.
func (c *client) panesFocused() bool { return !c.ws || c.ui.mode == modePanes }

// moveFocus moves focus to the view next to the focused one in the direction, as zellij's Alt
// keys: across nested splits and stacks, by where the views are on screen. Up or down past the
// end of a stack wraps to its other end. Left or right with no view that way goes to the
// previous or next tab in the workspace; up and down stop at the edge.
func (c *client) moveFocus(act string) {
	t := c.tab()
	horizontal := act == actFocusLeft || act == actFocusRight
	if len(t.views) > 0 {
		r := c.paneArea()
		i := t.neighbour(act, r.w, r.h)
		if i < 0 && !horizontal {
			if st := t.stackOf(t.focusedView()); st != nil {
				// Past the end of a stack: its other end.
				end := st.kids[0]
				if act == actFocusUp {
					end = st.kids[len(st.kids)-1]
				}
				cur := t.leafOf(t.focusedView())
				if cur != end {
					for k, v := range t.views {
						if v == end.v {
							i = k
						}
					}
				}
			}
		}
		if i >= 0 {
			t.focus = i
			c.layoutChanged()
			c.ui.focusChanged()
			return
		}
	}
	if horizontal && c.ws {
		if act == actFocusLeft {
			c.ui.command(actPrevTab)
		} else {
			c.ui.command(actNextTab)
		}
	}
}

// command runs one action of the keymap.
func (c *client) command(ev keyEvent) error {
	switch ev.act {
	case actDetach:
		return ErrDetached
	case actSend:
		if b := keyBytes(ev.key); b != nil {
			c.typeKeys(b)
		}
		return nil
	case actFocusLeft, actFocusRight, actFocusUp, actFocusDown:
		if !c.panesFocused() {
			return nil
		}
		c.moveFocus(ev.act)
		return nil
	case actMoveTabLeft, actMoveTabRight:
		if !c.panesFocused() {
			return nil
		}
	}
	if c.ws && c.ui.command(ev.act) {
		return nil
	}
	t := c.tab()
	v := c.focused()
	if v == nil {
		return nil
	}
	switch ev.act {
	case actFocusNext:
		t.focus = (t.focus + 1) % len(t.views)
		c.layoutChanged() // a stack may expand another member
		c.ui.focusChanged()
	case actZoom:
		if len(t.views) > 1 {
			t.zoom = !t.zoom
			c.layoutChanged()
		}
	case actSplit:
		t.flip(v)
		c.layoutChanged()
	case actRedraw, actNudge:
		mode := api.RedrawModeSnapshot
		if ev.act == actNudge {
			if v.spec.Observe {
				v.note, v.dirty = "observer: no nudge", true
				return nil
			}
			mode = api.RedrawModeNudge
		}
		v.h.ops.push(op{kind: opRedraw, pane: v.spec.ID, mode: mode})
		c.canvas.valid = false
		c.pass = nil
	case actFit:
		if v.spec.Observe {
			v.note, v.dirty = "observer: cannot fit", true
			return nil
		}
		v.h.ops.push(op{kind: opTake, pane: v.spec.ID})
	}
	return nil
}

// layoutChanged repaints at once and asks for the new sizes after the debounce.
func (c *client) layoutChanged() {
	c.canvas.valid = false
	for _, v := range c.tab().views {
		v.dirty = true
	}
	c.debounce.Reset(c.cfg.SizeDebounce)
}

// paneArea is where the current tab's panes go: the whole host, less the workspace's bars.
func (c *client) paneArea() rect {
	if !c.ws {
		return rect{0, 0, c.w, c.h}
	}
	return c.ui.paneArea()
}

// arrangement lays out the current tab in the pane area.
func (c *client) arrangement() arrangement {
	t := c.tab()
	if len(t.views) == 0 {
		return arrangement{}
	}
	r := c.paneArea()
	a := t.arrange(r.w, r.h)
	for i := range a.slots {
		s := &a.slots[i]
		s.frame.x += r.x
		s.frame.y += r.y
		s.content.x += r.x
		s.content.y += r.y
	}
	for i := range a.seps {
		a.seps[i].x += r.x
		a.seps[i].y += r.y
	}
	return a
}

// sendSizes asks for each shown pane's size where it changed. Hidden and observer views ask
// for nothing (hab-logic-api.md 6.4).
func (c *client) sendSizes() {
	a := c.arrangement()
	vs := c.tab().views
	for _, s := range a.slots {
		v := vs[s.view]
		if s.folded || v.spec.Observe || (v.sentCols == s.wantCols && v.sentRows == s.wantRows) {
			continue
		}
		v.sentCols, v.sentRows = s.wantCols, s.wantRows
		v.h.ops.push(op{kind: opSize, pane: v.spec.ID, cols: s.wantCols, rows: s.wantRows})
	}
}

// update handles one host update.
func (c *client) update(u hosts.Update) error {
	hm, known := c.cfg.Hosts.Host(u.Host)
	hs := c.hosts[u.Host]
	if hs == nil {
		if !known {
			return nil
		}
		hs = c.host(hm)
	}
	views := func(f func(*view)) {
		c.eachView(func(_ *tab, v *view) {
			if v.h == hs {
				f(v)
			}
		})
	}
	if u.Removed {
		hs.gone, hs.online = true, false
		views(func(v *view) {
			c.closeStream(v)
			v.ended, v.dirty = errors.New("host removed"), true
		})
		delete(c.hosts, u.Host)
		c.ui.changed()
		return nil
	}
	switch u.Kind {
	case habconn.UpdateOnline:
		hs.online = true
		hs.me = u.Welcome.ClientID
		if c.ws {
			c.ui.listUsage(hs)
			c.ui.listCatalog(hs)
		}
		if u.NewEpoch {
			// A new hab process keeps no desired sizes and no owners: ask again.
			views(func(v *view) { v.sentCols, v.sentRows = 0, 0 })
			c.sendSizes()
		}
		// Views of the current tab that waited for the host open now (a restored workspace).
		for _, v := range c.tab().views {
			if v.h == hs && v.stream == nil && v.stop == nil && v.exited == nil && v.ended == nil {
				c.openStream(v)
			}
		}
		if c.ws {
			c.ui.runPending()
		}
		views(func(v *view) { v.dirty = true })
	case habconn.UpdateOffline, habconn.UpdateRetrying:
		hs.online = false
		views(func(v *view) { v.dirty = true })
	case habconn.UpdateFailed:
		hs.online = false
		if !c.ws {
			return fmt.Errorf("hab connection: %w", u.Err)
		}
		views(func(v *view) { v.dirty = true })
	case habconn.UpdateEvent:
		c.ui.event(hs, u.Event)
	}
	c.ui.changed()
	return nil
}

// Host terminal sequences.
const (
	// Modes a pane may have set on the host while passing through, set back to the defaults:
	// a synchronized update first, so the host draws again, then the rest, with
	// modifyOtherKeys and the G0 charset (ESC ( B, then SI to use G0).
	resetPaneModes = "\x1b[?2026l\x1b[0m\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" +
		"\x1b[?1004l\x1b[?2004l\x1b[?1l\x1b>\x1b[=0;1u\x1b[4l\x1b[?6l\x1b[r\x1b[?7h\x1b[ q\x1b[?25h" +
		"\x1b[>4m\x1b(B\x0f"
	// The composed view asks for mouse reports (button events, SGR form) for the wheel and
	// clicks (gff-client.md 5.7), and turns them off as it leaves.
	enterCompose = "\x1b[?1049h\x1b[?7l\x1b[?25l\x1b[?1000h\x1b[?1006h"
	leaveCompose = "\x1b[0m\x1b[?1006l\x1b[?1000l\x1b[?7h\x1b[?25h\x1b[?1049l"
)

// HostReset sets the host terminal back from anything gff or a pane can leave on it: the pane
// modes, the composed view's modes, and the alternate screen. gff writes it when it ended
// without setting the terminal back itself, such as after a panic.
const HostReset = resetPaneModes + "\x1b[?1049l"

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
	defer c.updateTitle()
	if c.pass != nil && !c.ws {
		if done, _ := c.allEnded(); done {
			return // the last pane ended: the host keeps what it drew, as after ssh
		}
	}
	a := c.arrangement()
	if len(a.slots) == 1 && (!c.ws || c.ui.passOK()) && c.modeNote() == "" {
		v := c.tab().views[a.slots[0].view]
		r := c.paneArea()
		if v.term == nil && v.ended == nil {
			return // wait for the first snapshot: it may pass straight through
		}
		if v.live() && v.h.online && v.scroll == 0 && r.w == c.w && r.h == c.h && v.cols == c.w && v.rows == c.h {
			if c.pass != v {
				c.enterPass(v)
			}
			return
		}
	}
	if c.pass != nil || !c.composed {
		if c.carry != nil {
			c.out.WriteString(resetPaneModes)
			c.carry = nil
		}
		// The alternate screen comes clear; the main screen keeps what the pane drew.
		c.out.WriteString(enterCompose)
		c.pass, c.composed = nil, true
		c.canvas.valid = false
		c.cursorOn = false
		clear(c.modes)
	}
	c.compose(a)
}

// enterPass makes view v pass its bytes through: the host leaves the composed screen and
// gets the pane's screen and modes from the local parser, then the pane's bytes as they come.
func (c *client) enterPass(v *view) {
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
	c.pass, c.carry = v, v
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
	if c.titleSaved {
		c.out.WriteString("\x1b[23;2t")
		c.titleSaved = false
	}
	switch {
	case c.composed:
		c.out.WriteString(resetMirrored(c.modes))
		c.out.WriteString(leaveCompose)
	case c.carry != nil:
		v := c.carry
		leave := ""
		if v.term != nil {
			// Each way onto the alternate screen has its own way off.
			for _, m := range []int{1049, 1047, 47} {
				if on, _ := v.term.Mode(m, false); on {
					leave = "\x1b[?" + strconv.Itoa(m) + "l"
					break
				}
			}
		}
		c.out.WriteString(resetPaneModes)
		if leave != "" {
			c.out.WriteString(leave)
		} else {
			c.out.WriteString("\x1b[" + strconv.Itoa(c.h) + ";1H\r\n")
		}
	}
	c.composed, c.pass, c.carry = false, nil, nil
}

// compose draws the workspace's bars, every shown pane, its frame row and the separators, and
// writes the changes.
func (c *client) compose(a arrangement) {
	c.canvas.clear()
	var cursorX, cursorY int
	cursorOn := false
	board := c.ws && c.ui.mode == modeBoard
	if c.ws {
		cx, cy, on := c.ui.draw()
		cursorX, cursorY, cursorOn = cx, cy, on
	}
	t := c.tab()
	if !board {
		sepStyle := vt.Style{Faint: true}
		for _, r := range a.seps {
			for y := r.y; y < r.y+r.h; y++ {
				c.canvas.set(r.x, y, hcell{text: "│", style: sepStyle})
			}
		}
		paneFocus := !c.ws || c.ui.mode == modePanes
		for _, s := range a.slots {
			v := t.views[s.view]
			focused := s.view == t.focus
			if v.term != nil && !s.folded {
				if v.dirty || v.screen.Cells == nil {
					if err := v.term.ReadScreen(&v.screen); err != nil {
						c.cfg.Logf("pane %s: read screen: %v", v.spec.ID, err)
					}
					v.dirty = false
				}
				c.canvas.pane(&v.screen, s.content, !v.h.online || !v.live())
				if focused && paneFocus && v.live() && v.screen.CursorVisible {
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
	}
	start := c.out.Len()
	c.out.WriteString("\x1b[?25l")
	mark := c.out.Len()
	c.canvas.flush(&c.out)
	changed := c.out.Len() > mark
	// The focused pane's input modes, mirrored on the host while keys go to it.
	fv := c.focused()
	for _, m := range mirrored {
		want := false
		if fv != nil && fv.term != nil && fv.live() && (!c.ws || c.ui.mode == modePanes) {
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

// frameText is a pane's frame row: its label, in the workspace its host and agent state, and
// a short note (gff-client.md 3.1).
func (c *client) frameText(v *view, s slot) string {
	t := " " + v.spec.Label
	var notes []string
	if v.filter.title != "" {
		notes = append(notes, v.filter.title)
	}
	if c.ws {
		// A view of a session shows the session's name as the hab has it now.
		if r, ok := rowOf(c.ui.rows, v); ok && v.spec.Session != "" && r.Name != "" {
			t = " " + r.Name
		}
		notes = append(notes, v.h.label)
		if st := c.ui.agentWord(v); st != "" {
			notes = append(notes, st)
		}
		if c.cfg.Now().Before(v.bellUntil) {
			notes = append(notes, "bell")
		}
	}
	switch {
	case c.ws && c.ui.resumable(v):
		notes = append(notes, c.ui.exitedWord(v), "Enter resume")
	case v.exited != nil:
		notes = append(notes, "exited"+exitText(v.exited))
	case v.ended != nil:
		notes = append(notes, "gone")
	case !v.h.online:
		notes = append(notes, "offline")
	}
	if v.spec.Observe {
		notes = append(notes, "observer")
	}
	if v.scroll > 0 {
		notes = append(notes, "scrolled "+strconv.Itoa(v.scroll))
	}
	if v.term != nil && (v.cols != s.content.w || v.rows != s.content.h) {
		if v.owner != "" && v.owner != v.h.me {
			notes = append(notes, "owner "+short(v.owner)+" "+strconv.Itoa(v.cols)+"x"+strconv.Itoa(v.rows))
		} else {
			notes = append(notes, strconv.Itoa(v.cols)+"x"+strconv.Itoa(v.rows))
		}
	}
	if v.note != "" {
		notes = append(notes, v.note)
	}
	if m := c.modeNote(); m != "" && v == c.focused() {
		notes = append(notes, m)
	}
	for _, n := range notes {
		t += " · " + n
	}
	return t + " "
}

// modeNote names the key reader's mode and its way back for the focused frame row outside the
// workspace, where no bar shows them: "pane · Esc/Enter back", "locked · Ctrl+G unlock". It is
// "" in normal and session mode, and in the workspace. While it is not "", a pane alone is
// composed under its frame row rather than passed through, so the mode shows.
func (c *client) modeNote() string {
	m := c.keys.mode
	if c.ws || m == ModeNormal || m == ModeSession {
		return ""
	}
	parts := []string{string(m)}
	for _, h := range c.keys.km.hints(m) {
		if h.label == actionLabels[actBack] || h.label == actionLabels[actUnlock] {
			parts = append(parts, h.full())
		}
	}
	return strings.Join(parts, " · ")
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

// rowOf finds the board row of a view: its session's, when the view knows its session, else
// its pane's, if the hab knows it.
func rowOf(rows []board.Row, v *view) (board.Row, bool) {
	for _, r := range rows {
		if r.Host != v.h.label {
			continue
		}
		if v.spec.Session != "" && r.Session == v.spec.Session || v.spec.Session == "" && r.Pane == v.spec.ID {
			return r, true
		}
	}
	return board.Row{}, false
}

// isEnter reports whether p is one Enter key: CR, LF, or kitty's form.
func isEnter(p []byte) bool {
	switch string(p) {
	case "\r", "\n", "\x1b[13u", "\x1b[13;1u":
		return true
	}
	return false
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
