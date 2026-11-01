package control

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

type fakePeers struct {
	mu    sync.Mutex
	links map[uint64]api.PeerHello
	ended []uint64
}

func (f *fakePeers) List() api.PeerList {
	return api.PeerList{Peers: []api.Peer{{Label: "desk", State: api.PeerStateOnline}}}
}

func (f *fakePeers) Accept(label string) (api.PeerAccepted, error) {
	if label != "desk" {
		return api.PeerAccepted{}, Errorf(api.ErrorKindNotFound, "no peer %q", label)
	}
	return api.PeerAccepted{Label: label, Old: "SHA256:old", New: "SHA256:new"}, nil
}

func (f *fakePeers) Linked(id uint64, p api.PeerHello) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links[id] = p
}

func (f *fakePeers) Unlinked(id uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.links, id)
	f.ended = append(f.ended, id)
}

func (f *fakePeers) count() (live, ended int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.links), len(f.ended)
}

// TestPeerLink: a hello with client.kind hab and a peer field is reported to Peers for as long
// as the connection lasts; peers.list and peers.accept go to Peers; a hello without the peer
// field, or from another kind of client, is no link.
func TestPeerLink(t *testing.T) {
	path := socketPath(t)
	fp := &fakePeers{links: map[uint64]api.PeerHello{}}
	startHab(t, "", path, func(c *Config) { c.Peers = fp })

	c := dial(t, path)
	if _, err := c.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	other := dial(t, path)
	hello(t, other, "")
	bare := dial(t, path)
	if _, err := bare.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "x", Version: "0", Kind: api.ClientKindHab}}); err != nil {
		t.Fatal(err)
	}
	if live, _ := fp.count(); live != 1 {
		t.Fatalf("%d links, want 1", live)
	}
	fp.mu.Lock()
	for _, p := range fp.links {
		if p.DeviceID != "SHA256:peer" || p.Host != "laptop" {
			t.Fatalf("link %+v", p)
		}
	}
	fp.mu.Unlock()

	l, err := api.Client{Caller: other}.PeersList(ctx(t), api.PeersListParams{})
	if err != nil || len(l.Peers) != 1 || l.Unlisted == nil || l.Problems == nil {
		t.Fatalf("peers.list %+v %v", l, err)
	}
	r, err := api.Client{Caller: other}.PeersAccept(ctx(t), api.PeersAcceptParams{Label: "desk"})
	if err != nil || r.New != "SHA256:new" {
		t.Fatalf("peers.accept %+v %v", r, err)
	}
	_, err = api.Client{Caller: other}.PeersAccept(ctx(t), api.PeersAcceptParams{Label: "nobody"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Fatalf("peers.accept of an unknown label: %v", err)
	}

	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if live, ended := fp.count(); live == 0 && ended == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the link's end was not reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
