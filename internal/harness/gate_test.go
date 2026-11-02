package harness

import (
	"context"
	"testing"
	"time"
)

// TestGateSetWait: the hab-side setting hab.permission_wait drives the gate at run time
// (app.md 20.4). A change applies to the next hook; a hook already waiting keeps its wait.
func TestGateSetWait(t *testing.T) {
	g := NewGate(0)
	if g.Enabled() {
		t.Fatal("a zero gate is on")
	}
	if h := g.Hold("p", "r0"); h.g != nil {
		t.Fatal("a hold on an off gate registered a wait")
	}
	g.SetWait(30 * time.Second)
	if !g.Enabled() {
		t.Fatal("SetWait did not turn the gate on")
	}
	h := g.Hold("p", "r1")
	if h.g == nil || h.wait != 30*time.Second {
		t.Fatalf("hold: %+v", h)
	}
	// Turned off while the hook waits: that hook keeps waiting for its answer.
	g.SetWait(0)
	done := make(chan Decision, 1)
	go func() {
		d, written, ok := h.Wait(context.Background())
		if ok {
			written(nil)
			done <- d
		}
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	ack, ok := g.Answer("p", "r1", Decision{Allow: true})
	if !ok {
		t.Fatal("the waiting hook was dropped by SetWait(0)")
	}
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	if d := <-done; !d.Allow {
		t.Fatalf("decision: %+v", d)
	}
	// The next hook does not wait.
	if g.Enabled() {
		t.Fatal("the gate is still on")
	}
	if h := g.Hold("p", "r2"); h.g != nil {
		t.Fatal("the next hold waited on an off gate")
	}
	// The wait is capped.
	g.SetWait(time.Hour)
	if h := g.Hold("p", "r3"); h.wait != MaxPermissionWait {
		t.Fatalf("cap: %v", h.wait)
	}
}
