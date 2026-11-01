package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type fixture struct {
	Note           string     `json:"note"`
	HarnessVersion string     `json:"harness_version"`
	Events         []Envelope `json:"events"`
	Expect         []expected `json:"expect"`
}

// expected is the part of an agent.state a fixture asserts.
type expected struct {
	Harness string           `json:"harness"`
	Session string           `json:"session,omitempty"`
	State   string           `json:"state"`
	Last    string           `json:"last,omitempty"`
	Kind    string           `json:"kind,omitempty"`
	Turn    string           `json:"turn,omitempty"`
	Version string           `json:"version,omitempty"`
	Warning string           `json:"warning,omitempty"`
	Request *expectedRequest `json:"request,omitempty"`
}

type expectedRequest struct {
	ID      string   `json:"id,omitempty"`
	Tool    string   `json:"tool,omitempty"`
	Command string   `json:"command,omitempty"`
	Files   []string `json:"files,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
}

func project(st api.AgentState) expected {
	e := expected{Harness: st.Harness, Session: st.Session, State: string(st.State), Last: string(st.Last),
		Kind: string(st.Kind), Turn: st.Turn, Version: st.Version}
	if st.Warning != nil {
		e.Warning = st.Warning.Reason
	}
	if r := st.Request; r != nil {
		e.Request = &expectedRequest{ID: r.ID, Tool: r.Tool, Command: r.Command, Files: r.Files, Cwd: r.Cwd}
	}
	return e
}

func newTestStore(events *[]structure.Event) *Store {
	clock := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return NewStore(Config{
		Publish: func(e structure.Event) { *events = append(*events, e) },
		Now:     func() time.Time { clock = clock.Add(time.Millisecond); return clock },
	})
}

// TestFixtures feeds each hand-written hook sequence to one pane and asserts the exact
// agent.state stream (testing.md #11). A request id of "*" stands for one gff-hab made.
func TestFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var fx fixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			if fx.HarnessVersion == "" {
				t.Fatal("fixture does not state the harness version its shape follows")
			}
			var events []structure.Event
			s := newTestStore(&events)
			for _, env := range fx.Events {
				s.Hook("pane-1", env)
			}
			var got []expected
			for i, e := range events {
				st := e.Data.(api.AgentState)
				if e.Type != EventAgentState || e.ID != "pane-1" || e.Revision != uint64(i+1) || st.Revision != e.Revision || st.Source != api.StateSourceHook || st.At == "" {
					t.Errorf("event %d envelope: %+v", i, e)
				}
				g := project(st)
				if g.Request != nil && strings.HasPrefix(g.Request.ID, s.reqPrefix+"-") {
					g.Request.ID = "*" // an id gff-hab made: the payload carried none
				}
				got = append(got, g)
			}
			if !reflect.DeepEqual(got, fx.Expect) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(fx.Expect, "", "  ")
				t.Fatalf("stream:\n%s\nwant:\n%s", g, w)
			}
			snap := s.Snapshot()
			if len(snap) != 1 || !reflect.DeepEqual(project(snap[0]), fx.Expect[len(fx.Expect)-1]) {
				t.Fatalf("snapshot %+v does not hold the last state", snap)
			}
		})
	}
}

func TestUnknownHarnessDropped(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	if changed, _ := s.Hook("p", Envelope{Harness: "nonesuch", Event: "Stop", T: 1, Payload: json.RawMessage(`{}`)}); changed || len(events) > 0 {
		t.Fatal("unknown harness changed state")
	}
	if changed, _ := s.Hook("p", Envelope{Harness: "claude", Event: "Stop", T: 1, Payload: json.RawMessage(`not json`)}); changed {
		t.Fatal("bad payload changed state")
	}
}

func TestSourcePrecedence(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	working := Update{State: api.AgentStatusWorking}
	s.Apply("p", "claude", api.StateSourceHook, 10, Update{NewTurn: true, State: api.AgentStatusWorking})
	// A weaker source in the same turn cannot overwrite the hook's state.
	if s.Apply("p", "claude", api.StateSourceScreen, 20, Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission}) {
		t.Fatal("screen overwrote a hook state in the same turn")
	}
	if s.Apply("p", "claude", api.StateSourceOsc, 20, Update{State: api.AgentStatusIdle}) {
		t.Fatal("osc overwrote a hook state in the same turn")
	}
	// A stronger source may.
	if !s.Apply("p", "claude", api.StateSourceServer, 30, Update{State: api.AgentStatusDone}) {
		t.Fatal("server could not overwrite a hook state")
	}
	// A new turn resets precedence; then the screen may fill the gap.
	s.Apply("p", "claude", api.StateSourceHook, 40, Update{NewTurn: true, State: api.AgentStatusWorking})
	if s.Apply("p", "claude", api.StateSourceScreen, 50, working) {
		t.Fatal("no change expected: already working")
	}
	var fresh []structure.Event
	s2 := newTestStore(&fresh)
	if !s2.Apply("q", "codex", api.StateSourceScreen, 1, Update{State: api.AgentStatusBlocked}) {
		t.Fatal("screen could not fill a gap")
	}
	if st, _ := s2.Get("q"); st.Source != api.StateSourceScreen {
		t.Fatalf("source %s", st.Source)
	}
}

func TestLateResolutionClearsBlocked(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Apply("p", "claude", api.StateSourceHook, 100, Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission, Request: &api.PermissionRequest{ID: "r1"}})
	// A resolution for r1 stamped before the blocked event still clears it.
	if !s.Apply("p", "claude", api.StateSourceHook, 90, Update{Resolves: "r1", State: api.AgentStatusWorking}) {
		t.Fatal("late resolution did not apply")
	}
	if st, _ := s.Get("p"); st.State != api.AgentStatusWorking || st.Request != nil {
		t.Fatalf("after resolution: %+v", st)
	}
	// A late blocked for r1 after its resolution is dropped, even with a newer clock.
	if s.Apply("p", "claude", api.StateSourceHook, 200, Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission, Request: &api.PermissionRequest{ID: "r1"}}) {
		t.Fatal("blocked for a resolved request applied")
	}
}

func TestSeenTurnsDoneIntoIdle(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Apply("p", "codex", api.StateSourceHook, 1, Update{State: api.AgentStatusDone})
	s.Seen("p")
	s.Seen("p") // idle stays idle; no event
	if len(events) != 2 {
		t.Fatalf("%d events, want 2", len(events))
	}
	if st := events[1].Data.(api.AgentState); st.State != api.AgentStatusIdle || st.Revision != 2 {
		t.Fatalf("after Seen: %+v", st)
	}
}

func TestHarnessChangeAfterExit(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Apply("p", "claude", api.StateSourceHook, 1, Update{Bind: true, Session: "c1", State: api.AgentStatusIdle, Version: "2.9"})
	// Codex starting in a pane where Claude still lives is a nested agent: dropped.
	if s.Apply("p", "codex", api.StateSourceHook, 2, Update{Bind: true, Session: "x1", State: api.AgentStatusIdle}) {
		t.Fatal("nested codex rebound a live claude pane")
	}
	s.Apply("p", "claude", api.StateSourceHook, 3, Update{Unbind: true, State: api.AgentStatusExited})
	if !s.Apply("p", "codex", api.StateSourceHook, 4, Update{Bind: true, Session: "x1", State: api.AgentStatusIdle}) {
		t.Fatal("codex could not take the pane after claude exited")
	}
	st, _ := s.Get("p")
	if st.Harness != "codex" || st.Session != "x1" || st.Version != "" || st.Warning != nil {
		t.Fatalf("old harness carried over: %+v", st)
	}
}

// TestLargeInputLeftOut: a tool input over MaxInput stays out of the state; the command and
// files stay exact.
func TestLargeInputLeftOut(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"file_path": "/work/app/big.txt", "content": strings.Repeat("x", MaxInput)})
	payload, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Write", "tool_use_id": "t1", "tool_input": json.RawMessage(big)})
	u, err := Normalise(Envelope{Harness: "claude", Event: "PermissionRequest", T: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	r := u.Request
	if r == nil || !r.InputTruncated || r.Input != nil || len(r.Files) != 1 || r.Files[0] != "/work/app/big.txt" {
		t.Fatalf("request %+v", r)
	}
}

// TestClockStepBack: a wall clock stepped back does not freeze the pane.
func TestClockStepBack(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Apply("p", "codex", api.StateSourceHook, 100_000, Update{NewTurn: true, State: api.AgentStatusWorking})
	// A little behind: a late arrival, dropped.
	if s.Apply("p", "codex", api.StateSourceHook, 99_000, Update{State: api.AgentStatusDone}) {
		t.Fatal("a late event applied")
	}
	// Far behind: the clock was reset; the event is current.
	if !s.Apply("p", "codex", api.StateSourceHook, 10_000, Update{State: api.AgentStatusDone}) {
		t.Fatal("an event after a clock reset was dropped")
	}
	if !s.Apply("p", "codex", api.StateSourceHook, 10_100, Update{State: api.AgentStatusIdle}) {
		t.Fatal("the pane froze after a clock reset")
	}
	// A new turn is never late, even a little behind.
	if !s.Apply("p", "codex", api.StateSourceHook, 9_000, Update{NewTurn: true, State: api.AgentStatusWorking}) {
		t.Fatal("a new turn behind the clock was dropped")
	}
}

// TestSameHarnessSessionStartRebinds: a session that ended with no end hook does not hold the
// pane; the next session start of the same harness takes it.
func TestSameHarnessSessionStartRebinds(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	s.Apply("p", "codex", api.StateSourceHook, 1, Update{Bind: true, Session: "old", State: api.AgentStatusIdle})
	s.Apply("p", "codex", api.StateSourceHook, 2, Update{NewTurn: true, Session: "old", State: api.AgentStatusWorking})
	if !s.Apply("p", "codex", api.StateSourceHook, 3, Update{Bind: true, Session: "new", State: api.AgentStatusIdle}) {
		t.Fatal("same-harness session start did not rebind")
	}
	if s.Apply("p", "codex", api.StateSourceHook, 4, Update{Session: "old", State: api.AgentStatusDone}) {
		t.Fatal("an event of the old session applied after the rebind")
	}
	if st, _ := s.Get("p"); st.Session != "new" || st.State != api.AgentStatusIdle {
		t.Fatalf("state %+v", st)
	}
}
