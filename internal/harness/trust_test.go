package harness

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

func trustScreen(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/trust/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// TestTrustMoves: the moves come from the screen, one key each. Claude Code 2.1.317 lists "No,
// exit" first and selected, so trusting moves down; an older layout lists Yes first. The cursor
// may sit on either row. Exit is Esc, whatever the cursor. Anything else sends nothing.
func TestTrustMoves(t *testing.T) {
	cur := trustScreen(t, "claude-2.1.317.screen")
	old := trustScreen(t, "claude-older.screen")
	// The cursor moved to the second row in the 2.1.317 layout.
	moved := strings.Split(strings.NewReplacer(" ❯ No, exit", "   No, exit", "   Yes, I trust this folder", " ❯ Yes, I trust this folder").Replace(strings.Join(cur, "\n")), "\n")
	join := func(keys [][]byte) []string {
		var out []string
		for _, k := range keys {
			out = append(out, string(k))
		}
		return out
	}
	for _, c := range []struct {
		name  string
		rows  []string
		trust bool
		want  []string
		on    bool
	}{
		{"2.1.317 trust", cur, true, []string{"\x1b[B"}, false},
		{"2.1.317 exit", cur, false, nil, true},
		{"2.1.317 cursor on yes, trust", moved, true, nil, true},
		{"2.1.317 cursor on yes, exit", moved, false, []string{"\x1b[A"}, false},
		{"older trust", old, true, nil, true},
		{"older exit", old, false, []string{"\x1b[B"}, false},
	} {
		got, err := TrustMoves("claude", c.rows, c.trust)
		if err != nil || strings.Join(join(got), "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: moves %q, %v; want %q", c.name, join(got), err, c.want)
		}
		if on, err := TrustCursorOn("claude", c.rows, c.trust); err != nil || on != c.on {
			t.Errorf("%s: cursor on %v, %v", c.name, on, err)
		}
	}
	if k, err := TrustExitKey("claude"); err != nil || string(k) != "\x1b" {
		t.Errorf("exit key %q %v", k, err)
	}

	twoCursors := strings.Split(strings.Replace(strings.Join(cur, "\n"), "   Yes, I trust", " ❯ Yes, I trust", 1), "\n")
	noCursor := strings.Split(strings.Replace(strings.Join(cur, "\n"), " ❯ No, exit", "   No, exit", 1), "\n")
	noExit := strings.Split(strings.Replace(strings.Join(cur, "\n"), " ❯ No, exit", " ❯ No, leave", 1), "\n")
	for _, c := range []struct {
		name  string
		rows  []string
		trust bool
		want  error
	}{
		{"two cursors", twoCursors, true, ErrTrustLayout},
		{"no cursor", noCursor, true, ErrTrustLayout},
		{"no exit row", noExit, false, ErrTrustLayout},
		{"no dialog", []string{"❯ Try \"fix the tests\""}, true, ErrNoTrustDialog},
		{"a permission dialog", []string{"Do you want to proceed?", "❯ 1. Yes", "  2. No"}, false, ErrNoTrustDialog},
	} {
		if got, err := TrustMoves("claude", c.rows, c.trust); !errors.Is(err, c.want) || got != nil {
			t.Errorf("%s: moves %q, %v; want %v", c.name, got, err, c.want)
		}
		if on, err := TrustCursorOn("claude", c.rows, c.trust); !errors.Is(err, c.want) || on {
			t.Errorf("%s: cursor on %v, %v", c.name, on, err)
		}
	}
	if _, err := TrustMoves("codex", cur, true); !errors.Is(err, ErrUnsupported) {
		t.Errorf("codex: %v", err)
	}
	if _, err := TrustExitKey("codex"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("codex exit: %v", err)
	}
}

// TestTrustDialogFixtures: the screen rule sees both layouts, and only for Claude Code.
func TestTrustDialogFixtures(t *testing.T) {
	for _, f := range []string{"claude-2.1.317.screen", "claude-older.screen"} {
		screen := strings.Join(trustScreen(t, f), "\n")
		if !TrustDialog("claude", screen) {
			t.Errorf("%s: not seen", f)
		}
		if TrustDialog("codex", screen) {
			t.Errorf("%s: seen for codex", f)
		}
	}
}

// TestSetTrust: the prompt shows as blocked:trust with its folder from the screen source; it
// goes to working when the dialog goes; the harness's first hook wins and drops the prompt; a
// pane whose harness already reports is never covered.
func TestSetTrust(t *testing.T) {
	var events []structure.Event
	s := newTestStore(&events)
	if s.SetTrust("p", "claude", "/work/a", false) {
		t.Fatal("a prompt that never showed went")
	}
	if !s.SetTrust("p", "claude", "/work/a", true) {
		t.Fatal("the prompt did not show")
	}
	st, ok := s.Trusting("p")
	if !ok || st.Source != api.StateSourceScreen || st.Harness != "claude" || st.Trust == nil || st.Trust.Folder != "/work/a" {
		t.Fatalf("state %+v", st)
	}
	if s.SetTrust("p", "claude", "/work/a", true) {
		t.Error("the same prompt again changed the state")
	}
	if _, err := s.CheckSend("p"); !errors.Is(err, ErrBusy) {
		t.Errorf("send while the prompt shows: %v", err)
	}
	if !s.SetTrust("p", "claude", "/work/a", false) {
		t.Fatal("the prompt did not go")
	}
	st, _ = s.Get("p")
	if st.State != api.AgentStatusWorking || st.Trust != nil || st.Kind != "" {
		t.Fatalf("after the prompt: %+v", st)
	}
	if !s.Apply("p", "claude", api.StateSourceHook, 1, Update{Bind: true, Session: "x", State: api.AgentStatusIdle}) {
		t.Fatal("the first hook did not apply")
	}
	if s.SetTrust("p", "claude", "/work/a", true) {
		t.Error("the prompt covered a state the harness reported")
	}

	// A hook while the prompt shows replaces it, and the prompt is dropped from the state.
	s.SetTrust("q", "claude", "/work/b", true)
	s.Apply("q", "claude", api.StateSourceHook, 1, Update{Bind: true, Session: "y", State: api.AgentStatusIdle})
	if st, _ := s.Get("q"); st.State != api.AgentStatusIdle || st.Trust != nil {
		t.Fatalf("after a hook: %+v", st)
	}
	// A pane exit drops the prompt too.
	s.SetTrust("r", "claude", "/work/c", true)
	s.Exited("r")
	if st, _ := s.Get("r"); st.State != api.AgentStatusExited || st.Trust != nil {
		t.Fatalf("after the exit: %+v", st)
	}
	// The prompt went and the harness never reported: the hooks are missing.
	s.SetTrust("u", "claude", "/work/d", true)
	s.SetTrust("u", "claude", "/work/d", false)
	if !s.TrustStalled("u") {
		t.Fatal("a stalled start did not change")
	}
	if st, _ := s.Get("u"); st.State != api.AgentStatusNone || st.Hooks != api.HookHealthMissing {
		t.Fatalf("stalled: %+v", st)
	}
	if s.TrustStalled("p") {
		t.Error("a pane whose harness reported was stalled")
	}
	if len(events) == 0 {
		t.Fatal("no events published")
	}
}
