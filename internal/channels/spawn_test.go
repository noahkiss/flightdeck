package channels

import (
	"context"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestDeliveringAndSendTo: Delivering hears a seed and a message before each write, and whether
// it was delivered, never a note; SendTo reaches a session on this hab and one on a peer by
// device id, and refuses a device no peer has (harness-adapters.md, Spawn).
func TestDeliveringAndSendTo(t *testing.T) {
	type heard struct {
		session, from string
		seed          bool
		delivered     bool
		before        bool // heard before the write
	}
	var mu sync.Mutex
	var got []heard
	la := newLocal("alice", "rev")
	lb := newLocal("bob")
	a := newHabWith(t, "dev-a", "hosta", t.TempDir(), la, func(c *Config) {
		c.Delivering = func(sessionID string, from api.ChannelEnd, seed bool) func(bool) {
			n := len(la.got("rev"))
			mu.Lock()
			got = append(got, heard{session: sessionID, from: from.DeviceID + "/" + from.Session, seed: seed})
			i := len(got) - 1
			mu.Unlock()
			return func(ok bool) {
				mu.Lock()
				got[i].delivered, got[i].before = ok, len(la.got("rev")) == n+1
				mu.Unlock()
			}
		}
	})
	b := newHab(t, "dev-b", "hostb", t.TempDir(), lb)
	link(a, b, "bee", nil)
	link(b, a, "ay", nil)

	if _, err := a.s.Seed("rev", "hosta/alice", "Review the diff."); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the seed", func() bool { return len(la.got("rev")) == 1 })
	if _, err := a.s.Note("rev", "Confirmation 1: done."); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the note", func() bool { return len(la.got("rev")) == 2 })
	send(t, a.s, "alice", "rev", "And the tests.")
	waitFor(t, "the message", func() bool { return len(la.got("rev")) == 3 })
	waitFor(t, "the answer heard", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2 && got[1].delivered
	})
	mu.Lock()
	if len(got) != 2 || got[0].session != "id-rev" || !got[0].seed || !got[0].delivered || !got[0].before || got[0].from != "dev-a/hosta/alice" ||
		got[1].seed || got[1].from != "dev-a/alice" || !got[1].delivered {
		t.Fatalf("heard %+v", got)
	}
	mu.Unlock()

	// SendTo: this hab by its own device id, then the peer by its device id.
	ctx := context.Background()
	sent, err := a.s.SendTo(ctx, "rev", api.ChannelEnd{DeviceID: "dev-a", Session: "alice"}, "Looks good.")
	if err != nil || sent.To != "alice" {
		t.Fatalf("local SendTo: %+v %v", sent, err)
	}
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered || la.got("alice")[0] != Envelope(sent.ID, "rev", "Looks good.") {
		t.Fatalf("local report: %+v %q", r, la.got("alice"))
	}
	sent, err = a.s.SendTo(ctx, "rev", api.ChannelEnd{DeviceID: "dev-b", Host: "hostb", Session: "bob"}, "Done.")
	if err != nil || sent.To != "bee/bob" {
		t.Fatalf("peer SendTo: %+v %v", sent, err)
	}
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered || lb.got("bob")[0] != Envelope(sent.ID, "ay/rev", "Done.") {
		t.Fatalf("peer report: %+v %q", r, lb.got("bob"))
	}
	if _, err := a.s.SendTo(ctx, "rev", api.ChannelEnd{DeviceID: "dev-x", Host: "hostx", Session: "bob"}, "x"); kind(err) != api.ErrorKindNotFound {
		t.Fatalf("an unknown device: %v", err)
	}
	if _, err := a.s.SendTo(ctx, "ghost", api.ChannelEnd{DeviceID: "dev-a", Session: "alice"}, "x"); kind(err) != api.ErrorKindNotFound {
		t.Fatalf("from no session: %v", err)
	}
	if _, err := a.s.SendTo(ctx, "rev", api.ChannelEnd{DeviceID: "dev-a", Session: "rev"}, "x"); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("to itself: %v", err)
	}
}
