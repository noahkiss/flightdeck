package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestStandingInstruction: a capcom round the Capcom route makes (a user turn's standing
// instruction, capcom-service.md 9.6, stage A10) has no created_by, so it is the user's: no
// agent route, Capcom's included, edits, pauses or deletes it, and one may remove its words. The
// service hears it for its record card.
func TestStandingInstruction(t *testing.T) {
	t.Parallel()
	svc, st := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.catalog = capcomCatalog
	})
	var heard []string
	svc.capcomJob = func(r api.Round) { heard = append(heard, r.Name) }
	h := roundHook(st, svc)
	ctx := context.Background()
	call := func(pane, method string, p any) error {
		b, _ := json.Marshal(p)
		_, _, err := h(ctx, pane, method, b)
		return err
	}
	spec := api.RoundSpec{Name: "keeper", Mode: api.RoundModeCapcom, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Prompt: "keep api-lead fresh: compact it when its context passes 60%"}
	if err := call(capcomRoute, "round.create", api.RoundCreateParams{Round: spec}); err != nil {
		t.Fatal(err)
	}
	r, err := svc.Get(api.RoundRef{Name: "keeper"})
	if err != nil {
		t.Fatal(err)
	}
	if r.CreatedBy != "" {
		t.Fatalf("created_by %q, want none: the user's job", r.CreatedBy)
	}
	if len(heard) != 1 || heard[0] != "keeper" {
		t.Fatalf("heard %v", heard)
	}
	edit := r.RoundSpec
	edit.Prompt = "something else"
	if err := call(capcomRoute, "round.update", api.RoundUpdateParams{ID: r.ID, Round: edit}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("Capcom's edit of the user's job: %v", err)
	}
	for _, m := range []string{"round.pause", "round.delete"} {
		if err := call(capcomRoute, m, api.RoundRef{ID: r.ID}); errKind(err) != api.ErrorKindDenied {
			t.Errorf("%s from Capcom's route: %v", m, err)
		}
	}
	// The person grants a word; any route may still remove it.
	grant := r.RoundSpec
	grant.Actions = []api.RoundAction{"drive"}
	if _, err := svc.Update(ctx, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: grant}); err != nil {
		t.Fatal(err)
	}
	r, _ = svc.Get(api.RoundRef{Name: "keeper"})
	none := r.RoundSpec
	none.Actions = nil
	if err := call(capcomRoute, "round.update", api.RoundUpdateParams{ID: r.ID, Round: none}); err != nil {
		t.Errorf("removing a word from Capcom's route: %v", err)
	}
}
