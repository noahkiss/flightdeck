// Package modview draws a module's block tree as terminal lines (modules.md 6, the terminal
// column of 6.1). It knows nothing of the screen: it returns styled lines within a width and the
// targets a key can act on, and the gff client draws them and sends the actions back.
package modview

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/noahkiss/flightdeck/api/modules"
)

// Style is how a span draws. Tint is a meaning from modules.Tints, a #RRGGBB colour, or "" for
// none; the client maps it to its theme (modules.md 6.3).
type Style struct {
	Tint  string
	Bold  bool
	Dim   bool
	Key   bool // the key of a key hint
	Focus bool // part of the focused target
}

// Span is a run of text in one style. Text holds no control characters.
type Span struct {
	Text  string
	Style Style
}

// Line is one terminal row: never wider than the width it was drawn for, never wrapped.
type Line struct {
	Spans []Span
	// Target is the index in View.Targets of the target this line belongs to, or -1.
	Target int
}

// Text is the line's text without styles.
func (l Line) Text() string {
	var b strings.Builder
	for _, s := range l.Spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// Width is the line's width in columns: one per rune, as gff's canvas draws.
func (l Line) Width() int {
	n := 0
	for _, s := range l.Spans {
		n += runes(s.Text)
	}
	return n
}

// Kind is what a target is.
type Kind int

const (
	KindButton  Kind = iota // a button: its key, or Enter on focus, sends its action
	KindToggle              // a toggle: Enter or Space sends its action with the new value
	KindField               // a field: Enter asks for text, then sends its action with it
	KindRow                 // a row with an action: Enter sends it
	KindSection             // a section with an id: Enter folds or unfolds it (view state)
)

// Target is a block a key can act on, in tree order.
type Target struct {
	Kind Kind
	// Ref keys the target's view state: the block's id, else "#<n>" by its place among the
	// targets.
	Ref string
	// Key is a button's key hint, 0 for none.
	Key rune
	// Title is the button's, toggle's or row's title, the section's title, or a field's
	// placeholder.
	Title       string
	Action      modules.Action
	Confirm     string // the question to ask first, empty for none
	Destructive bool
	Value       bool // a toggle's value as drawn
	Collapsed   bool // a section's state as drawn
}

// Send is the action the target sends. A toggle adds "value", the new state, and a field adds
// "text", what was typed, to a copy of the args (modules.md 6.1). A section sends nothing.
func (t Target) Send(text string) (modules.Action, bool) {
	a := modules.Action{Name: t.Action.Name, Args: maps.Clone(t.Action.Args)}
	switch t.Kind {
	case KindSection:
		return modules.Action{}, false
	case KindToggle:
		if a.Args == nil {
			a.Args = map[string]any{}
		}
		a.Args["value"] = !t.Value
	case KindField:
		if a.Args == nil {
			a.Args = map[string]any{}
		}
		a.Args["text"] = text
	}
	return a, true
}

// IsClient reports whether the action is one the client handles itself (modules.md 6.2).
func IsClient(a modules.Action) bool { return strings.HasPrefix(a.Name, modules.ClientPrefix) }

// View is a drawn tree.
type View struct {
	Lines   []Line
	Targets []Target
	// Err is set when the tree could not be drawn; Lines is then the one error line.
	Err string
}

// TargetByKey returns the index of the target whose key hint is r, or -1.
func (v *View) TargetByKey(r rune) int {
	for i, t := range v.Targets {
		if t.Key != 0 && t.Key == r {
			return i
		}
	}
	return -1
}

// TargetByRef returns the index of the target with ref, or -1.
func (v *View) TargetByRef(ref string) int {
	for i, t := range v.Targets {
		if t.Ref == ref {
			return i
		}
	}
	return -1
}

// Options shape one drawing.
type Options struct {
	Width int
	// Now is the clock for a progress block's countdown. Zero: time.Now.
	Now time.Time
	// Age is how long ago the tree arrived, for stale_after.
	Age time.Duration
	// Focus is the ref of the focused target, empty for none.
	Focus string
	// Folded holds the sections the person folded or unfolded, by ref; it wins over the
	// block's own collapsed.
	Folded map[string]bool
	// Reserved are keys a key hint must not take: the keys the client binds where the tree
	// shows.
	Reserved string
	// LogLines is the most lines a log block shows. Zero: 8.
	LogLines int
}

// Error texts the client shows.
const (
	// NeedsNewer is the placeholder for a block type this build does not know (modules.md 6.4).
	NeedsNewer = "Needs a newer Flightdeck"
	errLead    = "module error: "
)

// ErrorLine is the one line a tree that cannot be drawn shows: the reason, cut to the width.
func ErrorLine(reason string, width int) Line {
	return cutLine(Line{Spans: []Span{{Text: clean(errLead + reason), Style: Style{Tint: "error"}}}, Target: -1}, width)
}

// Render draws a tree, as JSON, within opt.Width columns. Clients are lenient (modules.md 6.4):
// an unknown field is ignored, an unknown word draws as the field's default, an unknown type as
// its fallback. A tree that is not a block tree, or passes the tree limits, draws as one error
// line; Render never panics on its input.
func Render(tree []byte, opt Options) *View {
	if opt.Width < 1 {
		opt.Width = 1
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.LogLines <= 0 {
		opt.LogLines = 8
	}
	fail := func(reason string) *View {
		return &View{Lines: []Line{ErrorLine(reason, opt.Width)}, Err: reason}
	}
	if len(tree) > modules.MaxTreeBytes {
		return fail(fmt.Sprintf("tree is %d bytes; the limit is %d", len(tree), modules.MaxTreeBytes))
	}
	var root modules.Block
	if err := json.Unmarshal(tree, &root); err != nil {
		return fail("not a block tree: " + err.Error())
	}
	r := &renderer{opt: opt, used: map[rune]bool{}}
	for _, k := range opt.Reserved {
		r.used[k] = true
	}
	if reason := r.check(&root, 1); reason != "" {
		return fail(reason)
	}
	r.assignKeys(&root)
	lines := r.block(&root, opt.Width, false)
	for i := range lines {
		lines[i] = cutLine(lines[i], opt.Width)
	}
	return &View{Lines: lines, Targets: r.targets}
}

type renderer struct {
	opt     Options
	targets []Target
	used    map[rune]bool
	keys    map[*modules.ButtonBlock]rune
	blocks  int
}

// check walks the tree once for what would make drawing it unsafe or meaningless: depth, block
// count, a block with no type.
func (r *renderer) check(b *modules.Block, depth int) string {
	if depth > modules.MaxDepth {
		return fmt.Sprintf("tree is deeper than %d", modules.MaxDepth)
	}
	r.blocks++
	if r.blocks > modules.MaxBlocks {
		return fmt.Sprintf("tree has more than %d blocks", modules.MaxBlocks)
	}
	if b.Type == "" {
		return "a block has no type"
	}
	for i := range children(b) {
		if s := r.check(&children(b)[i], depth+1); s != "" {
			return s
		}
	}
	return ""
}

func children(b *modules.Block) []modules.Block {
	switch {
	case b.Stack != nil:
		return b.Stack.Children
	case b.Section != nil:
		return b.Section.Children
	case b.Metrics != nil:
		return b.Metrics.Children
	case b.List != nil:
		return b.List.Children
	}
	return nil
}

// assignKeys gives each button a key hint, in tree order: the first letter or digit of its title
// that no earlier button and no reserved key took, else the first free digit 1 to 9, else none
// (Enter on focus still sends it). The same tree always gets the same keys.
func (r *renderer) assignKeys(root *modules.Block) {
	r.keys = map[*modules.ButtonBlock]rune{}
	var buttons []*modules.ButtonBlock
	var walk func(b *modules.Block)
	walk = func(b *modules.Block) {
		if b.Button != nil {
			buttons = append(buttons, b.Button)
		}
		for i := range children(b) {
			walk(&children(b)[i])
		}
	}
	walk(root)
	for _, bt := range buttons {
		for _, c := range strings.ToLower(bt.Title) {
			if c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) && !r.used[c] {
				r.keys[bt], r.used[c] = c, true
				break
			}
		}
	}
	for _, bt := range buttons {
		if _, ok := r.keys[bt]; ok {
			continue
		}
		for c := '1'; c <= '9'; c++ {
			if !r.used[c] {
				r.keys[bt], r.used[c] = c, true
				break
			}
		}
	}
}

func (r *renderer) addTarget(id string, t Target) int {
	t.Ref = id
	if t.Ref == "" {
		t.Ref = "#" + strconv.Itoa(len(r.targets))
	}
	r.targets = append(r.targets, t)
	return len(r.targets) - 1
}

func (r *renderer) focused(i int) bool { return r.opt.Focus != "" && r.targets[i].Ref == r.opt.Focus }

// block draws one block in w columns. dim greys it and everything under it (stale_after).
func (r *renderer) block(b *modules.Block, w int, dim bool) []Line {
	if b.StaleAfter > 0 && r.opt.Age > time.Duration(b.StaleAfter)*time.Second {
		dim = true
	}
	var lines []Line
	switch {
	case b.Stack != nil:
		lines = r.stack(b.Stack, w, dim)
	case b.Section != nil:
		lines = r.section(b, w, dim)
	case b.Row != nil:
		lines = []Line{r.row(b, w)}
	case b.Text != nil:
		lines = r.text(b.Text, w)
	case b.Badge != nil:
		lines = []Line{one(Span{"[" + clean(b.Badge.Text) + "]", Style{Tint: b.Badge.DrawnTint()}})}
	case b.Metric != nil:
		lines = []Line{{Spans: metric(b.Metric), Target: -1}}
	case b.Metrics != nil:
		lines = []Line{r.metrics(b.Metrics)}
	case b.Progress != nil:
		lines = []Line{r.progress(b.Progress, w)}
	case b.Button != nil:
		lines = []Line{r.button(b)}
	case b.Toggle != nil:
		lines = []Line{r.toggle(b)}
	case b.Field != nil:
		lines = []Line{r.field(b)}
	case b.List != nil:
		lines = r.list(b.List, w, dim)
	case b.Log != nil:
		lines = r.log(b.Log)
	default:
		text := NeedsNewer
		if b.Fallback != "" {
			text = b.Fallback
		}
		lines = []Line{one(Span{clean(text), Style{Dim: true}})}
	}
	for i := range lines {
		lines[i] = cutLine(lines[i], w)
		if dim {
			for j := range lines[i].Spans {
				lines[i].Spans[j].Style.Dim = true
			}
		}
	}
	return lines
}

func one(spans ...Span) Line { return Line{Spans: spans, Target: -1} }

// gap is a stack's space between children: the block's steps; absent, none between rows and one
// column between columns, so a row of badges or buttons stays readable.
func gap(s *modules.StackBlock, horizontal bool) int {
	if s.Gap != nil {
		return max(0, min(*s.Gap, 4))
	}
	if horizontal {
		return 1
	}
	return 0
}

func (r *renderer) stack(s *modules.StackBlock, w int, dim bool) []Line {
	if s.DrawnAxis() == "horizontal" {
		return r.columns(s, w, dim)
	}
	g := gap(s, false)
	var out []Line
	for i := range s.Children {
		if i > 0 {
			for range g {
				out = append(out, one())
			}
		}
		out = append(out, r.block(&s.Children[i], w, dim)...)
	}
	return out
}

// columns lays a horizontal stack's children side by side, each as wide as its widest line, with
// the gap between them. The row is cut at the width, never wrapped (modules.md 6.1).
func (r *renderer) columns(s *modules.StackBlock, w int, dim bool) []Line {
	g := gap(s, true)
	var cols [][]Line
	widths := []int{}
	height := 0
	for i := range s.Children {
		ls := r.block(&s.Children[i], w, dim)
		cw := 0
		for _, l := range ls {
			cw = max(cw, l.Width())
		}
		if len(ls) == 0 {
			continue
		}
		cols = append(cols, ls)
		widths = append(widths, cw)
		height = max(height, len(ls))
	}
	out := make([]Line, height)
	for y := range out {
		out[y].Target = -1
		for i, ls := range cols {
			if i > 0 {
				out[y].Spans = append(out[y].Spans, Span{Text: strings.Repeat(" ", g)})
			}
			cell := Line{Target: -1}
			if y < len(ls) {
				cell = ls[y]
			}
			out[y].Spans = append(out[y].Spans, cell.Spans...)
			if pad := widths[i] - cell.Width(); pad > 0 && i < len(cols)-1 {
				out[y].Spans = append(out[y].Spans, Span{Text: strings.Repeat(" ", pad)})
			}
			// A line of several targets keeps the first; keys reach the others.
			if out[y].Target < 0 {
				out[y].Target = cell.Target
			}
		}
		out[y] = trimRight(out[y])
	}
	return out
}

func (r *renderer) section(b *modules.Block, w int, dim bool) []Line {
	s := b.Section
	collapsed := s.Collapsed != nil && *s.Collapsed
	ti := -1
	if b.ID != "" {
		if f, ok := r.opt.Folded[b.ID]; ok {
			collapsed = f
		}
		ti = r.addTarget(b.ID, Target{Kind: KindSection, Title: s.Title, Collapsed: collapsed})
	}
	mark := "▾ "
	if collapsed {
		mark = "▸ "
	}
	head := Line{Spans: []Span{{Text: mark + clean(s.Title), Style: Style{Bold: true}}}, Target: ti}
	if collapsed && len(s.Children) > 0 {
		head.Spans = append(head.Spans, Span{Text: " (" + strconv.Itoa(len(s.Children)) + ")", Style: Style{Dim: true}})
	}
	if ti >= 0 && r.focused(ti) {
		focus(&head)
	}
	out := []Line{head}
	if collapsed {
		// Targets inside a folded section are not drawn and take no keys.
		return out
	}
	for i := range s.Children {
		for _, l := range r.block(&s.Children[i], w-2, dim) {
			l.Spans = append([]Span{{Text: "  "}}, l.Spans...)
			out = append(out, l)
		}
	}
	return out
}

func (r *renderer) row(b *modules.Block, w int) Line {
	rw := b.Row
	tint := rw.DrawnTint()
	var left []Span
	titleStyle := Style{}
	if sym := Symbol(rw.Symbol); sym != "" {
		left = append(left, Span{sym + " ", Style{Tint: tint}})
	} else {
		titleStyle.Tint = tint
	}
	left = append(left, Span{clean(rw.Title), titleStyle})
	if rw.Body != "" {
		left = append(left, Span{" · " + clean(rw.Body), Style{Dim: true}})
	}
	l := Line{Spans: left, Target: -1}
	if rw.Action != nil {
		l.Target = r.addTarget(b.ID, Target{Kind: KindRow, Title: rw.Title, Action: *rw.Action})
	}
	if t := clean(rw.Trailing); t != "" {
		tw := runes(t)
		if w-tw-1 >= min(4, l.Width()) {
			l = cutLine(l, w-tw-1)
			l.Spans = append(l.Spans, Span{Text: strings.Repeat(" ", w-tw-l.Width())}, Span{t, Style{Dim: true}})
		} else {
			l.Spans = append(l.Spans, Span{" " + t, Style{Dim: true}})
		}
	}
	if l.Target >= 0 && r.focused(l.Target) {
		focus(&l)
	}
	return l
}

func (r *renderer) text(t *modules.TextBlock, w int) []Line {
	st := Style{}
	switch t.DrawnStyle() {
	case "caption":
		st.Dim = true
	case "title":
		st.Bold = true
	}
	var out []Line
	for _, s := range strings.Split(t.Text, "\n") {
		if t.DrawnStyle() != "mono" {
			s = strings.TrimRight(s, " \t\r")
		}
		out = append(out, one(Span{clean(s), st}))
	}
	return out
}

func metric(m *modules.MetricBlock) []Span {
	spans := []Span{{clean(m.Label) + " ", Style{Dim: true}}, {clean(m.Value), Style{Bold: true, Tint: m.DrawnTint()}}}
	if m.Unit != "" {
		spans = append(spans, Span{" " + clean(m.Unit), Style{}})
	}
	return spans
}

func (r *renderer) metrics(m *modules.MetricsBlock) Line {
	l := one()
	n := 0
	for i := range m.Children {
		c := &m.Children[i]
		var spans []Span
		switch {
		case c.Metric != nil:
			spans = metric(c.Metric)
		case c.Fallback != "":
			spans = []Span{{clean(c.Fallback), Style{Dim: true}}}
		default:
			continue
		}
		if n > 0 {
			l.Spans = append(l.Spans, Span{"  ·  ", Style{Dim: true}})
		}
		l.Spans = append(l.Spans, spans...)
		n++
	}
	return l
}

// barMax is the widest a progress bar draws.
const barMax = 20

func (r *renderer) progress(p *modules.ProgressBlock, w int) Line {
	l := one()
	var tail string
	switch {
	case p.Steps != nil:
		l.Spans = append(l.Spans, Span{fmt.Sprintf("%d/%d", p.Steps.Current, p.Steps.Total), Style{Bold: true}})
	case p.Value != nil:
		v := max(0, min(*p.Value, 1))
		tail = " " + strconv.Itoa(int(v*100+0.5)) + "%"
		bw := min(barMax, w-runes(tail))
		if bw >= 3 {
			fill := int(v*float64(bw) + 0.5)
			l.Spans = append(l.Spans, Span{strings.Repeat("█", fill), Style{Tint: "accent"}},
				Span{strings.Repeat("░", bw-fill), Style{Dim: true}})
		} else {
			tail = strings.TrimPrefix(tail, " ")
		}
		l.Spans = append(l.Spans, Span{Text: tail})
	}
	if p.EndsAt != "" {
		if t, err := time.Parse(time.RFC3339, p.EndsAt); err == nil {
			sep := " · "
			if len(l.Spans) == 0 {
				sep = ""
			}
			l.Spans = append(l.Spans, Span{sep + Countdown(t.Sub(r.opt.Now)), Style{Dim: true}})
		}
	}
	return l
}

// Countdown is the time left as the terminal shows it: "45s left", "12m left", "2h 5m left",
// "ended".
func Countdown(d time.Duration) string {
	switch {
	case d <= 0:
		return "ended"
	case d < time.Minute:
		return strconv.Itoa(int((d+time.Second-1)/time.Second)) + "s left"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m left"
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return strconv.Itoa(h) + "h left"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m left"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d left"
}

func (r *renderer) button(b *modules.Block) Line {
	bt := b.Button
	k := r.keys[bt]
	t := Target{Kind: KindButton, Key: k, Title: bt.Title, Action: bt.Action, Confirm: bt.Confirm,
		Destructive: bt.DrawnRole() == "destructive"}
	i := r.addTarget(b.ID, t)
	ts := Style{}
	if t.Destructive {
		ts.Tint = "error"
	}
	var l Line
	if k != 0 {
		l = Line{Spans: []Span{{string(k), Style{Key: true, Bold: true}}, {" " + clean(bt.Title), ts}}, Target: i}
	} else {
		l = Line{Spans: []Span{{"›", Style{Key: true, Bold: true}}, {" " + clean(bt.Title), ts}}, Target: i}
	}
	if r.focused(i) {
		focus(&l)
	}
	return l
}

func (r *renderer) toggle(b *modules.Block) Line {
	tg := b.Toggle
	i := r.addTarget(b.ID, Target{Kind: KindToggle, Title: tg.Title, Action: tg.Action, Value: tg.Value})
	box := "[ ] "
	if tg.Value {
		box = "[x] "
	}
	l := Line{Spans: []Span{{box, Style{Bold: true}}, {clean(tg.Title), Style{}}}, Target: i}
	if r.focused(i) {
		focus(&l)
	}
	return l
}

func (r *renderer) field(b *modules.Block) Line {
	f := b.Field
	i := r.addTarget(b.ID, Target{Kind: KindField, Title: f.Placeholder, Action: f.Action})
	ph := clean(f.Placeholder)
	if ph == "" {
		ph = "…"
	}
	l := Line{Spans: []Span{{"> ", Style{Bold: true}}, {ph, Style{Dim: true}}}, Target: i}
	if r.focused(i) {
		focus(&l)
	}
	return l
}

func (r *renderer) list(ls *modules.ListBlock, w int, dim bool) []Line {
	if len(ls.Children) == 0 {
		if ls.Empty == "" {
			return nil
		}
		return []Line{one(Span{clean(ls.Empty), Style{Dim: true}})}
	}
	var out []Line
	for i := range ls.Children {
		out = append(out, r.block(&ls.Children[i], w, dim)...)
	}
	return out
}

// log shows at most LogLines lines: the newest when it follows, else the first, with a line that
// counts the rest. The terminal's pager is a later stage; a sheet place could scroll it.
func (r *renderer) log(lg *modules.LogBlock) []Line {
	lines := lg.Lines
	n := r.opt.LogLines
	var out []Line
	follow := lg.Follow != nil && *lg.Follow
	more := ""
	if len(lines) > n {
		if follow {
			more = fmt.Sprintf("… %d earlier lines", len(lines)-n+1)
			lines = lines[len(lines)-n+1:]
		} else {
			more = fmt.Sprintf("… %d more lines", len(lines)-n+1)
			lines = lines[:n-1]
		}
	}
	if more != "" && follow {
		out = append(out, one(Span{more, Style{Dim: true}}))
	}
	for _, s := range lines {
		out = append(out, one(Span{clean(s), Style{}}))
	}
	if more != "" && !follow {
		out = append(out, one(Span{more, Style{Dim: true}}))
	}
	return out
}

func focus(l *Line) {
	for i := range l.Spans {
		l.Spans[i].Style.Focus = true
	}
}

// clean makes module text safe for a terminal: a tab becomes a space, and every other control
// character (C0, DEL, C1, which could start an escape sequence) becomes U+FFFD. A tree is data;
// it never reaches the host terminal as control bytes.
func clean(s string) string {
	ok := true
	for _, c := range s {
		if c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	return strings.Map(func(c rune) rune {
		switch {
		case c == '\t':
			return ' '
		case c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0):
			return '�'
		}
		return c
	}, s)
}

func runes(s string) int { return len([]rune(s)) }

// cutLine cuts a line to w columns, ending with "…" when it was longer (modules.md 6.1: cut,
// never wrap).
func cutLine(l Line, w int) Line {
	if l.Width() <= w {
		return l
	}
	out := Line{Target: l.Target}
	if w <= 0 {
		return out
	}
	left := w - 1
	last := l.Spans[0].Style
	for _, s := range l.Spans {
		if left <= 0 {
			break
		}
		rs := []rune(s.Text)
		if len(rs) > left {
			rs = rs[:left]
		}
		out.Spans = append(out.Spans, Span{string(rs), s.Style})
		left -= len(rs)
		last = s.Style
	}
	out.Spans = append(out.Spans, Span{"…", last})
	return out
}

func trimRight(l Line) Line {
	for len(l.Spans) > 0 {
		s := &l.Spans[len(l.Spans)-1]
		s.Text = strings.TrimRight(s.Text, " ")
		if s.Text != "" {
			break
		}
		l.Spans = l.Spans[:len(l.Spans)-1]
	}
	return l
}
