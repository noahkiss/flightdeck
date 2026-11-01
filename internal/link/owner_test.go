package link

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/holderclient"
)

// TestLostPaneForgetsOwner: a pane that goes lost loses its size owner, as the hab forgets it
// then. When the pane comes back on a new connection to its holder, its status (and so its
// welcome and resize frames) names no owner, not the one from before the blip.
func TestLostPaneForgetsOwner(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "blip", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(70)
	spawn(t, l, u, "echo hi; exec sleep 1000")
	if err := l.SetOwner(u, "client-a", api.OwnerReasonTake); err != nil {
		t.Fatal(err)
	}
	if st, _ := paneStatus(l, u); st.Owner != "client-a" {
		t.Fatalf("owner before the blip: %q", st.Owner)
	}
	var gone, back atomic.Int32
	defer l.Watch(func(n Notice) {
		if n.Pane.UUID != u {
			return
		}
		switch n.Kind {
		case PaneGone:
			gone.Add(1)
		case PaneAdded:
			if gone.Load() > 0 {
				back.Add(1)
			}
		}
	})()

	// A second client takes the holder: it drops the link's connection, so the pane goes lost.
	c2 := holderclient.New(holderclient.Config{Dir: hdir, Build: "test", Logf: t.Logf})
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := c2.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pane lost", func() bool { return gone.Load() > 0 })
	c2.Close()
	waitFor(t, "the pane back", func() bool { return back.Load() > 0 })
	st, ok := paneStatus(l, u)
	if !ok || st.Lost {
		t.Fatalf("pane after the blip: listed %v, lost %v", ok, st.Lost)
	}
	if st.Owner != "" || st.Reason != api.OwnerReasonDisown {
		t.Errorf("pane back after a blip names owner %q (%s), want none", st.Owner, st.Reason)
	}
}
