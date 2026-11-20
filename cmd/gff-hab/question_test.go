package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/progstatus"
)

// A hand-written Claude Code 2.1.329 PermissionRequest for AskUserQuestion: one question with a
// single choice.
const questionPayload = `{"session_id":"sess-7","cwd":"/work/app","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which color should the status badge use?","header":"Color","options":[{"label":"Red","description":"Matches the error banner."},{"label":"Blue","description":"Matches the link color."}],"multiSelect":false}]}}`

// questionDialog is the dialog Claude Code draws for questionPayload, as the screen rows read.
var questionDialog = []string{" ☐ Color", "", "Which color should the status badge use?", "", "❯ 1. Red",
	"     Matches the error banner.", "  2. Blue", "     Matches the link color.", "  3. Type something.", "  4. Chat about this", "",
	"Enter to select · ↑/↓ to navigate · Esc to cancel"}

// questionService is an agentService with the gate off and a Claude pane blocked on the
// question its hook opened, with questionDialog on screen.
func questionService(t *testing.T) (a *agentService, keys *keyLog, request string) {
	t.Helper()
	keys = &keyLog{}
	a = &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(0), input: keys, logf: t.Logf,
		screen: func(string) ([]string, error) { return questionDialog, nil }}
	a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"sess-7","hook_event_name":"SessionStart"}`, 1))
	_, request = a.agents.Hook("p", claudeEnv("PermissionRequest", questionPayload, 2))
	if request == "" {
		t.Fatal("the question did not open")
	}
	return a, keys, request
}

// TestQuestionAnswerKeys: with no hook waiting, choice answer with an option's label sends the
// digit of the row that carries it, and returns once Claude goes on. Choice allow is invalid.
// Free text sends nothing, answers unavailable with the words the app matches, and leaves the
// question open for another answer.
func TestQuestionAnswerKeys(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, keys, request := questionService(t)
	// An allow carries no answers: invalid, nothing sent, the question open for an answer.
	err := a.Answer(ctx, "c", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAllow})
	if errKind(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "answer it with answer") {
		t.Fatalf("allow on a question: %v", err)
	}
	err = a.Answer(ctx, "c", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAnswer, Text: "Green, but darker"})
	if errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "answer it in the terminal") {
		t.Fatalf("free text by keys: %v", err)
	}
	if len(keys.writes) != 0 {
		t.Fatalf("keys sent for free text: %q", keys.writes)
	}
	err = a.Answer(ctx, "c", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAnswer, Text: ""})
	if errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("an empty answer: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.Answer(ctx, "c", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAnswer, Text: "Blue"})
	}()
	waitUntil(t, "the option's key", func() bool { keys.mu.Lock(); defer keys.mu.Unlock(); return len(keys.writes) == 1 })
	// Claude runs the tool with the answer: OSC working, the read-back succeeds.
	a.agents.Report("p", progstatus.Report{State: "working", App: "claude"}, time.Now().UnixMilli(), true)
	if err := <-done; err != nil {
		t.Fatalf("answer by keys: %v", err)
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if len(keys.writes) != 1 || string(keys.writes[0]) != "2" {
		t.Fatalf("keys %q", keys.writes)
	}
}

// TestQuestionAnswerPermission: choice answer on a permission, not a question, is invalid and
// sends nothing; the permission still takes allow after it.
func TestQuestionAnswerPermission(t *testing.T) {
	t.Parallel()
	a, keys, request := holdingService(t)
	a.gate = harness.NewGate(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Answer(ctx, "c", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAnswer, Text: "yes"})
	if errKind(err) != api.ErrorKindInvalid || len(keys.writes) != 0 {
		t.Fatalf("answer on a permission: %v, keys %q", err, keys.writes)
	}
	if _, err := a.agents.BeginAnswer("p", request); err != nil {
		t.Fatalf("the refused answer kept its claim: %v", err)
	}
}

// TestQuestionAnswerHook: with the owner's gate on, the -wait hook of a question takes the
// answer as an allow whose updatedInput is the question's own input plus answers, keyed by
// question text, and prints it as Claude Code's hook output; no key reaches the pane. Free
// text goes the same way.
func TestQuestionAnswerHook(t *testing.T) {
	t.Parallel()
	t.Run("option", func(t *testing.T) { questionAnswerHook(t, "Blue") })
	t.Run("free text", func(t *testing.T) { questionAnswerHook(t, "Green, but darker") })
}

func questionAnswerHook(t *testing.T, answer string) {
	e := newHabEnv(t, 0)
	e.srv.stop(t)
	e.srv = startServeWith(t, e.sock, e.state, nil, "-holders", e.hdir, "-permission-wait", "20s")
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	if _, err := (api.Client{Caller: c}).EventsSubscribe(ctx, api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}
	text := "ask me"
	want := "\x1b[200~" + text + "\x1b[201~\r"
	ss := startFakeClaude(t, e, c, len(want))
	writeFile(t, filepath.Join(e.work, "permission.json"), questionPayload)
	writeFile(t, filepath.Join(e.work, "go-idle"), "")
	waitState(t, e.sock, ss.Pane, "OSC idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ss.Session, Text: text}, nil); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e.sock, ss.Pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked && s.Request != nil })
	if st.Request.Tool != harness.ToolQuestion {
		t.Fatalf("request %+v", st.Request)
	}
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: ss.Pane, Request: st.Request.ID, Choice: api.ApprovalChoiceAnswer, Text: answer, Via: api.AnswerViaApp}, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	waitUntil(t, "the hook's decision", func() bool { _, err := os.Stat(filepath.Join(e.work, "asked")); return err == nil })
	var out struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior     string         `json:"behavior"`
				UpdatedInput map[string]any `json:"updatedInput"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	b, _ := os.ReadFile(filepath.Join(e.work, "decision"))
	if err := json.Unmarshal(b, &out); err != nil || out.HookSpecificOutput.HookEventName != "PermissionRequest" || out.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("hook output %q: %v", b, err)
	}
	var payload struct {
		ToolInput map[string]any `json:"tool_input"`
	}
	json.Unmarshal([]byte(questionPayload), &payload)
	got := out.HookSpecificOutput.Decision.UpdatedInput
	wantAnswers := map[string]any{"Which color should the status badge use?": answer}
	if !reflect.DeepEqual(got["answers"], wantAnswers) {
		t.Fatalf("answers %v in %q", got["answers"], b)
	}
	delete(got, "answers")
	if !reflect.DeepEqual(got, payload.ToolInput) {
		t.Fatalf("the question's input changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.work, "key")); err == nil {
		t.Fatal("a structured answer also sent keys")
	}
	writeFile(t, filepath.Join(e.work, "end"), "")
}
