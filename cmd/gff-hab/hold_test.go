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
	waitA := hold("p", envA, request)
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
	_, openedB := a.agents.Hook("p", envB)
	if w := hold("p", envB, openedB); w != nil {
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
