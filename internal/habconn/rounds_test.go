package habconn

import (
	"encoding/json"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// Rounds in the copy (rounds.md 13): round.updated by revision, a deleted one removed, and
// round.run_updated setting LastRun for the same run or a newer one only.
func TestRoundsKept(t *testing.T) {
	st := newState()
	st.reset(api.Structure{Rounds: []api.Round{{ID: "r", Revision: 3, RoundSpec: api.RoundSpec{Name: "nightly"},
		LastRun: &api.RoundRun{ID: "run2", Number: 2, Round: "r", State: api.RoundRunStateSucceeded}}}})
	at := func(rev uint64, typ api.EventType, id string, v any) bool {
		b, _ := json.Marshal(v)
		e := api.Event{Type: typ, ID: id, Data: b}
		if rev > 0 {
			e.Revision = &rev
		}
		return st.apply(e)
	}
	if at(3, api.EventRoundUpdated, "r", api.Round{ID: "r", Revision: 3, RoundSpec: api.RoundSpec{Name: "old"}}) {
		t.Fatal("a round.updated at the held revision applied")
	}
	if !at(4, api.EventRoundUpdated, "r", api.Round{ID: "r", Revision: 4, RoundSpec: api.RoundSpec{Name: "nightly", Paused: true}}) {
		t.Fatal("a newer round.updated was ignored")
	}
	if r := st.rounds["r"]; !r.Paused || r.LastRun == nil || r.LastRun.ID != "run2" {
		t.Fatalf("round after update: %+v", r)
	}
	if at(0, api.EventRoundRunUpdated, "run1", api.RoundRun{ID: "run1", Number: 1, Round: "r", State: api.RoundRunStateFailed}) {
		t.Fatal("an older run replaced the last run")
	}
	if !at(0, api.EventRoundRunUpdated, "run3", api.RoundRun{ID: "run3", Number: 3, Round: "r", State: api.RoundRunStateRunning}) {
		t.Fatal("a newer run was ignored")
	}
	if !at(0, api.EventRoundRunUpdated, "run3", api.RoundRun{ID: "run3", Number: 3, Round: "r", State: api.RoundRunStateSucceeded}) {
		t.Fatal("the same run's change was ignored")
	}
	if got := st.structure().Rounds; len(got) != 1 || got[0].LastRun.State != api.RoundRunStateSucceeded {
		t.Fatalf("structure rounds: %+v", got)
	}
	if at(0, api.EventRoundRunUpdated, "x", api.RoundRun{ID: "x", Number: 1, Round: "unknown"}) {
		t.Fatal("a run of an unknown round changed the copy")
	}
	if !at(5, api.EventRoundUpdated, "r", api.Round{ID: "r", Revision: 5, Deleted: true}) || len(st.structure().Rounds) != 0 {
		t.Fatal("a deleted round stayed")
	}
	if at(5, api.EventRoundUpdated, "r", api.Round{ID: "r", Revision: 5, RoundSpec: api.RoundSpec{Name: "late"}}) {
		t.Fatal("a late update after the delete applied")
	}
}
