package main

// Event turns by level (capcom-service.md 9.4, 9.6, stage A10): at capcom.event_actions = all an
// event turn may use every word but card and fetch with no job, each call counted against the
// per-turn cap and shown as a card that names the event; card, fetch and round changes still need
// a job's word or stay refused. Below all, the word drive lets a job's turn type into panes.

import (
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomevents"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/settings"
)

func TestCapcomEventsAll(t *testing.T) {
	t.Parallel()
	h := newJobsHarness(t)
	h.set[settings.CapcomEventActions] = `"all"`

	// A notice-only event turn hands off and types into a pane, each with a card naming the
	// notice.
	n, _, _ := h.e.thread.Add(capcomthread.Item{Kind: capcomthread.KindEvent, Key: "notice:n1", Text: "build failed\nexit 2"})
	h.e.events.Add(capcomevents.Event{Item: n.ID})
	h.take(t, "t1")
	if _, err := h.handOff(); err != nil {
		t.Fatalf("hand_off at all: %v", err)
	}
	if _, err := h.call("pane.type", api.PaneTypeParams{Session: "api-lead", Text: "/compact", Enter: true}); err != nil {
		t.Fatalf("pane.type at all: %v", err)
	}
	if _, err := h.call("session.action", map[string]string{"session": "api-lead", "action": "compact"}); err != nil {
		t.Fatalf("session.action at all: %v", err)
	}
	cards := h.jobCards()
	if len(cards) != 3 || !strings.Contains(cards[1].Text, "After a notice: build failed, Capcom used drive: type into api-lead: /compact") ||
		cards[1].Card.Ref != "" || cards[1].Turn != "t1" {
		t.Fatalf("cards %+v", cards)
	}
	// card, fetch and round changes still need a job's word, or stay refused.
	if _, err := h.call("personal.calendar_add", map[string]string{"title": "x"}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("a card at all with no job: %v", err)
	}
	if _, err := h.e.eventGate("t1", "WebFetch"); err == nil {
		t.Error("a fetch at all with no job")
	}
	if _, err := h.call("round.create", api.RoundCreateParams{}); errKind(err) != api.ErrorKindDenied {
		t.Errorf("round.create at all: %v", err)
	}
	// The group check and the batch input see every word but card and fetch.
	if !h.e.turnAllows("group") || h.e.turnAllows("card") {
		t.Error("turnAllows at all")
	}
	if b := h.e.events.TurnBatches("t1"); len(b) != 1 || !slices.Equal(h.e.batchActions(b[0]),
		[]string{"hand_off", "start_session", "set_up_project", "group", "drive"}) {
		t.Errorf("batch actions at all: %v", h.e.batchActions(b[0]))
	}
	// The eleventh action of one turn is refused (default 10).
	for i := 4; i <= 10; i++ {
		if _, err := h.handOff(); err != nil {
			t.Fatalf("action %d: %v", i, err)
		}
	}
	if _, err := h.handOff(); errKind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "used its 10 action") {
		t.Fatalf("the eleventh: %v", err)
	}
	// A job that lists the word is named on its card at all, as at allow.
	h.job(t, "m1", "mail", "t2", "hand_off")
	if _, err := h.handOff(); err != nil {
		t.Fatal(err)
	}
	if c := h.jobCards(); !strings.Contains(c[len(c)-1].Text, "For job host-b/mail") {
		t.Fatalf("job card %q", c[len(c)-1].Text)
	}
	// An agent's report took no batch: it acts at all, counted apart.
	h.turn = capcomengine.Status{Turn: "r1", Origin: "agent"}
	if _, err := h.handOff(); err != nil {
		t.Fatalf("an agent's report turn: %v", err)
	}
	if c := h.jobCards(); !strings.Contains(c[len(c)-1].Text, "After an agent's report") {
		t.Fatalf("report card %q", c[len(c)-1].Text)
	}
}

// TestCapcomEventsDrive: below all, pane.type, pane.new and session.action need a job's drive
// word; a job without it is refused, and the A9 rules hold.
func TestCapcomEventsDrive(t *testing.T) {
	t.Parallel()
	h := newJobsHarness(t)
	h.set[settings.CapcomEventActions] = `"allow"`
	h.job(t, "m0", "mail", "t0", "hand_off")
	for _, m := range []string{"pane.type", "pane.new", "session.action"} {
		if _, err := h.call(m, map[string]string{"session": "x", "text": "y", "action": "compact"}); errKind(err) != api.ErrorKindDenied ||
			!strings.Contains(err.Error(), "allows drive") {
			t.Errorf("%s with no drive word: %v", m, err)
		}
	}
	h.job(t, "m1", "keeper", "t1", "drive")
	if _, err := h.call("pane.type", api.PaneTypeParams{Session: "api-lead", Text: "/compact"}); err != nil {
		t.Fatalf("pane.type under drive: %v", err)
	}
	if _, err := h.handOff(); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("hand_off under a drive-only job: %v", err)
	}
	if b := h.e.events.TurnBatches("t1"); len(b) != 1 || !slices.Equal(h.e.batchActions(b[0]), []string{"drive"}) {
		t.Errorf("batch actions: %v", h.e.batchActions(b[0]))
	}
	// A notice-only turn below all allows nothing.
	n, _, _ := h.e.thread.Add(capcomthread.Item{Kind: capcomthread.KindEvent, Key: "notice:n2", Text: "disk"})
	h.e.events.Add(capcomevents.Event{Item: n.ID})
	h.take(t, "t2")
	if _, err := h.call("pane.type", api.PaneTypeParams{Session: "a", Text: "b"}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a notice-only turn below all: %v", err)
	}
}
