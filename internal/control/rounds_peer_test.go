package control

import (
	"context"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// peerRounds records whether each round.create and round.update came marked as a peer call.
type peerRounds struct {
	Rounds // the other methods are not called
	mu     sync.Mutex
	peer   map[string]bool
}

func (f *peerRounds) Create(ctx context.Context, p api.RoundCreateParams, _ string) (api.RoundCreated, error) {
	f.mu.Lock()
	f.peer["create "+p.Round.Name] = PeerCall(ctx)
	f.mu.Unlock()
	return api.RoundCreated{Round: api.Round{ID: "round-1"}}, nil
}

func (f *peerRounds) Update(ctx context.Context, p api.RoundUpdateParams) (api.Round, error) {
	f.mu.Lock()
	f.peer["update "+p.Round.Name] = PeerCall(ctx)
	f.mu.Unlock()
	return api.Round{ID: p.ID}, nil
}

// TestRoundPeerCall: a round.create or round.update over a peer link (client kind hab) reaches the
// rounds service marked as a peer call, so it never counts as the owner's (capcom-service.md 9.3:
// by_person stays off, and a grant is refused); the same call from the app is not marked.
func TestRoundPeerCall(t *testing.T) {
	path := socketPath(t)
	rs := &peerRounds{peer: map[string]bool{}}
	startHab(t, "", path, func(c *Config) { c.Rounds = rs })

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	app := dial(t, path)
	hello(t, app, "")
	for who, c := range map[string]*Client{"peer": peer, "app": app} {
		var made api.RoundCreated
		if err := c.Call(ctx(t), "round.create", api.RoundCreateParams{Round: api.RoundSpec{Name: who}}, &made); err != nil {
			t.Fatalf("%s create: %v", who, err)
		}
		var res api.RoundResult
		if err := c.Call(ctx(t), "round.update", api.RoundUpdateParams{ID: "round-1", Round: api.RoundSpec{Name: who}}, &res); err != nil {
			t.Fatalf("%s update: %v", who, err)
		}
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for k, want := range map[string]bool{"create peer": true, "update peer": true, "create app": false, "update app": false} {
		if got, ok := rs.peer[k]; !ok || got != want {
			t.Errorf("%s: peer call %v (seen %v), want %v", k, got, ok, want)
		}
	}
}
