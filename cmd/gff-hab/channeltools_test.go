package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// paneFake is a pane's route to the hab: it records each channel call and answers from its
// table.
type paneFake struct {
	calls   []string
	params  []any
	answers map[string]any
}

func (*paneFake) fromPane() bool { return true }

func (f *paneFake) Call(_ context.Context, method string, params, result any) error {
	f.calls = append(f.calls, method)
	f.params = append(f.params, params)
	b, _ := json.Marshal(f.answers[method])
	return json.Unmarshal(b, result)
}

// plainFake is the control socket: no session is the caller.
type plainFake struct{ calls int }

func (f *plainFake) Call(context.Context, string, any, any) error { f.calls++; return nil }

// TestChannelTools: channel_send and channel_wait call channel.send and channel.wait through
// the pane route with the tool's arguments, refuse outside a pane before any call, and say
// what came (mesh.md 6.6).
func TestChannelTools(t *testing.T) {
	ctx := context.Background()
	send, _ := findIn(allTools(), "channel_send")
	wait, _ := findIn(allTools(), "channel_wait")

	plain := &plainFake{}
	for _, tl := range []kitTool{send, wait} {
		if _, _, err := tl.run(ctx, plain, toolArgs{"to": "bee/bob", "text": "x"}); err == nil || !strings.Contains(err.Error(), "from a session's pane") {
			t.Fatalf("%s outside a pane: %v", tl.Name, err)
		}
	}
	if plain.calls != 0 {
		t.Fatalf("%d calls outside a pane", plain.calls)
	}

	f := &paneFake{answers: map[string]any{"channel.send": api.ChannelSent{ID: "m1", To: "bee/bob", State: api.ChannelStateQueued}}}
	_, text, err := send.run(ctx, f, toolArgs{"to": "bob", "text": "hello", "reply_to": "r1"})
	if err != nil || !strings.Contains(text, "Sent message m1 to bee/bob: queued") {
		t.Fatalf("send: %v %q", err, text)
	}
	if p := f.params[0].(api.ChannelSendParams); f.calls[0] != "channel.send" || p.To != "bob" || p.Text != "hello" || p.ReplyTo != "r1" || p.From != "" {
		t.Fatalf("send params %s %+v", f.calls[0], p)
	}

	f = &paneFake{answers: map[string]any{"channel.wait": api.ChannelWaitResult{Message: &api.ChannelInbound{ID: "m2", From: "bee/bob", Text: "done here"}}}}
	_, text, err = wait.run(ctx, f, toolArgs{})
	if err != nil || !strings.Contains(text, "agent message from 'bee/bob'") || !strings.Contains(text, "done here") || !strings.Contains(text, `channel_send to "bee/bob", with reply_to m2`) {
		t.Fatalf("wait message: %v %q", err, text)
	}
	if p := f.params[0].(api.ChannelWaitParams); p.Session != "" || p.Agent != "" || p.TimeoutMS != 0 {
		t.Fatalf("wait params %+v: the route names the session and the pane default applies", p)
	}

	f = &paneFake{answers: map[string]any{"channel.wait": api.ChannelWaitResult{Agent: &api.ChannelAgent{Session: "bee/bob", State: api.AgentStatusIdle}}}}
	_, text, err = wait.run(ctx, f, toolArgs{"agent": "bee/bob", "for": "idle, done", "timeout_seconds": float64(30)})
	if err != nil || text != "bee/bob is idle.\n" {
		t.Fatalf("wait agent: %v %q", err, text)
	}
	if p := f.params[0].(api.ChannelWaitParams); p.Agent != "bee/bob" || len(p.For) != 2 || p.For[1] != api.AgentStatusDone || p.TimeoutMS != 30000 {
		t.Fatalf("wait params %+v", p)
	}

	f = &paneFake{answers: map[string]any{"channel.wait": api.ChannelWaitResult{TimedOut: true}}}
	if _, text, err = wait.run(ctx, f, toolArgs{"timeout_seconds": "5"}); err != nil || !strings.Contains(text, "Nothing came within the time") {
		t.Fatalf("wait timed out: %v %q", err, text)
	}
	for _, a := range []toolArgs{{"for": "idle"}, {"agent": "bob", "for": "sleepy"}, {"timeout_seconds": float64(601)}, {"timeout_seconds": float64(0)}} {
		f = &paneFake{}
		if _, _, err := wait.run(ctx, f, a); err == nil || len(f.calls) != 0 {
			t.Fatalf("%v: %v, %d calls", a, err, len(f.calls))
		}
	}
	if wait.Time <= mcpCallTime || wait.Time < actionCallTime {
		t.Fatalf("channel_wait's call time %v cannot hold the longest wait", wait.Time)
	}

	// Over MCP: the tools are listed as actions and answer through the pane door.
	f = &paneFake{answers: map[string]any{"channel.wait": api.ChannelWaitResult{Agent: &api.ChannelAgent{Session: "bee/bob", State: api.AgentStatusExited}}}}
	m := &mcpServer{dial: func(context.Context) (caller, func(), error) { return f, func() {}, nil }}
	ans := mcpSession(t, m,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"channel_wait","arguments":{"agent":"bee/bob","for":"exited"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"channel_send","arguments":{"to":"bob"}}}`,
	)
	if r := ans["1"].Result; r == nil || r.IsError || r.text() != "bee/bob is exited.\n" {
		t.Fatalf("mcp wait: %+v", ans["1"])
	}
	if r := ans["2"].Result; r == nil || !r.IsError || !strings.Contains(r.text(), "needs text") {
		t.Fatalf("mcp send with no text: %+v", ans["2"])
	}
}
