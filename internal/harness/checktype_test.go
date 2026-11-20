package harness

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestCheckType: pane.type's gate in the store (hab-logic-api.md 3.12). A shell and an exited
// agent take text; an open permission request and a blocked agent refuse it at every door; an
// agent known only through OSC 7501 refuses it at an agent's door.
func TestCheckType(t *testing.T) {
	s := NewStore(Config{})
	claude := func(event, payload string, at int64) Envelope {
		return Envelope{Harness: "claude", Event: event, T: at, N: 1, Payload: json.RawMessage(payload)}
	}
	busy := func(err error) bool { return errors.Is(err, ErrBusy) }

	// No state: a shell.
	if err := s.CheckType("shell", true); err != nil {
		t.Fatalf("shell: %v", err)
	}
	// An idle agent with hooks takes text at both doors.
	s.Hook("hooked", claude("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	if err := s.CheckType("hooked", true); err != nil {
		t.Fatalf("idle with hooks: %v", err)
	}
	// An open permission request refuses at both doors.
	if _, req := s.Hook("hooked", claude("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make"}}`, 2)); req == "" {
		t.Fatal("no request opened")
	}
	for _, agent := range []bool{true, false} {
		if err := s.CheckType("hooked", agent); !busy(err) || !strings.Contains(err.Error(), "permission request") {
			t.Fatalf("open request, agent door %v: %v", agent, err)
		}
	}
	// Blocked without an open request (OSC only): refused at both doors.
	s.Apply("osc", "claude", api.StateSourceOsc, 1, Update{State: api.AgentStatusBlocked})
	if st, _ := s.Get("osc"); st.State != api.AgentStatusBlocked {
		t.Fatalf("osc state %+v", st)
	}
	for _, agent := range []bool{true, false} {
		if err := s.CheckType("osc", agent); !busy(err) || !strings.Contains(err.Error(), "blocked") {
			t.Fatalf("blocked, agent door %v: %v", agent, err)
		}
	}
	// Idle, known only through OSC: refused at an agent's door, taken from the owner's.
	s.Apply("osc", "claude", api.StateSourceOsc, 2, Update{State: api.AgentStatusIdle, NewTurn: true})
	if err := s.CheckType("osc", true); !busy(err) || !strings.Contains(err.Error(), "no hook") {
		t.Fatalf("osc only, agent door: %v", err)
	}
	if err := s.CheckType("osc", false); err != nil {
		t.Fatalf("osc only, owner: %v", err)
	}
	// A hooked Claude ends; a Codex with no hooks starts in the same pane. Claude's hooks say
	// nothing of Codex's dialogs: an agent's door is refused.
	s.Hook("seq", claude("SessionStart", `{"session_id":"c","hook_event_name":"SessionStart"}`, 1))
	if err := s.CheckType("seq", true); err != nil {
		t.Fatalf("hooked claude: %v", err)
	}
	s.Hook("seq", claude("SessionEnd", `{"session_id":"c","hook_event_name":"SessionEnd"}`, 2))
	s.Apply("seq", "codex", api.StateSourceOsc, 3, Update{State: api.AgentStatusIdle, NewTurn: true})
	if st, _ := s.Get("seq"); st.Harness != "codex" || st.State != api.AgentStatusIdle {
		t.Fatalf("seq state %+v", st)
	}
	if err := s.CheckType("seq", true); !busy(err) || !strings.Contains(err.Error(), "no hook") {
		t.Fatalf("hook-less codex after a hooked claude: %v", err)
	}
	// The same without the SessionEnd: the harness change alone drops the mark.
	s.Hook("seq2", claude("SessionStart", `{"session_id":"d","hook_event_name":"SessionStart"}`, 1))
	s.Apply("seq2", "codex", api.StateSourceOsc, 2, Update{State: api.AgentStatusIdle, NewTurn: true})
	if st, _ := s.Get("seq2"); st.Harness == "codex" {
		if err := s.CheckType("seq2", true); !busy(err) {
			t.Fatalf("hook-less codex after a harness change: %v", err)
		}
	}
	// An exited agent is a shell again.
	s.Exited("osc")
	if err := s.CheckType("osc", true); err != nil {
		t.Fatalf("exited: %v", err)
	}
}

// TestPasteWrite: the text cannot end the paste early.
func TestPasteWrite(t *testing.T) {
	got := string(PasteWrite("a\x1b[201~\x1b[201\x1b[201~~~b"))
	if !strings.HasPrefix(got, pasteStart) || !strings.HasSuffix(got, pasteEnd) || strings.Count(got, pasteEnd) != 1 {
		t.Fatalf("%q", got)
	}
}
