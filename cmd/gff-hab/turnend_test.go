package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestTurnEnds: a turn ends when the state leaves working or blocked for done or error, and the
// event names the session and its last top-level agent message after the last user message.
// idle and exited end nothing; done with no turn running ends nothing; a transcript with no
// final message yet sends the event without one once the read time is up
// (hab-logic-api.md 3.9).
func TestTurnEnds(t *testing.T) {
	var mu sync.Mutex
	var got []api.TurnEnd
	sent := make(chan struct{}, 8)
	msgs := map[string][]api.ChatMessage{
		"s1": {
			{ID: "m1", Kind: api.ChatKindAgent, Text: "an older turn"},
			{ID: "m2", Kind: api.ChatKindUser, Text: "do it"},
			{ID: "m3", Kind: api.ChatKindAgent, Text: "done"},
			{ID: "m4", Kind: api.ChatKindAgent, Text: "a subagent", Parent: "m3"},
			{ID: "m5", Kind: api.ChatKindTool},
		},
		"s2": {{ID: "n1", Kind: api.ChatKindUser, Text: "hi"}},
	}
	te := newTurnEnds(func(pane string) (string, bool) {
		switch pane {
		case "p1":
			return "s1", true
		case "p2":
			return "s2", true
		}
		return "", false
	}, func(_ context.Context, s string) ([]api.ChatMessage, error) {
		return msgs[s], nil
	}, func(e structure.Event) {
		if e.Type != api.EventSessionTurnEnd {
			t.Errorf("published %s", e.Type)
		}
		d := e.Data.(api.TurnEnd)
		if e.ID != d.Session {
			t.Errorf("event id %s for session %s", e.ID, d.Session)
		}
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		sent <- struct{}{}
	})
	te.readFor, te.poll = 50*time.Millisecond, time.Millisecond
	defer te.Close()
	st := func(pane string, s api.AgentStatus) {
		te.agentState(api.AgentState{Pane: pane, State: s})
	}
	wait := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-sent:
			case <-time.After(5 * time.Second):
				t.Fatalf("turn end %d of %d never came", i+1, n)
			}
		}
	}

	st("p1", api.AgentStatusDone) // no turn ran: nothing
	st("p1", api.AgentStatusIdle)
	st("p1", api.AgentStatusWorking)
	st("p1", api.AgentStatusBlocked)
	st("p1", api.AgentStatusWorking)
	st("p1", api.AgentStatusDone)
	wait(1)
	st("p1", api.AgentStatusDone) // the same turn, reported twice: nothing more
	st("p1", api.AgentStatusWorking)
	st("p1", api.AgentStatusExited) // the program ended mid-turn: no turn end
	st("p1", api.AgentStatusDone)
	st("p2", api.AgentStatusWorking)
	st("p2", api.AgentStatusError) // no final message: the event goes without one
	wait(1)
	st("p9", api.AgentStatusWorking) // a pane with no session: nothing
	st("p9", api.AgentStatusDone)
	te.Close()
	mu.Lock()
	defer mu.Unlock()
	want := []api.TurnEnd{{Session: "s1", FinalMessage: "m3"}, {Session: "s2"}}
	if len(got) != len(want) {
		t.Fatalf("turn ends %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("turn end %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestTurnEndNoReader: with no chat reader open, the turn end goes at once without a final
// message, and the read opens none (hab-logic-api.md 3.9).
func TestTurnEndNoReader(t *testing.T) {
	sent := make(chan api.TurnEnd, 1)
	chats := newChatService(chatConfig{session: func(string) (api.Session, bool) { return api.Session{}, true }})
	defer chats.Close()
	te := newTurnEnds(func(string) (string, bool) { return "s1", true },
		func(ctx context.Context, s string) ([]api.ChatMessage, error) { return chats.openTail(ctx, s, 20) },
		func(e structure.Event) { sent <- e.Data.(api.TurnEnd) })
	te.readFor = time.Hour // a wait would hang the test: there must be none
	defer te.Close()
	te.agentState(api.AgentState{Pane: "p1", State: api.AgentStatusWorking})
	te.agentState(api.AgentState{Pane: "p1", State: api.AgentStatusDone})
	select {
	case got := <-sent:
		if got != (api.TurnEnd{Session: "s1"}) {
			t.Fatalf("turn end %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn end waited for a reader no client opened")
	}
	chats.mu.Lock()
	n := len(chats.readers)
	chats.mu.Unlock()
	if n != 0 {
		t.Fatalf("the read opened %d readers", n)
	}
}

// TestTurnEndLateFinalText: a text followed by a tool call is not taken while the read time
// lasts: the final text written late wins. With no late text, the mid-turn text is the answer at
// the deadline. A text followed only by its own children is taken at once (hab-logic-api.md 3.9).
func TestTurnEndLateFinalText(t *testing.T) {
	early := []api.ChatMessage{
		{ID: "u1", Kind: api.ChatKindUser, Text: "fix it"},
		{ID: "a1", Kind: api.ChatKindAgent, Text: "let me look"},
		{ID: "t1", Kind: api.ChatKindTool},
	}
	cases := []struct {
		name  string
		late  []api.ChatMessage // appended from the third read on
		want  string
		reads func(int) bool // the reads the case allows; nil: any
	}{
		{"late final text", []api.ChatMessage{{ID: "a2", Kind: api.ChatKindAgent, Text: "fixed"}}, "a2", nil},
		{"no late text", nil, "a1", nil},
		{"own children only", []api.ChatMessage{
			{ID: "a2", Kind: api.ChatKindAgent, Text: "fixed"},
			{ID: "c1", Kind: api.ChatKindTool, Parent: "a2"},
			{ID: "c2", Kind: api.ChatKindAgent, Text: "sub", Parent: "c1"},
		}, "a2", func(n int) bool { return n <= 3 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			reads := 0
			sent := make(chan api.TurnEnd, 1)
			te := newTurnEnds(func(string) (string, bool) { return "s1", true },
				func(context.Context, string) ([]api.ChatMessage, error) {
					mu.Lock()
					defer mu.Unlock()
					reads++
					if reads < 3 {
						return early, nil
					}
					return append(append([]api.ChatMessage{}, early...), tc.late...), nil
				},
				func(e structure.Event) { sent <- e.Data.(api.TurnEnd) })
			te.poll = time.Millisecond
			te.readFor = 200 * time.Millisecond
			defer te.Close()
			te.agentState(api.AgentState{Pane: "p1", State: api.AgentStatusWorking})
			te.agentState(api.AgentState{Pane: "p1", State: api.AgentStatusDone})
			select {
			case got := <-sent:
				if got.FinalMessage != tc.want {
					t.Errorf("final message %q, want %q", got.FinalMessage, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no turn end")
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.reads != nil && !tc.reads(reads) {
				t.Errorf("%d reads: a settled text should end the read", reads)
			}
		})
	}
}
