package control

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type fakeChat struct {
	mu  sync.Mutex
	got []api.ChatPageParams
}

func (f *fakeChat) calls() []api.ChatPageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.ChatPageParams(nil), f.got...)
}

func (f *fakeChat) ChatPage(_ context.Context, p api.ChatPageParams) (api.ChatMessages, error) {
	f.mu.Lock()
	f.got = append(f.got, p)
	f.mu.Unlock()
	if p.Session == "gone" {
		return api.ChatMessages{}, Errorf(api.ErrorKindNotFound, "no session")
	}
	return api.ChatMessages{Messages: []api.ChatMessage{{ID: "m1", Session: p.Session, Kind: api.ChatKindUser, At: "t"}}, Cursor: "c1"}, nil
}

// TestChatPage: chat.page runs through Chat and carries the seq; a missing session, or before and
// after together, is invalid; an *Error keeps its kind; with no Chat it answers unavailable.
func TestChatPage(t *testing.T) {
	fc := &fakeChat{}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Chat = fc })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	got, err := cl.ChatPage(ctx(t), api.ChatPageParams{Session: "s1", After: "c0", Limit: 3})
	if calls := fc.calls(); err != nil || len(got.Messages) != 1 || got.Cursor != "c1" || len(calls) != 1 || calls[0].After != "c0" || calls[0].Limit != 3 {
		t.Fatalf("chat.page: %+v %v %+v", got, err, calls)
	}
	var ce *Error
	for _, p := range []api.ChatPageParams{{}, {Session: "s1", Before: "a", After: "b"}} {
		if _, err := cl.ChatPage(ctx(t), p); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
			t.Errorf("%+v: %v", p, err)
		}
	}
	if _, err := cl.ChatPage(ctx(t), api.ChatPageParams{Session: "gone"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("not found from Chat: %v", err)
	}

	h2 := startHab(t, "", socketPath(t), nil)
	c2 := dial(t, h2.path)
	hello(t, c2, "")
	if _, err := (&api.Client{Caller: c2}).ChatPage(ctx(t), api.ChatPageParams{Session: "s1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no Chat: %v", err)
	}
}

// TestChatMessageScope: a chat.message event reaches a subscription that has its session in
// scope, carries no revision, and stays out of other scopes.
func TestChatMessageScope(t *testing.T) {
	e := structure.Event{Type: EventChatMessage, ID: "m1", Data: api.ChatMessage{ID: "m1", Session: "s1"}}
	if !newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"s1"}}).pass(e) {
		t.Error("scope sessions s1 dropped its chat message")
	}
	if newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"s2"}}).pass(e) {
		t.Error("scope sessions s2 got another session's chat message")
	}
	if !newFilter(api.Scope{Kind: api.ScopeKindHost}).pass(e) {
		t.Error("scope host dropped a chat message")
	}
}

// TestChatMessageNoRevision: over a real subscription, a chat.message notification has no
// revision key, while a stored object's event keeps its revision. Watching sees the
// subscription.
func TestChatMessageNoRevision(t *testing.T) {
	srv := New(testConfig(&fakeSource{}))
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	hello(t, c, "")
	if srv.Watching("s1") {
		t.Error("Watching with no subscription")
	}
	if _, err := (api.Client{Caller: c}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}
	if !srv.Watching("s1") {
		t.Error("a host subscription does not count as Watching")
	}
	srv.Publish(structure.Event{Type: EventChatMessage, ID: "m1", Revision: 0, Data: api.ChatMessage{ID: "m1", Session: "s1"}})
	srv.Publish(structure.Event{Type: structure.ProjectUpdated, ID: "p1", Revision: 3, Data: api.Project{ID: "p1"}})
	for _, want := range []struct {
		typ string
		rev bool
	}{{"chat.message", false}, {"project.updated", true}} {
		select {
		case n := <-c.Notifications():
			var params map[string]json.RawMessage
			if err := json.Unmarshal(n.Params, &params); err != nil || n.Method != "event" {
				t.Fatalf("%s %s: %v", n.Method, n.Params, err)
			}
			_, has := params["revision"]
			if string(params["type"]) != `"`+want.typ+`"` || has != want.rev {
				t.Errorf("event %s: revision key present %v, want %v", n.Params, has, want.rev)
			}
		case <-ctx(t).Done():
			t.Fatal("no event")
		}
	}
}

// slowChat holds each chat.page until its context ends, and reports that it ended.
type slowChat struct {
	started chan struct{}
	ended   chan error
}

func (f *slowChat) ChatPage(ctx context.Context, _ api.ChatPageParams) (api.ChatMessages, error) {
	f.started <- struct{}{}
	<-ctx.Done()
	f.ended <- ctx.Err()
	return api.ChatMessages{}, ctx.Err()
}

// TestChatPageOffReadLoop: a slow chat.page holds up no other call on its connection, and it
// stops when the connection ends.
func TestChatPageOffReadLoop(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	sc := &slowChat{started: make(chan struct{}, 1), ended: make(chan error, 1)}
	cfg.Chat, cfg.ActionTimeout = sc, time.Minute
	srv := New(cfg)
	t.Cleanup(srv.Close)
	nc := pipeServe(t, srv)
	c := NewClient(nc)
	hello(t, c, "")
	go c.Call(context.Background(), "chat.page", api.ChatPageParams{Session: "s1"}, nil)
	<-sc.started
	pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Call(pctx, "hab.ping", nil, nil); err != nil {
		t.Fatalf("hab.ping behind a running chat.page: %v", err)
	}
	nc.Close()
	select {
	case err := <-sc.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("chat.page ended with %v, want canceled by the connection", err)
		}
	case <-ctx(t).Done():
		t.Fatal("chat.page kept running after its connection ended")
	}
}
