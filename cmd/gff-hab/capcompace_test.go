package main

import (
	"os"
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

// A Capcom on the Codex engine under the user's login (capcom-service.md 10.1): its readings, the
// rollout's token_count records, land as a Codex account under Capcom's own key, and the
// governor paces it by that account's weekly window. Under an API key the same reading paces
// nothing: the plan basis is the login's.
func TestCapcomCodexPace(t *testing.T) {
	e := newServiceEnvFor(t, "codex", "")
	if err := os.WriteFile(e.config, []byte("[capcom]\nengine = \"codex\"\nauth = \"login\"\nrestart_delay = \"1s\"\npace_daily_share = 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	waitUntil(t, "the engine connected", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected && st.Engine == "codex"
	})
	var g api.GovernorConsumer
	waitUntil(t, "capcom in the catalog's governor section", func() bool {
		var ok bool
		g, ok = governorOf(t, e)
		return ok
	})
	if g.Basis != "none" {
		t.Fatalf("governor before a reading: %+v", g)
	}
	// Share 2 x 10% = 20%; 25% of the weekly window is 125%: user turns wait.
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	waitUntil(t, "the wait", func() bool { g, _ = governorOf(t, e); return g.Pace == "wait" })
	if g.Basis != "plan" || g.UsedPct != 25 || g.SharePct != 20 {
		t.Fatalf("governor at the wait: %+v", g)
	}
	var list api.UsageReadings
	call(t, c, "usage.list", api.UsageListParams{}, &list)
	if len(list.Accounts) != 1 {
		t.Fatalf("usage.list: %+v, want Capcom's account alone", list.Accounts)
	}
	a := list.Accounts[0]
	if a.Harness != "codex" || !strings.HasPrefix(a.Key, "codex-") || a.Label != "Plus" || len(a.Windows) != 2 ||
		a.Windows[1].Name != "7d" || a.Windows[1].UsedPct != 25 || a.Windows[1].StartsAt == "" {
		t.Fatalf("Capcom's Codex account: %+v", a)
	}
}

// The governor after a gff-hab restart and a pace_* edit (capcom-service.md 10.1): the last
// weekly reading is kept in governor.json, so the wait holds with no new reading; a user message
// rechecks with the config read again, so an edit that lifts the wait gives no pacing note; and
// an edit alone reaches the engine once the file held for one check.
func TestCapcomPaceRestartAndEdit(t *testing.T) {
	e := newServiceEnv(t, "pace_daily_share = 10\n")
	e.serve()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	waitUntil(t, "the engine connected", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, e.control(), "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	var g api.GovernorConsumer
	waitUntil(t, "the wait", func() bool { g, _ = governorOf(t, e); return g.Pace == "wait" })

	e.restart(false)
	c := e.control()
	waitUntil(t, "capcom in the catalog after the restart", func() bool {
		var ok bool
		g, ok = governorOf(t, e)
		return ok
	})
	if g.Basis != "plan" || g.Pace != "wait" || g.UsedPct != 25 {
		t.Fatalf("governor after a restart, before a reading: %+v", g)
	}

	// Share 20%: day 1's allowance is 40%, 25% is 62%. The message itself rechecks.
	write := func(share string) {
		t.Helper()
		if err := os.WriteFile(e.config, []byte("[capcom]\nengine = \"claude\"\nrestart_delay = \"1s\"\npace_daily_share = "+share+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("20")
	run(t, "capcom", "say", "-socket", e.sock, "hello")
	if n := notesWith(threadText(t, c), "Pacing usage until "); n != 0 {
		t.Fatalf("%d pacing notes after an edit that lifts the wait", n)
	}
	waitUntil(t, "ok in the catalog", func() bool { g, _ = governorOf(t, e); return g.Pace == "ok" })

	// Share 9%: 18%, 25% is 139%. No message: the config watch rechecks.
	write("9")
	waitUntil(t, "the wait again from the edit", func() bool { g, _ = governorOf(t, e); return g.Pace == "wait" })
}
