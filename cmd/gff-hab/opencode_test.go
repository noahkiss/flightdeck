package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestOpencodeAdapter is the opencode adapter end to end with the real holder and gff-hab and
// a fake opencode (testdata/fakeopencode): session.create starts the wrapper, which runs a
// private server with the pane's password and the TUI attached to it; server events set the
// pane's state; session.send goes through the prompt API and approval.answer through the reply
// API; chat.page reads the messages from the server; a gff-hab restart finds the server again
// from its record; closing the pane ends the server and removes the record.
func TestOpencodeAdapter(t *testing.T) {
	e := newHabEnv(t, 0)
	fake := filepath.Join(e.work, "opencode")
	if out, err := exec.Command("go", "build", "-o", fake, "./testdata/fakeopencode").CombinedOutput(); err != nil {
		t.Fatalf("build the fake opencode: %v\n%s", err, out)
	}
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	var ss api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Harness: "opencode", Argv: []string{fake}, Cwd: "/",
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "DIR=" + e.work}}, &ss); err != nil {
		t.Fatal(err)
	}
	pane := ss.Pane
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(e.work, name)); return string(b) }
	waitUntil(t, "the TUI", func() bool { return read("tui.args") != "" })
	if a := read("tui.args"); !strings.HasPrefix(a, "--server http://127.0.0.1:") || !strings.Contains(a, "password=set gff=absent") {
		t.Fatalf("TUI argv and env: %q", a)
	}
	if s := read("server.env"); !strings.Contains(s, "password=set gff=absent") {
		t.Fatalf("server env: %q", s)
	}
	record := filepath.Join(e.state, opencodeDir, pane+".json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("no record: %v", err)
	}
	emit := func(line string) {
		f, err := os.OpenFile(filepath.Join(e.work, "emit"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(line + "\n")
		f.Close()
	}
	// The stream may connect after the first emit: wait for the follower before going on.
	waitUntil(t, "a bound session", func() bool {
		emit(`{"type":"session.created","data":{"sessionID":"ses_1","version":"2.2.5"}}`)
		time.Sleep(100 * time.Millisecond)
		st := agentStates(t, e.sock)[pane]
		return st.Session == "ses_1"
	})
	st := agentStates(t, e.sock)[pane]
	if st.Harness != "opencode" || st.State != api.AgentStatusIdle || st.Source != api.StateSourceServer || st.Version != "2.2.5" {
		t.Fatalf("after session.created: %+v", st)
	}

	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: "fix it\nplease"}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitUntil(t, "the prompt", func() bool { return strings.Contains(read("prompts"), "ses_1: fix it") })
	waitState(t, e.sock, pane, "working", func(s api.AgentState) bool { return s.State == api.AgentStatusWorking })

	emit(`{"type":"permission.asked","location":{"directory":"/work"},"data":{"id":"per_1","sessionID":"ses_1","action":"shell","resources":["make test"]}}`)
	st = waitState(t, e.sock, pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked })
	if st.Request == nil || st.Request.ID != "per_1" || st.Request.Command != "make test" || st.Request.Cwd != "/work" {
		t.Fatalf("blocked: %+v", st)
	}
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: pane, Request: "per_1", Choice: api.ApprovalChoiceAllow}, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if r := read("replies"); r != "ses_1 per_1 once\n" {
		t.Fatalf("replies %q", r)
	}
	emit(`{"type":"permission.asked","data":{"id":"per_2","sessionID":"ses_1","action":"edit","resources":["/work/a.go"]}}`)
	waitState(t, e.sock, pane, "blocked again", func(s api.AgentState) bool { return s.Request != nil && s.Request.ID == "per_2" })
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: pane, Request: "per_2", Choice: api.ApprovalChoiceDeny}, nil); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if r := read("replies"); !strings.HasSuffix(r, "ses_1 per_2 reject\n") {
		t.Fatalf("replies %q", r)
	}
	emit(`{"type":"session.execution.succeeded","data":{"sessionID":"ses_1"}}`)
	waitState(t, e.sock, pane, "done", func(s api.AgentState) bool { return s.State == api.AgentStatusDone })

	var page api.ChatMessages
	if err := c.Call(ctx, "chat.page", api.ChatPageParams{Session: ss.Session}, &page); err != nil {
		t.Fatalf("chat.page: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Kind != api.ChatKindUser || page.Messages[0].Text != "fix it\nplease" {
		t.Fatalf("chat %+v", page.Messages)
	}

	// A restarted gff-hab finds the server from the record and binds the recorded session.
	c.Close()
	e.restart(false)
	waitState(t, e.sock, pane, "rebound after a restart", func(s api.AgentState) bool {
		return s.Session == "ses_1" && s.Source == api.StateSourceServer && s.State == api.AgentStatusIdle
	})
	c = e.control()
	defer c.Close()
	if err := c.Call(ctx, "pane.close", map[string]string{"pane": pane}, nil); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitUntil(t, "the server's end", func() bool { return read("server.exit") != "" })
	waitUntil(t, "the record's removal", func() bool { _, err := os.Stat(record); return os.IsNotExist(err) })
}
