package control

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// fakeLogbook records the writes it gets and the actor of each.
type fakeLogbook struct {
	mu     sync.Mutex
	actors []api.LogbookActor
}

func (f *fakeLogbook) Find(context.Context, api.LogbookFindParams) (api.LogbookRecords, error) {
	return api.LogbookRecords{Records: []api.LogbookRecord{{ID: "task-k3m9q2xz", Type: "task", Title: "t"}}}, nil
}
func (f *fakeLogbook) Get(context.Context, api.LogbookGetParams) (api.LogbookRecordResult, error) {
	return api.LogbookRecordResult{}, nil
}
func (f *fakeLogbook) List(context.Context, api.LogbookListParams) (api.LogbookRecords, error) {
	return api.LogbookRecords{}, nil
}
func (f *fakeLogbook) Ready(context.Context, api.LogbookReadyParams) (api.LogbookRecords, error) {
	return api.LogbookRecords{}, nil
}
func (f *fakeLogbook) History(context.Context, api.LogbookHistoryParams) (api.LogbookHistory, error) {
	return api.LogbookHistory{}, nil
}
func (f *fakeLogbook) Write(_ context.Context, a api.LogbookActor, _ api.LogbookWriteParams) (api.LogbookWritten, error) {
	f.mu.Lock()
	f.actors = append(f.actors, a)
	f.mu.Unlock()
	return api.LogbookWritten{State: api.LogbookWriteStateApplied, IDs: []string{"task-k3m9q2xz"}}, nil
}
func (f *fakeLogbook) Resolve(_ context.Context, a api.LogbookActor, _ api.LogbookResolveParams) (api.LogbookWritten, error) {
	return f.Write(context.Background(), a, api.LogbookWriteParams{})
}
func (f *fakeLogbook) Decide(_ context.Context, a api.LogbookActor, _ api.LogbookDecideParams) (api.LogbookWritten, error) {
	return f.Write(context.Background(), a, api.LogbookWriteParams{})
}
func (f *fakeLogbook) Projects(context.Context) (api.LogbookProjects, error) {
	return api.LogbookProjects{}, nil
}
func (f *fakeLogbook) Status(context.Context) (api.LogbookStatus, error) {
	return api.LogbookStatus{}, nil
}

// TestLogbookPeerWrites: over a peer link the Logbook reads answer and the writes are refused
// with invalid, reaching no store; a client's write and decide go through as a person by its label.
func TestLogbookPeerWrites(t *testing.T) {
	path := socketPath(t)
	lb := &fakeLogbook{}
	startHab(t, "", path, func(c *Config) { c.Logbook = lb })

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	pc := api.Client{Caller: peer}
	if r, err := pc.LogbookFind(ctx(t), api.LogbookFindParams{Words: "t"}); err != nil || len(r.Records) != 1 {
		t.Fatalf("a read over a peer link: %+v %v", r, err)
	}
	_, err := pc.LogbookWrite(ctx(t), api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "create", Type: "task", Title: "t"}}})
	var ce *Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("a write over a peer link: %v", err)
	}
	if _, err := pc.LogbookResolve(ctx(t), api.LogbookResolveParams{ID: "task-k3m9q2xz", Field: "title", Value: api.RawJSON(`"x"`)}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("a resolve over a peer link: %v", err)
	}
	if _, err := pc.LogbookDecide(ctx(t), api.LogbookDecideParams{ID: "proposal-k3m9q2xz", Decision: "approve"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("a decide over a peer link: %v", err)
	}
	lb.mu.Lock()
	n := len(lb.actors)
	lb.mu.Unlock()
	if n != 0 {
		t.Fatalf("the store got %d writes over a peer link", n)
	}

	app := dial(t, path)
	if _, err := app.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "flightdeck", Version: "0", Kind: api.ClientKindApp}, Label: "phone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := (api.Client{Caller: app}).LogbookWrite(ctx(t), api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "create", Type: "task", Title: "t"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := (api.Client{Caller: app}).LogbookDecide(ctx(t), api.LogbookDecideParams{ID: "proposal-k3m9q2xz", Decision: "reject"}); err != nil {
		t.Fatal(err)
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if len(lb.actors) != 2 || lb.actors[0] != (api.LogbookActor{Kind: "person", Device: "phone"}) || lb.actors[1] != lb.actors[0] {
		t.Fatalf("the client's write: %+v", lb.actors)
	}
}
