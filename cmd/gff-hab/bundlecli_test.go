package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
// TestBundleSignal: Ctrl-C or a TERM while the bundle writes removes its hidden work folder and
// the archive's temporary file, and leaves no archive (diagnostics review L10).
func TestBundleSignal(t *testing.T) {
	t.Parallel()
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		d := t.TempDir()
		hold := filepath.Join(t.TempDir(), "hold")
		cmd := exec.Command(bin, "bundle", "-state", filepath.Join(d, "state"), "-socket", filepath.Join(d, "none.sock"),
			"-config", filepath.Join(d, "none.toml"), "-o", filepath.Join(d, "b.tar.gz"))
		cmd.Env = append(os.Environ(), "GFF_TEST_BUNDLE_HOLD="+hold, noRaceExitSleep)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var work string
		waitUntil(t, "the work folder", func() bool {
			b, _ := os.ReadFile(hold)
			work = string(b)
			return work != ""
		})
		if st, err := os.Stat(work); err != nil || !st.IsDir() || filepath.Dir(work) != d {
			t.Fatalf("work folder %q: %v", work, err)
		}
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		var ee *exec.ExitError
		if err := cmd.Wait(); !errors.As(err, &ee) || ee.ExitCode() != 130 {
			t.Errorf("%v: exit %v, want 130", sig, err)
		}
		es, _ := os.ReadDir(d)
		for _, e := range es {
			if e.Name() != "state" {
				t.Errorf("%v: %s left in the folder", sig, e.Name())
			}
		}
	}
}

func TestBundleSince(t *testing.T) {
	for _, since := range []string{"-48h", "2999-01-01T00:00:00Z"} {
		out, err := runErr("bundle", "-state", t.TempDir(), "-socket", filepath.Join(t.TempDir(), "none.sock"), "-dir", filepath.Join(t.TempDir(), "b"), "-since", since)
		if err == nil || !strings.Contains(out, "future") {
			t.Errorf("-since %s: %v\n%s", since, err, out)
		}
	}
}
