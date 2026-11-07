package capcomengine

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

func (h *harnessT) capcom(turn, text string) capcomthread.Item {
	h.t.Helper()
	it, _, err := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindMessage, Author: capcomthread.Author{Kind: capcomthread.AuthorCapcom}, Turn: turn, Text: text})
	if err != nil {
		h.t.Fatal(err)
	}
	return it
}

func (h *harnessT) dividers() []capcomthread.Item {
	return h.thread.After("", func(it capcomthread.Item) bool { return it.Kind == capcomthread.KindDivider })
}

// noMessage fails when the hab sends anything but an ack within a short wait.
func (e *fakeEngine) noMessage(what string) {
	e.t.Helper()
	select {
	case m := <-e.in:
		e.t.Fatalf("%s: got %+v", what, m)
	case <-time.After(50 * time.Millisecond):
	}
}

// readyEngine connects an engine, answers ready and takes the inputs already waiting.
func (h *harnessT) readyEngine(waiting int) *fakeEngine {
	h.t.Helper()
	e, _ := h.connect()
	e.send(readyMsg())
	for range waiting {
		if m := e.next(); m.Type != TInput {
			h.t.Fatalf("want an input, got %+v", m)
		}
	}
	return e
}

// A turn that ends above reseed_at asks for one re-seed; a second turn end before the fresh
// session asks for none; the fresh session's start adds one divider with the summary the
// engine sent before it (6.2, 7.2).
func TestContextReseedAtSize(t *testing.T) {
	h := newHarness(t)
	u := h.user("I'm in Lisbon until Friday")
	e := h.readyEngine(1)
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user", Items: []string{u.ID}})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess, ContextUsed: 0.59})
	e.noMessage("a turn under 60%")
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t2"), Origin: "user"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t2"), Result: ResultSuccess, ContextUsed: 0.61})
	if m := e.next(); m.Type != TReseed || m.Reason != ReseedLimit {
		t.Fatalf("want reseed limit, got %+v", m)
	}
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t3"), Origin: "user"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t3"), Result: ResultSuccess, ContextUsed: 0.7})
	e.noMessage("a second reseed before the fresh session")
	e.send(Message{Type: TSummary, Text: "The user is in Lisbon until Friday.", Through: u.ID})
	e.send(Message{Type: TSessionStarted, Session: "s2", Why: "reseed"})
	e.acked(e.seq)
	d := h.dividers()
	if len(d) != 1 || d[0].Text != "Earlier messages summarised" || d[0].Summary != "The user is in Lisbon until Friday." {
		t.Fatalf("dividers %+v", d)
	}
	if r := h.rec.Get(); r.SummaryThrough != u.ID || r.Current() != "s2" {
		t.Fatalf("record %+v", r)
	}
	// The fresh session's turns are measured again.
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t4"), Origin: "user"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t4"), Result: ResultSuccess, ContextUsed: 0.65})
	if m := e.next(); m.Type != TReseed {
		t.Fatalf("want a reseed after the fresh session, got %+v", m)
	}
}

// reseed_at set lower moves the size trigger; a value above it then asks.
func TestContextReseedAtSetting(t *testing.T) {
	lim := DefaultContextLimits
	lim.ReseedAt = 0.3
	h := newHarnessWith(t, func(c *LinkConfig) { c.Limits = func() ContextLimits { return lim } })
	e := h.readyEngine(0)
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess, ContextUsed: 0.31})
	if m := e.next(); m.Type != TReseed || m.Reason != ReseedLimit {
		t.Fatalf("want reseed at 31%% with reseed_at 30%%, got %+v", m)
	}
}

// context.request answers with the summary at hand, the items since it, and the seed's parts
// within seed_logbook_bytes, seed_messages and seed_messages_chars; open cards are listed, a
// memory card is not (6.2, 6.4).
func TestContextSeedCaps(t *testing.T) {
	lim := DefaultContextLimits
	lim.SeedMessages, lim.SeedMessagesChars, lim.SeedLogbookBytes = 3, 30, 20
	asked := 0
	h := newHarnessWith(t, func(c *LinkConfig) {
		c.Limits = func() ContextLimits { return lim }
		c.Logbook = func(max int) string {
			asked = max
			return strings.Repeat("é", 40) // 80 bytes
		}
	})
	old := h.user("covered by the summary")
	h.rec.Update(func(r *Record) { r.Summary, r.SummaryThrough = "old summary", old.ID })
	h.user("one two three")     // 13
	h.capcom("t1", "four five") // 9
	h.user("six seven eight")   // 15
	h.capcom("t2", "nine ten")  // 8
	card, _, _ := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindCard, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Text: "Start a session on box?", Card: &capcomthread.Card{Type: "confirmation", Ref: "cf-1", Choices: []string{"confirm", "cancel"}}})
	h.thread.Add(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Text: "Noted: vegetarian", Card: &capcomthread.Card{Type: capcomthread.CardMemory, Ref: "g-1", Choices: []string{capcomthread.ChoiceUndo}}})
	e := h.readyEngine(3)
	e.send(Message{Type: TContextRequest, Request: "r1", For: ForReseed})
	m := e.next()
	if m.Type != TContext || m.Request != "r1" || m.For != ForReseed || m.Summary != "old summary" {
		t.Fatalf("context %+v", m)
	}
	if len(m.Recent) != 4 || m.Recent[0].Text != "one two three" || m.Recent[3].Text != "nine ten" {
		t.Fatalf("recent %+v", m.Recent)
	}
	if m.Through != m.Recent[3].ID {
		t.Fatalf("through %s, want the newest recent item %s", m.Through, m.Recent[3].ID)
	}
	s := m.Seed
	if s == nil || asked != 20 || len(s.Logbook) > 20 || !utf8.ValidString(s.Logbook) {
		t.Fatalf("logbook part %q (asked %d)", s.Logbook, asked)
	}
	// The last 3 messages are 9+15+8 = 32 characters, past 30: the oldest goes.
	if len(s.Messages) != 2 || s.Messages[0].Text != "six seven eight" || s.Messages[1].Author != "capcom" {
		t.Fatalf("messages %+v", s.Messages)
	}
	if len(s.Open) != 1 || !strings.Contains(s.Open[0], card.ID) || !strings.Contains(s.Open[0], "cf-1") {
		t.Fatalf("open %+v", s.Open)
	}
	// A compaction's context carries no seed.
	e.send(Message{Type: TContextRequest, Request: "r2", For: ForCompact})
	if m := e.next(); m.Type != TContext || m.Request != "r2" || m.Seed != nil || len(m.Recent) != 4 {
		t.Fatalf("compact context %+v", m)
	}
	// summary_input_chars keeps the newest items.
	lim.SummaryInputChars = 20
	e.send(Message{Type: TContextRequest, Request: "r3", For: ForCompact})
	if m := e.next(); len(m.Recent) != 2 || m.Recent[0].Text != "six seven ei" {
		t.Fatalf("cut recent %+v", m.Recent)
	}
}

// After summary_quiet with no turn the hab hands the engine a quiet summary update; after
// reseed_quiet it asks for a re-seed when the last turn left the context above
// reseed_quiet_at, and not below it; a turn in between stops both (6.1, 6.2).
func TestContextQuiet(t *testing.T) {
	h := newHarness(t)
	u := h.user("hello")
	e := h.readyEngine(1)
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user", Items: []string{u.ID}})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess, ContextUsed: 0.3})
	e.acked(e.seq)
	h.clock.Advance(9 * time.Minute)
	e.noMessage("before summary_quiet")
	h.clock.Advance(time.Minute)
	if m := e.next(); m.Type != TContext || m.For != ForQuiet || len(m.Recent) != 1 || m.Recent[0].ID != u.ID {
		t.Fatalf("quiet context %+v", m)
	}
	h.clock.Advance(50 * time.Minute)
	if m := e.next(); m.Type != TReseed || m.Reason != ReseedQuiet {
		t.Fatalf("want reseed quiet, got %+v", m)
	}

	// Below reseed_quiet_at: a summary, no re-seed.
	e.send(Message{Type: TSessionStarted, Session: "s2", Why: "reseed"})
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t2"), Origin: "user"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t2"), Result: ResultSuccess, ContextUsed: 0.2})
	e.acked(e.seq)
	h.clock.Advance(10 * time.Minute)
	e.next() // the quiet summary
	h.clock.Advance(time.Hour)
	e.noMessage("a quiet re-seed under 25%")

	// A turn stops the timers.
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t3"), Origin: "user"})
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t3"), Result: ResultSuccess, ContextUsed: 0.5})
	e.acked(e.seq)
	h.clock.Advance(5 * time.Minute)
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t4"), Origin: "user"})
	e.acked(e.seq)
	h.clock.Advance(2 * time.Hour)
	e.noMessage("timers after a turn started")
}

// A compaction adds one divider with the summary the engine sent before it; the summary is cut
// to summary_max_chars (6.1, 6.3).
func TestContextCompactionDivider(t *testing.T) {
	lim := DefaultContextLimits
	lim.SummaryMaxChars = 1000
	h := newHarnessWith(t, func(c *LinkConfig) { c.Limits = func() ContextLimits { return lim } })
	e := h.readyEngine(0)
	e.send(Message{Type: TSummary, Text: strings.Repeat("s", 1500)})
	e.send(Message{Type: TCompacted})
	e.acked(e.seq)
	d := h.dividers()
	if len(d) != 1 || len(d[0].Summary) != 1000 || d[0].Text != "Earlier messages summarised" {
		t.Fatalf("dividers %d %+v", len(d), d)
	}
}

// "New conversation" adds one divider and empties the summary (7.5).
func TestContextNewConversation(t *testing.T) {
	h := newHarness(t)
	h.rec.Update(func(r *Record) { r.Summary = "kept so far" })
	u := h.user("hi")
	e := h.readyEngine(1)
	e.send(Message{Type: TSessionStarted, Session: "s1", Why: "first"})
	if err := h.link.Reseed(ReseedNew); err != nil {
		t.Fatal(err)
	}
	if m := e.next(); m.Type != TReseed || m.Reason != ReseedNew {
		t.Fatalf("%+v", m)
	}
	e.send(Message{Type: TSessionStarted, Session: "s2", Why: "new"})
	e.acked(e.seq)
	d := h.dividers()
	if len(d) != 1 || d[0].Text != "New conversation" || d[0].Summary != "" {
		t.Fatalf("dividers %+v", d)
	}
	if r := h.rec.Get(); r.Summary != "" || r.SummaryThrough < u.ID {
		t.Fatalf("record %+v", r)
	}
}
