package main

import (
	"context"
	"os"
	"os/exec"
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
// and records each key it reads (hex) in DIR/keys.<pid>, with a space between two reads. Esc on
// its own, or Enter on "No, exit", exits 0; Enter on "Yes, I trust this folder" clears the
// screen, reports idle over OSC 7501, and writes everything pasted into it to DIR/in.<pid>.
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
  case $k in
    1b)
      stty min 0 time 2
      a=$(byte); b=$(byte)
      stty min 1 time 0
      printf '%s%s%s ' "$k" "$a" "$b" >> "$D/keys.$$"
      [ -n "$a" ] || exit 0
      case $b in
        42) cur=1 ;;
        41) cur=0 ;;
      esac
      draw
      ;;
    0d)
      printf '%s ' "$k" >> "$D/keys.$$"
      [ "$cur" = 1 ] || exit 0
      printf '\033[2J\033[H'
      break
      ;;
    *) printf '%s ' "$k" >> "$D/keys.$$" ;;
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
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(15*time.Second))
	defer cancel()
	var out api.TrustAnswered
	err := c.Call(ctx, "trust.answer", api.TrustAnswerParams{Pane: pane, Choice: choice}, &out)
	return out, err
}

// TestTrustCardTrust: a claude session started in a new folder shows blocked:trust with the
// folder, read from its screen, and its start message waits. trust.answer with trust sends Down
// and Enter as two writes; the agent starts, and the start message goes in. A second answer
// finds no prompt and gets conflict.
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
	if keys := pastedAll(e.work, "keys"); keys != "1b5b42 0d " {
		t.Errorf("keys %q, want Down, then Enter in a read of its own", keys)
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

// TestTrustCardExit: trust.answer with exit sends Esc; the agent exits, the start message is
// refused with the owner's reason, and the result counts it.
func TestTrustCardExit(t *testing.T) {
	e, c := trustEnv(t)
	made, _ := startTrust(t, e, c, "nope")
	out, err := trustAnswer(c, made.Pane, api.TrustChoiceExit)
	if err != nil || out.Dropped != 1 {
		t.Fatalf("exit: %+v %v", out, err)
	}
	if keys := pastedAll(e.work, "keys"); keys != "1b " {
		t.Errorf("keys %q, want Esc", keys)
	}
	time.Sleep(1500 * time.Millisecond)
	if s := pastedAll(e.work, "in"); s != "" {
		t.Fatalf("the start message went somewhere: %q", s)
	}
	if _, err := trustAnswer(c, made.Pane, api.TrustChoiceTrust); errKind(err) != api.ErrorKindConflict {
		t.Errorf("an answer after the exit: %v", err)
	}
}

// TestTrustCardAnsweredInTerminal: the owner answers in the terminal; the prompt leaves the
// state once the dialog stays gone, and a late trust.answer gets conflict and sends nothing.
// An answer right after typing in the pane is refused: the pane is not quiet.
func TestTrustCardAnsweredInTerminal(t *testing.T) {
	e, c := trustEnv(t)
	made, _ := startTrust(t, e, c, "term")
	call(t, c, "pane.input", api.InputParams{Pane: made.Pane, Text: "\x1b[B"}, nil)
	if _, err := trustAnswer(c, made.Pane, api.TrustChoiceExit); errKind(err) != api.ErrorKindConflict {
		t.Errorf("an answer right after typing: %v", err)
	}
	call(t, c, "pane.input", api.InputParams{Pane: made.Pane, Text: "\r"}, nil)
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
	if keys := pastedAll(e.work, "keys"); keys != "1b5b42 0d " {
		t.Errorf("keys %q: only the owner's", keys)
	}
}

// TestTrustAnswerCLIInPane: gff-hab api call trust.answer refuses to run in an agent's pane, as
// confirm.answer does.
func TestTrustAnswerCLIInPane(t *testing.T) {
	cmd := exec.Command(bin, "api", "call", "-socket", filepath.Join(t.TempDir(), "none.sock"), "trust.answer", `{"pane":"p","choice":"trust"}`)
	cmd.Env = append(os.Environ(), "GFF_HAB=unix:/nowhere#token")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "trust.answer is the owner's answer") {
		t.Errorf("in a pane: %v %s", err, out)
	}
}

// fakeTrustPane is one pane's screen and program for the trust service's units: the 2.1.317
// dialog, a cursor the keys move, and the outcome of Enter and Esc.
type fakeTrustPane struct {
	mu      sync.Mutex
	cursor  int // 0 "No, exit", 1 "Yes, I trust this folder"
	gone    bool
	exited  bool
	stuck   bool // keys reach nothing: the dialog stays as it is
	writes  [][]byte
	block   chan struct{} // when set, a write waits for it
	flicker int           // reads left that miss the dialog
}

func (f *fakeTrustPane) rows() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone || f.flicker > 0 {
		if f.flicker > 0 {
			f.flicker--
		}
		return []string{"❯ Try \"fix the tests\""}, nil
	}
	no, yes := "   No, exit", "   Yes, I trust this folder"
	if f.cursor == 0 {
		no = " ❯ No, exit"
	} else {
		yes = " ❯ Yes, I trust this folder"
	}
	return []string{" Accessing workspace:", " /work/a", " Quick safety check: Is this a project you created or one you trust?", no, yes, " Enter to confirm · Esc to cancel"}, nil
}

func (f *fakeTrustPane) write(_ context.Context, _ string, b []byte) error {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), b...))
	if f.stuck {
		return nil
	}
	switch string(b) {
	case "\x1b[B":
		f.cursor = 1
	case "\x1b[A":
		f.cursor = 0
	case "\r":
		if f.cursor == 1 {
			f.gone = true
		} else {
			f.gone, f.exited = true, true
		}
	case "\x1b":
		f.gone, f.exited = true, true
	}
	return nil
}

func (f *fakeTrustPane) live(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.exited
}

func (f *fakeTrustPane) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, w := range f.writes {
		out = append(out, string(w))
	}
	return out
}

// shortTrust shortens the trust times for a unit test.
func shortTrust(t *testing.T) {
	old := []time.Duration{trustPoll, trustReadBack, trustCursor, trustQuiet, trustSettle, trustKeyGap, trustStart}
	trustPoll, trustReadBack, trustCursor, trustQuiet, trustSettle, trustKeyGap, trustStart =
		20*time.Millisecond, 300*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond, time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() {
		trustPoll, trustReadBack, trustCursor, trustQuiet, trustSettle, trustKeyGap, trustStart = old[0], old[1], old[2], old[3], old[4], old[5], old[6]
	})
}

func newFakeTrust(t *testing.T) (*trustService, *harness.Store, *fakeTrustPane) {
	t.Helper()
	shortTrust(t)
	agents := harness.NewStore(harness.Config{Publish: func(structure.Event) {}})
	f := &fakeTrustPane{}
	ts := newTrustService(agents, func(string) ([]string, error) { return f.rows() }, f.write, f.live, func(string, ...any) {})
	t.Cleanup(ts.Close)
	agents.SetTrust("p", "claude", "/work/a", true)
	return ts, agents, f
}

func trustCall(ts *trustService, c api.TrustChoice) (api.TrustAnswered, error) {
	return ts.Answer(context.Background(), "c1", api.TrustAnswerParams{Pane: "p", Choice: c})
}

// TestTrustAnswerKeys: each key is its own write; trust moves down, then Enter; exit is Esc and
// marks the pane declined, and the waiting messages are refused and counted.
func TestTrustAnswerKeys(t *testing.T) {
	ts, agents, f := newFakeTrust(t)
	if _, err := trustCall(ts, api.TrustChoiceTrust); err != nil {
		t.Fatal(err)
	}
	if got := f.sent(); len(got) != 2 || got[0] != "\x1b[B" || got[1] != "\r" {
		t.Errorf("writes %q", got)
	}
	if st, _ := agents.Get("p"); st.State != api.AgentStatusWorking {
		t.Errorf("after trust: %+v", st)
	}

	ts, _, f = newFakeTrust(t)
	ts.refuse = func(pane, reason string) (int, error) {
		if reason != reasonUntrusted {
			t.Errorf("reason %q", reason)
		}
		return 2, nil
	}
	out, err := trustCall(ts, api.TrustChoiceExit)
	if err != nil || out.Dropped != 2 {
		t.Fatalf("exit: %+v %v", out, err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "\x1b" {
		t.Errorf("writes %q", got)
	}
	if d, p := ts.Declined("p"); !d || p {
		t.Errorf("declined %v pending %v", d, p)
	}
	ts.forget("p")
	if d, _ := ts.Declined("p"); d {
		t.Error("declined kept after the pane went")
	}
}

// TestTrustAnswerRefusals: nothing is chosen blind. A screen without the dialog or its cursor
// sends nothing; a cursor that does not reach the trust row gets no Enter; recent client input
// refuses; a second answer while one runs gets conflict.
func TestTrustAnswerRefusals(t *testing.T) {
	ts, _, f := newFakeTrust(t)
	f.gone = true
	if _, err := trustCall(ts, api.TrustChoiceTrust); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "no longer shows") {
		t.Errorf("screen gone: %v", err)
	}
	if len(f.sent()) != 0 {
		t.Errorf("writes %q", f.sent())
	}

	ts, _, f = newFakeTrust(t)
	f.stuck = true // Down reaches nothing, as if a terminal Up undid it
	if _, err := trustCall(ts, api.TrustChoiceTrust); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "did not reach") {
		t.Errorf("cursor stuck: %v", err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "\x1b[B" {
		t.Errorf("writes %q: an Enter went in", got)
	}

	ts, _, f = newFakeTrust(t)
	ts.inputStart("p")
	if _, err := trustCall(ts, api.TrustChoiceExit); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "still being written") {
		t.Errorf("queued input: %v", err)
	}
	ts.inputEnd("p")
	if _, err := trustCall(ts, api.TrustChoiceExit); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "a moment ago") {
		t.Errorf("recent input: %v", err)
	}
	if len(f.sent()) != 0 {
		t.Errorf("writes %q", f.sent())
	}

	ts, _, f = newFakeTrust(t)
	f.block = make(chan struct{})
	first := make(chan error, 1)
	go func() { _, err := trustCall(ts, api.TrustChoiceTrust); first <- err }()
	waitUntil(t, "the first answer running", func() bool {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		return ts.answering["p"]
	})
	if _, err := trustCall(ts, api.TrustChoiceExit); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "on its way") {
		t.Errorf("second answer: %v", err)
	}
	close(f.block)
	if err := <-first; err != nil {
		t.Errorf("first answer: %v", err)
	}
}

// TestTrustAnswerOutcome: the answer reports unavailable when its outcome does not show: the
// dialog stays after trust, or the agent still runs after exit (then the pane is not declined,
// and messages are not refused); trust that ends the agent is unavailable too.
func TestTrustAnswerOutcome(t *testing.T) {
	ts, _, f := newFakeTrust(t)
	f.mu.Lock()
	f.cursor = 1 // the cursor already sits on trust: only Enter goes
	f.mu.Unlock()
	ts.write = func(_ context.Context, _ string, b []byte) error {
		f.mu.Lock()
		f.writes = append(f.writes, b)
		f.mu.Unlock()
		return nil
	}
	if _, err := trustCall(ts, api.TrustChoiceTrust); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "still shows") {
		t.Errorf("dialog stays: %v", err)
	}
	refused := false
	ts.refuse = func(string, string) (int, error) { refused = true; return 1, nil }
	if _, err := trustCall(ts, api.TrustChoiceExit); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "still runs") {
		t.Errorf("exit with no exit: %v", err)
	}
	if d, p := ts.Declined("p"); d || p || refused {
		t.Errorf("declined %v pending %v refused %v", d, p, refused)
	}

	ts, _, f = newFakeTrust(t)
	ts.write = func(context.Context, string, []byte) error {
		f.mu.Lock()
		f.cursor, f.exited = 1, true
		f.mu.Unlock()
		return nil
	}
	if _, err := trustCall(ts, api.TrustChoiceTrust); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "exited") {
		t.Errorf("trust that ends the agent: %v", err)
	}
}

// TestTrustWatch: the watch shows the prompt, keeps it through a frame that misses the dialog,
// clears it after several misses, never shows it again right after an answer, and once the
// dialog went with no report from the harness, shows the hooks missing.
func TestTrustWatch(t *testing.T) {
	ts, agents, f := newFakeTrust(t)
	agents.SetTrust("p", "claude", "/work/a", false) // start from the starting state
	agents.TrustStalled("p")
	ts.watch("p", "claude", "/work/a", nil)
	waitUntil(t, "the prompt", func() bool { _, ok := agents.Trusting("p"); return ok })
	f.mu.Lock()
	f.flicker = 1
	f.mu.Unlock()
	time.Sleep(10 * trustPoll)
	if _, ok := agents.Trusting("p"); !ok {
		t.Fatal("one frame without the dialog cleared the prompt")
	}
	// An answer's settle time: the watch does not show the prompt again.
	ts.mu.Lock()
	ts.answered["p"] = time.Now()
	agents.SetTrust("p", "claude", "/work/a", false)
	ts.mu.Unlock()
	time.Sleep(3 * trustPoll)
	if _, ok := agents.Trusting("p"); ok {
		t.Error("the watch showed the prompt during an answer's settle time")
	}
	f.mu.Lock()
	f.gone = true
	f.mu.Unlock()
	waitUntil(t, "hooks missing", func() bool {
		st, _ := agents.Get("p")
		return st.State == api.AgentStatusNone && st.Hooks == api.HookHealthMissing
	})
}
