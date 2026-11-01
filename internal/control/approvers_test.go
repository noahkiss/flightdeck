package control

import (
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestApprovers: only an app or terminal client subscribed to a scope that holds the pane can
// answer its permission request; a cli client (gff-hab events), a client with no subscription,
// or one scoped elsewhere does not count, so it never makes a blocking hook wait.
func TestApprovers(t *testing.T) {
	h := startHab(t, "", socketPath(t), nil)
	join := func(kind api.ClientKind, scope *api.Scope) {
		c := dial(t, h.path)
		if _, err := c.Hello(ctx(t), api.ClientInfo{Name: "t", Version: "0", Kind: kind}, "", ""); err != nil {
			t.Fatal(err)
		}
		if scope != nil {
			if _, err := (api.Client{Caller: c}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: *scope}); err != nil {
				t.Fatal(err)
			}
		}
	}
	host := &api.Scope{Kind: api.ScopeKindHost}
	join(api.ClientKindCli, host)
	join(api.ClientKindApp, nil)
	join(api.ClientKindApp, &api.Scope{Kind: api.ScopeKindProjects, IDs: []string{"elsewhere"}})
	if n := h.srv.Approvers("pane-1"); n != 0 {
		t.Fatalf("approvers %d, want 0", n)
	}
	join(api.ClientKindTerminal, host)
	if n := h.srv.Approvers("pane-1"); n != 1 {
		t.Fatalf("approvers %d, want 1", n)
	}
}
