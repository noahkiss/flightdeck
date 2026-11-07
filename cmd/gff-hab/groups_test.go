package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestGroupNonMemberStart (groups.md 3.3): a group start from a session that is not a member
// carries the trust cover but no session.group or envelope; a member's start carries both.
// The refresh admits an unlisted agent only as a worker or reviewer on a plan item, on a plan
// host and in a plan harness, so a session outside the envelope never counts as a worker.
func TestGroupNonMemberStart(t *testing.T) {
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"),
		Holders: structure.NewFakeHolder(structure.HolderPane{ID: "p1"}, structure.HolderPane{ID: "p2"})})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	env := &api.GroupEnvelope{Group: "g1", Home: "d-home", Confirmation: "none", Hosts: []string{"d-home", "d-bee"}, Harnesses: []string{"claude"}, MaxWorkers: 2}
	if _, err := st.CreateSession(structure.SessionSpec{Name: "outsider", Pane: "p1", Index: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(structure.SessionSpec{Name: "lead-a", Pane: "p2", Index: -1,
		Group: &api.SessionGroup{ID: "g1", Role: "lead", Item: "a"}, Envelope: env}); err != nil {
		t.Fatal(err)
	}
	var got []api.SessionCreateParams
	r := &confirmRunner{host: "mac", store: st, local: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
		got = append(got, p)
		return api.SessionCreated{Session: "s-new", Pane: "p-new"}, nil
	}}
	s := api.ConfirmStart{Host: "mac", Folder: "/w", Harness: "claude", Group: "g1", Item: "a", Name: "w1"}
	for _, from := range []string{"outsider", "lead-a"} {
		if _, err := r.RunStart(context.Background(), "0123456789abcdef0123456789abcdef", from, s); err != nil {
			t.Fatal(err)
		}
	}
	if p := got[0]; p.TrustCover == nil || p.Group != nil || p.Envelope != nil {
		t.Fatalf("a non-member's start: cover %+v group %+v envelope %+v", p.TrustCover, p.Group, p.Envelope)
	}
	if p := got[1]; p.Group == nil || p.Group.Role != "worker" || p.Group.Item != "a" || p.Envelope == nil {
		t.Fatalf("a member's start: group %+v envelope %+v", p.Group, p.Envelope)
	}

	g := api.Group{ID: "g1", Items: []api.GroupItem{{ID: "a"}}, Envelope: env}
	worker := api.CatalogAgent{Session: "w1", Harness: "claude", Group: &api.SessionGroup{ID: "g1", Role: "worker", Item: "a"}}
	if !admitMember(g, worker, "d-bee") {
		t.Fatal("a worker inside the envelope is not admitted")
	}
	for name, c := range map[string]struct {
		a   api.CatalogAgent
		dev string
	}{
		"no group":        {api.CatalogAgent{Session: "x", Harness: "claude"}, "d-bee"},
		"a lead":          {api.CatalogAgent{Session: "x", Harness: "claude", Group: &api.SessionGroup{ID: "g1", Role: "lead", Item: "a"}}, "d-bee"},
		"another item":    {api.CatalogAgent{Session: "x", Harness: "claude", Group: &api.SessionGroup{ID: "g1", Role: "worker", Item: "z"}}, "d-bee"},
		"another host":    {worker, "d-other"},
		"another harness": {api.CatalogAgent{Session: "x", Harness: "codex", Group: worker.Group}, "d-bee"},
	} {
		if admitMember(g, c.a, c.dev) {
			t.Errorf("%s: admitted", name)
		}
	}
	if admitMember(api.Group{ID: "g1", Items: g.Items}, worker, "d-bee") {
		t.Error("admitted with no envelope")
	}
}
