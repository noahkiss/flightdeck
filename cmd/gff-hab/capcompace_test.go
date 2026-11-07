package main

import (
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// governorOf returns the capcom consumer of this hab's own catalog.
func governorOf(t *testing.T, e *serviceEnv) (api.GovernorConsumer, bool) {
	t.Helper()
	l, err := callJSON[api.CatalogList](t, e.control(), "catalog.list", api.CatalogListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range l.Hosts {
		if !h.Own || h.Catalog.Governor == nil {
			continue
		}
		for _, c := range h.Catalog.Governor.Consumers {
			if c.ID == "capcom" {
				return c, true
			}
		}
	}
	return api.GovernorConsumer{}, false
}

func notesWith(items []api.CapcomItem, prefix string) int {
	n := 0
	for _, it := range items {
		if it.Kind == "note" && strings.HasPrefix(it.Text, prefix) {
			n++
		}
	}
	return n
}

// Capcom paced by the usage governor end to end (capcom-service.md 10.1, 18 G), with the stub
// engine and hand-written readings: unpaced with no reading, the pace before the first turn,
// small and then wait as the engine's readings rise past the share, one "Pacing usage" note per
// wait, the governor's state and today's tokens in the catalog, and a send-now message marked.
func TestCapcomServicePace(t *testing.T) {
	e := newServiceEnv(t, "pace_daily_share = 10\n")
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	var st api.CapcomServiceStatus
	waitUntil(t, "the engine connected", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	// No reading yet: listed unpaced, and the engine has the pace before any input.
	var g api.GovernorConsumer
	waitUntil(t, "capcom in the catalog's governor section", func() bool {
		var ok bool
		g, ok = governorOf(t, e)
		return ok
	})
	if g.Basis != "none" || g.Pace != "ok" {
		t.Fatalf("governor before a reading: %+v", g)
	}
	waitUntil(t, "the first pace", func() bool { return strings.Contains(e.stubLog(), "pace ok  ok ") })

	// The window started a day and an hour ago: the share is 2 x 10% = 20%. 21% is 105% of it.
	run(t, "capcom", "say", "-socket", e.sock, "usage 21")
	waitUntil(t, "the small pace", func() bool { return strings.Contains(e.stubLog(), "pace small  wait 20") })
	// 25% is 125%: user turns wait, until the third window day makes the share 30%.
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	waitUntil(t, "the wait pace", func() bool { return strings.Contains(e.stubLog(), "pace wait 20") })
	waitUntil(t, "the wait in the catalog", func() bool { g, _ = governorOf(t, e); return g.Pace == "wait" })
	if g.Basis != "plan" || g.Pace != "wait" || g.EventPace != "wait" || g.UsedPct != 25 || g.SharePct != 20 || g.Until == "" {
		t.Fatalf("governor at the wait: %+v", g)
	}
	waitUntil(t, "today's tokens", func() bool {
		g, _ = governorOf(t, e)
		return g.InputTokens >= 200 && g.OutputTokens >= 20
	})

	// A message during the wait gets one note; a second message no second note.
	run(t, "capcom", "say", "-socket", e.sock, "hello")
	run(t, "capcom", "say", "-socket", e.sock, "again")
	waitUntil(t, "the pacing note", func() bool { return notesWith(threadText(t, c), "Pacing usage until ") == 1 })
	if n := notesWith(threadText(t, c), "Pacing usage until "); n != 1 {
		t.Fatalf("%d pacing notes, want 1", n)
	}

	// Send now: the item reaches the engine marked.
	if _, err := callJSON[api.Empty](t, c, "session.send", api.SessionSendParams{Session: st.Session, Text: "now please", Origin: api.SendOriginTyped, SendNow: true}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the send-now mark", func() bool { return strings.Contains(e.stubLog(), "send_now ") })
	if n := notesWith(threadText(t, c), "Pacing usage until "); n != 1 {
		t.Fatalf("%d pacing notes after send now, want 1", n)
	}
}
