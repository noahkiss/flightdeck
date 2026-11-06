package gffclient

import (
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// TestBoardHeavy: the status board lists the heavy-run lock of each host whose lock is in use or
// whose disk is low, from the catalog's heavy section; a host with an idle lock shows no line.
func TestBoardHeavy(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.Capabilities = []string{"catalog"}
	a.Stop()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	b := fakeHab(t, d, "b")
	now := time.Now().UTC().Format(time.RFC3339)
	a.SetCatalog(api.CatalogList{Hosts: []api.CatalogEntry{
		{Own: true, SeenAt: now, Catalog: api.Catalog{DeviceID: a.DeviceID, Host: "a",
			Heavy: &api.CatalogHeavy{Slots: 1, Holding: 1, Waiting: 2, DiskLow: true,
				Volumes: []api.HeavyVolume{{Path: "/s", FreeBytes: 5 << 30, TotalBytes: 100 << 30}, {Path: "/w", FreeBytes: 7 << 30, TotalBytes: 100 << 30}}}}},
		{Label: "b", SeenAt: now, Catalog: api.Catalog{DeviceID: b.DeviceID, Host: "b",
			Heavy: &api.CatalogHeavy{Slots: 1}}},
	}})
	sock := func(h string) []hosts.Endpoint { return []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: h}} }
	hst := newHost(t, 140, 30, false)
	startWorkspaceSpecs(t, hst,
		hosts.Spec{Label: "one", Endpoints: sock(a.Sock)},
		hosts.Spec{Label: "two", Endpoints: sock(b.Sock)})
	hst.typ(cmdKey + "g")
	hst.waitScreen("the heavy-run line", func(s string) bool {
		l := line(s, "  one ")
		return strings.Contains(s, " Heavy runs") && l >= 0 &&
			strings.Contains(strings.Split(s, "\n")[l], "1 running · 2 waiting · disk low: 5.0 GiB free, runs wait")
	})
	if s := hst.screen(); line(s, "  two ") >= 0 {
		t.Fatalf("an idle lock has a line:\n%s", s)
	}
	// A hab that goes down takes its line with it: its lock is no longer known.
	a.Stop()
	hst.waitScreen("one's line gone", func(s string) bool { return !strings.Contains(s, " Heavy runs") })
}

// TestKeepHeavy: the answering hab's own entry wins over a peer's copy; a peer's copy counts only
// for a host in gff's list, and only when newer.
func TestKeepHeavy(t *testing.T) {
	u := &workspaceUI{heavy: map[string]heavyCopy{}}
	labels := map[string]string{"A": "one", "B": "two"}
	at := func(s int) string { return time.Unix(int64(1_800_000_000+s), 0).UTC().Format(time.RFC3339) }
	entry := func(id, seen string, own bool, holding int) api.CatalogEntry {
		return api.CatalogEntry{Own: own, SeenAt: seen, Catalog: api.Catalog{DeviceID: id, Heavy: &api.CatalogHeavy{Holding: holding}}}
	}
	u.keepHeavy("one", entry("A", at(1), true, 1), labels)
	u.keepHeavy("two", entry("A", at(9), false, 5), labels)
	if c := u.heavy["A"]; c.section.Holding != 1 || c.host != "one" {
		t.Fatalf("own entry lost to a copy: %+v", c)
	}
	u.keepHeavy("one", entry("B", at(5), false, 2), labels)
	u.keepHeavy("one", entry("B", at(3), false, 3), labels)
	u.keepHeavy("one", entry("C", at(5), false, 4), labels)
	if c := u.heavy["B"]; c.section.Holding != 2 || c.host != "two" {
		t.Fatalf("B: %+v", c)
	}
	if _, ok := u.heavy["C"]; ok {
		t.Fatal("a host gff does not list was kept")
	}
	if got := u.heavyLines(); len(got) != 2 || !strings.Contains(got[0], "one") {
		t.Fatalf("lines %q", got)
	}
}
