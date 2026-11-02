package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeHarness stands in for claude in a Capcom folder: it appends its arguments to argv.log in
// its working folder, then waits until a file named stop appears there, and exits.
const fakeHarness = `#!/bin/sh
echo "$*" >> argv.log
while [ ! -f stop ]; do sleep 0.1; done
rm -f stop
exit 0
`

// fakeHarnessBin writes the stand-in as claude into a new folder and returns the folder.
func fakeHarnessBin(t *testing.T) string {
	t.Helper()
	d := shortDir(t)
	if err := os.WriteFile(filepath.Join(d, "claude"), []byte(fakeHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func capcomOpen(t *testing.T, c *control.Client, fresh bool) (api.CapcomOpened, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var res api.CapcomOpened
	err := c.Call(ctx, "capcom.open", api.CapcomOpenParams{New: fresh}, &res)
	return res, err
}

func mustOpen(t *testing.T, c *control.Client, fresh bool) api.CapcomOpened {
	t.Helper()
	res, err := capcomOpen(t, c, fresh)
	if err != nil {
		t.Fatalf("capcom.open new=%v: %v", fresh, err)
	}
	return res
}

func resume(t *testing.T, c *control.Client, session string) (api.SessionCreated, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var res api.SessionCreated
	err := c.Call(ctx, "session.resume", api.SessionRef{Session: session}, &res)
	return res, err
}

func sessionOf(t *testing.T, c *control.Client, id string) api.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var st api.StructureResult
	if err := c.Call(ctx, "structure.get", api.StructureGetParams{}, &st); err != nil {
		t.Fatal(err)
	}
	for _, ss := range st.Sessions {
		if ss.ID == id {
			return ss
		}
	}
	return api.Session{}
}

func waitSession(t *testing.T, c *control.Client, id, what string, ok func(api.Session) bool) api.Session {
	t.Helper()
	var ss api.Session
	waitUntil(t, what, func() bool { ss = sessionOf(t, c, id); return ok(ss) })
	return ss
}

// ownCapcom is the capcom section of a hab's own catalog.
func ownCapcom(t *testing.T, sock string) *api.CatalogCapcom {
	t.Helper()
	return catalogList(t, sock).Hosts[0].Catalog.Capcom
}

func waitHome(t *testing.T, sock, home, what string) *api.CatalogCapcom {
	t.Helper()
	var cc *api.CatalogCapcom
	waitUntil(t, what, func() bool { cc = ownCapcom(t, sock); return cc != nil && cc.Home == home })
	return cc
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// TestCapcomHost: with no home host, capcom.open says to assign one. After `gff-hab capcom
// assign self` it starts the harness in a new hab-owned folder that holds the manual and the
// settings registering `<gff-hab> mcp`, as a session of kind capcom with its folder in the
// record; a second open returns the same session. A gff-hab restart keeps it running in the same
// pane. After the harness exits, session.resume rewrites the hand-edited manual and settings,
// leaves an attachment alone, and starts the harness again with the same resume id. A new
// conversation gets a fresh folder and becomes the standing one; the old session stays.
func TestCapcomHost(t *testing.T) {
	e := newHolderEnv(t, 0)
	claudeDir := shortDir(t)
	e.env = []string{"PATH=" + fakeHarnessBin(t) + string(os.PathListSeparator) + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + claudeDir}
	e.serve()
	c := e.control()
	if w := welcome(t, e.sock); !strings.Contains(strings.Join(w.Capabilities, " "), "capcom") {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	if _, err := capcomOpen(t, c, false); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "capcom assign") {
		t.Fatalf("open with no home host: %v", err)
	}
	self := welcome(t, e.sock).DeviceID
	if out := run(t, "capcom", "assign", "-socket", e.sock, "self"); !strings.Contains(out, self) || !strings.Contains(out, "assignment ") {
		t.Fatalf("capcom assign self: %q", out)
	}

	first := mustOpen(t, c, false)
	if !first.Started || first.Session == "" || first.Pane == "" || !strings.HasPrefix(first.Dir, filepath.Join(e.state, "capcom")+string(filepath.Separator)) {
		t.Fatalf("first open: %+v", first)
	}
	ss := sessionOf(t, c, first.Session)
	if ss.Kind != api.SessionKindCapcom || ss.Dir != first.Dir || ss.Name != "capcom" || ss.Project != "" {
		t.Fatalf("session record: %+v", ss)
	}
	manual := filepath.Join(first.Dir, "CLAUDE.md")
	settings := filepath.Join(first.Dir, ".claude", "settings.local.json")
	mcp := filepath.Join(first.Dir, ".mcp.json")
	if b, _ := os.ReadFile(manual); string(b) != capcom.Manual {
		t.Fatalf("manual:\n%s", b)
	}
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	b, _ := os.ReadFile(mcp)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	wantExe, _ := filepath.EvalSymlinks(bin)
	if srv := m.MCPServers[capcom.MCPServer]; srv.Command != wantExe || strings.Join(srv.Args, " ") != "mcp" {
		t.Fatalf(".mcp.json: %s", b)
	}
	wantSettings, _ := os.ReadFile(settings)
	argvLog := filepath.Join(first.Dir, "argv.log")
	waitUntil(t, "the harness started", func() bool { _, err := os.Stat(argvLog); return err == nil })
	start := readLines(t, argvLog)
	if len(start) != 1 || !strings.HasPrefix(start[0], "--session-id ") {
		t.Fatalf("first start argv: %q", start)
	}
	id := strings.TrimPrefix(start[0], "--session-id ")

	if again := mustOpen(t, c, false); again.Started || again.Session != first.Session || again.Pane != first.Pane {
		t.Fatalf("second open: %+v, first %+v", again, first)
	}

	// A gff-hab restart keeps the Capcom pane running, and writes each folder's settings again
	// with its own path (an install retires the build an earlier gff-hab ran from).
	wantMCP, _ := os.ReadFile(mcp)
	writeFile(t, mcp, "{}\n")
	e.restart(false)
	c = e.control()
	if b, _ := os.ReadFile(mcp); string(b) != string(wantMCP) {
		t.Fatalf(".mcp.json after the restart: %s", b)
	}
	after := sessionOf(t, c, first.Session)
	if after.State != api.SessionStateRunning || after.Pane != first.Pane || after.Kind != api.SessionKindCapcom {
		t.Fatalf("after the restart: %+v", after)
	}
	if again := mustOpen(t, c, false); again.Started || again.Session != first.Session || again.Pane != first.Pane {
		t.Fatalf("open after the restart: %+v", again)
	}
	cc := waitHome(t, e.sock, self, "the catalog's capcom section")
	if len(cc.Sessions) != 1 || cc.Sessions[0].Session != first.Session || !cc.Sessions[0].Standing || cc.Sessions[0].Harness != "claude" {
		t.Fatalf("catalog capcom section: %+v", cc)
	}
	if len(readLines(t, argvLog)) != 1 {
		t.Fatal("the restart started the harness again")
	}

	// Hand edits to the manual and settings are undone at the next start; an attachment stays.
	writeFile(t, manual, "edited by hand\n")
	writeFile(t, settings, "{}\n")
	attachment := filepath.Join(first.Dir, "note.txt")
	writeFile(t, attachment, "keep me\n")
	writeFile(t, filepath.Join(first.Dir, "stop"), "")
	waitSession(t, c, first.Session, "the harness exited", func(s api.Session) bool { return s.State == api.SessionStateExited })
	res, err := resume(t, c, first.Session)
	if err != nil {
		t.Fatalf("session.resume: %v", err)
	}
	if res.Session != first.Session || res.Pane == "" || res.Pane == first.Pane {
		t.Fatalf("resume: %+v", res)
	}
	waitSession(t, c, first.Session, "running again", func(s api.Session) bool {
		return s.State == api.SessionStateRunning && s.Pane == res.Pane
	})
	// Claude Code wrote no transcript yet (no message was sent): the same id starts again.
	waitUntil(t, "the harness restarted", func() bool { return len(readLines(t, argvLog)) == 2 })
	if got := readLines(t, argvLog)[1]; got != "--session-id "+id {
		t.Fatalf("start again argv %q, want --session-id %s", got, id)
	}
	// With a transcript, the next start resumes it.
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects", "-capcom"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claudeDir, "projects", "-capcom", id+".jsonl"), "{}\n")
	writeFile(t, filepath.Join(first.Dir, "stop"), "")
	waitSession(t, c, first.Session, "the harness exited again", func(s api.Session) bool { return s.State == api.SessionStateExited })
	if res, err = resume(t, c, first.Session); err != nil {
		t.Fatalf("second session.resume: %v", err)
	}
	waitUntil(t, "the harness resumed", func() bool { return len(readLines(t, argvLog)) == 3 })
	if got := readLines(t, argvLog)[2]; got != "--resume "+id {
		t.Fatalf("resume argv %q, want --resume %s", got, id)
	}
	if b, _ := os.ReadFile(manual); string(b) != capcom.Manual {
		t.Fatalf("manual after resume:\n%s", b)
	}
	if b, _ := os.ReadFile(settings); string(b) != string(wantSettings) {
		t.Fatalf("settings after resume:\n%s", b)
	}
	if b, _ := os.ReadFile(attachment); string(b) != "keep me\n" {
		t.Fatalf("attachment after resume: %q", b)
	}
	if _, err := resume(t, c, first.Session); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("resume while running: %v", err)
	}

	// A new conversation: a fresh folder, now the standing one; the old session stays.
	fresh := mustOpen(t, c, true)
	if !fresh.Started || fresh.Session == first.Session || fresh.Dir == first.Dir || sessionOf(t, c, fresh.Session).Name != "capcom-2" {
		t.Fatalf("new conversation: %+v", fresh)
	}
	if again := mustOpen(t, c, false); again.Session != fresh.Session {
		t.Fatalf("open after a new conversation: %+v", again)
	}
	if old := sessionOf(t, c, first.Session); old.State != api.SessionStateRunning {
		t.Fatalf("old conversation: %+v", old)
	}
	waitUntil(t, "two sessions in the catalog", func() bool {
		cc := ownCapcom(t, e.sock)
		return cc != nil && len(cc.Sessions) == 2 && !cc.Sessions[0].Standing && cc.Sessions[1].Standing
	})

	// session.resume of a session that is not Capcom's is not built yet.
	other := e.spawn(c, "exit 0")
	if s, ok := sessionByPane(t, c, other); ok {
		waitSession(t, c, s.ID, "the shell exited", func(s api.Session) bool { return s.State == api.SessionStateExited })
		if _, err := resume(t, c, s.ID); errKind(err) != api.ErrorKindUnavailable {
			t.Fatalf("resume of a plain session: %v", err)
		}
	}

	// The holder dies: both Capcom sessions show exited with no status (pane lost). Neither
	// resumes, since its harness may still run once the holder is back.
	e.holder.Process.Kill()
	<-e.holderDone
	waitSession(t, c, fresh.Session, "the pane lost", func(s api.Session) bool { return s.State == api.SessionStateExited && s.Exit == nil })
	if _, err := resume(t, c, fresh.Session); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "holder is not connected") {
		t.Fatalf("resume of a lost pane: %v", err)
	}
	if _, err := capcomOpen(t, c, false); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("open with the standing pane lost: %v", err)
	}
}

func sessionByPane(t *testing.T, c *control.Client, pane string) (api.Session, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var st api.StructureResult
	if err := c.Call(ctx, "structure.get", api.StructureGetParams{}, &st); err != nil {
		t.Fatal(err)
	}
	for _, ss := range st.Sessions {
		if ss.Pane == pane {
			return ss, true
		}
	}
	return api.Session{}, false
}

// TestCapcomAssign: two habs linked over the ssh stand-in. A takes the mark and starts Capcom;
// `gff-hab capcom assign bee` on A moves the mark to B: both catalogs name B, A's open names
// B, and A's old conversation stays and resumes. Assigned back to A, B follows. With A stopped,
// B takes the mark; A, started again, drops its stale mark on its next link.
func TestCapcomAssign(t *testing.T) {
	d := shortDir(t)
	a := newHolderEnv(t, 0)
	sockB := filepath.Join(d, "b.sock")
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": sockB})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-a")
	a.env = []string{
		"PATH=" + standIn + string(os.PathListSeparator) + fakeHarnessBin(t) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GFF_HAB_PEERS=" + filesA,
		"XDG_RUNTIME_DIR=" + shortDir(t),
	}
	a.serve()
	peerHab(t, sockB, filepath.Join(d, "sb"), filesB, standIn, "host-b")
	idA, idB := welcome(t, a.sock).DeviceID, welcome(t, sockB).DeviceID
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	waitPeer(t, sockB, "ay", "b sees a online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })

	c := a.control()
	run(t, "capcom", "assign", "-socket", a.sock, "self")
	old := mustOpen(t, c, false)
	cc := waitHome(t, sockB, idA, "b learns a is home")
	first := cc.Assignment

	out := run(t, "capcom", "assign", "-socket", a.sock, "bee")
	if !strings.Contains(out, idB) {
		t.Fatalf("capcom assign bee: %q", out)
	}
	ccA, ccB := waitHome(t, a.sock, idB, "a drops the mark"), waitHome(t, sockB, idB, "b holds the mark")
	if ccA == nil || ccA.Home != idB || ccA.Assignment <= first || ccB.Assignment != ccA.Assignment {
		t.Fatalf("after the move: a %+v, b %+v", ccA, ccB)
	}
	if _, err := capcomOpen(t, c, false); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "bee") {
		t.Fatalf("open on the old home: %v", err)
	}
	// Re-assigning moved no conversation: A's session is still there, listed, and resumable.
	if len(ccA.Sessions) != 1 || ccA.Sessions[0].Session != old.Session || ccA.Sessions[0].Standing {
		t.Fatalf("a's sessions after the move: %+v", ccA.Sessions)
	}
	writeFile(t, filepath.Join(old.Dir, "stop"), "")
	waitSession(t, c, old.Session, "the old conversation exited", func(s api.Session) bool { return s.State == api.SessionStateExited })
	if res, err := resume(t, c, old.Session); err != nil || res.Session != old.Session {
		t.Fatalf("resume the old conversation: %+v, %v", res, err)
	}

	// Back to A: B follows on its next pull.
	run(t, "capcom", "assign", "-socket", a.sock, "self")
	back := waitHome(t, sockB, idA, "b learns a is home again")
	if fresh := mustOpen(t, c, false); fresh.Session == old.Session || !fresh.Started {
		t.Fatalf("open on the new home: %+v; a returning home starts a fresh standing conversation", fresh)
	} else {
		writeFile(t, filepath.Join(fresh.Dir, "stop"), "")
	}
	writeFile(t, filepath.Join(old.Dir, "stop"), "")

	// A is offline while B takes the mark; A drops its stale mark on its next link.
	a.stopHab(false)
	waitPeer(t, sockB, "ay", "b sees a offline", func(p api.Peer) bool { return p.State != api.PeerStateOnline })
	run(t, "capcom", "assign", "-socket", sockB, "self")
	if cc := waitHome(t, sockB, idB, "b takes the mark"); cc.Assignment <= back.Assignment {
		t.Fatalf("b after taking the mark: %+v", cc)
	}
	a.serve()
	waitHome(t, a.sock, idB, "a drops its stale mark")
	c = a.control()
	if _, err := capcomOpen(t, c, false); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("open on the stale home: %v", err)
	}
}
