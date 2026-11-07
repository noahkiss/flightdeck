package channels

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestHoldAndMove: a member's fresh-session swap (groups.md 9.3, 12.8). Held messages to the old
// session wait, are moved to the new session that took its name, and reach it in their order,
// delivered to the same address; none is refused as sent to a renamed session.
func TestHoldAndMove(t *testing.T) {
	l := newLocal("bob", "amy")
	l.busy["bob"] = true // bob works: messages wait
	h := newHab(t, "dev-a", "a", t.TempDir(), l)
	var ids []string
	for _, text := range []string{"one", "two", "three"} {
		ids = append(ids, send(t, h.s, "amy", "bob", text).ID)
	}
	release, err := h.s.Hold(context.Background(), "id-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	// The swap: bob is renamed, a new session takes the name, bob is free now.
	l.mu.Lock()
	l.sessions["bob-1"] = "id-bob"
	l.sessions["bob"] = "id-bob2"
	delete(l.busy, "bob")
	l.mu.Unlock()
	time.Sleep(60 * time.Millisecond) // delivery rounds run; the held messages stay
	l.mu.Lock()
	if n := len(l.delivered["bob"]) + len(l.delivered["bob-1"]); n != 0 {
		l.mu.Unlock()
		t.Fatalf("%d messages delivered while held", n)
	}
	l.mu.Unlock()
	for _, id := range ids {
		if st := status(t, h.s, id); st.State == api.ChannelStateRefused {
			t.Fatalf("message %s refused while held: %s", id, st.Reason)
		}
	}
	n, err := h.s.Move("id-bob", "id-bob2")
	if err != nil || n != 3 {
		t.Fatalf("moved %d, %v; want 3", n, err)
	}
	release()
	for _, id := range ids {
		if st := final(t, h.s, id); st.State != api.ChannelStateDelivered {
			t.Fatalf("message %s: %s %s", id, st.State, st.Reason)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	got := l.delivered["bob"]
	if len(got) != 3 || !strings.Contains(got[0], "one") || !strings.Contains(got[1], "two") || !strings.Contains(got[2], "three") {
		t.Fatalf("delivered to the new bob: %q", got)
	}
}

// TestHoldWaitsForAttempt: Hold waits for a delivery in flight, and gives up with ctx.
func TestHoldWaitsForAttempt(t *testing.T) {
	l := newLocal("bob", "amy")
	gate := make(chan struct{})
	l.gate = gate
	h := newHab(t, "dev-a", "a", t.TempDir(), l)
	id := send(t, h.s, "amy", "bob", "hi").ID
	waitFor(t, "a delivery in flight", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.inFlight == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.s.Hold(ctx, "id-bob", "bob"); err == nil {
		t.Fatal("Hold returned while a delivery was in flight")
	}
	close(gate)
	if st := final(t, h.s, id); st.State != api.ChannelStateDelivered {
		t.Fatalf("message: %s", st.State)
	}
	release, err := h.s.Hold(context.Background(), "id-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// TestHoldKeepsName: between the old session's rename and the new one's start no session holds
// the member's name; a message sent to it then waits for the held session and moves to the
// successor, never refused (review M3). After the release the name is free again.
func TestHoldKeepsName(t *testing.T) {
	l := newLocal("bob", "amy")
	h := newHab(t, "dev-a", "a", t.TempDir(), l)
	release, err := h.s.Hold(context.Background(), "id-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	// The rename: bob-1 is the old session, no session is named bob yet.
	l.mu.Lock()
	delete(l.sessions, "bob")
	l.sessions["bob-1"] = "id-bob"
	l.mu.Unlock()
	sent := send(t, h.s, "amy", "bob", "in the gap")
	time.Sleep(60 * time.Millisecond)
	if st := status(t, h.s, sent.ID); st.State == api.ChannelStateRefused {
		t.Fatalf("message to the held name refused: %s", st.Reason)
	}
	// The new session takes the name; the held message moves to it.
	l.mu.Lock()
	l.sessions["bob"] = "id-bob2"
	l.mu.Unlock()
	if n, err := h.s.Move("id-bob", "id-bob2"); err != nil || n != 1 {
		t.Fatalf("moved %d, %v; want 1", n, err)
	}
	release()
	if st := final(t, h.s, sent.ID); st.State != api.ChannelStateDelivered {
		t.Fatalf("message: %s %s", st.State, st.Reason)
	}
	l.mu.Lock()
	got := l.delivered["bob"]
	l.mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "in the gap") {
		t.Fatalf("delivered to the new bob: %q", got)
	}
	// Released, with no session named carol and no hold: refused as before.
	if _, err := h.s.Send(context.Background(), api.ChannelSendParams{From: "amy", To: "carol", Text: "x"}); err == nil {
		t.Fatal("a send to an unknown name went through")
	}
}
