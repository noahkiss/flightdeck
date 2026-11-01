package harness

import (
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/progstatus"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestDenyByKeysThenOSCIdle: Claude fires no hook after a deny by keys (Esc), only an OSC idle.
// Once Flightdeck sent the answer, that idle closes the request: the pane is idle, a send goes
// through, and the read-back sees the turn stopped. Before the answer went out, the same idle
// closes nothing.
func TestDenyByKeysThenOSCIdle(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	req := claudeTurn(t, s)
	idle := progstatus.Report{State: "idle", App: "claude"}
	if _, err := s.BeginAnswer("p", req); err != nil {
		t.Fatal(err)
	}
	s.Report("p", idle, 5000, true) // claimed, but no key sent yet
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrBusy) {
		t.Fatalf("an OSC idle before the answer went out opened the send gate: %v", err)
	}
	mark := s.OSCMark("p")
	s.Sent("p", req)
	s.Report("p", idle, 5100, true)
	st, ok := s.Get("p")
	if !ok || st.State != api.AgentStatusIdle || st.Request != nil || st.Source != api.StateSourceOsc {
		t.Fatalf("after the deny and the OSC idle: %+v", st)
	}
	if o := s.Outcome("p", req, mark); o != OutcomeInterrupted {
		t.Fatalf("outcome %v, want interrupted", o)
	}
	s.EndAnswer("p", req, true)
	if _, err := s.CheckSend("p"); err != nil {
		t.Fatalf("send after the deny: %v", err)
	}
	if _, err := s.CheckAnswer("p", req); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("the denied request is still open: %v", err)
	}
}

// TestPromptMark: a shell prompt after OSC working shows the pane exited (sends refused). A
// request nobody answered stays open and blocked; one Flightdeck answered closes. A state a hook
// set stays.
func TestPromptMark(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Report("p", progstatus.Report{State: "working", App: "claude"}, 1000, true)
	if !s.Prompt("p") {
		t.Fatal("a prompt after OSC working changed nothing")
	}
	if st, _ := s.Get("p"); st.State != api.AgentStatusExited {
		t.Fatalf("after the prompt: %+v", st)
	}
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("send after the prompt: %v", err)
	}

	s = newTestStore(&events)
	req := claudeTurn(t, s)
	s.Report("p", progstatus.Report{State: "blocked", Kind: "permission", App: "claude"}, 5000, true)
	if s.Prompt("p") {
		t.Fatal("a prompt moved a pane whose request nobody answered")
	}
	if _, err := s.CheckAnswer("p", req); err != nil || !s.Shown("p", req) {
		t.Fatalf("a prompt closed a request nobody answered: %v", err)
	}
	if _, err := s.BeginAnswer("p", req); err != nil {
		t.Fatal(err)
	}
	s.Sent("p", req)
	if !s.Prompt("p") {
		t.Fatal("a prompt after the answer changed nothing")
	}
	if _, err := s.CheckAnswer("p", req); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("the answered request is still open: %v", err)
	}
	if st, _ := s.Get("p"); st.State != api.AgentStatusExited {
		t.Fatalf("after the answer and the prompt: %+v", st)
	}

	s = newTestStore(&events)
	s.Hook("p", claudeHook("UserPromptSubmit", 1000, `{"session_id":"a","hook_event_name":"UserPromptSubmit","prompt_id":"t1"}`))
	if s.Prompt("p") {
		t.Fatal("a prompt moved a state a hook set")
	}
}

// TestOwnerInterrupt: Esc or Ctrl-C typed into a working Claude pane shows it idle, and the next
// report of any source wins again. Other keys, a pane that is not working, and an open request
// are left alone.
func TestOwnerInterrupt(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Hook("p", claudeHook("SessionStart", 1000, `{"session_id":"a","hook_event_name":"SessionStart"}`))
	if s.Interrupt("p", []byte("\x1b")) {
		t.Fatal("Esc moved an idle pane")
	}
	s.Hook("p", claudeHook("UserPromptSubmit", 1100, `{"session_id":"a","hook_event_name":"UserPromptSubmit","prompt_id":"t1"}`))
	for _, k := range []string{"a", "\x1b[A", "\x1b[200~\x03\x1b[201~"} {
		if s.Interrupt("p", []byte(k)) {
			t.Fatalf("%q interrupted the turn", k)
		}
	}
	if !s.Interrupt("p", []byte("\x03")) {
		t.Fatal("Ctrl-C changed nothing")
	}
	st, _ := s.Get("p")
	if st.State != api.AgentStatusIdle || st.Source != api.StateSourceServer {
		t.Fatalf("after Ctrl-C: %+v", st)
	}
	if _, err := s.CheckSend("p"); err != nil {
		t.Fatalf("send after the interrupt: %v", err)
	}
	// The key interrupted nothing after all: Claude's next OSC working wins.
	s.Report("p", progstatus.Report{State: "working", App: "claude"}, 1200, true)
	if st, _ := s.Get("p"); st.State != api.AgentStatusWorking {
		t.Fatalf("OSC working after the interrupt: %+v", st)
	}
	if !s.Interrupt("p", []byte("\x1b")) {
		t.Fatal("Esc changed nothing")
	}

	s = newTestStore(&events)
	req := claudeTurn(t, s)
	if s.Interrupt("p", []byte("\x1b")) {
		t.Fatal("Esc moved a pane with a request open")
	}
	if _, err := s.CheckAnswer("p", req); err != nil {
		t.Fatalf("Esc closed the request: %v", err)
	}
}
