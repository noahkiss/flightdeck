package capcomengine

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcomevents"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

// Event items go only in their batch, as one event input (9.2): a user message sent after them
// goes at once; taking the batch marks each item read; a user message joining the event turn
// makes it a user turn (9.4); the turn's end gives each round message its outcome (9.5).
func TestLinkEventBatches(t *testing.T) {
	var q *capcomevents.Queue
	var outs []string
	var mu sync.Mutex
	h := newHarnessWith(t, func(c *LinkConfig) {
		var err error
		q, err = capcomevents.Open(capcomevents.Config{Path: filepath.Join(t.TempDir(), capcomevents.File), Now: c.Now, After: c.After,
			Outcome: func(round, word string, _ time.Time) { mu.Lock(); outs = append(outs, round+" "+word); mu.Unlock() }})
		if err != nil {
			t.Fatal(err)
		}
		c.Events = q
	})
	q.Start()
	ev, _, _ := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindEvent, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}, Text: "disk full"})
	rd, _, _ := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindRound, Author: capcomthread.Author{Kind: capcomthread.AuthorAgent, Address: "round:cc"}, Text: "check the builds",
		Context: []capcomthread.Context{{Kind: "event", Title: "build failed"}}})
	q.Add(capcomevents.Event{Item: ev.ID})
	q.Add(capcomevents.Event{Item: rd.ID, Round: "m1"})
	e, _ := h.connect()
	e.send(readyMsg())
	u := h.user("hello")
	if m := e.next(); inputID(m) != u.ID {
		t.Fatalf("first input %s, want the user's %s", inputID(m), u.ID)
	}
	h.link.Kick()
	h.clock.Advance(capcomevents.DefaultLimits.BatchWindow)
	m := e.next()
	var in InputItem
	json.Unmarshal(m.Item, &in)
	if in.Kind != KindEventInput || len(in.Context) != 2 || in.Context[0].Text != "disk full" ||
		!strings.Contains(in.Context[1].Text, "check the builds\n- build failed") || !strings.Contains(in.Context[1].Title, "round:cc") {
		t.Fatalf("batch input %+v", in)
	}
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "event", Items: []string{in.ID}})
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + in.ID + `"`)})
	waitFor(t, "the batch read", func() bool {
		a, _ := h.thread.Get(ev.ID)
		b, _ := h.thread.Get(rd.ID)
		return !a.Read.IsZero() && !b.Read.IsZero()
	})
	if s := h.link.Status(); s.Turn != "t1" || s.UserTurn {
		t.Fatalf("event turn %+v", s)
	}
	u2 := h.user("and now")
	if m := e.next(); inputID(m) != u2.ID {
		t.Fatalf("input %s", inputID(m))
	}
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + u2.ID + `"`)})
	waitFor(t, "a user turn", func() bool { return h.link.Status().UserTurn })
	e.send(Message{Type: TReply, Turn: StrTurn("t1"), Text: "the disk is full"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess})
	waitFor(t, "the outcome", func() bool { mu.Lock(); defer mu.Unlock(); return slices.Equal(outs, []string{"m1 told"}) })
}
