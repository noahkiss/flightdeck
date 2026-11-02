package fakehab

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// roundBook is the fake hab's rounds: what the test adds, with the pause, unpause and run
// calls of rounds.md 13. It schedules nothing. Guarded by Hab.mu.
type roundBook struct {
	rounds map[string]api.Round
	live   map[string]bool // round id -> a run is live: round.run answers conflict
	rev    uint64
	runs   []string // the round ids round.run was called with, in order
}

func newRoundBook() roundBook {
	return roundBook{rounds: map[string]api.Round{}, live: map[string]bool{}}
}

func (b *roundBook) list() []api.Round {
	out := make([]api.Round, 0, len(b.rounds))
	for _, r := range b.rounds {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, c api.Round) int { return cmp.Compare(a.Name, c.Name) })
	if len(out) == 0 {
		return nil
	}
	return out
}

func (b *roundBook) find(ref api.RoundRef) (api.Round, error) {
	for _, r := range b.rounds {
		if (ref.ID != "" && r.ID == ref.ID) || (ref.ID == "" && r.Name == ref.Name) {
			return r, nil
		}
	}
	return api.Round{}, control.Errorf(api.ErrorKindNotFound, "no round %s%s", ref.ID, ref.Name)
}

// publish hands one event to the running server, if any. The caller holds no lock.
func (h *Hab) publish(e structure.Event) {
	h.mu.Lock()
	srv := h.srv
	h.mu.Unlock()
	if srv != nil {
		srv.Publish(e)
	}
}

// PutRound adds or replaces a round, by id, raising its revision, and publishes round.updated.
// It returns the round as stored.
func (h *Hab) PutRound(r api.Round) api.Round {
	h.mu.Lock()
	h.rounds.rev++
	r.Revision = h.rounds.rev
	if old, ok := h.rounds.rounds[r.ID]; ok && r.LastRun == nil {
		r.LastRun = old.LastRun
	}
	h.rounds.rounds[r.ID] = r
	h.mu.Unlock()
	h.publish(structure.Event{Type: control.EventRoundUpdated, ID: r.ID, Revision: r.Revision, Data: r})
	return r
}

// SetRunLive marks a round as having a live run, or not: round.run then answers conflict.
func (h *Hab) SetRunLive(id string, live bool) {
	h.mu.Lock()
	h.rounds.live[id] = live
	h.mu.Unlock()
}

// RunCalls returns the round ids round.run started a run for, in order.
func (h *Hab) RunCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.rounds.runs)
}

// Round returns one round as the hab holds it.
func (h *Hab) Round(id string) (api.Round, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rounds.rounds[id]
	return r, ok
}

// AddRunSession starts a fake pane and files it as a run session of a round's run.
func (h *Hab) AddRunSession(name, round, run string) (session, pane string, err error) {
	return h.createRun(name, "", &api.SessionRound{Round: round, Run: run})
}

// SetCatalog sets what catalog.list answers and publishes catalog.updated for each host in it.
// A client asks only a hab whose welcome lists "catalog": add it to Capabilities first.
func (h *Hab) SetCatalog(cl api.CatalogList) {
	h.mu.Lock()
	c := cl
	h.cat = &c
	h.mu.Unlock()
	for _, e := range cl.Hosts {
		h.publish(structure.Event{Type: control.EventCatalogUpdated, ID: e.Catalog.DeviceID,
			Data: api.CatalogUpdated{DeviceID: e.Catalog.DeviceID, Generation: e.Catalog.Generation}})
	}
}

type roundsAPI struct{ h *Hab }

var _ control.Rounds = roundsAPI{}

func (a roundsAPI) List() []api.Round {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if l := a.h.rounds.list(); l != nil {
		return l
	}
	return []api.Round{}
}

func (a roundsAPI) Get(ref api.RoundRef) (api.Round, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	return a.h.rounds.find(ref)
}

func (a roundsAPI) Create(context.Context, api.RoundCreateParams, string) (api.RoundCreated, error) {
	return api.RoundCreated{}, control.Errorf(api.ErrorKindUnavailable, "the fake hab does not create rounds")
}

func (a roundsAPI) Update(context.Context, api.RoundUpdateParams) (api.Round, error) {
	return api.Round{}, control.Errorf(api.ErrorKindUnavailable, "the fake hab does not edit rounds")
}

func (a roundsAPI) Delete(ref api.RoundRef) error {
	a.h.mu.Lock()
	r, err := a.h.rounds.find(ref)
	if err != nil {
		a.h.mu.Unlock()
		return err
	}
	delete(a.h.rounds.rounds, r.ID)
	a.h.rounds.rev++
	rev := a.h.rounds.rev
	a.h.mu.Unlock()
	a.h.publish(structure.Event{Type: control.EventRoundUpdated, ID: r.ID, Revision: rev,
		Data: api.Round{ID: r.ID, Revision: rev, Deleted: true}})
	return nil
}

func (a roundsAPI) pause(ref api.RoundRef, paused bool) (api.Round, error) {
	a.h.mu.Lock()
	r, err := a.h.rounds.find(ref)
	a.h.mu.Unlock()
	if err != nil {
		return api.Round{}, err
	}
	r.Paused = paused
	if paused {
		r.NextSlot = ""
	}
	return a.h.PutRound(r), nil
}

func (a roundsAPI) Pause(ref api.RoundRef) (api.Round, error)   { return a.pause(ref, true) }
func (a roundsAPI) Unpause(ref api.RoundRef) (api.Round, error) { return a.pause(ref, false) }

func (a roundsAPI) Run(ref api.RoundRef) (api.RoundRun, error) {
	a.h.mu.Lock()
	r, err := a.h.rounds.find(ref)
	switch {
	case err != nil:
	case a.h.rounds.live[r.ID]:
		err = control.Errorf(api.ErrorKindConflict, "the round has a live run")
	case r.Paused:
		err = control.Errorf(api.ErrorKindConflict, "the round is paused")
	}
	if err != nil {
		a.h.mu.Unlock()
		return api.RoundRun{}, err
	}
	n := 1
	if r.LastRun != nil {
		n = r.LastRun.Number + 1
	}
	run := api.RoundRun{ID: fmt.Sprintf("%s-run%d", r.ID, n), Number: n, Round: r.ID, Trigger: api.RoundTriggerKindManual,
		State: api.RoundRunStateRunning, Created: "2026-01-01T00:00:00Z"}
	r.LastRun = &run
	a.h.rounds.rounds[r.ID] = r
	a.h.rounds.live[r.ID] = true
	a.h.rounds.runs = append(a.h.rounds.runs, r.ID)
	a.h.mu.Unlock()
	a.h.publish(structure.Event{Type: control.EventRoundRunUpdated, ID: run.ID, Data: run})
	return run, nil
}

func (a roundsAPI) Runs(p api.RoundRunsParams) (api.RoundRuns, error) {
	return api.RoundRuns{Runs: []api.RoundRun{}}, nil
}

type catalogAPI struct{ h *Hab }

var _ control.Catalog = catalogAPI{}

func (c catalogAPI) Get(uint64) api.CatalogGetResult      { return api.CatalogGetResult{} }
func (c catalogAPI) Put(api.PeerHello, api.Catalog) error { return nil }
func (c catalogAPI) List() api.CatalogList {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if c.h.cat == nil {
		return api.CatalogList{Hosts: []api.CatalogEntry{}, Projects: []api.CatalogProjectGroup{}}
	}
	return *c.h.cat
}
