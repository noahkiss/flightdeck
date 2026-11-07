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

// TestGroupSync (groups.md 3.3, 5.6): a push from the group's home replaces the envelope of
// its members here and holds the pause until the catalog copy carries a later revision; an
// older push changes nothing; a push from another hab, or with another hab's envelope, is
// refused.
func TestGroupSync(t *testing.T) {
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"),
		Holders: structure.NewFakeHolder(structure.HolderPane{ID: "p1"}, structure.HolderPane{ID: "p2"})})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	env := api.GroupEnvelope{Group: "g1", Home: "d-home", Confirmation: "none", Hosts: []string{"d-bee"}, Harnesses: []string{"claude"}, MaxWorkers: 2}
	lead, err := st.CreateSession(structure.SessionSpec{Name: "lead-ui", Pane: "p1", Index: -1,
		Group: &api.SessionGroup{ID: "g1", Role: "lead", Item: "ui"}, Envelope: &env})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateSession(structure.SessionSpec{Name: "lead-x", Pane: "p2", Index: -1,
		Group: &api.SessionGroup{ID: "g2", Role: "lead", Item: "x"}, Envelope: &api.GroupEnvelope{Group: "g2", Home: "d-home", Hosts: []string{"d-bee"}}})
	if err != nil {
		t.Fatal(err)
	}
	cat := api.CatalogList{Hosts: []api.CatalogEntry{{Label: "ay", Catalog: api.Catalog{DeviceID: "d-home",
		Groups: []api.CatalogGroup{{ID: "g1", Home: "d-home", Revision: 4}}}}}}
	syncs := newGroupSyncs()
	s := &groupsService{cfg: groupsConfig{store: st, syncs: syncs, catalog: func() api.CatalogList { return cat }}}
	home := api.PeerHello{DeviceID: "d-home", Host: "ay"}

	amended := env
	amended.Hosts = []string{"d-bee", "d-home"}
	if err := s.Sync(home, api.GroupSyncParams{Group: "g1", Revision: 6, Envelope: &amended, Pause: &api.GroupPause{User: "2026-11-07T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if e, _ := st.Envelope(lead.ID); len(e.Hosts) != 2 {
		t.Fatalf("the member's envelope after the push: %+v", e)
	}
	if e, _ := st.Envelope(other.ID); len(e.Hosts) != 1 {
		t.Fatalf("another group's member changed: %+v", e)
	}
	if !syncs.paused(cat, "g1") {
		t.Fatal("the pushed pause does not hold while the copy is older")
	}
	// An older push changes nothing.
	if err := s.Sync(home, api.GroupSyncParams{Group: "g1", Revision: 5, Envelope: &env}); err != nil {
		t.Fatal(err)
	}
	if e, _ := st.Envelope(lead.ID); len(e.Hosts) != 2 || !syncs.paused(cat, "g1") {
		t.Fatalf("an older push changed the envelope %+v or the pause", e)
	}
	// The copy catches up with a later revision: it holds from then on.
	cat.Hosts[0].Catalog.Groups[0].Revision = 7
	if syncs.paused(cat, "g1") {
		t.Fatal("the copy's later revision (not paused) does not win")
	}
	// A resume pushed before the copy shows it lets messages through.
	cat.Hosts[0].Catalog.Groups[0].Pause = &api.GroupPause{User: "2026-11-07T00:00:00Z"}
	if err := s.Sync(home, api.GroupSyncParams{Group: "g1", Revision: 8, Envelope: &amended}); err != nil {
		t.Fatal(err)
	}
	if syncs.paused(cat, "g1") {
		t.Fatal("the pushed resume does not hold over the older copy")
	}
	// Refusals: another hab, an envelope from another home, a bad id.
	stranger := api.PeerHello{DeviceID: "d-other", Host: "zed"}
	if err := s.Sync(stranger, api.GroupSyncParams{Group: "g1", Revision: 9}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("a push from another hab: %v", err)
	}
	if err := s.Sync(home, api.GroupSyncParams{Group: "g1", Revision: 9, Envelope: &api.GroupEnvelope{Group: "g1", Home: "d-other"}}); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("another home's envelope: %v", err)
	}
	if err := s.Sync(home, api.GroupSyncParams{Group: "G 1", Revision: 9}); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("a bad group id: %v", err)
	}
	// A group the catalog does not list yet: a member here with that home's envelope is enough.
	cat = api.CatalogList{}
	if err := s.Sync(home, api.GroupSyncParams{Group: "g2", Revision: 1, Pause: &api.GroupPause{User: "2026-11-07T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if !syncs.paused(cat, "g2") {
		t.Fatal("a pause for a group the catalog does not list yet")
	}
}
