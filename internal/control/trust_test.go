package control

import (
	"context"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

type fakeTrust struct {
	mu    sync.Mutex
	calls []api.TrustAnswerParams
}

func (f *fakeTrust) Answer(_ context.Context, _ string, p api.TrustAnswerParams) (api.TrustAnswered, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	return api.TrustAnswered{Dropped: 1}, nil
}

// TestTrustAnswerRoute: trust.answer reaches Config.Trust from an app, with its choice checked;
// a peer link (client kind hab) is refused, as confirm.answer is: it carries an agent's calls,
// never the owner's answer (app.md 7.10).
func TestTrustAnswerRoute(t *testing.T) {
	path := socketPath(t)
	ft := &fakeTrust{}
	startHab(t, "", path, func(c *Config) { c.Trust = ft })

	app := dial(t, path)
	hello(t, app, "")
	var out api.TrustAnswered
	if err := app.Call(ctx(t), "trust.answer", api.TrustAnswerParams{Pane: "p1", Choice: api.TrustChoiceExit}, &out); err != nil || out.Dropped != 1 {
		t.Fatalf("app: %+v %v", out, err)
	}
	if err := app.Call(ctx(t), "trust.answer", api.TrustAnswerParams{Pane: "p1", Choice: "maybe"}, &out); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("bad choice: %v", err)
	}

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	if err := peer.Call(ctx(t), "trust.answer", api.TrustAnswerParams{Pane: "p1", Choice: api.TrustChoiceTrust}, &out); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("peer link: %v", err)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.calls) != 1 {
		t.Errorf("calls %+v", ft.calls)
	}
}
