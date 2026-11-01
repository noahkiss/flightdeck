package structure

import (
	"sort"
	"sync"

	"github.com/noahkiss/flightdeck/api"
)

// HolderPane is one live pane as a holder reports it. A later stage adapts the real holder link
// to this shape.
type HolderPane struct {
	// ID is the holder's stable pane id (its UUID as a string). It is unique on the host, across
	// every holder, and never reused.
	ID          string
	HolderID    string // which holder owns the pane: the current one or a draining one
	HolderBuild string
	Cols, Rows  int
	// Offset is the holder's absolute output offset for the pane (spec 5.4): the offset after the
	// last byte the pane produced. It is never a count of bytes delivered to a client.
	Offset uint64
	// Exit is set when the pane's child has been reaped but the holder still keeps the pane.
	Exit *api.ExitStatus
}

// NoticeKind says what changed on a holder.
type NoticeKind int

const (
	PaneAdded   NoticeKind = iota + 1 // a new pane appeared
	PaneResized                       // the pane's PTY size changed
	PaneExited                        // the pane's child was reaped; Exit is set
	PaneGone                          // the holder no longer has the pane
)

// Notice is one change a holder reports. Pane carries the pane as it is after the change; for
// PaneGone only Pane.ID is meaningful.
type Notice struct {
	Kind NoticeKind
	Pane HolderPane
	Exit *api.ExitStatus
}

// Holders is what the structure needs from the live holders on its host.
type Holders interface {
	// Panes lists every live pane on every live holder. It fails unless every holder answered:
	// a partial list would make the panes of a missing holder look gone.
	Panes() ([]HolderPane, error)
	// Subscribe calls fn for every notice until cancel is called. fn may be called from any
	// goroutine, but never from inside Panes, Subscribe or cancel, and never while the holder
	// side holds a lock. Notices for one pane arrive in order. PaneGone means the pane is
	// destroyed, not that its holder is out of reach.
	Subscribe(fn func(Notice)) (cancel func())
}

// Completer is a Holders that can tell whether its pane list holds every holder on the host.
// The holder link is complete only once it has reached every registered holder; until then a
// pane missing from Panes may run on a holder it has not reached.
type Completer interface {
	Complete() bool
}

// Complete reports whether h's pane list holds every holder on the host. A Holders that is not
// a Completer always answers for every holder.
func Complete(h Holders) bool {
	if c, ok := h.(Completer); ok {
		return c.Complete()
	}
	return true
}

// FakeHolder is an in-memory Holders for tests. It never runs a program.
type FakeHolder struct {
	mu      sync.Mutex
	panes   map[string]HolderPane
	subs    map[int]func(Notice)
	next    int
	partial bool
}

// Complete implements Completer: true unless SetPartial made the list partial.
func (f *FakeHolder) Complete() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.partial
}

// SetPartial makes the fake's pane list partial (a holder not reached) or complete again.
func (f *FakeHolder) SetPartial(partial bool) {
	f.mu.Lock()
	f.partial = partial
	f.mu.Unlock()
}

// NewFakeHolder returns a holder with the given panes.
func NewFakeHolder(panes ...HolderPane) *FakeHolder {
	f := &FakeHolder{panes: map[string]HolderPane{}, subs: map[int]func(Notice){}}
	for _, p := range panes {
		f.panes[p.ID] = p
	}
	return f
}

// Panes implements Holders, sorted by id.
func (f *FakeHolder) Panes() ([]HolderPane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]HolderPane, 0, len(f.panes))
	for _, p := range f.panes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Subscribe implements Holders.
func (f *FakeHolder) Subscribe(fn func(Notice)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.next
	f.next++
	f.subs[id] = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, id)
	}
}

// Add adds a pane and notifies subscribers.
func (f *FakeHolder) Add(p HolderPane) {
	f.mu.Lock()
	f.panes[p.ID] = p
	f.mu.Unlock()
	f.notify(Notice{Kind: PaneAdded, Pane: p})
}

// Resize changes a pane's size and notifies subscribers.
func (f *FakeHolder) Resize(id string, cols, rows int) {
	f.mu.Lock()
	p, ok := f.panes[id]
	if ok {
		p.Cols, p.Rows = cols, rows
		f.panes[id] = p
	}
	f.mu.Unlock()
	if ok {
		f.notify(Notice{Kind: PaneResized, Pane: p})
	}
}

// Write advances a pane's absolute offset by n bytes, as if the program printed them.
func (f *FakeHolder) Write(id string, n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.panes[id]; ok {
		p.Offset += n
		f.panes[id] = p
	}
}

// Exit marks a pane's child as reaped and notifies subscribers. The pane stays listed.
func (f *FakeHolder) Exit(id string, st api.ExitStatus) {
	f.mu.Lock()
	p, ok := f.panes[id]
	if ok {
		p.Exit = &st
		f.panes[id] = p
	}
	f.mu.Unlock()
	if ok {
		f.notify(Notice{Kind: PaneExited, Pane: p, Exit: &st})
	}
}

// Remove drops a pane and notifies subscribers.
func (f *FakeHolder) Remove(id string) {
	f.mu.Lock()
	_, ok := f.panes[id]
	delete(f.panes, id)
	f.mu.Unlock()
	if ok {
		f.notify(Notice{Kind: PaneGone, Pane: HolderPane{ID: id}})
	}
}

// RemoveQuietly drops a pane without a notice, as when the holder loses a pane while no logic
// process is connected.
func (f *FakeHolder) RemoveQuietly(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.panes, id)
}

func (f *FakeHolder) notify(n Notice) {
	f.mu.Lock()
	subs := make([]func(Notice), 0, len(f.subs))
	for _, fn := range f.subs {
		subs = append(subs, fn)
	}
	f.mu.Unlock()
	for _, fn := range subs {
		fn(n)
	}
}
