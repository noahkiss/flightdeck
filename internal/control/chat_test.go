package control

import (
	"context"
	"errors"
	"sync"
	"testing"

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
	ev, err := e.API(7)
	if err != nil || ev.Revision != nil {
		t.Errorf("chat.message on the wire: %+v %v", ev, err)
	}
}
