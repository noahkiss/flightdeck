package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/progstatus"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// oscFixture is a pane's OSC 7501 bytes mixed with its hooks, in arrival order.
type oscFixture struct {
	Note           string `json:"note"`
	HarnessVersion string `json:"harness_version"`
	Steps          []struct {
		T    int64     `json:"t"`
		OSC  string    `json:"osc"`
		Hook *Envelope `json:"hook"`
	} `json:"steps"`
	Expect []struct {
		expected
		Source string `json:"source"`
	} `json:"expect"`
}

// TestOSCFixtures feeds each hand-written mix of OSC 7501 bytes and hooks to one pane, the
// bytes through the same scanner gff-hab runs on pane output, and asserts the exact agent.state
// stream with each change's source. A request id of "*" stands for one gff-hab made.
func TestOSCFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/osc/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var fx oscFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			if fx.HarnessVersion == "" {
				t.Fatal("fixture does not state the harness version its shape follows")
			}
			var events []structure.Event
			s := newTestStore(&events)
			var sc progstatus.Scanner
			for _, step := range fx.Steps {
				if step.Hook != nil {
					s.Hook("pane-1", *step.Hook)
					continue
				}
				sc.Feed([]byte(step.OSC), func(r progstatus.Report) { s.Report("pane-1", r, step.T, true) })
			}
			type got struct {
				expected
				Source string `json:"source"`
			}
			var stream []got
			for _, e := range events {
				st := e.Data.(api.AgentState)
				g := got{project(st), string(st.Source)}
				if g.Request != nil && strings.HasPrefix(g.Request.ID, s.reqPrefix+"-") {
					g.Request.ID = "*"
				}
				stream = append(stream, g)
			}
			want := make([]got, len(fx.Expect))
			for i, e := range fx.Expect {
				want[i] = got(e)
			}
			if !reflect.DeepEqual(stream, want) {
				g, _ := json.MarshalIndent(stream, "", "  ")
				w, _ := json.MarshalIndent(want, "", "  ")
				t.Fatalf("stream:\n%s\nwant:\n%s", g, w)
			}
		})
	}
}

func TestReportNeedsAKnownHarness(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	if s.Report("p", progstatus.Report{State: "working"}, 1, true) {
		t.Fatal("a report with no app set state on a pane with no harness")
	}
	if s.Report("p", progstatus.Report{State: "working", App: "brew"}, 1, true) {
		t.Fatal("a program with no manifest became an agent")
	}
	if !s.Report("p", progstatus.Report{State: "working", App: "claude-code"}, 1, true) {
		t.Fatal("an alias did not resolve")
	}
	if !s.Report("p", progstatus.Report{State: "blocked", Kind: "question"}, 2, true) {
		t.Fatal("a report with no app did not count for the pane's harness")
	}
	if st, _ := s.Get("p"); st.Harness != "claude" || st.Kind != api.BlockedKindQuestion || st.Source != api.StateSourceOsc {
		t.Fatalf("state %+v", st)
	}
}

func TestCheckSend(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("no state: %v", err)
	}
	s.Apply("p", "claude", api.StateSourceHook, 1, Update{Session: "a", Bind: true, State: api.AgentStatusIdle})
	for _, c := range []struct {
		state api.AgentStatus
		want  error
	}{
		{api.AgentStatusWorking, ErrBusy},
		{api.AgentStatusBlocked, ErrBusy},
		{api.AgentStatusIdle, nil},
		{api.AgentStatusDone, nil},
		{api.AgentStatusError, nil},
		{api.AgentStatusExited, ErrNoAgent},
	} {
		s.Apply("p", "claude", api.StateSourceHook, 1, Update{Session: "a", NewTurn: true, State: c.state})
		if _, err := s.CheckSend("p"); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("%s: %v, want %v", c.state, err, c.want)
		}
	}
}

func TestCheckAnswer(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	req := &api.PermissionRequest{ID: "r1", Tool: "Bash"}
	// OSC alone never enables an answer.
	s.Apply("p", "claude", api.StateSourceOsc, 1, Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission})
	if _, err := s.CheckAnswer("p", "r1"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("osc blocked: %v", err)
	}
	s.Apply("p", "claude", api.StateSourceHook, 2, Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission, Request: req})
	if _, err := s.CheckAnswer("p", "r2"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("wrong id: %v", err)
	}
	if _, err := s.CheckAnswer("p", "r1"); err != nil {
		t.Fatalf("open request: %v", err)
	}
	s.Apply("p", "claude", api.StateSourceHook, 3, Update{State: api.AgentStatusWorking})
	if _, err := s.CheckAnswer("p", "r1"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("resolved request: %v", err)
	}
	// A question is not a permission.
	s.Apply("p", "claude", api.StateSourceHook, 4, Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindQuestion})
	if _, err := s.CheckAnswer("p", "r1"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("question: %v", err)
	}
}

func TestMessageWrites(t *testing.T) {
	w, err := MessageWrites("claude", "line one\r\nline two\x1b[201~\x03\tend")
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 2 || string(w[0]) != "\x1b[200~line one\nline two[201~\tend\x1b[201~" || string(w[1]) != "\r" {
		t.Fatalf("writes %q", w)
	}
	if _, err := MessageWrites("claude", " \x1b\n"); err == nil {
		t.Error("an empty message was accepted")
	}
	if _, err := MessageWrites("codex", "hi"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("codex: %v", err)
	}
}

func TestChangedFires(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	ch := s.Changed("p")
	if len(s.Snapshot()) != 0 {
		t.Fatal("Changed made a visible record")
	}
	s.Apply("p", "claude", api.StateSourceHook, 1, Update{State: api.AgentStatusIdle})
	select {
	case <-ch:
	default:
		t.Fatal("Changed did not fire")
	}
}

func TestGate(t *testing.T) {
	off := NewGate(0)
	if _, _, ok := off.Wait(context.Background(), "p", "r"); ok {
		t.Fatal("an off gate held a hook")
	}
	if _, ok := off.Answer("p", "r", Decision{}); ok {
		t.Fatal("an off gate took an answer")
	}
	g := NewGate(time.Minute)
	got := make(chan Decision, 1)
	go func() {
		d, written, ok := g.Wait(context.Background(), "p", "r1")
		if !ok {
			close(got)
			return
		}
		written(errors.New("broken pipe"))
		got <- d
	}()
	// Answer once the hook waits; a wrong request id reaches nothing.
	deadline := time.Now().Add(10 * time.Second)
	var ack <-chan error
	for {
		var ok bool
		if ack, ok = g.Answer("p", "r1", Decision{Allow: true}); ok {
			break
		}
		if _, ok := g.Answer("p", "r2", Decision{}); ok {
			t.Fatal("answered the wrong request")
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook never waited")
		}
		time.Sleep(time.Millisecond)
	}
	if d, ok := <-got; !ok || !d.Allow {
		t.Fatalf("decision %+v %v", d, ok)
	}
	// The hook's write result reaches the answer.
	if err := <-ack; err == nil {
		t.Fatal("a failed write was reported as delivered")
	}
	if g.Settle("p", "r1") != 0 {
		t.Fatal("a delivered decision asked keys to wait")
	}
	// A hook that gives up returns no decision, and keys to its request wait for the dialog.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, ok := g.Wait(ctx, "p", "r3"); ok {
		t.Fatal("a cancelled wait returned a decision")
	}
	if d := g.Settle("p", "r3"); d <= 0 || d > DialogSettle {
		t.Fatalf("settle after a give-up: %v", d)
	}
	// Release ends a pane's waits with no decision.
	h := g.Hold("p", "r4")
	g.Release("p")
	if _, _, ok := h.Wait(context.Background()); ok || g.Waiting("p", "r4") {
		t.Fatal("a released wait returned a decision")
	}
}

func claudeHook(event string, t int64, payload string) Envelope {
	return Envelope{Harness: "claude", Event: event, T: t, N: 1, Payload: json.RawMessage(payload)}
}

// TestHookReportsOpened: Hook names the permission request its envelope opened, and none for a
// nested session's request, which binding drops (the hook server holds only the former).
func TestHookReportsOpened(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Hook("p", claudeHook("SessionStart", 1, `{"session_id":"a","hook_event_name":"SessionStart"}`))
	_, opened := s.Hook("p", claudeHook("PermissionRequest", 2, `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`))
	st, _ := s.Get("p")
	if opened == "" || st.Request == nil || opened != st.Request.ID {
		t.Fatalf("opened %q, state %+v", opened, st)
	}
	if _, nested := s.Hook("p", claudeHook("PermissionRequest", 3, `{"session_id":"b","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf x"}}`)); nested != "" {
		t.Fatalf("a dropped nested request reported %q as opened", nested)
	}
	if st2, _ := s.Get("p"); st2.Request == nil || st2.Request.ID != opened {
		t.Fatalf("the nested request moved the pane: %+v", st2)
	}
}

// TestOSCIdleKeepsPermission: an OSC idle never ends a permission request a hook reported, alone
// or after an OSC blocked, however late, so session.send stays refused until a hook closes it.
// For an answer's read-back, an OSC idle after the answer reads as the dialog's Esc.
func TestOSCIdleKeepsPermission(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	idle := progstatus.Report{State: "idle", App: "claude"}
	s.Hook("p", claudeHook("UserPromptSubmit", 1000, `{"session_id":"a","hook_event_name":"UserPromptSubmit","prompt_id":"t1"}`))
	_, req := s.Hook("p", claudeHook("PermissionRequest", 1100, `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`))
	mark := s.OSCMark("p")
	if o := s.Outcome("p", req, mark); o != OutcomeOpen {
		t.Fatalf("outcome %v, want open", o)
	}
	s.Report("p", idle, 9000, true)
	s.Report("p", progstatus.Report{State: "blocked", Kind: "permission", App: "claude"}, 9100, true)
	s.Report("p", idle, 9200, true)
	if st, _ := s.Get("p"); st.State != api.AgentStatusBlocked || st.Request == nil || st.Request.ID != req {
		t.Fatalf("an OSC idle ended the permission block: %+v", st)
	}
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrBusy) {
		t.Fatalf("send with the request open: %v", err)
	}
	if o := s.Outcome("p", req, mark); o != OutcomeInterrupted {
		t.Fatalf("outcome %v, want interrupted", o)
	}
	s.Hook("p", claudeHook("Stop", 9300, `{"session_id":"a","hook_event_name":"Stop"}`))
	if _, err := s.CheckSend("p"); err != nil {
		t.Fatalf("send after the next hook: %v", err)
	}
}

// claudeTurn binds pane p to Claude session a and opens a permission request in a turn, as its
// hooks report them, and returns the request id.
func claudeTurn(t *testing.T, s *Store) string {
	t.Helper()
	s.Hook("p", claudeHook("SessionStart", 1000, `{"session_id":"a","hook_event_name":"SessionStart"}`))
	s.Hook("p", claudeHook("UserPromptSubmit", 1100, `{"session_id":"a","hook_event_name":"UserPromptSubmit","prompt_id":"t1"}`))
	_, req := s.Hook("p", claudeHook("PermissionRequest", 1200, `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`))
	if req == "" {
		t.Fatal("no request opened")
	}
	return req
}

// TestForgedBlockedThenIdle: a program in the pane forges Claude's OSC blocked, then idle,
// after the grace. Only a hook closes the request a hook opened, so the pane stays blocked and a
// send, whose Enter would approve the live dialog, stays refused.
func TestForgedBlockedThenIdle(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	req := claudeTurn(t, s)
	s.Report("p", progstatus.Report{State: "blocked", Kind: "permission", App: "claude"}, 5000, true)
	s.Report("p", progstatus.Report{State: "idle", App: "claude"}, 5100, true)
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrBusy) {
		t.Fatalf("send after a forged blocked and idle: %v", err)
	}
	if _, err := s.CheckAnswer("p", req); err != nil || !s.Shown("p", req) {
		t.Fatalf("the request was closed by OSC: %v", err)
	}
	s.Hook("p", claudeHook("PostToolUse", 6000, `{"session_id":"a","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"x"}`))
	s.Hook("p", claudeHook("Stop", 6100, `{"session_id":"a","hook_event_name":"Stop"}`))
	if _, err := s.CheckSend("p"); err != nil {
		t.Fatalf("send after Stop: %v", err)
	}
}

// TestNestedSessionStartMidTurn: a session start from another session while the bound session
// is inside a turn (a nested agent) neither closes the open request nor opens the send gate;
// the nested session's hooks stay dropped. Between turns, a new session takes the pane.
func TestNestedSessionStartMidTurn(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	req := claudeTurn(t, s)
	s.Hook("p", claudeHook("SessionStart", 2000, `{"session_id":"b","hook_event_name":"SessionStart"}`))
	s.Hook("p", claudeHook("Stop", 2100, `{"session_id":"b","hook_event_name":"Stop"}`))
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrBusy) {
		t.Fatalf("send after a nested session start: %v", err)
	}
	if _, err := s.CheckAnswer("p", req); err != nil {
		t.Fatalf("the nested session start closed the request: %v", err)
	}
	s.Hook("p", claudeHook("Stop", 3000, `{"session_id":"a","hook_event_name":"Stop"}`))
	s.Hook("p", claudeHook("SessionStart", 3100, `{"session_id":"c","hook_event_name":"SessionStart","source":"clear"}`))
	if st, _ := s.Get("p"); st.Session != "c" {
		t.Fatalf("a session start between turns did not take the pane: %+v", st)
	}
}

// TestForgedClearKeepsAnswer: an OSC clear shows the pane exited, but the request stays open
// for a waiting hook (never for keys) until the session's hooks go on.
func TestForgedClearKeepsAnswer(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	req := claudeTurn(t, s)
	s.Report("p", progstatus.Report{State: "clear", App: "claude"}, 5000, true)
	if st, _ := s.Get("p"); st.State != api.AgentStatusExited {
		t.Fatalf("after clear: %+v", st)
	}
	if _, err := s.CheckAnswer("p", req); err != nil || s.Shown("p", req) {
		t.Fatalf("after clear: answer %v, shown %v", err, s.Shown("p", req))
	}
	s.Hook("p", claudeHook("PostToolUse", 6000, `{"session_id":"a","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"x"}`))
	s.Hook("p", claudeHook("Stop", 6100, `{"session_id":"a","hook_event_name":"Stop"}`))
	if _, err := s.CheckSend("p"); err != nil {
		t.Fatalf("send after the session's hooks went on: %v", err)
	}
}

// TestBeginAnswer: one answer per request at a time; once one was sent, no other.
func TestBeginAnswer(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	req := claudeTurn(t, s)
	if _, err := s.BeginAnswer("p", req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginAnswer("p", req); !errors.Is(err, ErrAnswered) {
		t.Fatalf("second answer while one is on its way: %v", err)
	}
	s.EndAnswer("p", req, false)
	if _, err := s.BeginAnswer("p", req); err != nil {
		t.Fatalf("answer after one that sent nothing: %v", err)
	}
	s.EndAnswer("p", req, true)
	if _, err := s.BeginAnswer("p", req); !errors.Is(err, ErrAnswered) {
		t.Fatalf("answer after one was sent: %v", err)
	}
}

// TestOutcome: what a permission read-back sees: a hook closes the request (answered); an OSC
// working while it is open (the turn goes on); an exit (gone).
func TestOutcome(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	perm := `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`
	s.Hook("p", claudeHook("SessionStart", 1, `{"session_id":"a","hook_event_name":"SessionStart"}`))
	_, r1 := s.Hook("p", claudeHook("PermissionRequest", 2, perm))
	mark := s.OSCMark("p")
	s.Report("p", progstatus.Report{State: "working", App: "claude"}, 3, true)
	if o := s.Outcome("p", r1, mark); o != OutcomeContinued {
		t.Fatalf("after OSC working: %v", o)
	}
	s.Hook("p", claudeHook("PostToolUse", 4, `{"session_id":"a","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"x"}`))
	if o := s.Outcome("p", r1, mark); o != OutcomeAnswered {
		t.Fatalf("after PostToolUse: %v", o)
	}
	_, r2 := s.Hook("p", claudeHook("PermissionRequest", 5, perm))
	s.Exited("p")
	if o := s.Outcome("p", r2, s.OSCMark("p")); o != OutcomeGone {
		t.Fatalf("after the exit: %v", o)
	}
}

// TestClearExits: OSC 7501 clear from the pane's own harness marks it exited and refuses sends,
// even when SessionEnd was lost; a clear from another program changes nothing.
func TestClearExits(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Hook("p", claudeHook("SessionStart", 1, `{"session_id":"a","hook_event_name":"SessionStart"}`))
	if s.Report("p", progstatus.Report{State: "clear", App: "codex"}, 2, true) {
		t.Fatal("another harness's clear moved the pane")
	}
	if !s.Report("p", progstatus.Report{State: "clear", App: "claude"}, 3, true) {
		t.Fatal("claude's clear changed nothing")
	}
	if st, _ := s.Get("p"); st.State != api.AgentStatusExited {
		t.Fatalf("after clear: %+v", st)
	}
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("send after clear: %v", err)
	}
}

// TestReplayedReportIsScreen: a replayed report applies with source screen, and the next live
// report or hook in the turn overrides it.
func TestReplayedReportIsScreen(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Report("p", progstatus.Report{State: "idle", App: "claude"}, 1, false)
	if st, _ := s.Get("p"); st.State != api.AgentStatusIdle || st.Source != api.StateSourceScreen {
		t.Fatalf("replayed report: %+v", st)
	}
	s.Report("p", progstatus.Report{State: "working", App: "claude"}, 2, true)
	if st, _ := s.Get("p"); st.State != api.AgentStatusWorking || st.Source != api.StateSourceOsc {
		t.Fatalf("live report after it: %+v", st)
	}
}
