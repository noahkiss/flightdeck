package gffclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// shape writes a tab's tree: views by id, splits as cols(...) and rows(...), stacks as
// stack(...) with * on the expanded member.
func shape(t *tab) string {
	var w func(n *node) string
	w = func(n *node) string {
		switch n.kind {
		case leafNode:
			return n.v.spec.ID
		case stackNode:
			var parts []string
			for i, k := range n.kids {
				s := w(k)
				if i == n.open {
					s = "*" + s
				}
				parts = append(parts, s)
			}
			return "stack(" + strings.Join(parts, ",") + ")"
		}
		name := "cols"
		if n.dir == SplitRows {
			name = "rows"
		}
		var parts []string
		for _, k := range n.kids {
			parts = append(parts, w(k))
		}
		return name + "(" + strings.Join(parts, ",") + ")"
	}
	if t.root == nil {
		return ""
	}
	return w(t.root)
}

func views(ids ...string) map[string]*view {
	m := map[string]*view{}
	for _, id := range ids {
		m[id] = &view{spec: PaneSpec{ID: id}}
	}
	return m
}

func order(t *tab) string {
	var ids []string
	for _, v := range t.views {
		ids = append(ids, v.spec.ID)
	}
	return strings.Join(ids, " ")
}

// TestLayoutTree: a new view stacks onto the focused one or splits on any side; a split beside
// a stacked view splits the whole stack; closing a view folds away a split or stack left with
// one child and joins a split into its parent of the same direction; the view order follows
// the tree.
func TestLayoutTree(t *testing.T) {
	v := views("a", "b", "c", "d", "e", "f")
	tb := &tab{split: SplitColumns}
	tb.put(v["a"], nil, placeEnd)
	steps := []struct {
		add, at string
		how     place
		shape   string
	}{
		{"b", "a", placeStack, "stack(a,*b)"},
		{"c", "b", placeRight, "cols(stack(a,*b),c)"},
		{"d", "c", placeDown, "cols(stack(a,*b),rows(c,d))"},
		{"e", "a", placeLeft, "cols(e,stack(a,*b),rows(c,d))"},
		{"f", "d", placeUp, "cols(e,stack(a,*b),rows(c,f,d))"},
	}
	for _, s := range steps {
		tb.put(v[s.add], v[s.at], s.how)
		if got := shape(tb); got != s.shape {
			t.Fatalf("put %s %v %s: %s, want %s", s.add, s.how, s.at, got, s.shape)
		}
	}
	if o := order(tb); o != "e a b c f d" || tb.focusedView() != v["a"] {
		t.Fatalf("order %q, focus %v: the focus stays on a", o, tb.focusedView().spec.ID)
	}
	for _, s := range []struct{ take, shape string }{
		{"e", "cols(stack(a,*b),rows(c,f,d))"},
		{"f", "cols(stack(a,*b),rows(c,d))"},
		{"d", "cols(stack(a,*b),c)"},
		{"b", "cols(a,c)"},
	} {
		tb.take(v[s.take])
		if got := shape(tb); got != s.shape {
			t.Fatalf("take %s: %s, want %s", s.take, got, s.shape)
		}
	}
	// A split left inside a split of its own direction joins it.
	x := views("x", "y")
	tb.put(v["d"], v["c"], placeDown)
	tb.put(x["x"], v["d"], placeRight)
	tb.put(x["y"], x["x"], placeRight)
	if got := shape(tb); got != "cols(a,rows(c,cols(d,x,y)))" {
		t.Fatalf("before the join: %s", got)
	}
	tb.take(v["c"])
	if got := shape(tb); got != "cols(a,d,x,y)" {
		t.Fatalf("after the join: %s", got)
	}
	for _, id := range []string{"a", "d", "x"} {
		tb.take(map[string]*view{"a": v["a"], "d": v["d"], "x": x["x"]}[id])
	}
	if got := shape(tb); got != "y" || order(tb) != "y" {
		t.Fatalf("last view: %s", got)
	}
	tb.take(x["y"])
	if tb.root != nil || len(tb.views) != 0 {
		t.Fatalf("empty tab: %s %d", shape(tb), len(tb.views))
	}
}

// TestStackArrange: a stack shows its expanded member under the folded rows of those above it
// and over those below it; the expanded member follows focus; a folded member asks for no
// size; a new pane's size is its slot's.
func TestStackArrange(t *testing.T) {
	v := views("a", "b", "c")
	tb := &tab{split: SplitColumns}
	tb.put(v["a"], nil, placeEnd)
	if c, r := tb.sizeIfPut(v["a"], placeStack, 80, 24); c != 80 || r != 22 {
		t.Fatalf("stacked new pane size %dx%d, want 80x22", c, r)
	}
	if c, r := tb.sizeIfPut(v["a"], placeRight, 81, 24); c != 40 || r != 23 {
		t.Fatalf("new pane on the right %dx%d, want 40x23", c, r)
	}
	tb.put(v["b"], v["a"], placeStack)
	tb.put(v["c"], v["b"], placeStack)
	tb.focus = 1 // b
	a := tb.arrange(80, 24)
	want := []slot{
		{view: 0, frame: rect{0, 0, 80, 1}, content: rect{0, 1, 80, 0}, folded: true},
		{view: 1, frame: rect{0, 1, 80, 1}, content: rect{0, 2, 80, 21}, wantCols: 80, wantRows: 21},
		{view: 2, frame: rect{0, 23, 80, 1}, content: rect{0, 24, 80, 0}, folded: true},
	}
	if len(a.slots) != 3 {
		t.Fatalf("slots %+v", a.slots)
	}
	for i, s := range a.slots {
		if s != want[i] {
			t.Errorf("slot %d: %+v, want %+v", i, s, want[i])
		}
	}
	if shape(tb) != "stack(a,*b,c)" {
		t.Fatalf("shape %s", shape(tb))
	}
	tb.focus = 2
	tb.arrange(80, 24)
	if shape(tb) != "stack(a,b,*c)" {
		t.Fatalf("expanded member after focus moved: %s", shape(tb))
	}
}

// TestFocusAcrossNestedLayout: Alt+h/j/k/l move focus by where the views are on screen,
// across nested splits and stacks; up and down wrap at the ends of a stack; left and right
// with nothing that way go to the other tab.
func TestFocusAcrossNestedLayout(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := barClient(t, 160, 30, &now)
	c.debounce = &fakeTimer{c: make(chan time.Time, 1)}
	c.ui.mode = modePanes
	hs := c.hosts[c.first]
	v := map[string]*view{}
	for _, id := range []string{"a", "b", "s", "c", "d"} {
		// Exited views open no stream.
		v[id] = &view{spec: PaneSpec{ID: id}, h: hs, exited: &api.ExitedFrame{}}
	}
	tb := &tab{name: "1", split: SplitColumns}
	tb.put(v["a"], nil, placeEnd)
	tb.put(v["b"], v["a"], placeStack)
	tb.put(v["s"], v["b"], placeStack)
	tb.put(v["c"], v["s"], placeRight)
	tb.put(v["d"], v["c"], placeDown)
	if shape(tb) != "cols(stack(a,b,*s),rows(c,d))" {
		t.Fatalf("shape %s", shape(tb))
	}
	other := &tab{name: "2"}
	c.tabs, c.cur = []*tab{tb, other}, 0
	focus := func(id string) {
		for i, x := range tb.views {
			if x == v[id] {
				tb.focus = i
			}
		}
	}
	focused := func() string {
		if c.cur != 0 {
			return "tab 2"
		}
		return tb.focusedView().spec.ID
	}
	for _, step := range []struct{ from, keys, want string }{
		{"b", "\x1bk", "a"},     // up the stack
		{"a", "\x1bk", "s"},     // past the top: the bottom
		{"s", "\x1bj", "a"},     // past the bottom: the top
		{"b", "\x1bl", "c"},     // right: the view that overlaps it most, the first on a tie
		{"d", "\x1bk", "c"},     // up within the rows
		{"c", "\x1bh", "b"},     // left into the stack: its expanded member
		{"d", "\x1bj", "d"},     // down at the bottom: nothing
		{"d", "\x1bl", "tab 2"}, // right at the edge: the next tab
	} {
		c.cur = 0
		focus(step.from)
		tb.arrange(160, 28) // the stack expands the focused member, as on screen
		if err := c.input([]byte(step.keys)); err != nil {
			t.Fatal(err)
		}
		if got := focused(); got != step.want {
			t.Errorf("from %s, %q: %s, want %s", step.from, step.keys, got, step.want)
		}
	}
}

// pos returns the row and column (in runes) where s starts on the screen, or -1, -1.
func pos(screen, s string) (int, int) {
	for i, l := range strings.Split(screen, "\n") {
		if j := strings.Index(l, s); j >= 0 {
			return i, len([]rune(l[:j]))
		}
	}
	return -1, -1
}

// TestNewPaneStacksAndSplits: pane mode n stacks a new pane onto the focused one, one expanded
// and the other folded to its frame row; l, j, h and k put it right, below, left and above.
// Each new pane starts on the focused pane's host with cwd_of naming the focused pane, and
// takes the focus.
func TestNewPaneStacksAndSplits(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	_, jobPane, _ := a.AddSession("job")
	hst := newHost(t, 200, 50, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the running row", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })

	paneOf := func(name string) string {
		for _, ss := range a.Sessions() {
			if ss.Name == name {
				return ss.Pane
			}
		}
		return ""
	}
	type step struct {
		keys, anchor, name string
		where              func(ar, ac, nr, nc int) bool
	}
	for i, st := range []step{
		{"n", "job", "sh", func(ar, ac, nr, nc int) bool { return nr == ar+1 && nc == ac }}, // stacked under it
		{"l", "sh", "sh-2", func(ar, ac, nr, nc int) bool { return nr <= ar && nc > ac }},   // beside the stack
		{"j", "sh-2", "sh-3", func(ar, ac, nr, nc int) bool { return nr > ar && nc == ac }},
		{"h", "sh-3", "sh-4", func(ar, ac, nr, nc int) bool { return nr == ar && nc < ac }},
		{"k", "sh-4", "sh-5", func(ar, ac, nr, nc int) bool { return nr < ar && nc == ac }},
	} {
		anchorPane := paneOf(st.anchor)
		hst.typ("\x10" + st.keys)
		var ar, ac, nr, nc int
		hst.waitScreen(st.name+" placed against "+st.anchor, func(s string) bool {
			ar, ac = pos(s, " "+st.anchor+" · one")
			nr, nc = pos(s, " "+st.name+" · one")
			return ar >= 0 && nr >= 0 && st.where(ar, ac, nr, nc)
		})
		cr := a.Creates()
		if len(cr) != i+1 || cr[i].CwdOf != anchorPane || len(cr[i].Argv) != 0 {
			t.Fatalf("create %d: %+v, want cwd_of %s and the hab's shell", i, cr, anchorPane)
		}
	}
	// The stack: job folded to its frame row right above sh, which is expanded but not focused.
	s := hst.screen()
	jr, _ := pos(s, " job · one")
	sr, _ := pos(s, " sh · one")
	if sr != jr+1 {
		t.Fatalf("job at row %d, sh at %d: want a stack of two\n%s", jr, sr, s)
	}
	_ = jobPane
}

// TestNewPaneInFolder: with the real holder and gff-hab, a new pane from pane mode starts in
// the folder the focused pane's program is in.
func TestNewPaneInFolder(t *testing.T) {
	d := shortDir(t)
	hdir, state, sock, work := filepath.Join(d, "h"), filepath.Join(d, "s"), filepath.Join(d, "run", "hab.sock"), filepath.Join(d, "w")
	dir := filepath.Join(work, "proj-x")
	for _, p := range []string{hdir, dir} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	startHolderProc(t, hdir, "one")
	startServeProc(t, sock, state, hdir, append(os.Environ(), "SHELL=/bin/sh", "ENV=", "PS1=$ "))
	h := &hab{t: t, sock: sock}
	other := h.other()
	spawn(t, other, `cd "`+dir+`" && echo in-proj && exec sleep 600`, 80, 24)
	hst := newHost(t, 160, 40, false)
	startWorkspace(t, hst, sock)
	hst.waitScreen("the session", func(s string) bool { return line(s, " sh ") > 0 })
	hst.typ("\r")
	hst.waitScreen("it in a view", func(s string) bool { return strings.Contains(s, "in-proj") })
	hst.typ("\x10n")
	hst.waitScreen("the new pane", func(s string) bool { return strings.Contains(s, " sh-2 · one") })
	time.Sleep(200 * time.Millisecond)
	hst.typ("pwd -P\r")
	want, _ := filepath.EvalSymlinks(dir)
	hst.waitScreen("pwd in the first pane's folder", func(s string) bool { return strings.Contains(s, want) })
}

// TestNewPaneKeepsSidebar: switching to the sidebar during a pane start keeps its keys.
func TestNewPaneKeepsSidebar(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.AddSession("job")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("job", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job view", func(s string) bool { return strings.Contains(s, " job · one") })
	release := a.HoldCreates()
	defer release()
	hst.typ("\x10n")
	hst.waitScreen("start in flight", func(s string) bool { return strings.Contains(s, "starting on one") })
	hst.typ("\x0fw")
	hst.waitScreen("sidebar", func(s string) bool { return strings.Contains(s, "Enter open") })
	release()
	waitUntil(t, "new session", func() bool { return len(a.Sessions()) == 2 })
	hst.waitScreen("new view", func(s string) bool { return strings.Contains(s, " sh · one") })
	hst.typ("r")
	hst.waitScreen("rename prompt", func(s string) bool { return strings.Contains(s, "rename ") })
}
