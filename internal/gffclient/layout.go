package gffclient

// The layout of a tab (gff-client.md 5.2, 5.4): a tree of nested splits and stacks over the
// tab's views. A split shares its area among its children side by side (columns) or top to
// bottom (rows). A stack shows one member expanded and folds the others to their frame row; the
// expanded member follows focus. A stack holds views only: a split beside a stacked view splits
// the whole stack.

// Split says how a split's children share its area.
type Split int

const (
	SplitColumns Split = iota // side by side, a separator column between them
	SplitRows                 // top to bottom
)

// rect is a cell rectangle on the host screen.
type rect struct{ x, y, w, h int }

// slot is where one visible view goes.
type slot struct {
	view    int
	frame   rect // the frame row; h is 0 when the view has no frame
	content rect // where the pane's cells go; h is 0 for a folded stack member
	folded  bool // a folded stack member: only its frame row shows
	// want is the size gff asks for this pane (pane.set_desired_size). A view alone fills the
	// host and has no frame when it passes bytes through, so it asks for the whole host. A
	// folded member asks for nothing (0).
	wantCols, wantRows int
}

// box is the slot's whole area: its frame row and its content.
func (s slot) box() rect {
	return rect{s.frame.x, s.frame.y, s.frame.w, s.frame.h + s.content.h}
}

// arrangement is the layout of the host screen.
type arrangement struct {
	slots []slot
	seps  []rect // separator columns
}

type nodeKind int

const (
	leafNode nodeKind = iota
	splitNode
	stackNode
)

// node is one place in a tab's layout tree.
type node struct {
	kind nodeKind
	v    *view   // leafNode
	dir  Split   // splitNode
	kids []*node // splitNode, stackNode
	up   *node
	open int // stackNode: the expanded member
}

// place says where a new view goes against the focused one (gff-client.md 5.4).
type place int

const (
	placeEnd   place = iota // at the end of the tab, along its split (the sidebar's Open)
	placeStack              // stacked onto the focused view
	placeRight
	placeLeft
	placeDown
	placeUp
)

// sync builds the tree of a tab that has views and no tree yet (tests set views by hand): one
// split along the tab's split.
func (t *tab) sync() {
	if t.root != nil || len(t.views) == 0 {
		return
	}
	if len(t.views) == 1 {
		t.root = &node{kind: leafNode, v: t.views[0]}
		return
	}
	t.root = &node{kind: splitNode, dir: t.split}
	for _, v := range t.views {
		insertKid(t.root, len(t.root.kids), &node{kind: leafNode, v: v})
	}
}

// leafOf finds the leaf that holds v, or nil.
func (t *tab) leafOf(v *view) *node {
	t.sync()
	var found *node
	var walk func(n *node)
	walk = func(n *node) {
		if n == nil || found != nil {
			return
		}
		if n.kind == leafNode && n.v == v {
			found = n
			return
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(t.root)
	return found
}

// relist lists the views in tree order and keeps focus on keep when it is still there, else
// at the same index, clamped.
func (t *tab) relist(keep *view) {
	t.views = t.views[:0:0]
	var walk func(n *node)
	walk = func(n *node) {
		if n == nil {
			return
		}
		if n.kind == leafNode {
			t.views = append(t.views, n.v)
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(t.root)
	for i, v := range t.views {
		if v == keep {
			t.focus = i
			return
		}
	}
	t.focus = min(t.focus, max(len(t.views)-1, 0))
}

// focusedView is the tab's focused view, or nil.
func (t *tab) focusedView() *view {
	if len(t.views) == 0 {
		return nil
	}
	return t.views[min(t.focus, len(t.views)-1)]
}

// replace puts y where x is in the tree.
func (t *tab) replace(x, y *node) {
	p := x.up
	y.up = p
	if p == nil {
		t.root = y
		return
	}
	for i, k := range p.kids {
		if k == x {
			p.kids[i] = y
			return
		}
	}
}

func indexOf(p *node, x *node) int {
	for i, k := range p.kids {
		if k == x {
			return i
		}
	}
	return -1
}

// insertKid puts n into p at i.
func insertKid(p *node, i int, n *node) {
	n.up = p
	p.kids = append(p.kids, nil)
	copy(p.kids[i+1:], p.kids[i:])
	p.kids[i] = n
}

// put adds v to the tab against anchor, as how says. With no anchor in the tab, or placeEnd,
// v goes at the end of the tab along its split. The focus stays where it was.
func (t *tab) put(v, anchor *view, how place) {
	keep := t.focusedView()
	defer t.relist(keep)
	n := &node{kind: leafNode, v: v}
	if t.root == nil {
		t.root = n
		return
	}
	a := t.leafOf(anchor)
	if a == nil || how == placeEnd {
		r := t.root
		if r.kind == splitNode && r.dir == t.split {
			insertKid(r, len(r.kids), n)
			return
		}
		s := &node{kind: splitNode, dir: t.split}
		t.replace(r, s)
		insertKid(s, 0, r)
		insertKid(s, 1, n)
		return
	}
	if how == placeStack {
		if p := a.up; p != nil && p.kind == stackNode {
			i := indexOf(p, a) + 1
			insertKid(p, i, n)
			p.open = i
			return
		}
		s := &node{kind: stackNode, open: 1}
		t.replace(a, s)
		insertKid(s, 0, a)
		insertKid(s, 1, n)
		return
	}
	dir, after := SplitColumns, true
	switch how {
	case placeLeft:
		after = false
	case placeDown:
		dir = SplitRows
	case placeUp:
		dir, after = SplitRows, false
	}
	unit := a
	if a.up != nil && a.up.kind == stackNode {
		unit = a.up // a split beside a stacked view splits the stack
	}
	if p := unit.up; p != nil && p.kind == splitNode && p.dir == dir {
		i := indexOf(p, unit)
		if after {
			i++
		}
		insertKid(p, i, n)
		return
	}
	s := &node{kind: splitNode, dir: dir}
	t.replace(unit, s)
	if after {
		insertKid(s, 0, unit)
		insertKid(s, 1, n)
	} else {
		insertKid(s, 0, n)
		insertKid(s, 1, unit)
	}
}

// take removes v from the tab. A split or stack left with one child gives way to it, and a
// split that then sits in a split of its own direction joins it.
func (t *tab) take(v *view) {
	keep := t.focusedView()
	if keep == v {
		keep = nil
	}
	defer t.relist(keep)
	n := t.leafOf(v)
	if n == nil {
		return
	}
	p := n.up
	if p == nil {
		t.root = nil
		return
	}
	i := indexOf(p, n)
	p.kids = append(p.kids[:i], p.kids[i+1:]...)
	if p.kind == stackNode && p.open > i {
		p.open--
	}
	p.open = min(p.open, len(p.kids)-1)
	if len(p.kids) != 1 {
		return
	}
	only := p.kids[0]
	t.replace(p, only)
	if g := only.up; g != nil && only.kind == splitNode && g.kind == splitNode && g.dir == only.dir {
		j := indexOf(g, only)
		kids := append([]*node(nil), g.kids[:j]...)
		for _, k := range only.kids {
			k.up = g
			kids = append(kids, k)
		}
		g.kids = append(kids, g.kids[j+1:]...)
	}
}

// openPath expands, in each stack above v, the member that leads to v.
func (t *tab) openPath(v *view) {
	n := t.leafOf(v)
	for n != nil && n.up != nil {
		if n.up.kind == stackNode {
			n.up.open = indexOf(n.up, n)
		}
		n = n.up
	}
}

// flip turns the split that holds v, or the tab's own split when none does, the other way
// (pane mode's split key).
func (t *tab) flip(v *view) {
	for n := t.leafOf(v); n != nil; n = n.up {
		if n.kind == splitNode {
			n.dir = 1 - n.dir
			return
		}
	}
	t.split = 1 - t.split
}

// stackOf returns the stack that holds v, or nil.
func (t *tab) stackOf(v *view) *node {
	if n := t.leafOf(v); n != nil && n.up != nil && n.up.kind == stackNode {
		return n.up
	}
	return nil
}

// clone copies the tab's layout: the same views in a new tree.
func (t *tab) clone() *tab {
	var cp func(n, up *node) *node
	cp = func(n, up *node) *node {
		if n == nil {
			return nil
		}
		m := &node{kind: n.kind, v: n.v, dir: n.dir, open: n.open, up: up}
		for _, k := range n.kids {
			m.kids = append(m.kids, cp(k, m))
		}
		return m
	}
	c := &tab{split: t.split, zoom: t.zoom, focus: t.focus}
	c.root = cp(t.root, nil)
	c.relist(t.focusedView())
	return c
}

// sizeIfPut is the size a new view asks for when it goes against anchor as how says, on a
// w x h pane area, with focus on it. gff starts a new pane at this size, so a program starts
// at the size it keeps rather than getting a resize while it starts.
func (t *tab) sizeIfPut(anchor *view, how place, w, h int) (int, int) {
	c := t.clone()
	c.zoom = false
	nv := &view{}
	c.put(nv, anchor, how)
	for i, v := range c.views {
		if v == nv {
			c.focus = i
		}
	}
	for _, s := range c.arrange(w, h).slots {
		if c.views[s.view] == nv {
			return s.wantCols, s.wantRows
		}
	}
	return max(w, 1), max(h-1, 1)
}

// arrange lays out the tab on a w x h area. With zoom, only the focused view shows. A view
// alone asks for the whole area; when it cannot pass bytes through, it is composed under a
// frame row. The stacks above the focused view expand it.
func (t *tab) arrange(w, h int) arrangement {
	w, h = max(w, 1), max(h, 2)
	t.sync()
	n := len(t.views)
	if n == 0 {
		return arrangement{}
	}
	focus := min(t.focus, n-1)
	t.openPath(t.views[focus])
	if n == 1 || t.zoom {
		return arrangement{slots: []slot{{
			view:     focus,
			frame:    rect{0, 0, w, 1},
			content:  rect{0, 1, w, h - 1},
			wantCols: w, wantRows: h,
		}}}
	}
	idx := map[*view]int{}
	for i, v := range t.views {
		idx[v] = i
	}
	var a arrangement
	var lay func(nd *node, r rect)
	lay = func(nd *node, r rect) {
		switch nd.kind {
		case leafNode:
			a.slots = append(a.slots, slot{view: idx[nd.v], frame: rect{r.x, r.y, r.w, 1},
				content: rect{r.x, r.y + 1, r.w, max(r.h-1, 0)}, wantCols: r.w, wantRows: max(r.h-1, 1)})
		case stackNode:
			k := len(nd.kids)
			open := min(max(nd.open, 0), k-1)
			eh := max(r.h-(k-1), 2)
			y := r.y
			for i, kid := range nd.kids {
				if i == open {
					lay(kid, rect{r.x, y, r.w, eh})
					y += eh
					continue
				}
				a.slots = append(a.slots, slot{view: idx[kid.v], frame: rect{r.x, y, r.w, 1},
					content: rect{r.x, y + 1, r.w, 0}, folded: true})
				y++
			}
		case splitNode:
			k := len(nd.kids)
			if nd.dir == SplitColumns {
				avail := max(r.w-(k-1), k)
				x := r.x
				for i, kid := range nd.kids {
					pw := avail / k
					if i < avail%k {
						pw++
					}
					lay(kid, rect{x, r.y, pw, r.h})
					x += pw
					if i < k-1 {
						a.seps = append(a.seps, rect{x, r.y, 1, r.h})
						x++
					}
				}
				return
			}
			avail := max(r.h, 2*k)
			y := r.y
			for i, kid := range nd.kids {
				ph := avail / k
				if i < avail%k {
					ph++
				}
				lay(kid, rect{r.x, y, r.w, ph})
				y += ph
			}
		}
	}
	lay(t.root, rect{0, 0, w, h})
	return a
}

// neighbour is the view next to the focused one in a direction, on a w x h area: the nearest
// whose box lies wholly that way and overlaps it across, the largest overlap first. -1 when
// there is none.
func (t *tab) neighbour(act string, w, h int) int {
	zoom := t.zoom
	t.zoom = false
	a := t.arrange(w, h)
	t.zoom = zoom
	var cur rect
	found := false
	for _, s := range a.slots {
		if s.view == min(t.focus, len(t.views)-1) {
			cur, found = s.box(), true
		}
	}
	if !found {
		return -1
	}
	best, bestDist, bestOver := -1, 0, 0
	for _, s := range a.slots {
		b := s.box()
		var dist, over int
		switch act {
		case actFocusRight:
			dist, over = b.x-(cur.x+cur.w), overlap(cur.y, cur.h, b.y, b.h)
		case actFocusLeft:
			dist, over = cur.x-(b.x+b.w), overlap(cur.y, cur.h, b.y, b.h)
		case actFocusDown:
			dist, over = b.y-(cur.y+cur.h), overlap(cur.x, cur.w, b.x, b.w)
		case actFocusUp:
			dist, over = cur.y-(b.y+b.h), overlap(cur.x, cur.w, b.x, b.w)
		}
		if dist < 0 || over <= 0 {
			continue
		}
		if best < 0 || dist < bestDist || dist == bestDist && over > bestOver {
			best, bestDist, bestOver = s.view, dist, over
		}
	}
	return best
}

func overlap(a, al, b, bl int) int {
	return min(a+al, b+bl) - max(a, b)
}

// flatTab is a tab of n new views along split, for sizes before any pane exists.
func flatTab(n int, split Split) *tab {
	t := &tab{split: split}
	for range n {
		t.put(&view{}, nil, placeEnd)
	}
	return t
}

// ViewSizes returns the size each of n views asks for on a cols x rows host, with the first
// view focused and no zoom. gff starts new panes at these sizes, so a program starts at the
// size it keeps rather than getting a resize while it starts.
func ViewSizes(n int, split Split, cols, rows int) [][2]int {
	if n <= 0 {
		return nil
	}
	a := flatTab(n, split).arrange(cols, rows)
	out := make([][2]int, n)
	for _, s := range a.slots {
		out[s.view] = [2]int{s.wantCols, s.wantRows}
	}
	return out
}
