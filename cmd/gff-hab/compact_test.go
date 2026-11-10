package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakeCompactAgents is one pane's agent state, changed by the test.
type fakeCompactAgents struct {
	mu sync.Mutex
	st map[string]api.AgentState
	ch map[string]chan struct{}
}

func newFakeCompactAgents() *fakeCompactAgents {
	return &fakeCompactAgents{st: map[string]api.AgentState{}, ch: map[string]chan struct{}{}}
}

func (f *fakeCompactAgents) Get(pane string) (api.AgentState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.st[pane]
	return s, ok
}

func (f *fakeCompactAgents) Changed(pane string) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.ch[pane]
	if c == nil {
		c = make(chan struct{})
		f.ch[pane] = c
	}
	return c
}

func (f *fakeCompactAgents) set(pane string, s api.AgentState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.Pane = pane
	f.st[pane] = s
	if c := f.ch[pane]; c != nil {
		close(c)
		delete(f.ch, pane)
	}
}

// compactRig is a compactService over fakes: the sends and the events it made.
type compactRig struct {
	svc    *compactService
	agents *fakeCompactAgents
	mu     sync.Mutex
	sent   []string
	events []api.Compaction
	evCh   chan api.Compaction
	refuse error // the next send's answer
}

func newCompactRig(t *testing.T) *compactRig {
	r := &compactRig{agents: newFakeCompactAgents(), evCh: make(chan api.Compaction, 64)}
	r.svc = newCompactService(r.agents, func(pane string) (string, bool) {
		if pane == "loose" {
			return "", false
		}
		return "sess-" + pane, true
	}, func(_ context.Context, session, text string, guard func(string) error) error {
		if err := guard(strings.TrimPrefix(session, "sess-")); err != nil {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := r.refuse; err != nil {
			r.refuse = nil
			return err
		}
		r.sent = append(r.sent, session+": "+text)
		return nil
	}, func(e structure.Event) {
		c := e.Data.(api.Compaction)
		if e.Type != api.EventAgentCompact || e.ID != c.ID {
			t.Errorf("event %s %s", e.Type, e.ID)
		}
		r.mu.Lock()
		r.events = append(r.events, c)
		r.mu.Unlock()
		r.evCh <- c
	}, t.Logf)
	// Each test's pending compactions end before the test does: run logs through t.
	t.Cleanup(r.svc.Close)
	r.svc.limits = compactLimits{turn: 5 * time.Second, start: 2 * time.Second, end: 2 * time.Second, prompt: 2 * time.Second, poll: 20 * time.Millisecond}
	return r
}

// next waits for the next event and checks its state.
func (r *compactRig) next(t *testing.T, state string) api.Compaction {
	t.Helper()
	select {
	case c := <-r.evCh:
		if c.State != state {
			t.Fatalf("state %s (%s), want %s", c.State, c.Reason, state)
		}
		return c
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s event", state)
	}
	return api.Compaction{}
}

func (r *compactRig) sends() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func hookEnv(event, payload string) harness.Envelope {
	return harness.Envelope{Harness: "claude", Event: event, T: 1, N: 1, Payload: json.RawMessage(payload)}
}

// TestCompactRefusals: each refusal at the call, with its kind.
func TestCompactRefusals(t *testing.T) {
	r := newCompactRig(t)
	kind := func(err error) api.ErrorKind {
		var ce *control.Error
		if errors.As(err, &ce) {
			return ce.Data.Kind
		}
		return ""
	}
	if _, err := r.svc.Request("p1", api.CompactParams{}); kind(err) != api.ErrorKindUnavailable {
		t.Fatalf("no agent: %v", err)
	}
	r.agents.set("p1", api.AgentState{Harness: "codex", State: api.AgentStatusWorking})
	if _, err := r.svc.Request("p1", api.CompactParams{}); kind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("codex: %v", err)
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusExited})
	if _, err := r.svc.Request("p1", api.CompactParams{}); kind(err) != api.ErrorKindUnavailable {
		t.Fatalf("exited: %v", err)
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission, Request: &api.PermissionRequest{ID: "r1"}})
	if _, err := r.svc.Request("p1", api.CompactParams{}); kind(err) != api.ErrorKindConflict {
		t.Fatalf("open request: %v", err)
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	if _, err := r.svc.Request("p1", api.CompactParams{FollowUp: strings.Repeat("x", maxFollowUp+1)}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("long follow-up: %v", err)
	}
	r.agents.set("loose", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	if _, err := r.svc.Request("loose", api.CompactParams{}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("no session: %v", err)
	}
	c, err := r.svc.Request("p1", api.CompactParams{})
	if err != nil || c.State != compactQueued || c.Session != "sess-p1" || c.FollowUp {
		t.Fatalf("request: %+v %v", c, err)
	}
	if _, err := r.svc.Request("p1", api.CompactParams{}); kind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), c.ID) {
		t.Fatalf("second: %v", err)
	}
	if len(r.sends()) != 0 {
		t.Fatalf("sent while working: %v", r.sends())
	}
}

// TestCompactByHooks: /compact waits for the turn's end; an auto-compaction before it is not
// ours; PreCompact starts it, SessionStart compact ends it, and the follow-up goes in once the
// agent is at its prompt.
func TestCompactByHooks(t *testing.T) {
	r := newCompactRig(t)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	c, err := r.svc.Request("p1", api.CompactParams{FollowUp: "read handoff.md and continue"})
	if err != nil {
		t.Fatal(err)
	}
	r.next(t, compactQueued)
	// An auto-compaction mid-turn: not ours.
	r.svc.Observe("p1", hookEnv("PreCompact", `{"session_id":"n1","trigger":"auto"}`))
	r.svc.Observe("p1", hookEnv("SessionStart", `{"session_id":"n1","source":"compact"}`))
	time.Sleep(100 * time.Millisecond)
	if len(r.sends()) != 0 {
		t.Fatalf("sent mid-turn: %v", r.sends())
	}
	// A busy refusal: tried again.
	r.mu.Lock()
	r.refuse = control.Errorf(api.ErrorKindConflict, "another send")
	r.mu.Unlock()
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusDone})
	r.next(t, compactSent)
	if s := r.sends(); len(s) != 1 || s[0] != "sess-p1: /compact" {
		t.Fatalf("sends %v", s)
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	r.svc.Observe("p1", hookEnv("PreCompact", `{"session_id":"n1","trigger":"manual"}`))
	r.next(t, compactCompacting)
	time.Sleep(50 * time.Millisecond)
	if len(r.sends()) != 1 {
		t.Fatalf("follow-up before the end: %v", r.sends())
	}
	r.svc.Observe("p1", hookEnv("SessionStart", `{"session_id":"n1","source":"compact"}`))
	time.Sleep(50 * time.Millisecond)
	if len(r.sends()) != 1 {
		t.Fatalf("follow-up while working: %v", r.sends())
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	done := r.next(t, compactDone)
	if done.ID != c.ID || !done.FollowUp {
		t.Fatalf("done %+v", done)
	}
	if s := r.sends(); len(s) != 2 || s[1] != "sess-p1: read handoff.md and continue" {
		t.Fatalf("sends %v", s)
	}
	// The pane is free again.
	if _, err := r.svc.Request("p1", api.CompactParams{}); err != nil {
		t.Fatalf("after done: %v", err)
	}
}

// TestCompactByTranscript: with no PreCompact hook, a compact_boundary record the transcript
// gains after /compact starts and ends it; one that was there before does not.
func TestCompactByTranscript(t *testing.T) {
	r := newCompactRig(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	old := `{"type":"system","subtype":"compact_boundary","uuid":"b0"}` + "\n"
	os.WriteFile(path, []byte(old), 0o600)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	r.svc.Observe("p1", hookEnv("Stop", `{"session_id":"n1","transcript_path":"`+path+`"}`))
	if _, err := r.svc.Request("p1", api.CompactParams{}); err != nil {
		t.Fatal(err)
	}
	r.next(t, compactQueued)
	r.next(t, compactSent)
	time.Sleep(100 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","message":{"content":"/compact"}}` + "\n" + `{"type":"system","subtype":"compact_boundary"`)
	time.Sleep(100 * time.Millisecond)
	select {
	case c := <-r.evCh:
		t.Fatalf("a half-written line moved it: %+v", c)
	default:
	}
	f.WriteString(`,"uuid":"b1"}` + "\n")
	f.Close()
	r.next(t, compactCompacting)
	r.next(t, compactDone)
	if s := r.sends(); len(s) != 1 {
		t.Fatalf("sends %v (no follow-up given)", s)
	}
}

// TestCompactWaitsForTyping: while a client types into the pane, neither /compact nor the
// follow-up goes in (a draft would take the paste); once the input stops, each does.
func TestCompactWaitsForTyping(t *testing.T) {
	r := newCompactRig(t)
	var mu sync.Mutex
	typing := true
	r.svc.typing = func(pane string) bool {
		mu.Lock()
		defer mu.Unlock()
		return pane == "p1" && typing
	}
	setTyping := func(v bool) {
		mu.Lock()
		typing = v
		mu.Unlock()
	}
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	if _, err := r.svc.Request("p1", api.CompactParams{FollowUp: "go on"}); err != nil {
		t.Fatal(err)
	}
	r.next(t, compactQueued)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusDone})
	time.Sleep(300 * time.Millisecond)
	if s := r.sends(); len(s) != 0 {
		t.Fatalf("sent while the user typed: %v", s)
	}
	setTyping(false)
	r.next(t, compactSent)
	r.svc.Observe("p1", hookEnv("PreCompact", `{"session_id":"n1","trigger":"manual"}`))
	r.next(t, compactCompacting)
	setTyping(true)
	r.svc.Observe("p1", hookEnv("SessionStart", `{"session_id":"n1","source":"compact"}`))
	time.Sleep(300 * time.Millisecond)
	if s := r.sends(); len(s) != 1 {
		t.Fatalf("follow-up while the user typed: %v", s)
	}
	setTyping(false)
	r.next(t, compactDone)
	if s := r.sends(); len(s) != 2 || s[1] != "sess-p1: go on" {
		t.Fatalf("sends %v", s)
	}
}

// TestCompactFailures: no start in time; the agent leaving the pane.
func TestCompactFailures(t *testing.T) {
	r := newCompactRig(t)
	r.svc.limits.start = 200 * time.Millisecond
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusIdle})
	r.svc.Request("p1", api.CompactParams{FollowUp: "go on"})
	r.next(t, compactQueued)
	r.next(t, compactSent)
	if c := r.next(t, compactFailed); !strings.Contains(c.Reason, "did not start") || !strings.Contains(c.Reason, "follow-up was not sent") {
		t.Fatalf("reason %q", c.Reason)
	}
	r.agents.set("p2", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	r.svc.Request("p2", api.CompactParams{})
	r.next(t, compactQueued)
	r.agents.set("p2", api.AgentState{Harness: "claude", State: api.AgentStatusExited})
	if c := r.next(t, compactFailed); !strings.Contains(c.Reason, "/compact was not sent") {
		t.Fatalf("reason %q", c.Reason)
	}
	if len(r.sends()) != 1 {
		t.Fatalf("sends %v", r.sends())
	}
}

// TestCompactClose: Close ends a pending compaction as failed before it returns, and refuses
// the next request.
func TestCompactClose(t *testing.T) {
	r := newCompactRig(t)
	r.agents.set("p1", api.AgentState{Harness: "claude", State: api.AgentStatusWorking})
	if _, err := r.svc.Request("p1", api.CompactParams{}); err != nil {
		t.Fatal(err)
	}
	r.next(t, compactQueued)
	r.svc.Close()
	select {
	case c := <-r.evCh:
		if c.State != compactFailed || !strings.Contains(c.Reason, "stopping") {
			t.Fatalf("after Close: %+v", c)
		}
	default:
		t.Fatal("Close returned before the compaction ended")
	}
	if _, err := r.svc.Request("p1", api.CompactParams{}); err == nil {
		t.Fatal("a request after Close")
	}
	if len(r.sends()) != 0 {
		t.Fatalf("sends %v", r.sends())
	}
}

// TestNudgeLevels: the first level, each step above it below 100, one note per level, several
// levels at once in one note, a reset by compaction, a new session and a drop under the first.
func TestNudgeLevels(t *testing.T) {
	vals := map[string]string{settings.ContextNudge: "0", settings.ContextNudgeStep: "10", settings.ContextNudgeText: `"at {percent}%"`}
	var mu sync.Mutex
	n := newNudgeService(func(id string) json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		return json.RawMessage(vals[id])
	})
	status := func(pct float64) {
		b, _ := json.Marshal(map[string]any{"session_id": "s1", "context_window": map[string]any{"used_percentage": pct, "context_window_size": 200000}})
		n.Observe("p", harness.Envelope{Harness: "claude", Event: "StatusLine", Payload: b})
	}
	ask := func() string {
		return n.Context(context.Background(), "p", hookEnv("PostToolUse", `{"session_id":"s1"}`))
	}
	status(70)
	if got := ask(); got != "" {
		t.Fatalf("off: %q", got)
	}
	mu.Lock()
	vals[settings.ContextNudge] = "50"
	mu.Unlock()
	steps := []struct {
		pct  float64
		want string
	}{{40, ""}, {50, "at 50%"}, {55, ""}, {59.6, ""}, {60.2, "at 60%"}, {84, "at 84%"}, {89, ""}, {99, "at 99%"}, {100, ""}}
	for _, s := range steps {
		status(s.pct)
		if got := ask(); got != s.want {
			t.Fatalf("at %v: %q, want %q", s.pct, got, s.want)
		}
	}
	// A compaction clears the levels and the reading.
	n.Observe("p", hookEnv("PreCompact", `{"session_id":"s1"}`))
	if got := ask(); got != "" {
		t.Fatalf("after compaction with no reading: %q", got)
	}
	status(52)
	if got := ask(); got != "at 52%" {
		t.Fatalf("after compaction: %q", got)
	}
	// Under the first level, then over it again.
	status(20)
	ask()
	status(51)
	if got := ask(); got != "at 51%" {
		t.Fatalf("after a drop: %q", got)
	}
	// A new session in the pane.
	b, _ := json.Marshal(map[string]any{"session_id": "s2", "context_window": map[string]any{"used_percentage": 53}})
	n.Observe("p", harness.Envelope{Harness: "claude", Event: "StatusLine", Payload: b})
	if got := n.Context(context.Background(), "p", hookEnv("UserPromptSubmit", `{"session_id":"s2"}`)); got != "at 53%" {
		t.Fatalf("new session: %q", got)
	}
	// A subagent's hook and a hook that takes no context get none.
	status(75)
	if got := n.Context(context.Background(), "p", hookEnv("PostToolUse", `{"session_id":"s2","agent_id":"a1"}`)); got != "" {
		t.Fatalf("subagent: %q", got)
	}
	if got := n.Context(context.Background(), "p", hookEnv("Stop", `{"session_id":"s2"}`)); got != "" {
		t.Fatalf("Stop: %q", got)
	}
	if nudgeLevel(95, 50, 25) != 75 || nudgeLevel(49.9, 50, 10) != 0 || nudgeLevel(100, 90, 20) != 90 {
		t.Fatal("nudgeLevel")
	}
}

// TestNudgeTranscript: with no status line, the last main-thread assistant usage over 200,000
// tokens; a compact_boundary after it gives no reading; the default text.
func TestNudgeTranscript(t *testing.T) {
	n := newNudgeService(func(id string) json.RawMessage {
		if id == settings.ContextNudge {
			return json.RawMessage(`60`)
		}
		return nil
	})
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":120000}}}`,
		`{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":190000}}}`,
		`{"type":"user","message":{"content":"x"}}`,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	e := hookEnv("UserPromptSubmit", `{"session_id":"s1","transcript_path":"`+path+`"}`)
	got := n.Context(context.Background(), "p", e)
	if !strings.HasPrefix(got, "Context is at 61%.") || !strings.Contains(got, "gff agent compact") {
		t.Fatalf("note %q", got)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"system","subtype":"compact_boundary"}` + "\n")
	f.Close()
	n.Observe("p", hookEnv("SessionStart", `{"session_id":"s1","source":"compact"}`))
	if got := n.Context(context.Background(), "p", e); got != "" {
		t.Fatalf("after the boundary: %q", got)
	}
	if tokens, ok := transcriptContext(filepath.Join(t.TempDir(), "none")); ok || tokens != 0 {
		t.Fatal("a missing transcript read")
	}
}

// TestJoinContext: the hook server's one context handler asks every part, the Logbook habit
// first, and joins the texts that are due by a blank line; none due is "".
func TestJoinContext(t *testing.T) {
	var asked []string
	part := func(name string, text *string) hook.ContextHandler {
		return func(_ context.Context, pane string, env harness.Envelope) string {
			asked = append(asked, name+" "+pane+" "+env.Event)
			return *text
		}
	}
	habit, nudge := "", ""
	h := joinContext(part("habit", &habit), part("nudge", &nudge))
	ask := func() string { return h(context.Background(), "p", hookEnv("UserPromptSubmit", `{"session_id":"s1"}`)) }
	if got := ask(); got != "" {
		t.Fatalf("none due: %q", got)
	}
	nudge = "Context is at 61%."
	if got := ask(); got != nudge {
		t.Fatalf("nudge alone: %q", got)
	}
	habit = "Record findings with gff lb."
	if got := ask(); got != habit+"\n\n"+nudge {
		t.Fatalf("both: %q", got)
	}
	nudge = ""
	if got := ask(); got != habit {
		t.Fatalf("habit alone: %q", got)
	}
	if len(asked) != 8 || asked[0] != "habit p UserPromptSubmit" || asked[1] != "nudge p UserPromptSubmit" {
		t.Fatalf("asked %v", asked)
	}
}
