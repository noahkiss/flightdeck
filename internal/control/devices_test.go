package control

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestPeerMethodsRefusedFromClients: every peer-link method answers denied to a connection that
// is not a peer link, before its handler runs (spec 3.9, Peer-link methods); over a peer link
// the same call still reaches its handler.
func TestPeerMethodsRefusedFromClients(t *testing.T) {
	path := socketPath(t)
	startHab(t, "", path, func(c *Config) { c.Channels = peerMark{} })
	cl := dial(t, path)
	hello(t, cl, "")
	for _, m := range api.PeerMethods {
		var raw any
		err := cl.Call(ctx(t), m, struct{}{}, &raw)
		var ce *Error
		if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied || !strings.Contains(ce.Data.Hint, "peer link only") {
			t.Errorf("%s from a client: %v, want denied", m, err)
		}
	}
	// A hab-kind client with no peer field is no peer link either.
	h := dial(t, path)
	if _, err := h.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab}}); err != nil {
		t.Fatal(err)
	}
	var ce *Error
	if err := h.Call(ctx(t), "mail.pull", api.MailPullParams{}, &api.MailPullResult{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied {
		t.Errorf("mail.pull from a hab with no peer field: %v", err)
	}

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	if err := peer.Call(ctx(t), "mail.pull", api.MailPullParams{}, &api.MailPullResult{}); err != nil {
		t.Fatalf("mail.pull over a peer link: %v", err)
	}
}

// fakeDevices records the calls the devices.* handler makes.
type fakeDevices struct {
	mu      sync.Mutex
	revoked []string
}

func (f *fakeDevices) List() ([]api.Device, error) {
	return []api.Device{{ID: "SHA256:a", Label: "phone", Kind: "tablet", State: "paired", PairedAt: "2026-01-02T03:04:05Z"}}, nil
}
func (f *fakeDevices) Revoke(q string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, q)
	return nil
}

// TestDevicesMethods: devices.list and devices.revoke reach the device service and carry the
// seq; devices.pair answers unavailable (not built); a peer link is refused list and revoke; a
// hab with no service answers unavailable (transport.md 5, 6.6).
func TestDevicesMethods(t *testing.T) {
	fd := &fakeDevices{}
	path := socketPath(t)
	startHab(t, "", path, func(c *Config) { c.Devices = fd })
	cl := dial(t, path)
	hello(t, cl, "")

	var list api.DeviceList
	if err := cl.Call(ctx(t), "devices.list", api.DevicesListParams{}, &list); err != nil || len(list.Devices) != 1 || list.Devices[0].Kind != "tablet" {
		t.Fatalf("devices.list: %+v %v", list, err)
	}
	var ce *Error
	if err := cl.Call(ctx(t), "devices.pair", api.DevicesPairParams{}, &api.PairingOffer{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("devices.pair: %v, want unavailable: it is not built", err)
	}
	var raw any
	if err := cl.Call(ctx(t), "devices.confirm", struct{}{}, &raw); !errors.As(err, &ce) || ce.Code != codeMethodNotFound {
		t.Errorf("devices.confirm: %v, want method not found", err)
	}
	if err := cl.Call(ctx(t), "devices.revoke", api.DevicesRevokeParams{}, &api.Empty{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Errorf("devices.revoke with no id: %v", err)
	}
	if err := cl.Call(ctx(t), "devices.revoke", api.DevicesRevokeParams{ID: "phone"}, &api.Empty{}); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	if len(fd.revoked) != 1 || fd.revoked[0] != "phone" {
		t.Errorf("the service revoked %v", fd.revoked)
	}
	fd.mu.Unlock()

	peer := dial(t, path)
	if _, err := peer.HelloAs(ctx(t), api.HelloParams{Client: api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"devices.list", "devices.revoke"} {
		var raw any
		if err := peer.Call(ctx(t), m, map[string]string{"id": "phone"}, &raw); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied {
			t.Errorf("%s over a peer link: %v, want denied", m, err)
		}
	}

	path2 := socketPath(t)
	startHab(t, "", path2, nil)
	none := dial(t, path2)
	hello(t, none, "")
	for _, m := range []string{"devices.list", "devices.pair", "devices.revoke"} {
		var raw any
		if err := none.Call(ctx(t), m, map[string]string{"id": "phone"}, &raw); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
			t.Errorf("%s with no device service: %v, want unavailable", m, err)
		}
	}
}
