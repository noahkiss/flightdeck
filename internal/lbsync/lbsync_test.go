package lbsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// hab is one replica with its sync service, in this process. Its links reach other habs' Pull
// and Push through JSON, as a peer link would.
type hab struct {
	name  string
	id    string
	store *logbook.Store
	svc   *Service
	mu    sync.Mutex
	links []Link
	wakes map[string]int // Wake calls by device
	// wake decides what Wake answers and does; default: the device's link took it.
	wake func(device string) bool
}

func (h *hab) Links() []Link {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Link(nil), h.links...)
}

func newHab(t *testing.T, name string, opts ...func(*hab, *Config)) *hab {
	t.Helper()
	h := &hab{name: name, id: "SHA256:" + name, wakes: map[string]int{}}
	path := filepath.Join(t.TempDir(), "logbook.db")
	s, err := logbook.Open(context.Background(), path, logbook.Options{Origin: h.id})
	if err != nil {
		t.Fatal(err)
	}
	h.store = s
	cfg := Config{Self: h.id, Store: s, Path: path, Peers: h, Poll: time.Hour, Watch: 20 * time.Millisecond,
		Wake: func(device string) bool {
			h.mu.Lock()
			h.wakes[device]++
			w := h.wake
			h.mu.Unlock()
			if w != nil {
				return w(device)
			}
			return true
		},
		Logf: t.Logf}
	for _, o := range opts {
		o(h, &cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	t.Cleanup(func() {
		svc.Close()
		_ = s.Close()
	})
	return h
}

// caller is a peer link from one hab to another: each call goes through JSON both ways.
type caller struct {
	from *hab
	to   *hab
	down atomic.Bool
	// before runs before a call is served and may fail it; after runs once it is served,
	// before the answer goes back. Either may block.
	before atomic.Pointer[func(method string) error]
	after  atomic.Pointer[func(method string)]
}

func (c *caller) Call(ctx context.Context, method string, params, result any) error {
	if c.down.Load() {
		return errors.New("link down")
	}
	if f := c.before.Load(); f != nil {
		if err := (*f)(method); err != nil {
			return err
		}
	}
	defer func() {
		if f := c.after.Load(); f != nil {
			(*f)(method)
		}
	}()
	from := api.PeerHello{DeviceID: c.from.id, Host: c.from.name}
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var res any
	switch method {
	case "logbook.pull":
		var p api.LogbookPullParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		res, err = c.to.svc.Pull(ctx, from, p)
	case "logbook.push":
		var p api.LogbookPushParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		res, err = c.to.svc.Push(ctx, from, p)
	default:
		return fmt.Errorf("unexpected method %s", method)
	}
	if err != nil {
		return err
	}
	out, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, result)
}

// dial gives from a link to to, labelled with to's name.
func dial(from, to *hab) *caller {
	c := &caller{from: from, to: to}
	from.mu.Lock()
	from.links = append(from.links, Link{Label: to.name, DeviceID: to.id, Online: true, Caps: []string{Capability}, Caller: c})
	from.mu.Unlock()
	from.svc.Kick()
	return c
}

func write(t *testing.T, h *hab, title string) {
	t.Helper()
	if _, err := h.store.Write(context.Background(), logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": title})}}); err != nil {
		t.Fatal(err)
	}
}

func seqOf(t *testing.T, h *hab, origin string) uint64 {
	t.Helper()
	v, err := h.store.Vector(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v[origin]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 15 s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *hab) wakesOf(device string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wakes[device]
}

// A hab that dials pulls at once, and pushes each write as soon as the file changes: through
// Changed, and through the file check for a write another process made.
func TestDialerPullsAndPushes(t *testing.T) {
	a, b := newHab(t, "a"), newHab(t, "b")
	write(t, b, "on b before the link")
	dial(a, b)
	waitFor(t, "a pulls b's op", func() bool { return seqOf(t, a, b.id) == 1 })
	write(t, a, "on a")
	a.svc.Changed()
	waitFor(t, "b gets a's op", func() bool { return seqOf(t, b, a.id) == 1 })
	// A second writer of a's file (the CLI): only the file check sees it.
	other, err := logbook.Open(context.Background(), a.svc.cfg.Path, logbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Write(context.Background(), logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "by the cli"})}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b gets the cli's op", func() bool { return seqOf(t, b, a.id) == 2 })
	if st := a.svc.LinkStats()["b"]; st.Err != "" {
		t.Fatalf("stats %+v", st)
	}
}

// The dialed hab wakes a hab that pulls from it once per pull: a burst of writes sends one
// wake-up; after the next pull, a write sends another. A device whose link did not take the
// wake-up is told again at the next change. A hab this one pushes to gets none.
func TestWakeOncePerPull(t *testing.T) {
	a, b := newHab(t, "a"), newHab(t, "b")
	dial(a, b)
	waitFor(t, "a's first pull", func() bool {
		b.svc.mu.Lock()
		defer b.svc.mu.Unlock()
		_, ok := b.svc.have[a.id]
		return ok
	})
	for i := range 3 {
		write(t, b, fmt.Sprintf("burst %d", i))
		b.svc.Changed()
	}
	waitFor(t, "one wake-up", func() bool { return b.wakesOf(a.id) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := b.wakesOf(a.id); n != 1 {
		t.Fatalf("%d wake-ups for a burst", n)
	}
	a.svc.Woken("b") // what mesh.changed does on a
	waitFor(t, "a pulls the burst", func() bool { return seqOf(t, a, b.id) == 3 })
	write(t, b, "after the pull")
	b.svc.Changed()
	waitFor(t, "a second wake-up", func() bool { return b.wakesOf(a.id) == 2 })

	// No link from a now: the wake-up is not taken, so the next change tries again.
	b.mu.Lock()
	b.wake = func(string) bool { return false }
	b.mu.Unlock()
	a.svc.Woken("b")
	waitFor(t, "a pulls again", func() bool { return seqOf(t, a, b.id) == 4 })
	// a's link goes down, and its last exchange ends, so a pulls nothing more by itself.
	a.mu.Lock()
	a.links[0].Online = false
	a.mu.Unlock()
	waitFor(t, "a's exchanges end", func() bool {
		a.svc.mu.Lock()
		defer a.svc.mu.Unlock()
		return len(a.svc.running) == 0
	})
	write(t, b, "while a is away")
	b.svc.Changed()
	waitFor(t, "a wake-up not taken", func() bool { return b.wakesOf(a.id) == 3 })
	write(t, b, "again")
	b.svc.Changed()
	waitFor(t, "tried again", func() bool { return b.wakesOf(a.id) == 4 })

	// b dials a too: b pushes, so a gets no wake-up.
	a.mu.Lock()
	a.links[0].Online = true
	a.mu.Unlock()
	dial(b, a)
	waitFor(t, "b pushes to a", func() bool { return seqOf(t, a, b.id) == 6 })
	before := b.wakesOf(a.id)
	write(t, b, "pushed")
	b.svc.Changed()
	waitFor(t, "b pushes again", func() bool { return seqOf(t, a, b.id) == 7 })
	if n := b.wakesOf(a.id); n != before {
		t.Fatalf("%d wake-ups to a hab this one pushes to", n-before)
	}
}

// Ops travel more than one hop: a and c both dial b and never link. a's write reaches b by a's
// push, and b's wake-up makes c pull it (logbook.md 7.2).
func TestMultiHopWake(t *testing.T) {
	a, b, c := newHab(t, "a"), newHab(t, "b"), newHab(t, "c")
	b.wake = func(device string) bool {
		for _, h := range []*hab{a, c} {
			if h.id == device {
				h.svc.Woken("b")
			}
		}
		return true
	}
	dial(a, b)
	dial(c, b)
	waitFor(t, "c's first pull", func() bool {
		b.svc.mu.Lock()
		defer b.svc.mu.Unlock()
		_, ok := b.svc.have[c.id]
		return ok
	})
	write(t, a, "from a")
	a.svc.Changed()
	waitFor(t, "c gets a's op through b", func() bool { return seqOf(t, c, a.id) == 1 })
	write(t, c, "from c")
	c.svc.Changed()
	waitFor(t, "a gets c's op through b", func() bool { return seqOf(t, a, c.id) == 1 })
}

// A link that fails waits out a backoff; a wake-up runs an exchange past it.
func TestBackoffAndWake(t *testing.T) {
	a, b := newHab(t, "a"), newHab(t, "b")
	l := dial(a, b)
	waitFor(t, "first exchange", func() bool { _, ok := a.svc.LinkStats()["b"]; return ok })
	l.down.Store(true)
	write(t, a, "while the link fails")
	a.svc.Changed()
	waitFor(t, "a failed exchange", func() bool { return a.svc.LinkStats()["b"].Err != "" })
	l.down.Store(false)
	a.svc.Woken("b")
	waitFor(t, "b gets the op after the wake-up", func() bool { return seqOf(t, b, a.id) == 1 })
}

// Pull and push serve peer links only, never this hab's own id, and a push of more than one
// page is refused.
func TestServeRefusals(t *testing.T) {
	a := newHab(t, "a")
	ctx := context.Background()
	isInvalid := func(err error) bool {
		var ce *control.Error
		return errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindInvalid
	}
	if _, err := a.svc.Pull(ctx, api.PeerHello{}, api.LogbookPullParams{}); !isInvalid(err) {
		t.Fatalf("no peer: %v", err)
	}
	if _, err := a.svc.Push(ctx, api.PeerHello{DeviceID: a.id}, api.LogbookPushParams{}); !isInvalid(err) || !strings.Contains(err.Error(), "own device id") {
		t.Fatalf("own id: %v", err)
	}
	big := make([][]byte, logbook.PageOps+1)
	for i := range big {
		big[i] = []byte(`{}`)
	}
	if _, err := a.svc.Push(ctx, api.PeerHello{DeviceID: "SHA256:x"}, api.LogbookPushParams{Ops: big}); !isInvalid(err) {
		t.Fatalf("a push over a page: %v", err)
	}
	// A page of ops this replica refuses still answers, with the vector.
	res, err := a.svc.Push(ctx, api.PeerHello{DeviceID: "SHA256:x"}, api.LogbookPushParams{Ops: [][]byte{[]byte(`{"broken"`)}})
	if err != nil || len(res.Vector) != 0 {
		t.Fatalf("a refused op: %+v %v", res, err)
	}
}
