package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// spawnDelay widens the window between the holder starting a pane's program and gff-hab
// following the pane (the testseam build's GFF_TEST_SPAWN_DELAY, internal/link). Before
// session.create recorded the session ahead of the spawn, a program that called back in this
// window got "the pane token names no pane with a session"; under load it happened by chance.
const spawnDelay = "GFF_TEST_SPAWN_DELAY=400ms"

// callBackAtOnce starts name on e running script, which calls back through $GFF_HAB as its
// first act and leaves the exit code in rc, its output in out and its errors in err. It
// returns the session and what the call printed.
func callBackAtOnce(t *testing.T, e *habEnv, name, script string) (api.SessionCreated, string) {
	t.Helper()
	dir := filepath.Join(e.work, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	full := script + ` > out 2> err; echo $? > rc.tmp; mv rc.tmp rc; while [ ! -e end ]; do sleep 0.05; done`
	created := createSession(t, e.sock, api.SessionCreateParams{Name: name, Argv: []string{"/bin/sh", "-c", full}, Cwd: dir,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HAB=" + bin, noRaceExitSleep}})
	t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "end"), nil, 0o600) })
	waitUntil(t, name+"'s call", func() bool { _, err := os.Stat(filepath.Join(dir, "rc")); return err == nil })
	rc, _ := os.ReadFile(filepath.Join(dir, "rc"))
	out, _ := os.ReadFile(filepath.Join(dir, "out"))
	stderr, _ := os.ReadFile(filepath.Join(dir, "err"))
	if strings.TrimSpace(string(rc)) != "0" {
		t.Fatalf("%s: the pane's call exited %s: %q %q", name, strings.TrimSpace(string(rc)), out, stderr)
	}
	return created, string(out)
}

// TestShowAtFirstByte: a pane whose program runs `gff-hab show` as its very first act names
// its session by the pane token, even when that call lands before gff-hab follows the pane.
func TestShowAtFirstByte(t *testing.T) {
	e := newHolderEnv(t, 0)
	e.env = []string{spawnDelay}
	e.serve()
	writeFile(t, filepath.Join(e.work, "notes.md"), "# Notes\n")
	created, out := callBackAtOnce(t, e, "shower", `"$HAB" show `+filepath.Join(e.work, "notes.md"))
	var shown api.ContentShown
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("show output %q: %v", out, err)
	}
	if shown.Session != created.Session || shown.ID == "" {
		t.Fatalf("shown %+v, session %s", shown, created.Session)
	}
}

// TestSendAtFirstByte: a pane whose program runs `gff-hab send` as its very first act sends
// as its own session through the channels pane door, even when that call lands before gff-hab
// follows the pane.
func TestSendAtFirstByte(t *testing.T) {
	e := newHolderEnv(t, 0)
	e.env = []string{spawnDelay}
	e.serve()
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	startReceiver(t, e, "working") // bob holds what it gets
	_, out := callBackAtOnce(t, e, "alice", `"$HAB" send bob hello`)
	f := strings.Fields(out)
	if len(f) != 3 {
		t.Fatalf("send output %q", out)
	}
	if r := channelStatus(t, e.sock, f[0], 0); r.From != "alice" {
		t.Fatalf("sent as %q, want alice: %+v", r.From, r)
	}
}

// TestFailedSpawnLeavesNoSession: session.create records the session before it spawns. When
// the spawn fails (here: no holder runs), the call answers unavailable, session.deleted follows
// the record's session.updated, no session is left, and the name is free.
func TestFailedSpawnLeavesNoSession(t *testing.T) {
	d := shortDir(t)
	hdir := filepath.Join(d, "h")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(d, "run", "hab.sock")
	startServeWith(t, sock, filepath.Join(d, "state"), nil, "-holders", hdir)

	ev := exec.Command(bin, "events", "-socket", sock)
	stdout, err := ev.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ev.Process.Kill(); ev.Wait() })
	lineCh := make(chan []byte, 64)
	go func() {
		lines := bufio.NewScanner(stdout)
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			lineCh <- slices.Clone(lines.Bytes())
		}
		close(lineCh)
	}()
	next := func() []byte {
		t.Helper()
		select {
		case b, ok := <-lineCh:
			if !ok {
				t.Fatal("the event stream ended")
			}
			return b
		case <-time.After(scaleTimeout(10 * time.Second)):
			t.Fatal("no event within the timeout")
		}
		return nil
	}
	next() // the welcome
	next() // the snapshot

	params := api.SessionCreateParams{Name: "ghost", Argv: []string{"/bin/true"}}
	if kind := callErr(t, sock, "session.create", params); kind != "unavailable" {
		t.Fatalf("create with no holder: %s, want unavailable", kind)
	}
	// The name is free: a second try fails the same way, not with conflict.
	if kind := callErr(t, sock, "session.create", params); kind != "unavailable" {
		t.Fatalf("second create with no holder: %s, want unavailable", kind)
	}

	var seen, panes []string
	var id string
	for len(seen) < 4 {
		line := next()
		var n struct {
			Params api.Event `json:"params"`
		}
		var e api.Event
		if json.Unmarshal(line, &n) == nil && n.Params.Type != "" {
			e = n.Params
		} else if json.Unmarshal(line, &e) != nil || e.Type == "" {
			t.Fatalf("event line %s", line)
		}
		if e.Type == "session.updated" {
			var s api.Session
			json.Unmarshal(e.Data, &s)
			if s.Name != "ghost" {
				t.Fatalf("session.updated for %+v", s)
			}
			if id == "" || seen[len(seen)-1] == "session.deleted" {
				id = e.ID
				panes = append(panes, s.Pane)
			}
		}
		if e.ID != id {
			t.Fatalf("event %s for %s, want %s", e.Type, e.ID, id)
		}
		seen = append(seen, string(e.Type))
	}
	if want := []string{"session.updated", "session.deleted", "session.updated", "session.deleted"}; !slices.Equal(seen, want) {
		t.Fatalf("events %v, want %v", seen, want)
	}

	var st api.StructureResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "structure.get")), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Sessions) != 0 || len(st.Panes) != 0 {
		t.Fatalf("after failed spawns: sessions %+v, panes %+v", st.Sessions, st.Panes)
	}

	// The attempted panes' tokens were live only while their spawns ran: now they are unknown.
	secret, _, err := hook.LoadOrCreateKey(filepath.Join(d, "state"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := hook.NewTokens(secret, nil)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	for _, pane := range panes {
		u, ok := parseUUID(pane)
		if !ok {
			t.Fatalf("pane id %q", pane)
		}
		addr := hook.Addr(filepath.Join(d, "run", "hook.sock"), tokens.Token(u))
		var sent api.ChannelSent
		err := hook.Call(ctx, addr, "channel.send", api.ChannelSendParams{To: "x", Text: "hi"}, &sent)
		if err == nil || !strings.Contains(err.Error(), "unknown token") {
			t.Fatalf("the failed spawn's token: %v, want unknown token", err)
		}
	}
}

// TestNeverSpawnedSessionDropped: gff-hab stopped between filing a session and its spawn, so the
// state file holds a session whose pane never ran. The next gff-hab drops it at start, and the
// name is free.
func TestNeverSpawnedSessionDropped(t *testing.T) {
	e := newHolderEnv(t, 0)
	st, err := structure.Open(structure.Config{Dir: e.state, Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(structure.SessionSpec{Name: "ghost", Pane: newUUID().String(), Index: -1, NewPane: true}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	e.serve()
	var got api.StructureResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", e.sock, "structure.get")), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("sessions after start: %+v", got.Sessions)
	}
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	createSession(t, e.sock, api.SessionCreateParams{Name: "ghost", Argv: []string{"/bin/sh", "-c", `while [ ! -e "$DIR/end" ]; do sleep 0.05; done`},
		Env: []string{"PATH=/usr/bin:/bin", "DIR=" + e.work, noRaceExitSleep}})
}
