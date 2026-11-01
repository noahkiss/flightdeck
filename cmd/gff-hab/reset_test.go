package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// idleClaude is Claude Code at its prompt: it reports idle once, then reads one message of
// $MSGLEN bytes.
const idleClaude = `
stty raw -echo
printf '\033]7501;state=idle:app=claude\033\\'
head -c "$MSGLEN" > "$DIR/msg.tmp" && mv "$DIR/msg.tmp" "$DIR/msg"
while [ ! -e "$DIR/end" ]; do sleep 0.02; done
`

// TestAgentStateSurvivesRestart: after a planned gff-hab restart, which replays from the pane's
// checkpoint past the bytes that held Claude's last OSC 7501 report, the pane still has its
// agent state, and session.send goes through with no hook in between.
func TestAgentStateSurvivesRestart(t *testing.T) {
	e := newHabEnv(t, 0)
	c := e.control()
	text := "after the restart"
	want := "\x1b[200~" + text + "\x1b[201~\r"
	var ss api.SessionCreated
	call(t, c, "session.create", api.SessionCreateParams{
		Name: "claude", Argv: []string{"/bin/sh", "-c", idleClaude}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "MSGLEN=" + strconv.Itoa(len(want)), noRaceExitSleep}}, &ss)
	waitState(t, e.sock, ss.Pane, "OSC idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })

	e.restart(false)
	c = e.control()
	st := waitState(t, e.sock, ss.Pane, "the state after the restart", func(s api.AgentState) bool { return s.State != "" })
	if st.State != api.AgentStatusIdle || st.Harness != "claude" || st.Source != api.StateSourceScreen {
		t.Fatalf("state after the restart: %+v", st)
	}
	call(t, c, "session.send", api.SessionSendParams{Session: ss.Session, Text: text}, nil)
	waitUntil(t, "the message in the pane", fileIs(filepath.Join(e.work, "msg"), want))
	writeFile(t, filepath.Join(e.work, "end"), "")
}

// interruptedClaude is Claude Code in a turn. It takes one key (the owner's interrupt), then
// opens a permission request by its hook and reads one key from the dialog. On Esc (a deny) it
// fires no hook and only reports idle, as Claude Code does. Then it reads one message.
const interruptedClaude = `
stty raw -echo
printf '\033]7501;state=working:app=claude\033\\'
head -c 1 > "$DIR/k1.tmp" && mv "$DIR/k1.tmp" "$DIR/k1"
while [ ! -e "$DIR/ask" ]; do sleep 0.02; done
"$HAB" hook claude PermissionRequest < "$DIR/permission.json"
head -c 1 > "$DIR/key.tmp" && mv "$DIR/key.tmp" "$DIR/key"
printf '\033]7501;state=idle:app=claude\033\\'
head -c "$MSGLEN" > "$DIR/msg.tmp" && mv "$DIR/msg.tmp" "$DIR/msg"
while [ ! -e "$DIR/end" ]; do sleep 0.02; done
`

// TestInterruptAndDenyByKeys: Esc typed into a working Claude pane (pane.input) shows it idle.
// A permission request then denied through approval.answer by keys, which Claude follows with no
// hook and only an OSC idle, returns the pane to idle, and session.send goes through.
func TestInterruptAndDenyByKeys(t *testing.T) {
	e := newHabEnv(t, 0)
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	writeFile(t, filepath.Join(e.work, "permission.json"), permissionPayload)
	text := "try another way"
	want := "\x1b[200~" + text + "\x1b[201~\r"
	var ss api.SessionCreated
	call(t, c, "session.create", api.SessionCreateParams{
		Name: "claude", Argv: []string{"/bin/sh", "-c", interruptedClaude}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "HAB=" + bin, "MSGLEN=" + strconv.Itoa(len(want)), noRaceExitSleep}}, &ss)
	pane := ss.Pane
	waitState(t, e.sock, pane, "OSC working", func(s api.AgentState) bool { return s.State == api.AgentStatusWorking })

	// A client may name the pane in upper case; the store keys it by the canonical id.
	call(t, c, "pane.input", api.InputParams{Pane: strings.ToUpper(pane), Data: []byte("\x1b")}, nil)
	st := waitState(t, e.sock, pane, "idle after Esc", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	if st.Source != api.StateSourceServer {
		t.Fatalf("state after the owner's Esc: %+v", st)
	}
	waitUntil(t, "the Esc in the pane", fileIs(filepath.Join(e.work, "k1"), "\x1b"))
	// Display only: Claude has not reported since the key, so a send waits.
	err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text}, nil)
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("send before the harness confirmed the interrupt: %v", err)
	}

	writeFile(t, filepath.Join(e.work, "ask"), "")
	st = waitState(t, e.sock, pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked && s.Request != nil })
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: strings.ToUpper(pane), Request: st.Request.ID, Choice: api.ApprovalChoiceDeny}, nil); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if !fileIs(filepath.Join(e.work, "key"), "\x1b")() {
		t.Fatal("the dialog did not get Esc")
	}
	waitState(t, e.sock, pane, "idle after the deny", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle && s.Request == nil })
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text}, nil); err != nil {
		t.Fatalf("send after the deny: %v", err)
	}
	waitUntil(t, "the message in the pane", fileIs(filepath.Join(e.work, "msg"), want))
	writeFile(t, filepath.Join(e.work, "end"), "")
}
