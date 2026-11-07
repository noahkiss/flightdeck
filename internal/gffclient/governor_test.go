package gffclient

import (
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

func TestGovernorText(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		k    api.GovernorConsumer
		want string
	}{
		{api.GovernorConsumer{ID: "capcom", Basis: "plan", Pace: "ok", EventPace: "ok", UsedPct: 5, SharePct: 30}, ""},
		{api.GovernorConsumer{ID: "capcom", Basis: "plan", Pace: "ok", EventPace: "wait", EventUntil: "2026-11-07T15:00:00Z", UsedPct: 26, SharePct: 30},
			"capcom event turns wait until 15:00 · 26% of the week used, share 30%"},
		{api.GovernorConsumer{ID: "capcom", Basis: "plan", Pace: "small", EventPace: "wait", UsedPct: 31, SharePct: 30},
			"capcom small model · 31% of the week used, share 30%"},
		{api.GovernorConsumer{ID: "group:g1", Basis: "plan", Pace: "wait", EventPace: "wait", Until: "2026-11-09T09:00:00Z", UsedPct: 40, SharePct: 30},
			"group:g1 waits until Mon 09:00 · 40% of the week used, share 30%"},
		{api.GovernorConsumer{ID: "capcom", Basis: "usd", Pace: "wait", EventPace: "wait", Until: "2026-11-08T00:00:00Z", SpentCents: 125, CapCents: 100},
			"capcom waits until Sun 00:00 · $1.25 of $1.00 today"},
		{api.GovernorConsumer{ID: "capcom", Basis: "none", Pace: "ok", EventPace: "ok", InputTokens: 12000, OutputTokens: 400},
			"capcom unpaced: 12.4k tokens today"},
	} {
		if got := governorText(c.k, now); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.k, got, c.want)
		}
	}
}

// TestBoardGovernor: the status board lists each paced consumer that is not at ok under Usage
// pacing, from the catalog's governor section, and drops it when its hab goes down.
func TestBoardGovernor(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.Capabilities = []string{"catalog"}
	a.Stop()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	a.SetCatalog(api.CatalogList{Hosts: []api.CatalogEntry{
		{Own: true, SeenAt: now, Catalog: api.Catalog{DeviceID: a.DeviceID, Host: "a",
			Governor: &api.CatalogGovernor{Consumers: []api.GovernorConsumer{
				{ID: "capcom", Basis: "plan", Pace: "small", EventPace: "wait", UsedPct: 31, SharePct: 30}}}}},
	}})
	sock := func(h string) []hosts.Endpoint { return []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: h}} }
	hst := newHost(t, 140, 30, false)
	startWorkspaceSpecs(t, hst, hosts.Spec{Label: "one", Endpoints: sock(a.Sock)})
	hst.typ(cmdKey + "g")
	hst.waitScreen("the pacing line", func(s string) bool {
		return strings.Contains(s, " Usage pacing") && strings.Contains(s, "capcom small model · 31% of the week used, share 30%")
	})
	a.Stop()
	hst.waitScreen("the line gone", func(s string) bool { return !strings.Contains(s, " Usage pacing") })
}
