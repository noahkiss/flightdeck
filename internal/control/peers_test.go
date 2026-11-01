package control

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// nextEvent returns the next event notification on c, skipping heartbeats.
func nextEvent(t *testing.T, c *Client) api.Event {
	t.Helper()
	for {
		select {
		case n := <-c.Notifications():
			if n.Method != "event" {
				continue
			}
			e, err := n.Event()
			if err != nil {
				t.Fatal(err)
			}
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("no event")
		}
	}
}

// TestNotifyLink: NotifyLink reaches the peer links that the named device dialed, though they
// did not subscribe, with the connection's next seq and no revision; it reaches no other link
// and no subscribed client.
func TestNotifyLink(t *testing.T) {
	path := socketPath(t)
	fp := &fakePeers{links: map[uint64]api.PeerHello{}}
	h := startHab(t, "", path, func(c *Config) { c.Peers = fp })
	link := func(device string) *Client {
		c := dial(t, path)
		if _, err := c.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
			Peer: &api.PeerHello{DeviceID: device, Host: "laptop"}}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	peer, other := link("SHA256:peer"), link("SHA256:other")
	app := dial(t, path)
	if _, err := app.Hello(ctx(t), api.ClientInfo{Name: "app", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := (api.Client{Caller: app}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}

	wake := func(id string) structure.Event {
		return structure.Event{Type: api.EventMeshChanged, ID: id, Data: api.MeshChanged{Reasons: []api.MeshReason{api.MeshReasonMail}}}
	}
	for i, id := range []string{"first", "second"} {
		if n := h.srv.NotifyLink("SHA256:peer", wake(id)); n != 1 {
			t.Fatalf("notified %d links", n)
		}
		e := nextEvent(t, peer)
		if e.Type != api.EventMeshChanged || e.ID != id || e.Seq != uint64(i+1) || e.Revision != nil || string(e.Data) != `{"reasons":["mail"]}` {
			t.Fatalf("event %+v %s", e, e.Data)
		}
	}
	if n := h.srv.NotifyLink("SHA256:nobody", wake("x")); n != 0 {
		t.Fatalf("notified %d links for a device with none", n)
	}
	// The next event each other connection sees is the one meant for it.
	h.srv.NotifyLink("SHA256:other", wake("third"))
	if e := nextEvent(t, other); e.ID != "third" || e.Seq != 1 {
		t.Fatalf("other link: %+v", e)
	}
	h.srv.Publish(structure.Event{Type: EventCatalogUpdated, ID: "dev", Data: api.CatalogUpdated{DeviceID: "dev"}})
	if e := nextEvent(t, app); e.Type != EventCatalogUpdated {
		t.Fatalf("subscriber: %+v", e)
	}
}

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
	// The hello's reply is queued before Linked runs, so the link is awaited, not read once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		live, _ := fp.count()
		if live == 1 {
			break
		}
		if live > 1 || time.Now().After(deadline) {
			t.Fatalf("%d links, want 1", live)
		}
		time.Sleep(5 * time.Millisecond)
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
	deadline = time.Now().Add(5 * time.Second)
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
