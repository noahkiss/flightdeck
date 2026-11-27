package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestNoText: -no-text keeps a permission request's id and tool only, drops the trust folder,
// and keeps the names of a process's own name and argv[0], and a manifest's match, never a name
// taken from a later argument such as node -e code (diagnostics review H1, L7).
func TestNoText(t *testing.T) {
	const code = "console.log('cnrytext')"
	r := api.DetectExplain{
		Agent: &api.AgentState{State: api.AgentStatusBlocked, Request: &api.PermissionRequest{ID: "req-1", Tool: "Bash",
			Command: "curl cnrytext", Files: []string{"/x/cnrytext"}, Cwd: "/x/cnrytext", Input: api.RawJSON(`{"command":"curl cnrytext"}`),
			Title: "cnrytext", Description: "cnrytext", ContextLine: "cnrytext"}, Trust: &api.TrustPrompt{Folder: "/x/cnrytext"}},
		Process: api.DetectProcess{MatchPID: 2, MatchName: "claude", Match: "claude", Processes: []api.DetectProc{
			{PID: 1, Name: "node", Args: []string{"/usr/bin/node", "-e", code}, Names: []string{"node", strings.ToLower(code)}},
			{PID: 2, Name: "node", Args: []string{"node", "/opt/claude.js"}, Names: []string{"node", "claude"}, Match: "claude"},
		}},
	}
	noText(&r)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "cnrytext") || strings.Contains(string(b), `"args"`) {
		t.Errorf("no-text answer: %s", b)
	}
	if q := r.Agent.Request; q == nil || q.ID != "req-1" || q.Tool != "Bash" || r.Agent.Trust != nil {
		t.Errorf("agent: %+v", r.Agent)
	}
	if p := r.Process.Processes; !slices.Equal(p[0].Names, []string{"node"}) || !slices.Equal(p[1].Names, []string{"node", "claude"}) ||
		r.Process.MatchName != "claude" {
		t.Errorf("process: %+v", r.Process)
	}
}

// TestBundleNames: the name list takes a host's first label, the session names of
// structure.json with gff-hab stopped, and from a running gff-hab the session names and titles,
// rounds and groups (diagnostics review M3).
func TestBundleNames(t *testing.T) {
	d := t.TempDir()
	peersFile := filepath.Join(d, "peers.toml")
	writeFile(t, peersFile, "[[peers]]\nlabel = \"fixture-peer\"\nhost = \"studio.example.org\"\nendpoints = [\"https://forge-box.example.net:7300/x\", \"192.0.2.7:7300\"]\n")
	writeFile(t, filepath.Join(d, structure.StateFile), `{"schema":1,"projects":[{"name":"orchard-notes","sessions":[]}],"sessions":[{"id":"s1","name":"night-ledger","state":"stopped"}]}`)

	stopped := bundleNames("", peersFile, d, nil)
	for _, n := range []string{"fixture-peer", "studio.example.org", "studio", "forge-box.example.net", "forge-box", "orchard-notes", "night-ledger"} {
		if !slices.Contains(stopped, n) {
			t.Errorf("names with gff-hab stopped lack %q: %v", n, stopped)
		}
	}
	if slices.Contains(stopped, "192") {
		t.Errorf("an IP endpoint gave a label: %v", stopped)
	}
	st := &api.Structure{Host: api.HostInfo{ID: "desk-box.example.org"},
		Sessions: []api.Session{{Name: "quiet-heron", Title: "plan the move"}},
		Rounds:   []api.Round{{RoundSpec: api.RoundSpec{Name: "check-invoices"}}},
		Groups:   []api.Group{{Title: "ledger cleanup"}}}
	running := bundleNames("", "", t.TempDir(), st)
	for _, n := range []string{"desk-box.example.org", "desk-box", "quiet-heron", "plan the move", "check-invoices", "ledger cleanup"} {
		if !slices.Contains(running, n) {
			t.Errorf("names from a running gff-hab lack %q: %v", n, running)
		}
	}
}

// TestBundleSince: -since refuses a start in the future (diagnostics review L11).
func TestBundleSince(t *testing.T) {
	for _, since := range []string{"-48h", "2999-01-01T00:00:00Z"} {
		out, err := runErr("bundle", "-state", t.TempDir(), "-socket", filepath.Join(t.TempDir(), "none.sock"), "-dir", filepath.Join(t.TempDir(), "b"), "-since", since)
		if err == nil || !strings.Contains(out, "future") {
			t.Errorf("-since %s: %v\n%s", since, err, out)
		}
	}
}
