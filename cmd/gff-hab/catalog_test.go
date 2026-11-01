package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func catalogList(t *testing.T, sock string) api.CatalogList {
	t.Helper()
	var l api.CatalogList
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "catalog.list")), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func waitCatalog(t *testing.T, sock, what string, ok func(api.CatalogList) bool) api.CatalogList {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		l := catalogList(t, sock)
		if ok(l) {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: catalog.list is %+v", what, l)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestCatalogDetectOff: with $GFF_HAB_DETECT=off, as every test hab runs, the catalog runs no
// program from PATH; a stand-in claude that would leave a mark is never run.
func TestCatalogDetectOff(t *testing.T) {
	d := shortDir(t)
	binDir, mark := filepath.Join(d, "bin"), filepath.Join(d, "ran")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ntouch '"+mark+"'\necho 9.9.9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(d, "h.sock")
	startServeWith(t, sock, filepath.Join(d, "s"), []string{"GFF_HAB_DETECT=off",
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")})
	l := waitCatalog(t, sock, "the own catalog", func(l api.CatalogList) bool { return len(l.Hosts) > 0 })
	if c := l.Hosts[0].Catalog; c.System.OS == "" || len(c.Harnesses) != 0 || len(c.Toolchains) != 0 {
		t.Fatalf("own catalog %+v", c)
	}
	// Detection runs in the background right after the start: give it time to show.
	for range 20 {
		if _, err := os.Stat(mark); err == nil {
			t.Fatal("detection ran claude from PATH")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestCatalogOverSSH: two gff-hab serve processes linked over the ssh stand-in each hold the
// other's catalog, with the declared facts from each one's own config file; when one stops,
// the other keeps its copy, stale.
func TestCatalogOverSSH(t *testing.T) {
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	standIn := sshStandIn(t, map[string]string{"hab-a": sockA, "hab-b": sockB})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-a")
	cfgB := filepath.Join(d, "b-hab.toml")
	if err := os.WriteFile(cfgB, []byte("[catalog]\ncapabilities = [\"signing\"]\nalways_on = true\n\n[[catalog.services]]\nname = \"docs\"\nurl = \"http://localhost:2799\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	peerHab(t, sockA, filepath.Join(d, "sa"), filesA, standIn, "host-a")
	b := peerHab(t, sockB, filepath.Join(d, "sb"), filesB, standIn, "host-b", "-config", cfgB)
	if w := welcome(t, sockA); !slices.Contains(w.Capabilities, "catalog") {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	idB := welcome(t, sockB).DeviceID

	l := waitCatalog(t, sockA, "a holds b's catalog", func(l api.CatalogList) bool {
		for _, e := range l.Hosts {
			if e.Catalog.DeviceID == idB && !e.Stale && e.Label == "bee" && len(e.Catalog.Services) == 1 {
				return true
			}
		}
		return false
	})
	if !l.Hosts[0].Self || l.Hosts[0].Catalog.Host != "host-a" || l.Hosts[0].Catalog.System.OS == "" {
		t.Fatalf("own entry %+v", l.Hosts[0])
	}
	eb := l.Hosts[1]
	if !eb.Catalog.AlwaysOn || eb.Catalog.Host != "host-b" || eb.Catalog.Generation == 0 ||
		!slices.ContainsFunc(eb.Catalog.Capabilities, func(c api.CatalogCapability) bool { return c.Name == "signing" && c.Declared }) {
		t.Fatalf("b's copy %+v", eb.Catalog)
	}
	waitCatalog(t, sockB, "b holds a's catalog", func(l api.CatalogList) bool {
		return len(l.Hosts) == 2 && l.Hosts[1].Label == "ay" && !l.Hosts[1].Stale
	})

	b.stop(t)
	waitCatalog(t, sockA, "b stale, not removed", func(l api.CatalogList) bool {
		return len(l.Hosts) == 2 && l.Hosts[1].Catalog.DeviceID == idB && l.Hosts[1].Stale
	})
	if s := run(t, "api", "call", "-socket", sockA, "catalog.get", `{"since":0}`); !strings.Contains(s, `"host":"host-a"`) {
		t.Fatalf("catalog.get %s", s)
	}
}
