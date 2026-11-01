package gffclient

// Split says how a tab's panes share the screen (gff-client.md 5.2). The client core keeps one
// tab with one row or column of panes; nested splits, stacks and several tabs are later stages.
type Split int

const (
	SplitColumns Split = iota // side by side, a separator column between them
	SplitRows                 // stacked
)

// rect is a cell rectangle on the host screen.
type rect struct{ x, y, w, h int }

// slot is where one visible view goes.
type slot struct {
	view    int
	frame   rect // the frame row; h is 0 when the view has no frame
	content rect // where the pane's cells go
	// want is the size gff asks for this pane (pane.set_desired_size). A view alone fills the
	// host and has no frame when it passes bytes through, so it asks for the whole host.
	wantCols, wantRows int
}

// arrangement is the layout of the host screen.
type arrangement struct {
	slots []slot
	seps  []rect // separator columns
}

// arrange lays out n views on a w x h host. With zoom, only the focused view shows. A view
// alone asks for the whole host; when it cannot pass bytes through, it is composed under a
// frame row.
func arrange(n, focus int, split Split, zoom bool, w, h int) arrangement {
	w, h = max(w, 1), max(h, 2)
	if n == 1 || zoom {
		return arrangement{slots: []slot{{
			view:     min(focus, n-1),
			frame:    rect{0, 0, w, 1},
			content:  rect{0, 1, w, h - 1},
			wantCols: w, wantRows: h,
		}}}
	}
	var a arrangement
	switch split {
	case SplitColumns:
		avail := max(w-(n-1), n)
		x := 0
		for i := range n {
			pw := avail / n
			if i < avail%n {
				pw++
			}
			a.slots = append(a.slots, slot{view: i, frame: rect{x, 0, pw, 1}, content: rect{x, 1, pw, h - 1},
				wantCols: pw, wantRows: max(h-1, 1)})
			x += pw
			if i < n-1 {
				a.seps = append(a.seps, rect{x, 0, 1, h})
				x++
			}
		}
	case SplitRows:
		avail := max(h, 2*n)
		y := 0
		for i := range n {
			ph := avail / n
			if i < avail%n {
				ph++
			}
			a.slots = append(a.slots, slot{view: i, frame: rect{0, y, w, 1}, content: rect{0, y + 1, w, ph - 1},
				wantCols: w, wantRows: max(ph-1, 1)})
			y += ph
		}
	}
	return a
}

// ViewSizes returns the size each of n views asks for on a cols x rows host, with the first
// view focused and no zoom. gff starts new panes at these sizes, so a program starts at the
// size it keeps rather than getting a resize while it starts.
func ViewSizes(n int, split Split, cols, rows int) [][2]int {
	if n <= 0 {
		return nil
	}
	a := arrange(n, 0, split, false, cols, rows)
	out := make([][2]int, n)
	for _, s := range a.slots {
		out[s.view] = [2]int{s.wantCols, s.wantRows}
	}
	return out
}
