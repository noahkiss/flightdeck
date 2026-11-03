package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakeTrustClaude plays Claude Code 2.1.317 in a folder it has not trusted: it draws the
// workspace trust dialog with "No, exit" first and selected, moves its cursor on Up and Down,
// and records each key it reads (hex) in DIR/keys.<pid>. Enter on "No, exit" exits 0; Enter on
// "Yes, I trust this folder" clears the screen, reports idle over OSC 7501, and writes
// everything pasted into it to DIR/in.<pid>.
const fakeTrustClaude = `#!/bin/sh
D=DIR
stty raw -echo
cur=0
draw() {
  printf '\033[2J\033[H Accessing workspace:\r\n\r\n %s\r\n\r\n Quick safety check: Is this a project you created or one you trust?\r\n\r\n Claude Code'"'"'ll be able to read, edit, and execute files here.\r\n\r\n' "$(pwd -P)"
  if [ "$cur" = 0 ]; then
    printf ' \342\235\257 No, exit\r\n   Yes, I trust this folder\r\n'
  else
    printf '   No, exit\r\n \342\235\257 Yes, I trust this folder\r\n'
  fi
  printf '\r\n Enter to confirm \302\267 Esc to cancel\r\n'
}
byte() { dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'; }
draw
while :; do
  k=$(byte)
  printf '%s' "$k" >> "$D/keys.$$"
  case $k in
    1b)
      a=$(byte); b=$(byte)
      printf '%s%s' "$a" "$b" >> "$D/keys.$$"
      case $b in
        42) cur=1 ;;
        41) cur=0 ;;
      esac
      draw
      ;;
    0d)
      [ "$cur" = 1 ] || exit 0
      printf '\033[2J\033[H'
      break
      ;;
  esac
done
printf '\033]7501;state=idle:app=claude\033\\'
exec cat > "$D/in.$$"
`

// trustEnv starts a holder and gff-hab with fakeTrustClaude as claude.
func trustEnv(t *testing.T) (*habEnv, *control.Client) {
	t.Helper()
	e := newHolderEnv(t, 0)
	binDir := filepath.Join(e.work, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(strings.ReplaceAll(fakeTrustClaude, "DIR", e.work)), 0o700); err != nil {
		t.Fatal(err)
	}
	e.env = []string{"PATH=" + binDir + ":/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + filepath.Join(e.work, "claude-config")}
	e.serve()
	return e, e.control()
}

// agentOf returns the pane's agent state from structure.get.
func agentOf(t *testing.T, c *control.Client, pane string) (api.AgentState, bool) {
	t.Helper()
	var st api.StructureResult
	call(t, c, "structure.get", api.StructureGetParams{}, &st)
	for _, a := range st.Agents {
		if a.Pane == pane {
			return a, true
		}
	}
	return api.AgentState{}, false
}

// startTrust starts a claude session named name in a new folder, with a start message.
func startTrust(t *testing.T, e *habEnv, c *control.Client, name string) (api.SessionCreated, string) {
	t.Helper()
	dir := filepath.Join(e.work, "proj-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)
	var made api.SessionCreated
	call(t, c, "session.create", api.SessionCreateParams{Name: name, Harness: "claude", Cwd: dir, Seed: "Build the cache for " + name + "."}, &made)
	waitUntil(t, name+" blocked on the trust prompt", func() bool {
		a, ok := agentOf(t, c, made.Pane)
		return ok && a.State == api.AgentStatusBlocked && a.Kind == api.BlockedKindTrust && a.Trust != nil && a.Trust.Folder == dir &&
			a.Harness == "claude" && a.Source == api.StateSourceScreen
	})
	return made, dir
}

func trustAnswer(c *control.Client, pane string, choice api.TrustChoice) (api.TrustAnswered, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var out api.TrustAnswered
	err := c.Call(ctx, "trust.answer", api.TrustAnswerParams{Pane: pane, Choice: choice}, &out)
	return out, err
}

// TestTrustCardTrust: a claude session started in a new folder shows blocked:trust with the
// folder, read from its screen, and its start message waits. trust.answer with trust moves the
// cursor down from "No, exit" and presses Enter; the agent starts, and the start message goes
// in. A second answer finds no prompt and gets conflict.
func TestTrustCardTrust(t *testing.T) {
	e, c := trustEnv(t)
	made, _ := startTrust(t, e, c, "demo")
	time.Sleep(1500 * time.Millisecond)
	if s := pastedAll(e.work, "in"); s != "" {
		t.Fatalf("the start message went in before the trust answer: %q", s)
	}
	if _, err := trustAnswer(c, made.Pane, "maybe"); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("a choice outside the set: %v", err)
	}
	out, err := trustAnswer(c, made.Pane, api.TrustChoiceTrust)
	if err != nil || out.Dropped != 0 {
		t.Fatalf("trust: %+v %v", out, err)
	}
	if keys := pastedAll(e.work, "keys"); keys != "1b5b420d" {
		t.Errorf("keys %q, want Down then Enter", keys)
	}
	waitUntil(t, "the start message in the agent", func() bool {
		s := pastedAll(e.work, "in")
		return strings.Contains(s, "[begin start message ") && strings.Contains(s, "Build the cache for demo.")
	})
	if a, _ := agentOf(t, c, made.Pane); a.Kind == api.BlockedKindTrust || a.Trust != nil {
		t.Errorf("after trust: %+v", a)
	}
	if _, err := trustAnswer(c, made.Pane, api.TrustChoiceTrust); errKind(err) != api.ErrorKindConflict {
		t.Errorf("a second answer: %v", err)
	}
}

// TestTrustCardExit: trust.answer with exit presses Enter on "No, exit"; the agent exits, the
// start message is refused with the owner's reason, and the result counts it.
func TestTrustCardExit(t *testing.T) {
	e, c := trustEnv(t)
	made, _ := startTrust(t, e, c, "nope")
	out, err := trustAnswer(c, made.Pane, api.TrustChoiceExit)
	if err != nil || out.Dropped != 1 {
		t.Fatalf("exit: %+v %v", out, err)
	}
	if keys := pastedAll(e.work, "keys"); keys != "0d" {
		t.Errorf("keys %q, want Enter", keys)
	}
	waitUntil(t, "the session exited", func() bool {
		var st api.StructureResult
		call(t, c, "structure.get", api.StructureGetParams{}, &st)
		for _, s := range st.Sessions {
			if s.ID == made.Session {
				return s.State == api.SessionStateExited
			}
		}
		return false
	})
	time.Sleep(1500 * time.Millisecond)
	if s := pastedAll(e.work, "in"); s != "" {
		t.Fatalf("the start message went somewhere: %q", s)
	}
	if _, err := trustAnswer(c, made.Pane, api.TrustChoiceTrust); errKind(err) != api.ErrorKindConflict {
		t.Errorf("an answer after the exit: %v", err)
	}
}

// TestTrustCardAnsweredInTerminal: the owner answers in the terminal; the prompt leaves the
// state at the next screen read, and a late trust.answer gets conflict and sends nothing.
func TestTrustCardAnsweredInTerminal(t *testing.T) {
	e, c := trustEnv(t)
	made, _ := startTrust(t, e, c, "term")
	call(t, c, "pane.input", api.InputParams{Pane: made.Pane, Text: "\x1b[B\r"}, nil)
	waitUntil(t, "the prompt gone", func() bool {
		a, ok := agentOf(t, c, made.Pane)
		return ok && a.Kind != api.BlockedKindTrust
	})
	if _, err := trustAnswer(c, made.Pane, api.TrustChoiceTrust); errKind(err) != api.ErrorKindConflict {
		t.Errorf("a late answer: %v", err)
	}
	waitUntil(t, "the start message in the agent", func() bool {
		return strings.Contains(pastedAll(e.work, "in"), "Build the cache for term.")
	})
	if keys := pastedAll(e.work, "keys"); keys != "1b5b420d" {
		t.Errorf("keys %q: only the owner's", keys)
	}
}

// TestTrustAnswerScreenGone: the state still shows the prompt, but the screen no longer does
// (the dialog went between two reads): the answer is refused with conflict, and nothing is
// written. A screen whose options cannot be read is refused the same way.
func TestTrustAnswerScreenGone(t *testing.T) {
	var events []structure.Event
	agents := harness.NewStore(harness.Config{Publish: func(e structure.Event) { events = append(events, e) }})
	var mu sync.Mutex
	rows := []string{"❯ Try \"fix the tests\""}
	var wrote [][]byte
	ts := newTrustService(agents,
		func(string) ([]string, error) { mu.Lock(); defer mu.Unlock(); return rows, nil },
		func(_ context.Context, _ string, b []byte) error {
			mu.Lock()
			defer mu.Unlock()
			wrote = append(wrote, b)
			return nil
		},
		func(string) bool { return true }, func(string, ...any) {})
	defer ts.Close()
	agents.SetTrust("p", "claude", "/work/a", true)
	_, err := ts.Answer(context.Background(), "c1", api.TrustAnswerParams{Pane: "p", Choice: api.TrustChoiceTrust})
	if errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "no longer shows") {
		t.Errorf("screen gone: %v", err)
	}
	mu.Lock()
	rows = []string{"Quick safety check", "  Yes, I trust this folder", "  No, exit"} // no cursor
	mu.Unlock()
	if _, err := ts.Answer(context.Background(), "c1", api.TrustAnswerParams{Pane: "p", Choice: api.TrustChoiceExit}); errKind(err) != api.ErrorKindConflict {
		t.Errorf("no cursor: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(wrote) != 0 {
		t.Errorf("keys sent: %q", wrote)
	}
	if ts.Declined("p") {
		t.Error("a refused exit marked the pane declined")
	}
}
