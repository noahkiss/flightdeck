package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomevents"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// eventsEngine is a capcomEngine with only what the writes-first paths read: the thread, the
// queue, the mark and the settings.
func eventsEngine(t *testing.T, set map[string]string) *capcomEngine {
	t.Helper()
	dir := t.TempDir()
	th, _, err := capcomthread.Open(filepath.Join(dir, "thread"), nil)
	if err != nil {
		t.Fatal(err)
	}
	q, err := capcomevents.Open(capcomevents.Config{Path: filepath.Join(dir, capcomevents.File)})
	if err != nil {
		t.Fatal(err)
	}
	svc := &capcomService{self: "dev-a"}
	svc.st.Home = "dev-a"
	e := &capcomEngine{svc: svc, device: "dev-a", thread: th, events: q, session: "svc-1", logf: t.Logf,
		moved:   func() string { return "bee" },
		setting: func(id string) json.RawMessage { return json.RawMessage(set[id]) }}
	return e
}

func roundMsg(id string, level string) api.ChannelMessage {
	return api.ChannelMessage{ID: id, From: api.ChannelEnd{Host: "host-b", Session: "round:cc"}, To: api.ChannelEnd{Session: "capcom"},
		Text: "check the builds", Round: &api.ChannelRound{Round: "r1", Name: "cc", Run: "run1", Trigger: "manual", Level: level,
			Events: []api.RoundEvent{{Kind: "notice.raised", Title: "disk full", Level: "active"}}}}
}

func TestDeliverRound(t *testing.T) {
	set := map[string]string{}
	e := eventsEngine(t, set)
	if st, r := e.DeliverRound(roundMsg("m1", "")); st != api.ChannelStateAccepted || r != "" {
		t.Fatalf("first: %s %s", st, r)
	}
	if st, _ := e.DeliverRound(roundMsg("m1", "")); st != api.ChannelStateDuplicate {
		t.Fatalf("again: %s", st)
	}
	it, ok := e.thread.ByKey("round:m1")
	if !ok || it.Kind != capcomthread.KindRound || it.Author.Address != "host-b/round:cc" || len(it.Context) != 1 || it.Context[0].Title != "notice.raised: disk full (active)" {
		t.Fatalf("round item %+v", it)
	}
	if w := e.events.Waiting(); len(w) != 1 || w[0].Item != it.ID || w[0].Round != "m1" || w[0].Urgent {
		t.Fatalf("waiting %+v", w)
	}
	if e.DeliverRound(roundMsg("m2", "time-sensitive")); !e.events.Waiting()[1].Urgent {
		t.Fatal("a time-sensitive round message is not urgent")
	}
	set[settings.CapcomEventsRoundMessages] = "false"
	if st, r := e.DeliverRound(roundMsg("m3", "")); st != api.ChannelStateDeclined || r != "off" {
		t.Fatalf("off: %s %s", st, r)
	}
	if st, _ := e.DeliverRound(roundMsg("m1", "")); st != api.ChannelStateDuplicate {
		t.Fatalf("a repeat while off: %s", st)
	}
	e.svc.st.Home = "dev-b"
	if st, r := e.DeliverRound(roundMsg("m4", "")); st != api.ChannelStateRefused || r != "Capcom moved to bee" {
		t.Fatalf("not home: %s %s", st, r)
	}
}

func TestObserveNotices(t *testing.T) {
	set := map[string]string{}
	e := eventsEngine(t, set)
	raise := func(id string, n api.Notice) {
		n.ID = id
		e.observe(structure.Event{Type: control.EventNoticeRaised, ID: id, Data: n})
	}
	raise("n1", api.Notice{Title: "quiet one", Level: api.NoticeLevelPassive})
	raise("n2", api.Notice{Title: "disk full", Body: "93%", Level: api.NoticeLevelActive})
	raise("n2", api.Notice{Title: "disk full", Body: "93%", Level: api.NoticeLevelActive})
	raise("n3", api.Notice{Title: "build broke", Level: api.NoticeLevelTimeSensitive})
	raise("n4", api.Notice{Title: "Capcom crashed", Level: api.NoticeLevelActive, Session: "svc-1"})
	w := e.events.Waiting()
	if len(w) != 2 || w[0].Urgent || !w[1].Urgent {
		t.Fatalf("waiting %+v", w)
	}
	if it, _ := e.thread.Get(w[0].Item); it.Kind != capcomthread.KindEvent || it.Text != "disk full\n93%" {
		t.Fatalf("event item %+v", it)
	}
	set[settings.CapcomEventsNotices] = "false"
	raise("n5", api.Notice{Title: "more", Level: api.NoticeLevelActive})
	raise("n6", api.Notice{Title: "round failed", Level: api.NoticeLevelActive, Round: &api.NoticeRound{Round: "r1"}})
	if len(e.events.Waiting()) != 3 {
		t.Fatalf("notices off, round ends on: %+v", e.events.Waiting())
	}
	set[settings.CapcomEventsRounds] = "false"
	raise("n7", api.Notice{Title: "round failed again", Level: api.NoticeLevelActive, Round: &api.NoticeRound{Round: "r1"}})
	if len(e.events.Waiting()) != 3 {
		t.Fatal("a round end with rounds off")
	}
	// Not the home host: nothing.
	e.session = ""
	set[settings.CapcomEventsNotices] = "true"
	raise("n8", api.Notice{Title: "later", Level: api.NoticeLevelActive})
	if len(e.events.Waiting()) != 3 {
		t.Fatal("a notice reached a hab without the service")
	}
}

func TestEventLimitsFromSettings(t *testing.T) {
	e := eventsEngine(t, map[string]string{settings.CapcomQuietHours: `"23:15-06:45"`, settings.CapcomDailyTurns: `5`,
		settings.CapcomBatchWindow: `30`, settings.CapcomBatchMax: `7`})
	l := e.eventLimits()
	if l.QuietStart != 23*60+15 || l.QuietEnd != 6*60+45 || l.DailyTurns != 5 || l.BatchWindow.Seconds() != 30 || l.BatchMax != 7 {
		t.Fatalf("limits %+v", l)
	}
	if d := eventsEngine(t, map[string]string{}).eventLimits(); d != capcomevents.DefaultLimits {
		t.Fatalf("defaults %+v", d)
	}
}
