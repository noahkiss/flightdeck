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
	// gate, when set, runs first in each of a's calls to b, with the method.
	gate atomic.Pointer[func(method string)]
	// flushes counts b's Wake calls for dev-c, a device with no link: see flush.
	flushes atomic.Int32
}

// flush returns once b's wake loop has handled every item queued before the call. It sends
// to sea, a peer with no link, twice, and waits for each wake attempt: the second can only be
// taken after the batch that held the first, and so every earlier item, is done.
func (w *oneWay) flush(t *testing.T) {
	t.Helper()
	for range 2 {
		n := w.flushes.Load()
		send(t, w.b.s, "alice", "sea/carol", "flush")
		waitFor(t, "b's wake loop", func() bool {
			w.b.s.mu.Lock()
			defer w.b.s.mu.Unlock()
			return w.flushes.Load() > n && !w.b.s.woken["dev-c"] // the failed wake is undone
		})
	}
}

// pullCounter wraps a's link to b to count pulls and those in flight.
type pullCounter struct {
	w     *oneWay
	inner linkCaller
}

func (c pullCounter) Call(ctx context.Context, method string, params, result any) error {
	if g := c.w.gate.Load(); g != nil {
		(*g)(method)
	}
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
			if dev == "dev-c" {
				w.flushes.Add(1)
				return false
			}
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
	w.b.peers.setList([]api.Peer{{Label: "ay", DeviceID: "dev-a", State: api.PeerStateOnline, Inbound: true},
		{Label: "sea", DeviceID: "dev-c", State: api.PeerStateOffline}})
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

// TestWakeAgainAfterPull: each message sent once the peer has pulled wakes it again, and
// reaches it at once.
func TestWakeAgainAfterPull(t *testing.T) {
	w := newOneWay(t, time.Hour, 0)
	for i := range 3 {
		start := time.Now()
		sent := send(t, w.b.s, "alice", "ay/bob", fmt.Sprintf("m%d", i))
		waitFor(t, "the message in bob's pane", func() bool { return len(w.a.local.got("bob")) == i+1 })
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("m%d delivered after %v", i, d)
		}
		if r := final(t, w.b.s, sent.ID); r.State != api.ChannelStateDelivered {
			t.Fatalf("record %+v", r)
		}
	}
	w.flush(t)
	if n := w.wakes.Load(); n != 3 {
		t.Fatalf("%d wake-ups for 3 messages, each sent after a pull", n)
	}
}

// TestWakeOnceForBurst: while the peer has not pulled, further mail sends no further wake-up.
func TestWakeOnceForBurst(t *testing.T) {
	w := newOneWay(t, time.Hour, 0)
	w.down.Store(true)
	send(t, w.b.s, "alice", "ay/bob", "m0")
	waitFor(t, "a wake-up", func() bool { return w.wakes.Load() == 1 })
	for i := 1; i < 3; i++ {
		send(t, w.b.s, "alice", "ay/bob", fmt.Sprintf("m%d", i))
		w.flush(t)
	}
	if n := w.wakes.Load(); n != 1 {
		t.Fatalf("%d wake-ups before a pull", n)
	}
}

// TestWakeStatus: the delivered state of a message the pull-only side sent is a status item
// in the other side's outbox; it wakes the pull-only side too.
func TestWakeStatus(t *testing.T) {
	w := newOneWay(t, time.Hour, 0)
	start := time.Now()
	sent := send(t, w.a.s, "bob", "bee/alice", "hi")
	if r := final(t, w.a.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on a %+v", r)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("delivered state back after %v", d)
	}
}

// TestWakeAfterFailedExchange: a wake-up that comes while an exchange is in flight, and that
// exchange then fails, is served as soon as the link's backoff ends, not at the next round
// (an hour here).
func TestWakeAfterFailedExchange(t *testing.T) {
	w := newOneWay(t, time.Hour, 0)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var released atomic.Bool
	open := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer open() // a failed assertion must not leave service cleanup waiting on the gate
	gate := func(method string) {
		if method == "mail.push" {
			entered <- struct{}{}
			<-release
		}
	}
	w.gate.Store(&gate)
	send(t, w.a.s, "bob", "bee/alice", "from a") // a's round pushes it, and waits at the gate
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("push did not reach the gate")
	}
	// The push is paused before the caller checks down; startup pulls stay healthy.
	w.down.Store(true)
	w.gate.Store(nil)
	send(t, w.b.s, "alice", "ay/bob", "from b") // the wake-up comes during that exchange
	waitFor(t, "the wake-up held for the running exchange", func() bool {
		w.a.s.mu.Lock()
		defer w.a.s.mu.Unlock()
		return w.a.s.again["bee"]
	})
	open() // the push fails: the link is down
	waitFor(t, "the failed exchange", func() bool {
		w.a.s.mu.Lock()
		defer w.a.s.mu.Unlock()
		return w.a.s.fails["bee"] != nil && !w.a.s.running["bee"]
	})
	failed := time.Now()
	w.down.Store(false)
	waitFor(t, "the message in bob's pane", func() bool { return len(w.a.local.got("bob")) == 1 })
	if d := time.Since(failed); d > 3*time.Second {
		t.Fatalf("pulled %v after the failure; the backoff is 1 s", d)
	}
}

// TestWakeWherePush: a hab that holds a working link to the peer pushes and sends no wake-up;
// once its pushes fail and the link waits out a backoff, it wakes the peer instead.
func TestWakeWherePush(t *testing.T) {
	w := newOneWay(t, time.Hour, 0)
	var downB atomic.Bool
	w.b.peers.mu.Lock()
	w.b.peers.links = []Link{{Label: "ay", DeviceID: "dev-a", Online: true, Caps: []string{Capability},
		Caller: linkCaller{from: api.PeerHello{DeviceID: "dev-b", Host: "hostb"}, to: func() *Service { return w.a.s }, down: &downB}}}
	w.b.peers.mu.Unlock()
	send(t, w.b.s, "alice", "ay/bob", "pushed")
	waitFor(t, "the pushed message in bob's pane", func() bool { return len(w.a.local.got("bob")) == 1 })
	w.flush(t)
	if n := w.wakes.Load(); n != 0 {
		t.Fatalf("%d wake-ups where push works", n)
	}

	downB.Store(true)
	send(t, w.b.s, "alice", "ay/bob", "push fails")
	waitFor(t, "b's link in backoff", func() bool {
		w.b.s.mu.Lock()
		defer w.b.s.mu.Unlock()
		return w.b.s.fails["ay"] != nil
	})
	send(t, w.b.s, "alice", "ay/bob", "wake instead")
	waitFor(t, "a wake-up", func() bool { return w.wakes.Load() > 0 })
	waitFor(t, "both messages in bob's pane", func() bool { return len(w.a.local.got("bob")) == 3 })
}
