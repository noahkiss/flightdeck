package main

// Capcom writes first, stage A10 (capcom-service.md 9.2, 9.3): quiet hours hold pushes, not
// turns; a budget of 0 is no cap; a person's own job neither counts nor waits, and only a round
// runner's message can say so.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomevents"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// TestEventLimitsA10: quiet hours off and a budget of 0 reach the queue as none.
func TestEventLimitsA10(t *testing.T) {
	t.Parallel()
	l := eventsEngine(t, map[string]string{settings.CapcomQuietHours: `"off"`, settings.CapcomDailyTurns: `0`}).eventLimits()
	if l.QuietStart != l.QuietEnd || l.DailyTurns != 0 {
		t.Fatalf("limits %+v", l)
	}
}

// TestDeliverRoundByPerson: by_person counts only from a round runner's message. The runner's
// message for a person's job queues a free event; the same field from a session's message is
// ignored, so no agent skips the budget.
func TestDeliverRoundByPerson(t *testing.T) {
	t.Parallel()
	e := eventsEngine(t, map[string]string{})
	m := roundMsg("m1", "")
	m.Round.ByPerson = true
	if st, r := e.DeliverRound(m); st != api.ChannelStateAccepted {
		t.Fatalf("runner: %s %s", st, r)
	}
	forged := roundMsg("m2", "")
	forged.Round.ByPerson = true
	forged.From.Session = "api-lead"
	if st, r := e.DeliverRound(forged); st != api.ChannelStateAccepted {
		t.Fatalf("session: %s %s", st, r)
	}
	free := map[string]bool{}
	for _, w := range e.events.Waiting() {
		free[w.Round] = w.Free
	}
	if !free["m1"] || free["m2"] {
		t.Fatalf("free %v", free)
	}
}

// TestQuietHoldsPush: a message an event turn writes in quiet hours pushes nothing and is
// counted; once a user message joins the turn, its messages push.
func TestQuietHoldsPush(t *testing.T) {
	t.Parallel()
	e := eventsEngine(t, map[string]string{})
	now := time.Now()
	m := now.Hour()*60 + now.Minute()
	// Quiet hours that hold now, whatever the hour the test runs.
	q, err := capcomevents.Open(capcomevents.Config{Path: filepath.Join(t.TempDir(), capcomevents.File), Host: now.Location(),
		Limits: func() capcomevents.Limits {
			return capcomevents.Limits{QuietStart: (m + 1440 - 60) % 1440, QuietEnd: (m + 60) % 1440, DailyTurns: 10, BatchWindow: time.Hour, BatchMax: 1}
		}})
	if err != nil {
		t.Fatal(err)
	}
	q.Start()
	e.events = q
	n, _, _ := e.thread.Add(capcomthread.Item{Kind: capcomthread.KindEvent, Key: "notice:n1", Text: "disk"})
	q.Add(capcomevents.Event{Item: n.ID})
	b := q.Released()
	if len(b) != 1 {
		t.Fatal("the event did not run in quiet hours")
	}
	q.Taken(b[0].ID, "t1")
	turn := capcomengine.Status{Turn: "t1", Origin: "event"}
	e.turnNow = func() capcomengine.Status { return turn }
	if !e.quietHolds(capcomthread.Item{Turn: "t1"}) || q.Held() != 1 {
		t.Fatalf("an event turn's message: held %d", q.Held())
	}
	turn.UserTurn = true
	if e.quietHolds(capcomthread.Item{Turn: "t1"}) {
		t.Fatal("a user turn's message was held")
	}
}
