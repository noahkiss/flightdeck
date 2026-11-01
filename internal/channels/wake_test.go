package channels

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// oneWay is a pair with a one-way link: a dials b, and b cannot dial a. b's Wake stands in for
// mesh.changed over a's link: it counts the wake-up and hands it to a's Woken. pulls counts a's
// mail.pull calls that reach b; most is the most that were in flight at once.
type oneWay struct {
	a, b                   *hab
	down                   atomic.Bool // a's calls to b fail
	wakes, pulls, inflight atomic.Int32
	most                   atomic.Int32
	slow                   time.Duration // each pull takes this long
}

// pullCounter wraps a's link to b to count pulls and those in flight.
type pullCounter struct {
	w     *oneWay
	inner linkCaller
}

func (c pullCounter) Call(ctx context.Context, method string, params, result any) error {
	if method == "mail.pull" && !c.w.down.Load() {
		n := c.w.inflight.Add(1)
		defer c.w.inflight.Add(-1)
		for {
			m := c.w.most.Load()
			if n <= m || c.w.most.CompareAndSwap(m, n) {
				break
			}
		}
		c.w.pulls.Add(1)
		time.Sleep(c.w.slow)
	}
	return c.inner.Call(ctx, method, params, result)
}

// newOneWay starts the pair with a mail round of poll on both sides. bob is on a, alice on b.
func newOneWay(t *testing.T, poll, slow time.Duration) *oneWay {
	t.Helper()
	w := &oneWay{slow: slow}
	w.a = newHabWith(t, "dev-a", "hosta", t.TempDir(), newLocal("bob"), func(c *Config) { c.Poll = poll })
	w.b = newHabWith(t, "dev-b", "hostb", t.TempDir(), newLocal("alice"), func(c *Config) {
		c.Poll = poll
		c.Wake = func(dev string) bool {
			if dev != "dev-a" {
				return false
			}
			w.wakes.Add(1)
			w.a.s.Woken("bee")
			return true
		}
	})
	w.a.peers.mu.Lock()
	w.a.peers.list.Peers = []api.Peer{{Label: "bee", DeviceID: "dev-b", State: api.PeerStateOnline}}
	w.a.peers.links = []Link{{Label: "bee", DeviceID: "dev-b", Online: true, Caps: []string{Capability},
		Caller: pullCounter{w: w, inner: linkCaller{from: api.PeerHello{DeviceID: "dev-a", Host: "hosta"},
			to: func() *Service { return w.b.s }, down: &w.down}}}}
	w.a.peers.mu.Unlock()
	// b knows a by its pin and sees its link come in, but holds no link to it.
	w.b.peers.setList([]api.Peer{{Label: "ay", DeviceID: "dev-a", State: api.PeerStateOnline, Inbound: true}})
	return w
}

// TestWakeOneWay: mail for the side that only pulls reaches it at once, not at its next mail
// round (an hour here); a burst is handed over in order with never two pulls in flight, and
// each wake-up after the first follows a pull.
func TestWakeOneWay(t *testing.T) {
	w := newOneWay(t, time.Hour, 0)
	start := time.Now()
	sent := send(t, w.b.s, "alice", "ay/bob", "wake up")
	if sent.State != api.ChannelStateQueued {
		t.Fatalf("send %+v", sent)
	}
	waitFor(t, "the message in bob's pane", func() bool { return len(w.a.local.got("bob")) == 1 })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("delivered after %v; the pull side's round is an hour", d)
	}
	if r := final(t, w.b.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on b: %+v", r)
	}

	var want []string
	want = append(want, Envelope(sent.ID, "bee/alice", "wake up"))
	for i := range 40 {
		text := fmt.Sprintf("burst %d", i)
		want = append(want, Envelope(send(t, w.b.s, "alice", "ay/bob", text).ID, "bee/alice", text))
	}
	waitFor(t, "the burst in bob's pane", func() bool { return len(w.a.local.got("bob")) == len(want) })
	if got := w.a.local.got("bob"); !slices.Equal(got, want) {
		t.Fatalf("delivered %q", got)
	}
	if m := w.most.Load(); m != 1 {
		t.Fatalf("%d pulls in flight at once", m)
	}
	if wakes, pulls := w.wakes.Load(), w.pulls.Load(); wakes > pulls+1 {
		t.Fatalf("%d wake-ups for %d pulls: a wake-up went out with one unanswered", wakes, pulls)
	}
}

// TestWakeSlowPull: wake-ups that come while a slow pull is in flight start no second pull;
// the pull runs once more after it ends, and every message arrives.
func TestWakeSlowPull(t *testing.T) {
	w := newOneWay(t, time.Hour, 30*time.Millisecond)
	var ids []string
	for i := range 20 {
		ids = append(ids, send(t, w.b.s, "alice", "ay/bob", fmt.Sprintf("m%d", i)).ID)
		time.Sleep(5 * time.Millisecond) // the sends overlap the pulls
	}
	waitFor(t, "every message in bob's pane", func() bool { return len(w.a.local.got("bob")) == len(ids) })
	if m := w.most.Load(); m != 1 {
		t.Fatalf("%d pulls in flight at once", m)
	}
}

// TestWakeIdle: an idle pair sends no wake-ups over many mail rounds. Mail the peer cannot pull
// yet wakes it once, not once per message or per round; once the peer pulls, nothing more goes
// out.
func TestWakeIdle(t *testing.T) {
	w := newOneWay(t, 5*time.Millisecond, 0)
	rounds := func(n int32) {
		t.Helper()
		ra, rb := w.a.peers.reads.Load(), w.b.peers.reads.Load()
		waitFor(t, fmt.Sprintf("%d more mail rounds on each side", n), func() bool {
			return w.a.peers.reads.Load() >= ra+n && w.b.peers.reads.Load() >= rb+n
		})
	}
	rounds(40)
	if n := w.wakes.Load(); n != 0 {
		t.Fatalf("an idle pair sent %d wake-ups", n)
	}

	w.down.Store(true) // a's pulls fail: the mail waits on b
	var ids []string
	for i := range 3 {
		ids = append(ids, send(t, w.b.s, "alice", "ay/bob", fmt.Sprintf("m%d", i)).ID)
	}
	waitFor(t, "a wake-up", func() bool { return w.wakes.Load() > 0 })
	rounds(40)
	if n := w.wakes.Load(); n != 1 {
		t.Fatalf("%d wake-ups for mail that waits", n)
	}

	w.down.Store(false)
	for _, id := range ids {
		if r := final(t, w.b.s, id); r.State != api.ChannelStateDelivered {
			t.Fatalf("record %+v", r)
		}
	}
	rounds(40)
	if n := w.wakes.Load(); n != 1 {
		t.Fatalf("%d wake-ups after the mail was handed over", n)
	}
}
