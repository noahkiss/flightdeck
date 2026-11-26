package control

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// countInput takes every write at once, except to the pane "gone", which does not exist.
type countInput struct{}

func (countInput) QueueInput(_ context.Context, _, pane string, _ []byte) func() error {
	if pane == "gone" {
		return func() error { return Errorf(api.ErrorKindNotFound, "no pane %s", pane) }
	}
	return func() error { return nil }
}

// TestPaneActivity: pane.input sends pane.activity with the typing client's id and label, at
// most once per second per client and pane; another pane or another client is not held back;
// input that fails sends nothing and leaves no throttle entry (spec 7.1). session.turn_end goes to a subscription by its session (spec 3.9).
func TestPaneActivity(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	cfg.Input = countInput{}
	srv := New(cfg)
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv.activityNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	watcher := NewClient(pipeServe(t, srv))
	hello(t, watcher, "")
	if _, err := (api.Client{Caller: watcher}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}
	typist := NewClient(pipeServe(t, srv))
	w, err := typist.Hello(ctx(t), testClient, "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	other := NewClient(pipeServe(t, srv))
	wo := hello(t, other, "")
	in := func(c *Client, pane string) {
		t.Helper()
		if err := c.Call(ctx(t), "pane.input", api.InputParams{Pane: pane, Text: "x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := typist.Call(ctx(t), "pane.input", api.InputParams{Pane: "gone", Text: "x"}, nil); err == nil {
		t.Fatal("input to a missing pane succeeded")
	}
	in(typist, "p1")
	in(typist, "p1") // within the second: no event
	in(typist, "p2")
	in(other, "p1")
	advance(time.Second)
	in(typist, "p1")

	type seen struct{ pane, client, label string }
	want := []seen{{"p1", w.ClientID, "phone"}, {"p2", w.ClientID, "phone"}, {"p1", wo.ClientID, "control-test"}, {"p1", w.ClientID, "phone"}}
	for i, s := range want {
		e := nextEvent(t, watcher)
		var a api.PaneActivity
		json.Unmarshal(e.Data, &a)
		if e.Type != api.EventPaneActivity || e.ID != s.pane || a.Client != s.client || a.Label != s.label || e.Revision != nil {
			t.Fatalf("event %d: %s %s %+v rev %v, want %+v", i, e.Type, e.ID, a, e.Revision, s)
		}
	}
	// A marker after the inputs: had a fifth pane.activity been sent, it would come first.
	srv.Publish(structure.Event{Type: api.EventUsageUpdated, ID: "marker", Data: api.UsageAccount{}})
	if e := nextEvent(t, watcher); e.Type != api.EventUsageUpdated {
		t.Fatalf("an extra event: %s %s", e.Type, e.ID)
	}
	srv.activityMu.Lock()
	n := len(srv.activityAt)
	srv.activityMu.Unlock()
	if n != 3 {
		t.Errorf("%d throttle entries, want 3: the failed input left one", n)
	}
}

// TestTurnEndScope: a session-scoped subscription gets session.turn_end for its session only.
func TestTurnEndScope(t *testing.T) {
	f := newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"s1"}})
	if !f.pass(structure.Event{Type: api.EventSessionTurnEnd, ID: "s1", Data: api.TurnEnd{Session: "s1"}}) {
		t.Error("its own session's turn end did not pass")
	}
	if f.pass(structure.Event{Type: api.EventSessionTurnEnd, ID: "s2", Data: api.TurnEnd{Session: "s2"}}) {
		t.Error("another session's turn end passed")
	}
}
