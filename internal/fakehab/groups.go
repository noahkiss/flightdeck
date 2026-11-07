package fakehab

import (
	"cmp"
	"slices"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// PutGroup adds or replaces a group record, by id, raising its revision, and publishes
// group.updated. It serves no group.* method: the snapshot and the event are what clients read.
func (h *Hab) PutGroup(g api.Group) api.Group {
	h.mu.Lock()
	h.grev++
	g.Revision = h.grev
	h.groups[g.ID] = g
	h.mu.Unlock()
	h.publish(structure.Event{Type: control.EventGroupUpdated, ID: g.ID, Revision: g.Revision, Data: g})
	return g
}

// groupList is the snapshot's groups, by id. The caller holds h.mu.
func (h *Hab) groupList() []api.Group {
	if len(h.groups) == 0 {
		return nil
	}
	out := make([]api.Group, 0, len(h.groups))
	for _, g := range h.groups {
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b api.Group) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
