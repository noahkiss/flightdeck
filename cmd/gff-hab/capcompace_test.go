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
// small as the engine's readings rise past the share and still small far past it (the owner's
// messages never wait, and get no pacing note), the governor's state and today's tokens in the
// catalog, and a send-now message marked.
func TestCapcomServicePace(t *testing.T) {
	t.Parallel()
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
	// 25% is 125%, past the old wait step: user turns stay on the small model.
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	waitUntil(t, "the reading in the catalog", func() bool { g, _ = governorOf(t, e); return g.UsedPct == 25 })
	if g.Basis != "plan" || g.Pace != "small" || g.EventPace != "wait" || g.SharePct != 20 || g.Until != "" || g.EventUntil == "" {
		t.Fatalf("governor at 125%%: %+v", g)
	}
	waitUntil(t, "today's tokens", func() bool {
		g, _ = governorOf(t, e)
		return g.InputTokens >= 200 && g.OutputTokens >= 20
	})

	// A message past the share runs: it reaches the engine, with no pacing note.
	run(t, "capcom", "say", "-socket", e.sock, "hello")
	waitUntil(t, "the message at the engine", func() bool { return strings.Contains(e.stubLog(), "hello") })

	// Send now: the item reaches the engine marked.
	if _, err := callJSON[api.Empty](t, c, "session.send", api.SessionSendParams{Session: st.Session, Text: "now please", Origin: api.SendOriginTyped, SendNow: true}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the send-now mark", func() bool { return strings.Contains(e.stubLog(), "send_now ") })
	if n := notesWith(threadText(t, c), "Pacing usage"); n != 0 {
		t.Fatalf("%d pacing notes, want none", n)
	}
	if strings.Contains(e.stubLog(), "pace wait") {
		t.Fatalf("the engine heard a user wait:\n%s", e.stubLog())
	}
}

// A Capcom on the Codex engine under the user's login (capcom-service.md 10.1): its readings, the
// rollout's token_count records, land as a Codex account under Capcom's own key, and the
// governor paces it by that account's weekly window. Under an API key the same reading paces
// nothing: the plan basis is the login's.
func TestCapcomCodexPace(t *testing.T) {
	t.Parallel()
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
	// Share 2 x 10% = 20%; 25% of the weekly window is 125%: user turns go to the small model.
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	waitUntil(t, "the small model", func() bool { g, _ = governorOf(t, e); return g.Pace == "small" })
	if g.Basis != "plan" || g.UsedPct != 25 || g.SharePct != 20 {
		t.Fatalf("governor past the share: %+v", g)
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

// A switch from the Claude engine to the Codex engine (capcom-service.md 10.1): the governor
// moves to the Codex account, which has no reading yet, so Capcom is unpaced until the Codex
// engine's first reading; then usage.list holds both of Capcom's accounts.
func TestCapcomPaceEngineSwitch(t *testing.T) {
	t.Parallel()
	e := newServiceEnv(t, "pace_daily_share = 10\n")
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	engine := func(kind string) {
		t.Helper()
		waitUntil(t, "the "+kind+" engine", func() bool {
			st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
			return st.Connected && st.Engine == kind
		})
	}
	engine("claude")
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	var g api.GovernorConsumer
	waitUntil(t, "the small model on claude", func() bool { g, _ = governorOf(t, e); return g.Pace == "small" && g.UsedPct == 25 })

	// The switch, as the owner makes it: pause, edit the config, resume.
	run(t, "capcom", "pause", "-socket", e.sock)
	waitUntil(t, "the engine paused", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.State == "paused" && !st.Connected
	})
	if err := os.WriteFile(e.config, []byte("[capcom]\nengine = \"codex\"\nauth = \"login\"\nrestart_delay = \"1s\"\npace_daily_share = 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "capcom", "resume", "-socket", e.sock)
	engine("codex")
	waitUntil(t, "unpaced on the codex account", func() bool { g, _ = governorOf(t, e); return g.Basis == "none" && g.Pace == "ok" })

	run(t, "capcom", "say", "-socket", e.sock, "usage 30")
	waitUntil(t, "the reading on codex", func() bool { g, _ = governorOf(t, e); return g.Basis == "plan" && g.UsedPct == 30 })
	var list api.UsageReadings
	call(t, c, "usage.list", api.UsageListParams{}, &list)
	got := map[string]int{}
	for _, a := range list.Accounts {
		for _, w := range a.Windows {
			if w.Name == "7d" {
				got[a.Harness] = w.UsedPct
			}
		}
	}
	if len(list.Accounts) != 2 || got["claude"] != 25 || got["codex"] != 30 {
		t.Fatalf("usage.list after the switch: %+v", list.Accounts)
	}
}

// The governor after a gff-hab restart and a pace edit (capcom-service.md 10.1, 19.1): the last
// weekly reading is kept in governor.json, so the pace holds with no new reading; a [capcom]
// pace_daily_share edit reaches the engine once the file held for one check; and with the key
// gone, settings.set moves the pace at once, with no restart: event pacing off, then the share;
// and those values hold across a second restart.
func TestCapcomPaceRestartAndEdit(t *testing.T) {
	t.Parallel()
	e := newServiceEnv(t, "pace_daily_share = 10\n")
	e.serve()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	waitUntil(t, "the engine connected", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, e.control(), "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	run(t, "capcom", "say", "-socket", e.sock, "usage 25")
	var g api.GovernorConsumer
	waitUntil(t, "the small model", func() bool { g, _ = governorOf(t, e); return g.Pace == "small" })

	e.restart(false)
	c := e.control()
	waitUntil(t, "capcom in the catalog after the restart", func() bool {
		var ok bool
		g, ok = governorOf(t, e)
		return ok
	})
	if g.Basis != "plan" || g.Pace != "small" || g.EventPace != "wait" || g.UsedPct != 25 {
		t.Fatalf("governor after a restart, before a reading: %+v", g)
	}

	write := func(extra string) {
		t.Helper()
		if err := os.WriteFile(e.config, []byte("[capcom]\nengine = \"claude\"\nrestart_delay = \"1s\"\n"+extra), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Share 20%: day 1's allowance is 40%, 25% is 62%.
	write("pace_daily_share = 20\n")
	waitUntil(t, "ok from the edit", func() bool { g, _ = governorOf(t, e); return g.Pace == "ok" && g.EventPace == "ok" })

	// No pace key: the default share, 15% (30% today), puts 25% at 83%, past the event step.
	write("")
	waitUntil(t, "the default share", func() bool { g, _ = governorOf(t, e); return g.SharePct == 30 && g.EventPace == "wait" })
	set := func(id, v string) {
		t.Helper()
		var cur api.SettingsResult
		call(t, c, "settings.get", api.SettingsGetParams{IDs: []string{id}}, &cur)
		if len(cur.Settings) != 1 || cur.Settings[0].Source == api.SettingSourceConfig {
			t.Fatalf("%s: %+v", id, cur.Settings)
		}
		call(t, c, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{{ID: id, Value: api.RawJSON(v), Revision: cur.Settings[0].Revision}}}, &cur)
	}
	// From the app: event pacing off, then a share of 9% (18% today, 25% is 139%).
	set("capcom.pace.events", "false")
	waitUntil(t, "event turns run", func() bool { g, _ = governorOf(t, e); return g.EventPace == "ok" })
	set("capcom.pace.daily_share", "9")
	waitUntil(t, "the small model from the app", func() bool { g, _ = governorOf(t, e); return g.Pace == "small" && g.SharePct == 18 })
	if g.EventPace != "ok" || g.Until != "" {
		t.Fatalf("governor after the app's edits: %+v", g)
	}

	// The app's values outlive a gff-hab restart: the governor paces by them, not the defaults.
	e.restart(false)
	waitUntil(t, "capcom in the catalog after the second restart", func() bool {
		var ok bool
		g, ok = governorOf(t, e)
		return ok && g.Basis == "plan" && g.SharePct == 18
	})
	if g.Pace != "small" || g.SharePct != 18 || g.EventPace != "ok" {
		t.Fatalf("governor after a restart with the app's values: %+v", g)
	}
}
