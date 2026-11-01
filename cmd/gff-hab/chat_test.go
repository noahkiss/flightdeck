package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// chatWorld is the state a chatService looks up: sessions, agent states, a Claude config dir.
type chatWorld struct {
	mu       sync.Mutex
	sessions map[string]api.Session
	agents   map[string]api.AgentState
	events   []structure.Event
	dir      string
}

func (w *chatWorld) service(t *testing.T, tweak ...func(*chatConfig)) *chatService {
	cfg := chatConfig{
		session: func(id string) (api.Session, bool) {
			w.mu.Lock()
			defer w.mu.Unlock()
			s, ok := w.sessions[id]
			return s, ok
		},
		agent: func(pane string) (api.AgentState, bool) {
			w.mu.Lock()
			defer w.mu.Unlock()
			a, ok := w.agents[pane]
			return a, ok
		},
		find:    func(native string) (string, error) { return transcript.FindClaude(w.dir, native) },
		publish: func(e structure.Event) { w.mu.Lock(); w.events = append(w.events, e); w.mu.Unlock() },
		every:   10 * time.Millisecond,
	}
	for _, f := range tweak {
		f(&cfg)
	}
	c := newChatService(cfg)
	t.Cleanup(c.Close)
	return c
}

func (w *chatWorld) eventCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.events)
}

func kindOf(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

// TestChatService: a session's transcript is found through its pane's agent state; a client
// pages it and resumes with a cursor; what the harness appends goes out as chat.message events.
// A transcript written after a client paged it empty sends its messages with no new page; one
// that existed at the first page sends none for it.
func TestChatService(t *testing.T) {
	native := "11111111-2222-4333-8444-555555555555"
	w := &chatWorld{
		dir:      t.TempDir(),
		sessions: map[string]api.Session{"s1": {ID: "s1", Pane: "p1"}, "s2": {ID: "s2", Pane: "p2"}, "s3": {ID: "s3"}},
		agents:   map[string]api.AgentState{"p1": {Pane: "p1", Harness: "claude", Session: native}, "p2": {Pane: "p2", Harness: "codex", Session: "x"}},
	}
	c := w.service(t)
	ctx := context.Background()

	for sess, want := range map[string]api.ErrorKind{"nope": api.ErrorKindNotFound, "s2": api.ErrorKindUnavailable, "s3": api.ErrorKindUnavailable} {
		if _, err := c.ChatPage(ctx, api.ChatPageParams{Session: sess}); kindOf(err) != want {
			t.Errorf("%s: %v, want %s", sess, err, want)
		}
	}
	// Not written yet: an empty page.
	if got, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s1"}); err != nil || len(got.Messages) != 0 {
		t.Fatalf("before the file exists: %+v %v", got, err)
	}

	fixture, err := os.ReadFile("../../internal/transcript/testdata/claude-2.1.310.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(fixture, []byte("\n"))
	path := filepath.Join(w.dir, "projects", "-work-demo", native+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Join(lines[:8], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the new transcript's messages as events", func() bool { return w.eventCount() == 3 })
	first, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s1"})
	if err != nil || len(first.Messages) != 3 || first.Messages[0].Session != "s1" || first.Cursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	native4 := "11111111-2222-4333-8444-666666666666"
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), native4+".jsonl"), bytes.Join(lines[:8], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.sessions["s4"] = api.Session{ID: "s4", Pane: "p4"}
	w.agents["p4"] = api.AgentState{Pane: "p4", Harness: "claude", Session: native4}
	w.events = nil
	w.mu.Unlock()
	if got, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s4"}); err != nil || len(got.Messages) != 3 {
		t.Fatalf("s4 page: %+v %v", got, err)
	}
	f4, err := os.OpenFile(filepath.Join(filepath.Dir(path), native4+".jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f4.Write(lines[8]) // the Edit call: once its event arrives, the polls have run
	f4.Close()
	waitUntil(t, "s4's appended message", func() bool { return w.eventCount() > 0 })
	w.mu.Lock()
	if len(w.events) != 1 || w.events[0].Data.(api.ChatMessage).Tool == nil {
		t.Fatalf("an existing transcript sent events for its first read: %+v", w.events)
	}
	w.events = nil
	w.mu.Unlock()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(bytes.Join(lines[8:10], nil)) // the Edit call and its result
	f.Close()
	waitUntil(t, "chat.message events", func() bool { return w.eventCount() >= 2 })
	w.mu.Lock()
	for _, e := range w.events {
		m, ok := e.Data.(api.ChatMessage)
		if e.Type != control.EventChatMessage || !ok || m.Session != "s1" || e.ID != m.ID {
			t.Errorf("event %+v", e)
		}
	}
	w.mu.Unlock()

	next, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s1", After: first.Cursor})
	if err != nil || len(next.Messages) != 2 || next.Messages[0].Tool == nil || next.Messages[0].Tool.Name != "Edit" || next.Messages[1].Event != api.ChatEventPermission {
		t.Fatalf("after the cursor: %+v %v", next, err)
	}
	if _, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s1", After: "bad"}); kindOf(err) != api.ErrorKindConflict {
		t.Errorf("bad cursor: %v", err)
	}
	if _, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s1", Before: "nope"}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("unknown before: %v", err)
	}

	// The agent leaves the pane: the last conversation stays readable.
	w.mu.Lock()
	delete(w.agents, "p1")
	w.mu.Unlock()
	if got, err := c.ChatPage(ctx, api.ChatPageParams{Session: "s1"}); err != nil || len(got.Messages) != 5 {
		t.Fatalf("after the agent left: %+v %v", got, err)
	}
	// The session records go: the poll drops their readers.
	w.mu.Lock()
	delete(w.sessions, "s1")
	delete(w.sessions, "s4")
	w.mu.Unlock()
	waitUntil(t, "reader dropped", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.readers) == 0
	})
}

// TestChatEvict: a reader with no chat.page for the idle time is closed when no subscription
// gets its session's events, or when its agent is gone and the file has stopped growing. One
// with a subscriber and a running agent stays open.
func TestChatEvict(t *testing.T) {
	native := "11111111-2222-4333-8444-555555555555"
	w := &chatWorld{
		dir:      t.TempDir(),
		sessions: map[string]api.Session{"s1": {ID: "s1", Pane: "p1"}, "s2": {ID: "s2", Pane: "p2"}},
		agents: map[string]api.AgentState{"p1": {Pane: "p1", Harness: "claude", Session: native},
			"p2": {Pane: "p2", Harness: "claude", Session: "22222222-2222-4333-8444-555555555555"}},
	}
	path := filepath.Join(w.dir, "projects", "-work-demo", native+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../internal/transcript/testdata/claude-2.1.310.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	now, watched := time.Unix(1000, 0), false
	c := w.service(t, func(cfg *chatConfig) {
		cfg.every = time.Hour // the test runs each pass itself
		cfg.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		cfg.watched = func(string) bool { mu.Lock(); defer mu.Unlock(); return watched }
	})
	set := func(d time.Duration, watch bool) {
		mu.Lock()
		now, watched = now.Add(d), watch
		mu.Unlock()
	}
	open := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.readers)
	}
	page := func(sess string) {
		t.Helper()
		if _, err := c.ChatPage(context.Background(), api.ChatPageParams{Session: sess}); err != nil {
			t.Fatal(err)
		}
	}

	page("s1")
	page("s2") // not written: a reader that waits for the file
	set(chatIdle-time.Second, false)
	if c.pass(); open() != 2 {
		t.Fatalf("closed before the idle time: %d open", open())
	}
	set(2*time.Second, false)
	if c.pass(); open() != 0 {
		t.Fatalf("no subscriber, idle: %d readers still open", open())
	}

	page("s1")
	set(chatIdle+time.Second, true)
	if c.pass(); open() != 1 {
		t.Fatal("closed a reader whose agent runs and whose session has a subscriber")
	}
	w.mu.Lock()
	delete(w.agents, "p1")
	w.mu.Unlock()
	set(chatIdle+time.Second, true)
	if c.pass(); open() != 0 {
		t.Fatal("kept a reader whose agent is gone and whose file stopped growing")
	}
}
