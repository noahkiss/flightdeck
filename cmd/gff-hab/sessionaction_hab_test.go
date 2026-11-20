package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// sessionsNow is the hab's sessions by name.
func sessionsNow(t *testing.T, sock string) map[string]api.Session {
	t.Helper()
	var st api.Structure
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "structure.get")), &st); err != nil {
		t.Fatal(err)
	}
	out := map[string]api.Session{}
	for _, s := range st.Sessions {
		out[s.Name] = s
	}
	return out
}

// TestSessionActionHab runs session.action against a real hab (hab-logic-api.md 3.13): the
// capability, gff-hab action from an agent's pane refused, close and recreate of a shell
// session from the owner's door through the CLI, each to its end.
func TestSessionActionHab(t *testing.T) {
	t.Parallel()
	e := newHolderEnv(t, 0)
	e.serve()
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	if w := welcome(t, e.sock); !slices.Contains(w.Capabilities, sessionActionCapability) {
		t.Fatalf("capabilities %v lack %s", w.Capabilities, sessionActionCapability)
	}
	addr := startSender(t, e) // alice
	shellSession(t, e, "work")
	// From an agent's pane: refused, nothing happens.
	if out, code := runCode(t, []string{"GFF_HAB=" + addr}, "action", "work", "close"); code != 3 {
		t.Fatalf("from a pane: exit %d %q", code, out)
	}
	if _, ok := sessionsNow(t, e.sock)["work"]; !ok {
		t.Fatal("a refused close closed the session")
	}
	// close from the owner's door: the pane closes, then the record goes.
	out, code := runCode(t, nil, "action", "-socket", e.sock, "-json", "work", "close")
	var a api.SessionAction
	if code != 0 || json.Unmarshal([]byte(out), &a) != nil || a.State != actionQueued || a.Action != "close" {
		t.Fatalf("close: %d %q", code, out)
	}
	waitUntil(t, "work closed", func() bool { _, ok := sessionsNow(t, e.sock)["work"]; return !ok })

	// recreate: a fresh session under the same name, in the same folder; the old one aside.
	dir := filepath.Join(e.work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := createSession(t, e.sock, api.SessionCreateParams{Name: "keep", Argv: []string{"/bin/sh"}, Cwd: dir,
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "PS1=$ ", "ENV=", noRaceExitSleep}})
	out, code = runCode(t, nil, "action", "-socket", e.sock, "keep", "recreate")
	if code != 0 || !strings.Contains(out, "recreate on keep: queued") {
		t.Fatalf("recreate: %d %q", code, out)
	}
	waitUntil(t, "keep recreated", func() bool {
		s := sessionsNow(t, e.sock)
		k, ok := s["keep"]
		aside, ok2 := s["keep-1"]
		return ok && ok2 && k.ID != old.Session && aside.ID == old.Session && k.Pane != "" && k.State == api.SessionStateRunning
	})
}
