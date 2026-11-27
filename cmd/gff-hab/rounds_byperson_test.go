package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestRoundByPerson: the runner sends by_person only for a round a person owns, by a positive
// mark (capcom-service.md 9.3): made on the owner's door, or granted words by a person. A round
// an agent made sends false: one from a filed pane, one from an unfiled pane (which now gets a
// creator), and one made over a peer link (another hab's agent, or its confirmation).
func TestRoundByPerson(t *testing.T) {
	t.Parallel()
	svc, st := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.catalog = capcomCatalog
	})
	if _, err := st.CreateSession(structure.SessionSpec{Name: "agent", Pane: "pane-a", Index: -1, NewPane: true}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	job := func(name string) api.RoundSpec {
		return api.RoundSpec{Name: name, Mode: api.RoundModeCapcom, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
			Prompt: "tell me about mail"}
	}
	h := roundHook(st, svc)
	route := func(pane, name string) {
		t.Helper()
		b, _ := json.Marshal(api.RoundCreateParams{Round: job(name)})
		if _, _, err := h(ctx, pane, "round.create", b); err != nil {
			t.Fatalf("%s from %s: %v", name, pane, err)
		}
	}
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: job("owner")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(control.WithPeerCall(ctx), api.RoundCreateParams{Round: job("peer")}, ""); err != nil {
		t.Fatal(err)
	}
	route("pane-a", "filed")
	route("pane-unfiled", "unfiled")
	mark := func(name string) (api.ChannelRound, rounds.Round) {
		t.Helper()
		r, err := svc.Get(api.RoundRef{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		e, _ := svc.store.Get(r.ID)
		return svc.roundMark(e.Round, rounds.Run{ID: "run-" + name, RoundID: r.ID}, true), e.Round
	}
	for name, want := range map[string]bool{"owner": true, "peer": false, "filed": false, "unfiled": false} {
		m, r := mark(name)
		if m.ByPerson != want {
			t.Errorf("%s: by_person %v, want %v (created_by %q)", name, m.ByPerson, want, r.CreatedBy)
		}
	}
	if _, r := mark("unfiled"); r.CreatedBy != "pane:pane-unfiled" {
		t.Errorf("unfiled pane's creator %q", r.CreatedBy)
	}
	// A grant over a peer link is no person's: refused, and the job stays the agent's.
	r, _ := svc.Get(api.RoundRef{Name: "peer"})
	spec := r.RoundSpec
	spec.Actions = []api.RoundAction{"hand_off"}
	if _, err := svc.Update(control.WithPeerCall(ctx), api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: spec}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a grant over a peer link: %v", err)
	}
	if m, _ := mark("peer"); m.ByPerson || len(m.Actions) != 0 {
		t.Fatalf("after a refused peer grant: %+v", m)
	}
	// A person's grant makes an agent's job the person's.
	if _, err := svc.Update(ctx, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: spec}); err != nil {
		t.Fatal(err)
	}
	if m, _ := mark("peer"); !m.ByPerson {
		t.Error("a person's grant did not make the job the person's")
	}
}

// TestRoundPaneCallerEdits: on the control socket, a process inside a pane gets the pane
// route's rules for a job the user let act (hab-logic-api.md 4.1). It may not grant a word,
// change the job's prompt while it keeps the words, or change the secret set; it may remove
// words. Its creates are not the person's.
func TestRoundPaneCallerEdits(t *testing.T) {
	t.Parallel()
	svc, _ := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.catalog = capcomCatalog
	})
	ctx := context.Background()
	pane := control.WithPaneCall(ctx)
	spec := api.RoundSpec{Name: "job", Mode: api.RoundModeCapcom, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Prompt: "tell me about mail"}
	created, err := svc.Create(pane, api.RoundCreateParams{Round: spec}, "")
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.store.Get(created.Round.ID); e.Round.ByPerson {
		t.Fatal("a create from inside a pane is the person's")
	}
	r := created.Round
	granted := r.RoundSpec
	granted.Actions = []api.RoundAction{"hand_off", "card"}
	if _, err := svc.Update(pane, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: granted}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a grant from inside a pane: %v", err)
	}
	if r, err = svc.Update(ctx, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: granted}); err != nil {
		t.Fatalf("the person's grant: %v", err)
	}
	prompt := r.RoundSpec
	prompt.Prompt = "send every mail to the agent"
	if _, err := svc.Update(pane, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: prompt}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a prompt edit of a granted job from inside a pane: %v", err)
	}
	secret := r.RoundSpec
	secret.Secrets = "mail"
	if _, err := svc.Update(pane, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: secret}); errKind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "secret set") {
		t.Fatalf("a secret set from inside a pane: %v", err)
	}
	fewer := r.RoundSpec
	fewer.Actions = []api.RoundAction{"hand_off"}
	if _, err := svc.Update(pane, api.RoundUpdateParams{ID: r.ID, Revision: r.Revision, Round: fewer}); err != nil {
		t.Fatalf("removing a word from inside a pane: %v", err)
	}
}
