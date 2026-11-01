package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeLocal is a hab's sessions: each delivered text is recorded; busy sessions hold.
type fakeLocal struct {
	mu        sync.Mutex
	sessions  map[string]bool
	busy      map[string]bool
	noState   map[string]bool
	delivered map[string][]string
	changed   chan struct{}
}

func newLocal(names ...string) *fakeLocal {
	l := &fakeLocal{sessions: map[string]bool{}, busy: map[string]bool{}, noState: map[string]bool{},
		delivered: map[string][]string{}, changed: make(chan struct{})}
	for _, n := range names {
		l.sessions[n] = true
	}
	return l
}

func (l *fakeLocal) HasSession(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[name]
}

func (l *fakeLocal) Deliver(_ context.Context, session, text string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case !l.sessions[session]:
		return errors.New("session gone")
	case l.noState[session]:
		return ErrNoState
	case l.busy[session]:
		return fmt.Errorf("%w: working", ErrBusy)
	}
	l.delivered[session] = append(l.delivered[session], text)
	return nil
}

func (l *fakeLocal) Agent(session string) (api.AgentStatus, bool, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.sessions[session] {
		return "", false, nil
	}
	if l.busy[session] {
		return api.AgentStatusWorking, true, l.changed
	}
	return api.AgentStatusIdle, true, l.changed
}

func (l *fakeLocal) setBusy(session string, b bool) {
	l.mu.Lock()
	l.busy[session] = b
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

func (l *fakeLocal) got(session string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.delivered[session]...)
}

// linkCaller is a peer link: calls go to the other service as from its hab.
type linkCaller struct {
	from api.PeerHello
	to   func() *Service
	down *atomic.Bool
}

func (c linkCaller) Call(_ context.Context, method string, params, result any) error {
	if c.down != nil && c.down.Load() {
		return errors.New("link down")
	}
	b, _ := json.Marshal(params)
	var out any
	var err error
	switch method {
	case "mail.push":
		var p api.MailPushParams
		json.Unmarshal(b, &p)
		out, err = c.to().Push(c.from, p)
	case "mail.pull":
		var p api.MailPullParams
		json.Unmarshal(b, &p)
		out, err = c.to().Pull(c.from, p)
	default:
		return fmt.Errorf("no method %s", method)
	}
	if err != nil {
		return err
	}
	b, _ = json.Marshal(out)
	return json.Unmarshal(b, result)
}

type fakePeers struct {
	mu    sync.Mutex
	list  api.PeerList
	links []Link
}

func (p *fakePeers) List() api.PeerList {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.list
}

func (p *fakePeers) Links() []Link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Link(nil), p.links...)
}

// hab is one service with its fakes.
type hab struct {
	s     *Service
	local *fakeLocal
	peers *fakePeers
	cat   api.CatalogList
	cfg   Config
}

func newHab(t *testing.T, self, host, state string, local *fakeLocal) *hab {
	t.Helper()
	h := &hab{local: local, peers: &fakePeers{}}
	h.cfg = Config{Self: self, Host: host, State: state, Peers: h.peers, Local: local,
		Catalog: func() api.CatalogList { return h.cat }, Poll: 20 * time.Millisecond, Retry: 10 * time.Millisecond, Logf: t.Logf}
	h.start(t)
	return h
}

func (h *hab) start(t *testing.T) {
	t.Helper()
	s, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.s = s
	t.Cleanup(s.Close)
}

// link makes a dial from a to b under label, and lists b in a's peer list.
func link(a, b *hab, label string, down *atomic.Bool) {
	a.peers.mu.Lock()
	defer a.peers.mu.Unlock()
	a.peers.list.Peers = append(a.peers.list.Peers, api.Peer{Label: label, DeviceID: b.cfg.Self, State: api.PeerStateOnline})
	a.peers.links = append(a.peers.links, Link{Label: label, DeviceID: b.cfg.Self, Online: true, Caps: []string{Capability},
		Caller: linkCaller{from: api.PeerHello{DeviceID: a.cfg.Self, Host: a.cfg.Host}, to: func() *Service { return b.s }, down: down}})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func final(t *testing.T, s *Service, id string) api.ChannelRecord {
	t.Helper()
	r, err := s.Status(context.Background(), api.ChannelStatusParams{ID: id, WaitMS: 10000})
	if err != nil {
		t.Fatal(err)
	}
	return r.Record
}

func TestLocalSend(t *testing.T) {
	a := newHab(t, "dev-a", "hosta", t.TempDir(), newLocal("alice", "bob"))
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bob", Text: "hi"})
	if err != nil || sent.State != api.ChannelStateHanded || sent.To != "bob" {
		t.Fatalf("send %+v %v", sent, err)
	}
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
	if got := a.local.got("bob"); len(got) != 1 || got[0] != Envelope("alice", "hi") {
		t.Fatalf("delivered %q", got)
	}
	// Bad sends.
	for _, p := range []api.ChannelSendParams{
		{From: "alice", To: "nobody", Text: "x"},
		{From: "ghost", To: "bob", Text: "x"},
		{From: "alice", To: "bob", Text: "  "},
		{From: "alice", To: "bob", Text: strings.Repeat("x", MaxText+1)},
		{From: "alice", To: "nolabel/bob", Text: "x"},
	} {
		if _, err := a.s.Send(context.Background(), p); err == nil {
			t.Errorf("send %+v: no error", p)
		}
	}
	// The own host id as a label reaches this hab.
	if sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "hosta/bob", Text: "x"}); err != nil || sent.To != "bob" {
		t.Fatalf("hosta/bob: %+v %v", sent, err)
	}
}

// TestRemoteSendStatusBack: a message pushed to a peer is delivered there with the sender's
// address as the peer names it, and its delivered state comes back to the sender.
func TestRemoteSendStatusBack(t *testing.T) {
	a := newHab(t, "dev-a", "hosta", t.TempDir(), newLocal("alice"))
	b := newHab(t, "dev-b", "hostb", t.TempDir(), newLocal("bob"))
	link(a, b, "bee", nil)
	link(b, a, "ay", nil)
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bee/bob", Text: "line one\nline two"})
	if err != nil || sent.State != api.ChannelStateQueued || sent.To != "bee/bob" {
		t.Fatalf("send %+v %v", sent, err)
	}
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
	if got := b.local.got("bob"); len(got) != 1 || got[0] != Envelope("ay/alice", "line one\nline two") {
		t.Fatalf("delivered %q", got)
	}
	// An unknown name is refused by the target.
	sent, err = a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bee/nobody", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateRefused || !strings.Contains(r.Reason, "no session named nobody") {
		t.Fatalf("record %+v", r)
	}
}

// TestOfflineRestartOnce: the target is offline; the sender restarts from disk; once the link is
// up the message is delivered exactly once, and a repeated push of the same item is dropped.
func TestOfflineRestartOnce(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	stA := t.TempDir()
	a := newHab(t, "dev-a", "hosta", stA, newLocal("alice"))
	b := newHab(t, "dev-b", "hostb", t.TempDir(), newLocal("bob"))
	link(a, b, "bee", &down)
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bee/bob", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // a few failed rounds
	a.s.Close()
	a.start(t) // from disk
	if r, err := a.s.Status(context.Background(), api.ChannelStatusParams{ID: sent.ID}); err != nil || r.Record.State != api.ChannelStateQueued {
		t.Fatalf("after restart: %+v %v", r, err)
	}
	down.Store(false)
	a.s.Kick()
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
	// The same item again, as after a lost answer.
	msg := api.ChannelMessage{ID: sent.ID, From: api.ChannelEnd{DeviceID: "dev-a", Session: "alice"}, To: api.ChannelEnd{DeviceID: "dev-b", Session: "bob"}, Text: "hello"}
	res, err := b.s.Push(api.PeerHello{DeviceID: "dev-a"}, api.MailPushParams{Items: []api.MailItem{{Kind: api.MailKindMessage, Message: &msg}}})
	if err != nil || len(res.Results) != 1 || res.Results[0].State != api.ChannelStateHanded {
		t.Fatalf("repeat push: %+v %v", res, err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := b.local.got("bob"); len(got) != 1 {
		t.Fatalf("delivered %d times: %q", len(got), got)
	}
}

// TestBusyHolds: a working agent holds the message until it is idle.
func TestBusyHolds(t *testing.T) {
	l := newLocal("alice", "bob")
	l.busy["bob"] = true
	a := newHab(t, "dev-a", "hosta", t.TempDir(), l)
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bob", Text: "later"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if r, _ := a.s.Status(context.Background(), api.ChannelStatusParams{ID: sent.ID}); r.Record.State != api.ChannelStateHanded || len(l.got("bob")) != 0 {
		t.Fatalf("while busy: %+v %q", r.Record, l.got("bob"))
	}
	l.setBusy("bob", false)
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
}

// TestNoStateGrace: a pane with no agent state is held, then refused after the grace.
func TestNoStateGrace(t *testing.T) {
	l := newLocal("alice", "bob")
	l.noState["bob"] = true
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a := &hab{local: l, peers: &fakePeers{}}
	a.cfg = Config{Self: "dev-a", Host: "hosta", Local: l, Retry: 5 * time.Millisecond, Now: clock}
	a.start(t)
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bob", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if r, _ := a.s.Status(context.Background(), api.ChannelStatusParams{ID: sent.ID}); r.Record.State != api.ChannelStateHanded {
		t.Fatalf("in the grace: %+v", r.Record)
	}
	mu.Lock()
	now = now.Add(NoStateGrace)
	mu.Unlock()
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateRefused || !strings.Contains(r.Reason, "agent not running") {
		t.Fatalf("record %+v", r)
	}
}

// TestPullOnly: the target cannot dial the sender's peer; the target pulls its mail over its
// own link, and the delivered state goes back on the next pull.
func TestPullOnly(t *testing.T) {
	a := newHab(t, "dev-a", "hosta", t.TempDir(), newLocal("alice"))
	b := newHab(t, "dev-b", "hostb", t.TempDir(), newLocal("bob"))
	var down atomic.Bool
	down.Store(true)
	link(a, b, "bee", &down) // a lists b but cannot reach it
	link(b, a, "ay", nil)
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bee/bob", Text: "pulled"})
	if err != nil {
		t.Fatal(err)
	}
	if r := final(t, a.s, sent.ID); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
	if got := b.local.got("bob"); len(got) != 1 {
		t.Fatalf("delivered %q", got)
	}
	// a sends status items nowhere: its outbox for b is empty once b pulled.
	waitFor(t, "a's outbox drained", func() bool {
		a.s.mu.Lock()
		defer a.s.mu.Unlock()
		return len(a.s.outbox["dev-b"]) == 0
	})
}

func TestResolveBareNames(t *testing.T) {
	a := newHab(t, "dev-a", "hosta", t.TempDir(), newLocal("alice"))
	a.peers.list.Peers = []api.Peer{{Label: "bee", DeviceID: "dev-b"}, {Label: "sea", DeviceID: "dev-c"}}
	a.cat = api.CatalogList{Hosts: []api.CatalogEntry{
		{Own: true, Catalog: api.Catalog{DeviceID: "dev-a", Agents: []api.CatalogAgent{{Session: "alice"}}}},
		{Label: "bee", Catalog: api.Catalog{DeviceID: "dev-b", Host: "hostb", Agents: []api.CatalogAgent{{Session: "bob"}, {Session: "twin"}}}},
		{Label: "sea", Stale: true, Catalog: api.Catalog{DeviceID: "dev-c", Host: "hostc", Agents: []api.CatalogAgent{{Session: "twin"}}}},
		{Catalog: api.Catalog{DeviceID: "dev-d", Host: "hostd", Agents: []api.CatalogAgent{{Session: "stray"}}}},
	}}
	if tg, err := a.s.resolve("bob"); err != nil || tg.address != "bee/bob" || tg.device != "dev-b" {
		t.Fatalf("bob: %+v %v", tg, err)
	}
	_, err := a.s.resolve("twin")
	if k := kind(err); k != api.ErrorKindConflict || !strings.Contains(err.Error(), "bee/twin, sea/twin") {
		t.Fatalf("twin: %v", err)
	}
	if _, err := a.s.resolve("stray"); kind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "add hostd as a peer") {
		t.Fatalf("stray: %v", err)
	}
	if _, err := a.s.resolve("ghost"); kind(err) != api.ErrorKindNotFound {
		t.Fatalf("ghost: %v", err)
	}
	if got := ClashLabel(a.cat, "dev-a", "twin"); got != "bee" {
		t.Fatalf("clash twin: %q", got)
	}
	if got := ClashLabel(a.cat, "dev-a", "alice"); got != "" {
		t.Fatalf("clash alice: %q", got)
	}
}

func kind(err error) api.ErrorKind {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Data.Kind
	}
	return ""
}

// TestWaitTakesMessage: a waiting agent gets its message from the wait, which counts as
// delivered; nothing reaches its PTY.
func TestWaitTakesMessage(t *testing.T) {
	l := newLocal("alice", "bob")
	l.busy["bob"] = true // waiting in a tool call: working
	a := newHab(t, "dev-a", "hosta", t.TempDir(), l)
	res := make(chan api.ChannelWaitResult, 1)
	go func() {
		r, _ := a.s.Wait(context.Background(), api.ChannelWaitParams{Session: "bob", TimeoutMS: 10000})
		res <- r
	}()
	sent, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bob", Text: "wake"})
	if err != nil {
		t.Fatal(err)
	}
	r := <-res
	if r.Message == nil || r.Message.Text != "wake" || r.Message.From != "alice" || r.Message.ID != sent.ID {
		t.Fatalf("wait %+v", r)
	}
	if rec := final(t, a.s, sent.ID); rec.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", rec)
	}
	if got := l.got("bob"); len(got) != 0 {
		t.Fatalf("also in the PTY: %q", got)
	}
	// Timeout and an agent state wait.
	if r, _ := a.s.Wait(context.Background(), api.ChannelWaitParams{Session: "bob", TimeoutMS: 20}); !r.TimedOut {
		t.Fatalf("no timeout: %+v", r)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		l.setBusy("bob", false)
	}()
	if r, _ := a.s.Wait(context.Background(), api.ChannelWaitParams{Agent: "bob", For: []api.AgentStatus{api.AgentStatusIdle}, TimeoutMS: 10000}); r.Agent == nil || r.Agent.State != api.AgentStatusIdle {
		t.Fatalf("agent wait %+v", r)
	}
}

func TestOutboxFull(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	a := newHab(t, "dev-a", "hosta", "", newLocal("alice"))
	b := newHab(t, "dev-b", "hostb", "", newLocal("bob"))
	link(a, b, "bee", &down)
	for i := range MaxOutbox {
		if _, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bee/bob", Text: "x"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	_, err := a.s.Send(context.Background(), api.ChannelSendParams{From: "alice", To: "bee/bob", Text: "x"})
	if kind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "outbox full for bee") {
		t.Fatalf("over the cap: %v", err)
	}
}

// TestNoForwarding: a pushed message from another hab than the link's, or for another hab, is
// refused.
func TestNoForwarding(t *testing.T) {
	b := newHab(t, "dev-b", "hostb", "", newLocal("bob"))
	items := []api.MailItem{
		{Kind: api.MailKindMessage, Message: &api.ChannelMessage{ID: "1", From: api.ChannelEnd{DeviceID: "dev-c", Session: "x"}, To: api.ChannelEnd{DeviceID: "dev-b", Session: "bob"}, Text: "t"}},
		{Kind: api.MailKindMessage, Message: &api.ChannelMessage{ID: "2", From: api.ChannelEnd{DeviceID: "dev-a", Session: "x"}, To: api.ChannelEnd{DeviceID: "dev-c", Session: "bob"}, Text: "t"}},
		{Kind: "future"},
	}
	res, err := b.s.Push(api.PeerHello{DeviceID: "dev-a"}, api.MailPushParams{Items: items})
	if err != nil || len(res.Results) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, r := range res.Results {
		if r.State != api.ChannelStateRefused {
			t.Fatalf("result %+v", r)
		}
	}
	if _, err := b.s.Push(api.PeerHello{}, api.MailPushParams{}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("no peer: %v", err)
	}
}
