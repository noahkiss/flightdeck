package suggest

import (
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

func caps(words ...string) []api.CatalogCapability {
	var out []api.CatalogCapability
	for _, w := range words {
		out = append(out, api.CatalogCapability{Name: w, Detected: true})
	}
	return out
}

var claude = []api.CatalogTool{{Name: "claude", Version: "2.1"}}

// fleet: a Mac with xcode and the app checked out dirty, a Linux box with the app clean on main,
// a GPU box that is offline, and a Linux box with no harness.
func fleet() api.CatalogList {
	yes, no := true, false
	return api.CatalogList{
		Hosts: []api.CatalogEntry{
			{Own: true, Catalog: api.Catalog{DeviceID: "d-mac", Host: "mac", Capabilities: caps("macos", "xcode"), Harnesses: claude}},
			{Label: "devbox", Catalog: api.Catalog{DeviceID: "d-dev", Host: "dev", Capabilities: caps("linux", "docker"), Harnesses: claude,
				Agents: []api.CatalogAgent{{Session: "old", Harness: "claude", Project: "app", State: api.AgentStatusIdle, Since: "2026-10-30T10:00:00Z"}}}},
			{Label: "gpu", Stale: true, SeenAt: "2026-10-29T08:00:00Z", Catalog: api.Catalog{DeviceID: "d-gpu", Host: "gpu", Capabilities: caps("linux", "gpu"), Harnesses: claude}},
			{Label: "bare", Catalog: api.Catalog{DeviceID: "d-bare", Host: "bare", Capabilities: caps("linux")}},
		},
		Projects: []api.CatalogProjectGroup{{Name: "app", Remote: "example.com/app", Locations: []api.CatalogLocation{
			{DeviceID: "d-mac", Folders: []api.CatalogFolder{{Path: "/Users/u/app", Branch: "main", Dirty: &yes}}},
			{DeviceID: "d-dev", Label: "devbox", Folders: []api.CatalogFolder{{Path: "/srv/app", Branch: "main", Dirty: &no}}},
		}}},
	}
}

func excluded(r Result, host string) string {
	for _, e := range r.Excluded {
		if e.Host == host {
			return e.Reason
		}
	}
	return ""
}

func TestReadyProjectWins(t *testing.T) {
	r := Rank(fleet(), Request{Project: "app", Branch: "main", Harness: "claude"})
	if r.Top == nil || r.Top.Host != "devbox" || r.Top.Folder != "/srv/app" || !r.Top.Ready {
		t.Fatalf("top: %+v", r.Top)
	}
	if !slices.ContainsFunc(r.Top.Reasons, func(s string) bool { return strings.Contains(s, "no changes") }) ||
		!slices.ContainsFunc(r.Top.Reasons, func(s string) bool { return strings.Contains(s, "used last") }) {
		t.Fatalf("reasons: %q", r.Top.Reasons)
	}
	if len(r.Next) != 1 || r.Next[0].Host != "mac" {
		t.Fatalf("next: %+v", r.Next)
	}
	if !strings.Contains(excluded(r, "gpu"), "offline") || !strings.Contains(excluded(r, "bare"), "harness claude") {
		t.Fatalf("excluded: %+v", r.Excluded)
	}
}

// A host without a needed capability is never suggested, and the reason names it.
func TestMissingCapability(t *testing.T) {
	r := Rank(fleet(), Request{Project: "app", Needs: []string{"xcode"}, Harness: "claude"})
	if r.Top == nil || r.Top.Host != "mac" || len(r.Next) != 0 {
		t.Fatalf("xcode: %+v next %+v", r.Top, r.Next)
	}
	if !strings.Contains(excluded(r, "devbox"), "lacks xcode") {
		t.Fatalf("excluded: %+v", r.Excluded)
	}
	r = Rank(fleet(), Request{Needs: []string{"windows"}})
	if r.Top != nil || !strings.Contains(r.Why, "no online host has windows") || !slices.Equal(r.Missing, []string{"windows"}) {
		t.Fatalf("windows: %+v", r)
	}
}

func TestEveryFittingHostOffline(t *testing.T) {
	r := Rank(fleet(), Request{Needs: []string{"gpu"}})
	if r.Top != nil || !strings.Contains(r.Why, "every host that fits is offline: gpu") {
		t.Fatalf("gpu: %+v", r)
	}
}

func TestProjectNotSetUp(t *testing.T) {
	r := Rank(fleet(), Request{Project: "docs", Harness: "claude"})
	if r.Top != nil || !r.NotProject || r.SetupOn != "mac" || !strings.Contains(r.Why, "not set up") {
		t.Fatalf("docs: %+v", r)
	}
}

func TestNamedHostBreaksTies(t *testing.T) {
	cl := fleet()
	cl.Projects = nil
	r := Rank(cl, Request{Harness: "claude", Host: "devbox"})
	if r.Top == nil || r.Top.Host != "devbox" {
		t.Fatalf("named: %+v", r.Top)
	}
	r = Rank(cl, Request{Harness: "claude"})
	if r.Top == nil || r.Top.Host != "mac" || !r.Top.Self {
		t.Fatalf("no project, no name: %+v", r.Top)
	}
	// The other branch ranks below the right one.
	r = Rank(fleet(), Request{Project: "app", Branch: "release", Harness: "claude"})
	if r.Top == nil || !strings.Contains(strings.Join(r.Top.Reasons, " "), "not release") {
		t.Fatalf("branch: %+v", r.Top)
	}
}
