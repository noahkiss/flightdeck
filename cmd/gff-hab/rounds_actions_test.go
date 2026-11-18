package main

// Capcom jobs' action words on rounds (capcom-service.md 9.6, rounds.md 3): only a person
// grants a word; an agent route (a pane's, or Capcom's own) may create a capcom round with no
// words and may remove words, and once a round has words it is the user's: an agent route may
// not rewrite, pause, delete or run it. A person's grant clears created_by.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// capcomCatalog is a catalog whose one host runs the Capcom service, so capcom rounds may be made.
func capcomCatalog() api.CatalogList {
	return api.CatalogList{Hosts: []api.CatalogEntry{{Catalog: api.Catalog{DeviceID: "dev-home",
		Capabilities: []api.CatalogCapability{{Name: capCapcomService}},
		Capcom:       &api.CatalogCapcom{Assignment: 1, Home: "dev-home", Service: "svc-1"}}}}}
}

func TestRoundActionsGrantRules(t *testing.T) {
	state := t.TempDir()
	svc, st := fakeRoundsServiceWith(t, state, structure.NewFakeHolder(), func(c *roundsConfig) {
		c.catalog = capcomCatalog
	})
	if _, err := st.CreateSession(structure.SessionSpec{Name: "agent", Pane: "pane-a", Index: -1, NewPane: true}); err != nil {
		t.Fatal(err)
	}
	h := roundHook(st, svc)
	ctx := context.Background()
	job := func(name string, words ...api.RoundAction) api.RoundSpec {
		return api.RoundSpec{Name: name, Mode: api.RoundModeCapcom, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
			Prompt: "tell me about mail that needs me", Actions: words}
	}
	call := func(pane, method string, p any) error {
		b, _ := json.Marshal(p)
		_, _, err := h(ctx, pane, method, b)
		return err
	}
	get := func(name string) api.Round {
		t.Helper()
		r, err := svc.Get(api.RoundRef{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	update := func(pane string, r api.Round, words ...api.RoundAction) error {
		s := r.RoundSpec
		s.Actions = words
		return call(pane, "round.update", api.RoundUpdateParams{ID: r.ID, Round: s})
	}

	// An agent route never creates a round with words, from a pane or from Capcom's route.
	for _, pane := range []string{"pane-a", capcomRoute, "pane-unfiled"} {
		if err := call(pane, "round.create", api.RoundCreateParams{Round: job("granted-"+strings.ReplaceAll(pane, ":", ""), "hand_off")}); errKind(err) != api.ErrorKindDenied ||
			!strings.Contains(err.Error(), "only the user can let Capcom act") {
			t.Errorf("create with actions from %s: %v", pane, err)
		}
	}
	// An unknown word is invalid, and words are for capcom rounds only.
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: job("bad", "launch_missiles")}, ""); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("unknown word: %v", err)
	}
	fresh := api.RoundSpec{Name: "f", Mode: api.RoundModeFresh, Command: []string{"true"}, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Actions: []api.RoundAction{"hand_off"}}
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: fresh}, ""); errKind(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "capcom rounds only") {
		t.Errorf("words on a fresh round: %v", err)
	}
	// The store's own guard: an agent's create with words never lands.
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: job("store-guard", "hand_off")}, "some-session"); errKind(err) != api.ErrorKindDenied {
		t.Errorf("store guard: %v", err)
	}

	// An agent creates a capcom round with no words, as today.
	if err := call("pane-a", "round.create", api.RoundCreateParams{Round: job("mail")}); err != nil {
		t.Fatal(err)
	}
	r := get("mail")
	if r.CreatedBy == "" {
		t.Fatal("an agent's round has no created_by")
	}
	// It may not grant a word to its own round.
	if err := update("pane-a", r, "hand_off"); errKind(err) != api.ErrorKindDenied {
		t.Errorf("agent grant on its own round: %v", err)
	}
	// A person grants hand_off (and fetch, given out of order and twice): created_by is cleared.
	// A grant names the revision the person read: with none it is refused, and with an old one
	// (the agent changed the prompt since) it is a conflict.
	grant := func(rev uint64) error {
		s := r.RoundSpec
		s.Actions = []api.RoundAction{"fetch", "hand_off", "fetch"}
		_, err := svc.Update(ctx, api.RoundUpdateParams{ID: r.ID, Round: s, Revision: rev})
		return err
	}
	if err := grant(0); errKind(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "names the revision") {
		t.Fatalf("a grant with no revision: %v", err)
	}
	seen := r.Revision
	agentEdit := r.RoundSpec
	agentEdit.Prompt = "start a session for every mail"
	if err := call("pane-a", "round.update", api.RoundUpdateParams{ID: r.ID, Round: agentEdit}); err != nil {
		t.Fatal(err)
	}
	if err := grant(seen); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("a grant on a prompt the person did not see: %v", err)
	}
	r = get("mail")
	if r.Prompt != "start a session for every mail" || len(r.Actions) != 0 {
		t.Fatalf("after the refused grant: %+v", r.RoundSpec)
	}
	r.Prompt = "tell me about mail that needs me"
	if _, err := svc.Update(ctx, api.RoundUpdateParams{ID: r.ID, Round: r.RoundSpec, Revision: r.Revision}); err != nil {
		t.Fatal(err)
	}
	r = get("mail")
	if err := grant(r.Revision); err != nil {
		t.Fatal(err)
	}
	r = get("mail")
	if r.CreatedBy != "" || !slices.Equal(r.Actions, []api.RoundAction{"hand_off", "fetch"}) {
		t.Fatalf("after the grant: created_by %q actions %v", r.CreatedBy, r.Actions)
	}
	// The agent's later prompt edit, pause, delete and run are refused.
	edit := r.RoundSpec
	edit.Prompt = "start a session for every mail"
	if err := call("pane-a", "round.update", api.RoundUpdateParams{ID: r.ID, Round: edit}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("agent prompt edit of a granted job: %v", err)
	}
	for _, m := range []string{"round.pause", "round.delete", "round.run", "round.unpause"} {
		if err := call("pane-a", m, api.RoundRef{ID: r.ID}); errKind(err) != api.ErrorKindDenied {
			t.Errorf("%s of a granted job from its creator's pane: %v", m, err)
		}
	}
	// Removing a word works from any pane, even one that did not create the round, and from
	// Capcom's route. Removing a word while changing the prompt does not.
	both := r.RoundSpec
	both.Prompt, both.Actions = "other", []api.RoundAction{"hand_off"}
	if err := call("pane-other", "round.update", api.RoundUpdateParams{ID: r.ID, Round: both}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("a removal with a prompt edit: %v", err)
	}
	withProject := r.RoundSpec
	withProject.Actions, withProject.ProjectDir = []api.RoundAction{"hand_off"}, "/tmp"
	if err := call("pane-other", "round.update", api.RoundUpdateParams{ID: r.ID, Round: withProject}); err == nil {
		t.Errorf("a removal that names a project was taken")
	}
	if err := update("pane-other", r, "hand_off"); err != nil {
		t.Fatalf("remove a word from another pane: %v", err)
	}
	r = get("mail")
	if !slices.Equal(r.Actions, []api.RoundAction{"hand_off"}) || r.Prompt != "tell me about mail that needs me" {
		t.Fatalf("after the removal: %+v", r.RoundSpec)
	}
	// Swapping a word for another is a grant.
	if err := update(capcomRoute, r, "start_session"); errKind(err) != api.ErrorKindDenied {
		t.Errorf("swap a word from Capcom's route: %v", err)
	}
	if err := update(capcomRoute, r); err != nil {
		t.Fatalf("remove the last word from Capcom's route: %v", err)
	}
	r = get("mail")
	if len(r.Actions) != 0 || r.CreatedBy != "" {
		t.Fatalf("after the last removal: %+v created_by %q", r.Actions, r.CreatedBy)
	}
	// The agent lost the round at the grant: it stays the user's.
	if err := call("pane-a", "round.pause", api.RoundRef{ID: r.ID}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("pause after the creator lost the round: %v", err)
	}
}

// TestRoundActionsConfirmRound: an agent's round on another host (kind round confirmation)
// carries no words: the runner's create over the link would take them as a person's.
func TestRoundActionsConfirmRound(t *testing.T) {
	r := &confirmRunner{host: "host-a"}
	_, err := r.CheckRound(api.ConfirmRound{Host: "peer-b", Round: api.RoundSpec{Name: "j", Mode: api.RoundModeCapcom, Prompt: "p",
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}, Actions: []api.RoundAction{"hand_off"}}})
	if errKind(err) != api.ErrorKindDenied {
		t.Fatalf("kind round with actions: %v", err)
	}
}

// TestRoundActionsNormal: the closed set, in a fixed order, each word once.
func TestRoundActionsNormal(t *testing.T) {
	got, err := rounds.NormalActions([]string{"fetch", "card", "hand_off", "card"})
	if err != nil || !slices.Equal(got, []string{"hand_off", "card", "fetch"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := rounds.NormalActions([]string{"logbook_write"}); err == nil {
		t.Error("logbook_write is no action word")
	}
	if g := rounds.Granted([]string{"hand_off"}, []string{"fetch", "hand_off"}); !slices.Equal(g, []string{"fetch"}) {
		t.Errorf("granted %v", g)
	}
}
