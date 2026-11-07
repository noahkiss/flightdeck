package groups

import (
	"slices"

	"github.com/noahkiss/flightdeck/api"
)

// The lane's rules on the record (groups.md 7.2, 7.3). gff-hab's lane service runs git through
// internal/lane and writes the outcome here.

// MayLand reports whether an item may land: it is ready, and its review passed on the head it
// lands, or review is off (groups.md 7.3: the lane never lands without a pass).
func MayLand(it api.GroupItem) bool {
	if it.State != ItemReady && it.State != ItemLanding {
		return false
	}
	if it.Land == "" {
		return false
	}
	if it.Review == "off" || it.Reviewer == "" {
		return it.Land == it.Submitted
	}
	v := it.Verdict
	if v == nil || v.Verdict != "pass" || v.Head != it.Submitted {
		return false
	}
	return it.Land == it.Submitted || (v.FixedHead != "" && it.Land == v.FixedHead)
}

// waitsForBase reports whether an item stacks on one that has not landed yet: it never lands
// before the item it stacks on (groups.md 7.3). A dropped base holds nothing back.
func waitsForBase(g api.Group, it api.GroupItem) bool {
	if it.After == "" {
		return false
	}
	b := ItemIndex(g, it.After)
	return b >= 0 && g.Items[b].State != ItemLanded && g.Items[b].State != ItemDropped
}

// NextLand returns the item the lane takes now: the one landing (a run cut short by a
// restart), else the first in the queue that may land and whose base landed. "" for none.
func NextLand(g api.Group) string {
	if g.Lane.Landing != "" {
		if i := ItemIndex(g, g.Lane.Landing); i >= 0 && MayLand(g.Items[i]) {
			return g.Lane.Landing
		}
	}
	for _, id := range g.Lane.Queue {
		i := ItemIndex(g, id)
		if i < 0 || !MayLand(g.Items[i]) || waitsForBase(g, g.Items[i]) {
			continue
		}
		return id
	}
	return ""
}

// Upstream is the head of the item it stacks on, whose commits drop out of its rebase
// (groups.md 7.2): that item's landed-from head, else its submitted head. "" for none.
func Upstream(g api.Group, it api.GroupItem) string {
	if it.After == "" {
		return ""
	}
	b := ItemIndex(g, it.After)
	if b < 0 {
		return ""
	}
	if g.Items[b].Land != "" {
		return g.Items[b].Land
	}
	return g.Items[b].Submitted
}

// Upstreams lists every head the item it stacks on has had, newest first: its land head, the
// commit it landed as, the reviewer's fixed head, its submitted head and the heads the lane
// pushed for it. The lane rebases from the newest one the item holds. nil for none.
func Upstreams(g api.Group, it api.GroupItem) []string {
	if it.After == "" {
		return nil
	}
	b := ItemIndex(g, it.After)
	if b < 0 {
		return nil
	}
	base := g.Items[b]
	var out []string
	add := func(h string) {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	add(base.Land)
	add(base.Landed)
	if base.Verdict != nil {
		add(base.Verdict.FixedHead)
		add(base.Verdict.Head)
	}
	add(base.Submitted)
	for _, h := range Prior(base) {
		add(h)
	}
	return out
}

// Prior lists the rebased heads the lane pushed for an item since its last submit.
func Prior(it api.GroupItem) []string {
	var out []string
	for _, a := range it.Attempts {
		if a.Rebased != "" && !slices.Contains(out, a.Rebased) {
			out = append(out, a.Rebased)
		}
	}
	return out
}

// Finished reports whether every item is landed or dropped (groups.md 7.5).
func Finished(g api.Group) bool {
	if len(g.Items) == 0 {
		return false
	}
	for _, it := range g.Items {
		if it.State != ItemLanded && it.State != ItemDropped {
			return false
		}
	}
	return true
}

// Dequeue takes an item out of the lane queue.
func Dequeue(g *api.Group, id string) {
	g.Lane.Queue = slices.DeleteFunc(g.Lane.Queue, func(q string) bool { return q == id })
	if g.Lane.Landing == id {
		g.Lane.Landing = ""
	}
}
