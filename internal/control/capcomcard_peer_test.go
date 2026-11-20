package control

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// cardAnswers records the capcom.card.answer calls that reach the Capcom service.
type cardAnswers struct {
	CapcomThread // the other methods are not called
	mu           sync.Mutex
	clients      []string
}

func (f *cardAnswers) CardAnswer(_ context.Context, client string, p api.CapcomCardAnswerParams) (api.CapcomCardAnswered, error) {
	f.mu.Lock()
	f.clients = append(f.clients, client)
	f.mu.Unlock()
	return api.CapcomCardAnswered{Item: api.CapcomItem{ID: p.Item}}, nil
}

// TestCapcomCardAnswerPeer: a card answer over a peer link never reaches the Capcom service, so
// another hab cannot tap a grant card's Grant (capcom-service.md 9.9). The app's answer does, with
// its device label.
func TestCapcomCardAnswerPeer(t *testing.T) {
	path := socketPath(t)
	ct := &cardAnswers{}
	startHab(t, "", path, func(c *Config) { c.CapcomThread = ct })

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	var res api.CapcomCardAnswered
	err := peer.Call(ctx(t), "capcom.card.answer", api.CapcomCardAnswerParams{Item: "c1", Choice: "grant"}, &res)
	var e *Error
	if !errors.As(err, &e) || e.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("over a peer link: %v", err)
	}
	app := dial(t, path)
	hello(t, app, "")
	if err := app.Call(ctx(t), "capcom.card.answer", api.CapcomCardAnswerParams{Item: "c1", Choice: "grant"}, &res); err != nil {
		t.Fatal(err)
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if len(ct.clients) != 1 {
		t.Fatalf("answers that reached the service: %v", ct.clients)
	}
}
