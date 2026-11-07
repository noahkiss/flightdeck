package capcomengine

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

// The pace goes to the engine before any input on each connection, and again on a change; a
// send-now item carries its mark; usage readings and turn usage reach their hooks (10.1).
func TestLinkPace(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var readings, turns []string
	h.link.cfg.OnUsage = func(_ string, r json.RawMessage) { mu.Lock(); readings = append(readings, string(r)); mu.Unlock() }
	h.link.cfg.OnTurnUsage = func(origin string, in, out int64, usd float64) {
		mu.Lock()
		turns = append(turns, origin)
		mu.Unlock()
	}
	h.link.SetPace("wait", "2026-11-07T00:00:00Z", "wait", "2026-11-08T00:00:00Z")
	h.user("first")
	it, _, _ := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindMessage, Author: capcomthread.Author{Kind: capcomthread.AuthorUser}, Text: "now", SendNow: true})
	e, _ := h.connect()
	e.send(readyMsg())
	m := e.next()
	if m.Type != TPace || m.Pace != "wait" || m.Until != "2026-11-07T00:00:00Z" || m.EventPace != "wait" {
		t.Fatalf("first message after ready %+v, want the pace", m)
	}
	if m = e.next(); m.Type != TInput {
		t.Fatalf("%+v", m)
	}
	m = e.next()
	var in InputItem
	json.Unmarshal(m.Item, &in)
	if in.ID != it.ID || !in.SendNow {
		t.Fatalf("send-now input %+v", in)
	}
	h.link.SetPace("small", "", "wait", "2026-11-08T00:00:00Z")
	h.link.SetPace("small", "", "wait", "2026-11-08T00:00:00Z") // unchanged: not sent again
	h.link.SetPace("ok", "", "ok", "")
	if m = e.next(); m.Type != TPace || m.Pace != "small" {
		t.Fatalf("%+v", m)
	}
	if m = e.next(); m.Type != TPace || m.Pace != "ok" || m.EventPace != "ok" {
		t.Fatalf("%+v", m)
	}
	e.send(Message{Type: TUsage, Reading: json.RawMessage(`{"status":"allowed"}`)})
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "event"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess, InputTokens: 10, OutputTokens: 2, CostUSD: 0.01})
	e.acked(e.seq)
	mu.Lock()
	defer mu.Unlock()
	if len(readings) != 1 || readings[0] != `{"status":"allowed"}` || len(turns) != 1 || turns[0] != "event" {
		t.Fatalf("readings %q, turns %q", readings, turns)
	}
	// A new connection gets the newest pace first.
	e2, _ := h.connect()
	e2.send(readyMsg())
	if m := e2.next(); m.Type != TPace || m.Pace != "ok" {
		t.Fatalf("after a reconnect %+v", m)
	}
}
