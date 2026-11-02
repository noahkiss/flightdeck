package gffclient

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// The rounds tests run gff's workspace over fake habs (rounds.md 10.3, gff-client.md 7.5).

func cronRound(id, name string, next time.Time) api.Round {
	r := api.Round{ID: id, RoundSpec: api.RoundSpec{Name: name, Mode: api.RoundModeFresh,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: "0 * * * *", Zone: "UTC"}}}
	if !next.IsZero() {
		r.NextSlot = next.UTC().Format(time.RFC3339)
	}
	return r
}

// TestNextText: the next run in the viewer's zone, by day, with the round's clock beside it only
// when that clock differs.
func TestNextText(t *testing.T) {
	now := time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, "-"},
		{now.Add(time.Hour), "today 09:00"},
		{now.Add(25 * time.Hour), "tomorrow 09:00"},
		{now.Add(73 * time.Hour), "Mar 13 09:00"},
	} {
		if got := nextText(now, c.at, ""); got != c.want {
			t.Errorf("%v: %q, want %q", c.at, got, c.want)
		}
	}
	zone := "Asia/Kolkata"
	at := now.Add(time.Hour)
	if loc, err := time.LoadLocation(zone); err == nil && at.In(loc).Format("15:04") != at.Format("15:04") {
		if got := nextText(now, at, zone); !strings.HasSuffix(got, "("+at.In(loc).Format("15:04")+" "+zone+")") {
			t.Errorf("round zone: %q", got)
		}
	}
}

// TestRunSessionBadge: a run session carries the round badge in the sidebar; the filter key
// hides it and shows it again, and a hidden run session that needs the owner still shows.
func TestRunSessionBadge(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.PutRound(cronRound("ra", "nightly", time.Now().Add(time.Hour)))
	_, runPane, err := a.AddRunSession("nightly-run", "ra", "ra-run1")
	if err != nil {
		t.Fatal(err)
	}
	a.AddSession("plain")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the badge on the run session", func(s string) bool {
		return line(s, roundBadge+"nightly-run") > 0 && line(s, "plain") > 0
	})

	hst.typ("R") // the sidebar has the keys at start
	hst.waitScreen("run sessions hidden", func(s string) bool {
		return line(s, "run sessions hidden") > 0 && line(s, "nightly-run") < 0 && line(s, "plain") > 0
	})
	a.SetAgent(runPane, api.AgentStatusBlocked, api.BlockedKindPermission)
	hst.waitScreen("a run session that needs the owner shows while hidden", func(s string) bool {
		return line(s, roundBadge+"nightly-run") > 0 && line(s, "run sessions hidden") > 0
	})
	a.SetAgent(runPane, api.AgentStatusWorking, "")
	hst.waitScreen("hidden again once working", func(s string) bool { return line(s, "nightly-run") < 0 })

	hst.typ(cmdKey + "R") // session mode's key
	hst.waitScreen("run sessions shown", func(s string) bool {
		return line(s, roundBadge+"nightly-run") > 0 && line(s, "run sessions hidden") < 0
	})
}

// TestBoardRounds: the board lists the rounds of two hosts in one list in next-run order, paused
// last; Run now calls round.run and shows a conflict in the hab's words; Pause and Unpause move
// the round; Run now opens the run's session once it shows.
func TestBoardRounds(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	b := fakeHab(t, d, "b")
	now := time.Now()
	a.PutRound(cronRound("ra", "alpha", now.Add(2*time.Hour)))
	held := cronRound("rg", "gamma", time.Time{})
	held.Paused = true
	a.PutRound(held)
	b.PutRound(cronRound("rb", "beta", now.Add(time.Hour)))
	hst := newHost(t, 140, 30, false)
	startWorkspace(t, hst, a.Sock, b.Sock)
	hst.typ(cmdKey + "g")
	order := func(s string, names ...string) bool {
		prev := line(s, " Rounds")
		if prev < 0 {
			return false
		}
		for _, n := range names {
			l := line(s, "  "+n+" ")
			if l <= prev {
				return false
			}
			prev = l
		}
		return true
	}
	hst.waitScreen("both hosts' rounds, soonest first, paused last", func(s string) bool {
		return order(s, "beta", "alpha", "gamma") && strings.Contains(s, "every 1h in UTC") && strings.Contains(s, "paused")
	})

	// The selection starts on beta (no session rows); j moves to alpha.
	a.SetRunLive("ra", true)
	hst.typ("jr")
	hst.waitScreen("the conflict in the hab's words", func(s string) bool {
		return strings.Contains(s, "run alpha: the round has a live run")
	})
	if calls := a.RunCalls(); len(calls) != 0 {
		t.Fatalf("a refused run was recorded: %v", calls)
	}

	hst.typ("kr")
	hst.waitScreen("beta's run from round.run_updated", func(s string) bool {
		l := line(s, "  beta ")
		return l > 0 && strings.Contains(strings.Split(s, "\n")[l], "running")
	})
	if calls := b.RunCalls(); !slices.Equal(calls, []string{"rb"}) {
		t.Fatalf("round.run calls on two: %v", calls)
	}

	hst.typ("p")
	hst.waitScreen("beta paused, below alpha", func(s string) bool { return order(s, "alpha", "beta", "gamma") })
	if r, _ := b.Round("rb"); !r.Paused {
		t.Fatal("beta not paused on its hab")
	}
	hst.typ("p")
	hst.waitScreen("beta unpaused", func(s string) bool {
		r, _ := b.Round("rb")
		return !r.Paused && line(s, "  beta ") > 0
	})

	// The run's session shows: Run now opens it, leaving the board.
	if _, _, err := b.AddRunSession("beta-run", "rb", "rb-run1"); err != nil {
		t.Fatal(err)
	}
	hst.waitScreen("the run session opened", func(s string) bool {
		return !strings.Contains(s, "status board") && strings.Contains(s, "beta-run")
	})
}

// TestBoardRoundsStale: a hab that goes down keeps its rounds on the board, stale, with no
// action; a host gff never reached shows the rounds another hab's catalog holds for it.
func TestBoardRoundsStale(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.Capabilities = []string{"catalog"}
	a.Stop()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	b := fakeHab(t, d, "b")
	b.PutRound(cronRound("rb", "kept", time.Now().Add(time.Hour)))
	c := fakeHab(t, d, "c")
	c.Stop() // never reached
	a.PutRound(cronRound("ra", "local", time.Now().Add(3*time.Hour)))
	a.SetCatalog(api.CatalogList{Hosts: []api.CatalogEntry{
		{Own: true, SeenAt: time.Now().UTC().Format(time.RFC3339), Catalog: api.Catalog{DeviceID: a.DeviceID, Host: "a"}},
		{Label: "c", Stale: true, SeenAt: time.Now().UTC().Format(time.RFC3339), Catalog: api.Catalog{DeviceID: c.DeviceID, Host: "c",
			Rounds: []api.CatalogRound{{ID: "rc", Name: "remote", Mode: api.RoundModeFresh, Trigger: "every 1d",
				LastRun: api.RoundRunStateSucceeded}}}},
	}})
	sock := func(h string) []hosts.Endpoint { return []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: h}} }
	hst := newHost(t, 140, 30, false)
	startWorkspaceSpecs(t, hst,
		hosts.Spec{Label: "one", Endpoints: sock(a.Sock)},
		hosts.Spec{Label: "two", Endpoints: sock(b.Sock)},
		hosts.Spec{Label: "three", Endpoints: sock(c.Sock), DeviceID: c.DeviceID})
	hst.typ(cmdKey + "g")
	row := func(s, name string) string {
		if l := line(s, "  "+name+" "); l >= 0 {
			return strings.Split(s, "\n")[l]
		}
		return ""
	}
	hst.waitScreen("the catalog's rounds for the host never reached", func(s string) bool {
		r := row(s, "remote")
		return strings.Contains(r, "three") && strings.Contains(r, "succeeded") && strings.Contains(r, "stale") &&
			row(s, "kept") != "" && !strings.Contains(row(s, "kept"), "stale")
	})

	b.Stop()
	hst.waitScreen("two's round stale", func(s string) bool { return strings.Contains(row(s, "kept"), "stale") })
	// The selection is on the first round: kept, the soonest. Run now refuses a stale row.
	hst.typ("r")
	hst.waitScreen("no action on a stale round", func(s string) bool { return strings.Contains(s, "two is offline") })
	if calls := b.RunCalls(); len(calls) != 0 {
		t.Fatalf("a stale round ran: %v", calls)
	}
}

// TestKeepCatalog: gff keeps a peer's copy of a host in its own host list, the newest one; it
// skips the answering hab's own entry (its snapshot is newer, its time another clock) and
// copies of devices no host names.
func TestKeepCatalog(t *testing.T) {
	u := &workspaceUI{cats: map[string]catCopy{}}
	ids := map[string]bool{"A": true, "B": true}
	at := func(m int) string { return time.Date(2026, 1, 1, 10, m, 0, 0, time.UTC).Format(time.RFC3339) }
	entry := func(id, seen string, own bool, name string) api.CatalogEntry {
		return api.CatalogEntry{Own: own, SeenAt: seen,
			Catalog: api.Catalog{DeviceID: id, Rounds: []api.CatalogRound{{ID: "r", Name: name}}}}
	}
	u.keepCatalog(entry("A", at(30), true, "own"), ids)
	u.keepCatalog(entry("C", at(30), false, "unlisted"), ids)
	if len(u.cats) != 0 {
		t.Fatalf("kept %v", u.cats)
	}
	u.keepCatalog(entry("A", at(5), false, "first"), ids)
	u.keepCatalog(entry("A", at(1), false, "older"), ids)
	if c, ok := u.catalog("A"); !ok || c.Rounds[0].Name != "first" {
		t.Fatalf("copy %+v %v", c, ok)
	}
	u.keepCatalog(entry("A", at(9), false, "newer"), ids)
	if c, _ := u.catalog("A"); c.Rounds[0].Name != "newer" {
		t.Fatalf("copy %+v", c)
	}
}
