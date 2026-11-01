package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/progstatus"
)

// keyLog records the writes an agentService queues.
type keyLog struct {
	mu     sync.Mutex
	writes [][]byte
}

func (k *keyLog) QueueInputs(_ context.Context, _ string, data ...[]byte) []func() error {
	k.mu.Lock()
	k.writes = append(k.writes, data...)
	k.mu.Unlock()
	waits := make([]func() error, len(data))
	for i := range waits {
		waits[i] = func() error { return nil }
	}
	return waits
}

func claudeEnv(event, payload string, t int64) harness.Envelope {
	return harness.Envelope{Harness: "claude", Event: event, T: t, N: 1, Payload: json.RawMessage(payload)}
}

// holdingService is an agentService with the permission gate on and a pane bound to Claude
// session a, blocked on a permission request its hook opened.
func holdingService(t *testing.T) (a *agentService, keys *keyLog, request string) {
	t.Helper()
	keys = &keyLog{}
	a = &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf}
	a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	_, request = a.agents.Hook("p", claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`, 2))
	if request == "" {
		t.Fatal("the request did not open")
	}
	return a, keys, request
}

// TestHoldOnlyOwnRequest: a nested session's PermissionRequest, dropped by binding, never holds
// its hook, so it cannot take over the wait of the request the pane is blocked on, and the
// owner's answer reaches the hook that asked.
func TestHoldOnlyOwnRequest(t *testing.T) {
	a, _, request := holdingService(t)
	hold := a.holdPermission(func(string) int { return 1 })
	envA := claudeEnv("PermissionRequest", `{}`, 2)
	waitA := hold("p", envA, func() string { return request })
	if waitA == nil {
		t.Fatal("the request's own hook was not held")
	}
	got := make(chan []byte, 1)
	go func() {
		b, written := waitA(context.Background())
		if written != nil {
			written(nil)
		}
		got <- b
	}()
	envB := claudeEnv("PermissionRequest", `{"session_id":"b","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf x"}}`, 3)
	if w := hold("p", envB, func() string { _, opened := a.agents.Hook("p", envB); return opened }); w != nil {
		t.Fatal("a nested session's dropped request was held")
	}
	waitUntil(t, "the hook to wait", func() bool { return a.gate.Waiting("p", request) })
	written, ok := a.gate.Answer("p", request, harness.Decision{Allow: true})
	if !ok {
		t.Fatal("no hook took the answer")
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if b := <-got; string(b) != `{"behavior":"allow"}` {
		t.Fatalf("the asking hook got %q", b)
	}
}

// TestAnswerLostByHook: when the hook cannot write the decision (it gave up at that instant),
// approval.answer reports unavailable, never success, and sends no keys of its own.
func TestAnswerLostByHook(t *testing.T) {
	a, keys, request := holdingService(t)
	go func() {
		_, written, ok := a.gate.Wait(context.Background(), "p", request)
		if ok {
			written(errors.New("broken pipe"))
		}
	}()
	waitUntil(t, "the hook to wait", func() bool { return a.gate.Waiting("p", request) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Answer(ctx, "c", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAllow})
	if errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("answer whose decision the hook lost: %v", err)
	}
	if len(keys.writes) != 0 {
		t.Fatalf("keys sent after a lost decision: %q", keys.writes)
	}
}

// TestHoldRegistersBeforePublish: the held hook's wait is registered in the same step as its
// state change, so an answer that comes before the hook starts waiting still reaches it as a
// decision, never as keys.
func TestHoldRegistersBeforePublish(t *testing.T) {
	keys := &keyLog{}
	a := &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf}
	a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	env := claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`, 2)
	var request string
	wait := a.holdPermission(func(string) int { return 1 })("p", env, func() string { _, request = a.agents.Hook("p", env); return request })
	if wait == nil || request == "" {
		t.Fatal("the hook was not held")
	}
	if !a.gate.Waiting("p", request) {
		t.Fatal("the wait was not registered when the holder returned")
	}
	written, ok := a.gate.Answer("p", request, harness.Decision{Allow: true})
	if !ok {
		t.Fatal("an answer before the hook waited found no wait")
	}
	b, w := wait(context.Background())
	w(nil)
	if string(b) != `{"behavior":"allow"}` || <-written != nil {
		t.Fatalf("the hook got %q", b)
	}
}

// TestApproversDropReleasesHook: when the last client that can answer goes away, the waiting
// hook ends with no decision at once, so Claude shows its own dialog.
func TestApproversDropReleasesHook(t *testing.T) {
	a, _, request := holdingService(t)
	approvers := 1
	wait := a.holdPermission(func(string) int { return approvers })("p", claudeEnv("PermissionRequest", `{}`, 2), func() string { return request })
	if wait == nil {
		t.Fatal("not held")
	}
	got := make(chan []byte, 1)
	go func() { b, _ := wait(context.Background()); got <- b }()
	a.releaseUnwatched(func(string) int { return approvers })
	if !a.gate.Waiting("p", request) {
		t.Fatal("a watched pane's hook was released")
	}
	approvers = 0
	a.releaseUnwatched(func(string) int { return approvers })
	select {
	case b := <-got:
		if b != nil {
			t.Fatalf("a released hook got %q", b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook still waits with no approver")
	}
}

// TestConcurrentAnswers: while one answer to a request is on its way, a second gets conflict and
// sends no keys; after the first was sent, the request takes no other answer.
func TestConcurrentAnswers(t *testing.T) {
	a, keys, request := holdingService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		first <- a.Answer(ctx, "c1", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAllow})
	}()
	waitUntil(t, "the first answer's keys", func() bool { keys.mu.Lock(); defer keys.mu.Unlock(); return len(keys.writes) == 1 })
	err := a.Answer(ctx, "c2", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceDeny})
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("second answer: %v", err)
	}
	// Claude goes on with the tool: OSC working, the first answer's read-back succeeds.
	a.agents.Report("p", progstatus.Report{State: "working", App: "claude"}, time.Now().UnixMilli(), true)
	if err := <-first; err != nil {
		t.Fatalf("first answer: %v", err)
	}
	err = a.Answer(ctx, "c2", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceDeny})
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("answer after the first was sent: %v", err)
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if len(keys.writes) != 1 || string(keys.writes[0]) != "1" {
		t.Fatalf("keys %q", keys.writes)
	}
}
