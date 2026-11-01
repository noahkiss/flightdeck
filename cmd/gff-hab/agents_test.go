package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeClaude stands in for Claude Code in a pane. It reports its state with OSC 7501 and runs
// the real hook command with hand-written payloads, as Claude's hook config would. Each step
// waits for a file the test creates. $MSGLEN is the length of the message the test sends. With
// a decision from the permission hook it reports working (the tool runs); else it reads one
// key from its dialog, then runs PostToolUse.
const fakeClaude = `
stty raw -echo
printf '\033]7501;state=working:app=claude\033\\'
while [ ! -e "$DIR/go-idle" ]; do sleep 0.02; done
printf '\033]7501;state=idle:app=claude\033\\'
head -c "$MSGLEN" > "$DIR/msg.tmp" && mv "$DIR/msg.tmp" "$DIR/msg"
"$HAB" hook -wait claude PermissionRequest < "$DIR/permission.json" > "$DIR/decision"
touch "$DIR/asked"
if [ -s "$DIR/decision" ]; then
  printf '\033]7501;state=working:app=claude\033\\'
else
  head -c 1 > "$DIR/key.tmp" && mv "$DIR/key.tmp" "$DIR/key"
  "$HAB" hook claude PostToolUse < "$DIR/posttool.json"
fi
while [ ! -e "$DIR/end" ]; do sleep 0.02; done
`

// Hand-written hook payloads in the shape of Claude Code 2.1.3xx. PermissionRequest carries no
// tool_use_id, as the hooks reference says.
const (
	permissionPayload = `{"session_id":"sess-7","cwd":"/work/app","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`
	postToolPayload   = `{"session_id":"sess-7","cwd":"/work/app","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"make test"},"tool_use_id":"toolu_9","tool_response":{"stdout":"ok"}}`
)

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileIs(path, want string) func() bool {
	return func() bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) == want
	}
}

func errKind(err error) api.ErrorKind {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Data.Kind
	}
	return ""
}

// startFakeClaude files a session running fakeClaude and returns it.
func startFakeClaude(t *testing.T, e *habEnv, c *control.Client, msgLen int) api.SessionCreated {
	t.Helper()
	writeFile(t, filepath.Join(e.work, "permission.json"), permissionPayload)
	writeFile(t, filepath.Join(e.work, "posttool.json"), postToolPayload)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var res api.SessionCreated
	err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Name: "claude", Argv: []string{"/bin/sh", "-c", fakeClaude}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "HAB=" + bin, "MSGLEN=" + strconv.Itoa(msgLen), noRaceExitSleep}}, &res)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func waitState(t *testing.T, sock, pane, what string, ok func(api.AgentState) bool) api.AgentState {
	t.Helper()
	var st api.AgentState
	waitUntil(t, what, func() bool {
		st = agentStates(t, sock)[pane]
		return ok(st)
	})
	return st
}

// TestClaudeAdapter is the Claude Code adapter end to end with the real holder and gff-hab: OSC
// 7501 reports in the pane's output set its state; session.send is refused with conflict while
// the agent works and lands as one bracketed paste plus Enter once it is idle; a -wait permission
// hook returns at once while the owner's gate is off; approval.answer refuses a wrong request id,
// then answers the open one by keys and returns once the request closed; the program's exit
// leaves the pane exited.
func TestClaudeAdapter(t *testing.T) {
	e := newHabEnv(t, 0)
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	text := "fix the bug\nin two lines"
	want := "\x1b[200~" + text + "\x1b[201~\r"
	ss := startFakeClaude(t, e, c, len(want))
	pane := ss.Pane

	st := waitState(t, e.sock, pane, "OSC working", func(s api.AgentState) bool { return s.State == api.AgentStatusWorking })
	if st.Harness != "claude" || st.Source != api.StateSourceOsc {
		t.Fatalf("state from OSC 7501: %+v", st)
	}
	err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text, Origin: api.SendOriginTyped}, nil)
	if errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "working") {
		t.Fatalf("send while working: %v", err)
	}

	writeFile(t, filepath.Join(e.work, "go-idle"), "")
	waitState(t, e.sock, pane, "OSC idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	// Two sends at once: one is delivered, the other refused, never two messages in one turn.
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text, Origin: api.SendOriginTyped}, nil)
		}()
	}
	e1, e2 := <-errs, <-errs
	if (e1 == nil) == (e2 == nil) || errKind(errors.Join(e1, e2)) != api.ErrorKindConflict {
		t.Fatalf("two sends while idle: %v; %v", e1, e2)
	}
	waitUntil(t, "the message in the pane", fileIs(filepath.Join(e.work, "msg"), want))

	// The permission hook ran with -wait; the gate is off, so it printed nothing and the hook's
	// state is blocked:permission with an id gff-hab made.
	waitUntil(t, "the permission hook", func() bool { _, err := os.Stat(filepath.Join(e.work, "asked")); return err == nil })
	if b, _ := os.ReadFile(filepath.Join(e.work, "decision")); len(b) != 0 {
		t.Fatalf("a -wait hook with the gate off printed %q", b)
	}
	st = waitState(t, e.sock, pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked })
	if st.Kind != api.BlockedKindPermission || st.Request == nil || st.Request.ID == "" || st.Request.Command != "make test" || st.Session != "sess-7" {
		t.Fatalf("blocked state: %+v", st)
	}
	err = c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: pane, Request: "not-it", Choice: api.ApprovalChoiceAllow}, nil)
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("answer with a wrong id: %v", err)
	}
	err = c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: "more"}, nil)
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("send while blocked: %v", err)
	}
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: pane, Request: st.Request.ID, Choice: api.ApprovalChoiceAllow, Via: api.AnswerViaApp}, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !fileIs(filepath.Join(e.work, "key"), "1")() {
		t.Fatal("the dialog did not get the allow key")
	}
	if st := agentStates(t, e.sock)[pane]; st.State != api.AgentStatusWorking {
		t.Fatalf("after the answer: %+v", st)
	}

	writeFile(t, filepath.Join(e.work, "end"), "")
	waitState(t, e.sock, pane, "exited", func(s api.AgentState) bool { return s.State == api.AgentStatusExited })
}

// TestBlockingPermissionHook: with the owner's gate on, a -wait PermissionRequest hook waits while
// an app client that subscribed is connected, and approval.answer reaches it as Claude's
// structured decision, and returns once the harness goes on; no key reaches the pane.
func TestBlockingPermissionHook(t *testing.T) {
	e := newHabEnv(t, 0)
	e.srv.stop(t)
	e.srv = startServeWith(t, e.sock, e.state, nil, "-holders", e.hdir, "-permission-wait", "20s")
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	if _, err := (api.Client{Caller: c}).EventsSubscribe(ctx, api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}
	text := "go"
	want := "\x1b[200~" + text + "\x1b[201~\r"
	ss := startFakeClaude(t, e, c, len(want))
	writeFile(t, filepath.Join(e.work, "go-idle"), "")
	waitState(t, e.sock, ss.Pane, "OSC idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text}, nil); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e.sock, ss.Pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked && s.Request != nil })
	if _, err := os.Stat(filepath.Join(e.work, "asked")); err == nil {
		t.Fatal("the hook did not wait")
	}
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: ss.Pane, Request: st.Request.ID, Choice: api.ApprovalChoiceDeny}, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	waitUntil(t, "the hook's decision", func() bool { _, err := os.Stat(filepath.Join(e.work, "asked")); return err == nil })
	var out struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	b, _ := os.ReadFile(filepath.Join(e.work, "decision"))
	if err := json.Unmarshal(b, &out); err != nil || out.HookSpecificOutput.HookEventName != "PermissionRequest" ||
		out.HookSpecificOutput.Decision.Behavior != "deny" || out.HookSpecificOutput.Decision.Message == "" {
		t.Fatalf("hook output %q: %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(e.work, "key")); err == nil {
		t.Fatal("a structured answer also sent keys")
	}
}

// forgingPane is Claude Code blocked on a permission dialog, plus a job in the same pane that
// forges Claude's OSC 7501 idle once the test creates "forge". The job then sends the status
// query and reads the answer, so the test knows gff-hab has parsed the forged report.
const forgingPane = `
stty raw -echo
printf '\033]7501;state=working:app=claude\033\\'
"$HAB" hook claude PermissionRequest < "$DIR/permission.json"
while [ ! -e "$DIR/forge" ]; do sleep 0.02; done
printf '\033]7501;state=idle:app=claude\033\\\033]7501;?\033\\'
head -c "$REPLYLEN" > /dev/null
touch "$DIR/forged"
while [ ! -e "$DIR/end" ]; do sleep 0.02; done
`

// TestForgedIdleKeepsDialog: any program in the pane can write OSC 7501. A forged idle, sent
// well after the interrupt grace, must not end a permission block a hook reported, so
// session.send, whose Enter would approve the dialog, stays refused.
func TestForgedIdleKeepsDialog(t *testing.T) {
	e := newHabEnv(t, 0)
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	writeFile(t, filepath.Join(e.work, "permission.json"), permissionPayload)
	reply := "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x1b\\"
	var ss api.SessionCreated
	err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Name: "claude", Argv: []string{"/bin/sh", "-c", forgingPane}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "HAB=" + bin, "REPLYLEN=" + strconv.Itoa(len(reply)), noRaceExitSleep}}, &ss)
	if err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e.sock, ss.Pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked && s.Request != nil })
	at, err := time.Parse(time.RFC3339Nano, st.At)
	if err != nil {
		t.Fatal(err)
	}
	// The forged report must come after the grace in which an OSC idle counts as stale.
	waitUntil(t, "the interrupt grace to pass", func() bool { return time.Since(at) > 1500*time.Millisecond })
	writeFile(t, filepath.Join(e.work, "forge"), "")
	waitUntil(t, "the forged report parsed", func() bool { _, err := os.Stat(filepath.Join(e.work, "forged")); return err == nil })
	err = c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: "yes"}, nil)
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("send after a forged idle: %v", err)
	}
	if now := agentStates(t, e.sock)[ss.Pane]; now.State != api.AgentStatusBlocked || now.Request == nil || now.Request.ID != st.Request.ID {
		t.Fatalf("after a forged idle: %+v", now)
	}
	writeFile(t, filepath.Join(e.work, "end"), "")
}
