package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakeReceiver stands in for Claude Code taking channel messages. It reports $START over OSC
// 7501 (working waits for go-idle, then reports idle), then reads $COUNT messages, each of the
// length in len-<n>, into msg-<n>, reporting working and idle after each, as a turn would.
const fakeReceiver = `
stty raw -echo
printf '\033]7501;state=%s:app=claude\033\\' "$START"
if [ "$START" = working ]; then
  while [ ! -e "$DIR/go-idle" ]; do sleep 0.02; done
  printf '\033]7501;state=idle:app=claude\033\\'
fi
n=1
while [ "$n" -le "$COUNT" ]; do
  head -c "$(cat "$DIR/len-$n")" > "$DIR/msg-$n.tmp" && mv "$DIR/msg-$n.tmp" "$DIR/msg-$n"
  printf '\033]7501;state=working:app=claude\033\\'
  printf '\033]7501;state=idle:app=claude\033\\'
  n=$((n+1))
done
while [ ! -e "$DIR/end" ]; do sleep 0.05; done
`

// fakeSender is a session that only saves its GFF_HAB, so the test can run gff-hab send as
// the pane would.
const fakeSender = `
printf '%s' "$GFF_HAB" > "$DIR/addr.tmp" && mv "$DIR/addr.tmp" "$DIR/addr"
while [ ! -e "$DIR/end" ]; do sleep 0.05; done
`

// meshPair is two habs, each with its own holder, linked both ways over the ssh stand-in. a
// names b "bee"; b names a "ay".
type meshPair struct {
	a, b *habEnv
}

func newMeshPair(t *testing.T) *meshPair {
	t.Helper()
	return newMeshPairEnv(t, "")
}

// newMeshPairEnv is newMeshPair with path first on both habs' PATH (none when empty) and env
// added to both habs' environment.
func newMeshPairEnv(t *testing.T, path string, env ...string) *meshPair {
	t.Helper()
	a, b := newHolderEnv(t, 0), newHolderEnv(t, 0)
	d := shortDir(t)
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": b.sock})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-a")
	for _, x := range []struct {
		e     *habEnv
		peers string
	}{{a, filesA}, {b, filesB}} {
		pathList := standIn + string(os.PathListSeparator) + os.Getenv("PATH")
		if path != "" {
			pathList = path + string(os.PathListSeparator) + pathList
		}
		x.e.env = append([]string{
			"PATH=" + pathList,
			"GFF_HAB_PEERS=" + x.peers,
			"XDG_RUNTIME_DIR=" + shortDir(t),
		}, env...)
		x.e.serve()
	}
	if w := welcome(t, a.sock); !strings.Contains(strings.Join(w.Capabilities, " "), channels.Capability) {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	waitPeer(t, b.sock, "ay", "b sees a online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	t.Cleanup(func() {
		writeFile(t, filepath.Join(a.work, "end"), "")
		writeFile(t, filepath.Join(b.work, "end"), "")
	})
	return &meshPair{a: a, b: b}
}

// anyID stands for a message id not known yet; it has an id's length.
var anyID = strings.Repeat("X", 32)

// pasted is what a message from sender looks like in the receiving pane, with anyID for its id.
func pasted(from, text string) string {
	return "\x1b[200~" + channels.Envelope(anyID, from, text) + "\x1b[201~\r"
}

// withID puts the message id in a pasted message.
func withID(p, id string) string { return strings.ReplaceAll(p, anyID, id) }

// startReceiver files bob on e, running fakeReceiver for the messages wants, in order.
func startReceiver(t *testing.T, e *habEnv, start string, wants ...string) api.SessionCreated {
	t.Helper()
	for i, w := range wants {
		writeFile(t, filepath.Join(e.work, "len-"+strconv.Itoa(i+1)), strconv.Itoa(len(w)))
	}
	ss := createSession(t, e.sock, api.SessionCreateParams{
		Name: "bob", Argv: []string{"/bin/sh", "-c", fakeReceiver}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "START=" + start, "COUNT=" + strconv.Itoa(len(wants)), noRaceExitSleep}})
	waitState(t, e.sock, ss.Pane, "bob reports "+start, func(s api.AgentState) bool { return string(s.State) == start })
	return ss
}

// startSender files alice on e and returns her GFF_HAB.
func startSender(t *testing.T, e *habEnv) string {
	t.Helper()
	createSession(t, e.sock, api.SessionCreateParams{
		Name: "alice", Argv: []string{"/bin/sh", "-c", fakeSender}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}})
	addr := filepath.Join(e.work, "addr")
	waitUntil(t, "alice's GFF_HAB", func() bool { b, err := os.ReadFile(addr); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addr)
	return string(b)
}

// sendFromPane runs gff-hab send as a program in the pane with GFF_HAB addr would, and returns
// the message id and state.
func sendFromPane(t *testing.T, addr, to, text string) (id string, state api.ChannelState) {
	t.Helper()
	cmd := exec.Command(bin, "send", to, text)
	cmd.Env = append(os.Environ(), "GFF_HAB="+addr)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gff-hab send %s: %v %s", to, err, out)
	}
	f := strings.Fields(string(out))
	if len(f) != 3 {
		t.Fatalf("send output %q", out)
	}
	return f[0], api.ChannelState(f[1])
}

func channelStatus(t *testing.T, sock, id string, waitMS int) api.ChannelRecord {
	t.Helper()
	var r api.ChannelStatusResult
	params := `{"id":"` + id + `","wait_ms":` + strconv.Itoa(waitMS) + `}`
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "channel.status", params)), &r); err != nil {
		t.Fatal(err)
	}
	return r.Record
}

// TestChannelsAcrossHabs: a message from an agent on A reaches an agent on B, wrapped in the
// envelope, by label/name and then by a bare name the catalog resolves; its delivered state
// comes back to A (task acceptance 1).
func TestChannelsAcrossHabs(t *testing.T) {
	p := newMeshPair(t)
	m1, m2 := pasted("ay/alice", "hello bob"), pasted("ay/alice", "second line\nby name")
	startReceiver(t, p.b, "idle", m1, m2)
	addr := startSender(t, p.a)

	id, st := sendFromPane(t, addr, "bee/bob", "hello bob")
	if st != api.ChannelStateQueued && st != api.ChannelStateHanded {
		t.Fatalf("state %s", st)
	}
	waitUntil(t, "the first message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-1"), withID(m1, id)))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered || r.From != "alice" || r.To != "bee/bob" {
		t.Fatalf("record on a: %+v", r)
	}

	waitCatalog(t, p.a.sock, "a's copy of b lists bob", func(l api.CatalogList) bool {
		return channels.ClashLabel(l, welcome(t, p.a.sock).DeviceID, "bob") == "bee"
	})
	id, _ = sendFromPane(t, addr, "bob", "second line\nby name")
	waitUntil(t, "the second message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-2"), withID(m2, id)))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered || r.To != "bee/bob" {
		t.Fatalf("record on a: %+v", r)
	}

	// A name nobody holds is refused at once; one only the target can check is refused there.
	cmd := exec.Command(bin, "send", "nobody", "x")
	cmd.Env = append(os.Environ(), "GFF_HAB="+addr)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "not_found") {
		t.Fatalf("send to nobody: %v %s", err, out)
	}
	id, _ = sendFromPane(t, addr, "bee/nobody", "x")
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateRefused || !strings.Contains(r.Reason, "no session named nobody") {
		t.Fatalf("record on a: %+v", r)
	}
}

// TestChannelsTargetRestart: B is down when A sends; A restarts with the message in its outbox;
// B comes back; the message arrives exactly once: the next message is the next thing bob reads
// (task acceptance 2).
func TestChannelsTargetRestart(t *testing.T) {
	p := newMeshPair(t)
	m1, m2 := pasted("ay/alice", "while you slept"), pasted("ay/alice", "and after")
	startReceiver(t, p.b, "idle", m1, m2)
	addr := startSender(t, p.a)

	p.b.stopHab(false)
	waitPeer(t, p.a.sock, "bee", "a sees b offline", func(p api.Peer) bool { return p.State == api.PeerStateOffline })
	id, st := sendFromPane(t, addr, "bee/bob", "while you slept")
	if st != api.ChannelStateQueued {
		t.Fatalf("state %s", st)
	}
	p.a.restart(false) // the outbox is on disk
	if r := channelStatus(t, p.a.sock, id, 0); r.State != api.ChannelStateQueued {
		t.Fatalf("after a's restart: %+v", r)
	}
	p.b.serve()
	waitUntil(t, "the queued message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-1"), withID(m1, id)))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on a: %+v", r)
	}
	id, _ = sendFromPane(t, addr, "bee/bob", "and after")
	waitUntil(t, "the next message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-2"), withID(m2, id)))
}

// TestChannelsOneWayWake: A reaches B, but B cannot reach A, so mail from B to A waits in B's
// outbox for A to pull. B sends mesh.changed over A's link and A pulls at once: the message
// arrives well inside A's 15 s mail round (mesh.md 6.5).
func TestChannelsOneWayWake(t *testing.T) {
	a, b := newHolderEnv(t, 0), newHolderEnv(t, 0)
	d := shortDir(t)
	standIn := sshStandIn(t, map[string]string{"hab-b": b.sock}) // nothing reaches a
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	for _, x := range []struct {
		e     *habEnv
		peers string
	}{{a, filesA}, {b, filesB}} {
		x.e.env = []string{
			"PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GFF_HAB_PEERS=" + x.peers,
			"XDG_RUNTIME_DIR=" + shortDir(t),
		}
	}
	a.serve()
	// b lists a at an endpoint it cannot reach, pinned, so ay/bob resolves.
	writeFile(t, filesB, fmt.Sprintf("[[peers]]\nlabel = \"ay\"\nendpoints = [\"hab-a\"]\ndevice_id = %q\n", welcome(t, a.sock).DeviceID))
	b.serve()
	t.Cleanup(func() {
		writeFile(t, filepath.Join(a.work, "end"), "")
		writeFile(t, filepath.Join(b.work, "end"), "")
	})
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	waitPeer(t, b.sock, "ay", "b sees a's link", func(p api.Peer) bool { return p.State == api.PeerStateOnline && p.Inbound })

	m1 := pasted("bee/alice", "you only pull")
	startReceiver(t, a, "idle", m1)
	addr := startSender(t, b)
	start := time.Now()
	id, st := sendFromPane(t, addr, "ay/bob", "you only pull")
	if st != api.ChannelStateQueued {
		t.Fatalf("state %s", st)
	}
	waitUntil(t, "the message in bob's pane", fileIs(filepath.Join(a.work, "msg-1"), withID(m1, id)))
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("delivered after %v: a pulled at its mail round, not at the wake-up", d)
	}
	if r := channelStatus(t, b.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on b: %+v", r)
	}
}

// TestChannelsWaitForIdle: a message to a working agent is handed over but waits in B's inbox
// until the agent reports idle (task acceptance 3).
func TestChannelsWaitForIdle(t *testing.T) {
	p := newMeshPair(t)
	m1 := pasted("ay/alice", "when you are free")
	startReceiver(t, p.b, "working", m1)
	addr := startSender(t, p.a)

	id, _ := sendFromPane(t, addr, "bee/bob", "when you are free")
	waitUntil(t, "handed to b", func() bool { return channelStatus(t, p.a.sock, id, 0).State == api.ChannelStateHanded })
	if _, err := os.Stat(filepath.Join(p.b.work, "msg-1")); err == nil {
		t.Fatal("delivered while working")
	}
	writeFile(t, filepath.Join(p.b.work, "go-idle"), "")
	waitUntil(t, "the message after idle", fileIs(filepath.Join(p.b.work, "msg-1"), withID(m1, id)))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on a: %+v", r)
	}
}

// TestChannelsWaitPeerAgent (task-mz98sb7l, task-dokhx3sg): gff-hab wait -agent on hab a waits
// on an agent on hab b and returns at its state change; from alice's pane on a, the MCP tools
// send to that agent and wait, by a bare name, for it to exit (mesh.md 6.6).
func TestChannelsWaitPeerAgent(t *testing.T) {
	p := newMeshPair(t)
	m1 := pasted("ay/alice", "over the tool")
	startReceiver(t, p.b, "working", m1)
	addr := startSender(t, p.a)

	done := make(chan string, 1)
	go func() {
		out, err := exec.Command(bin, "wait", "-socket", p.a.sock, "-agent", "bee/bob", "-for", "idle", "-timeout", "30s").CombinedOutput()
		if err != nil {
			out = append(out, []byte(" err: "+err.Error())...)
		}
		done <- string(out)
	}()
	select {
	case out := <-done:
		t.Fatalf("the wait returned while bob works: %q", out)
	case <-time.After(500 * time.Millisecond):
	}
	writeFile(t, filepath.Join(p.b.work, "go-idle"), "")
	if out := <-done; out != "bee/bob idle\n" {
		t.Fatalf("wait printed %q", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(40*time.Second))
	defer cancel()
	c := hookCaller{addr}
	send, _ := findIn(allTools(), "channel_send")
	res, text, err := send.run(ctx, c, toolArgs{"to": "bee/bob", "text": "over the tool"})
	if err != nil {
		t.Fatal(err)
	}
	sent := res.(api.ChannelSent)
	if !strings.Contains(text, "to bee/bob") {
		t.Fatalf("channel_send: %q", text)
	}
	waitUntil(t, "the tool's message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-1"), withID(m1, sent.ID)))
	if r := channelStatus(t, p.a.sock, sent.ID, 20000); r.State != api.ChannelStateDelivered || r.From != "alice" {
		t.Fatalf("record on a: %+v", r)
	}

	waitCatalog(t, p.a.sock, "a's copy of b lists bob", func(l api.CatalogList) bool {
		return channels.ClashLabel(l, welcome(t, p.a.sock).DeviceID, "bob") == "bee"
	})
	wait, _ := findIn(allTools(), "channel_wait")
	got := make(chan string, 1)
	go func() {
		_, text, err := wait.run(ctx, c, toolArgs{"agent": "bob", "for": "exited", "timeout_seconds": float64(30)})
		if err != nil {
			text += " err: " + err.Error()
		}
		got <- text
	}()
	select {
	case out := <-got:
		t.Fatalf("channel_wait returned while bob runs: %q", out)
	case <-time.After(500 * time.Millisecond):
	}
	writeFile(t, filepath.Join(p.b.work, "end"), "")
	if out := <-got; out != "bee/bob is exited.\n" {
		t.Fatalf("channel_wait: %q", out)
	}
}

// TestChannelsNameClash: creating, or renaming to, a name an agent holds on a peer fails
// (task acceptance 4).
func TestChannelsNameClash(t *testing.T) {
	p := newMeshPair(t)
	startReceiver(t, p.b, "idle")
	self := welcome(t, p.a.sock).DeviceID
	waitCatalog(t, p.a.sock, "a's copy of b lists bob", func(l api.CatalogList) bool { return channels.ClashLabel(l, self, "bob") == "bee" })

	c := p.a.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var res api.SessionCreated
	err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "bob", Argv: []string{"/bin/sh", "-c", fakeSender}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "DIR=" + p.a.work}}, &res)
	if errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "in use on bee") {
		t.Fatalf("create bob on a: %v", err)
	}
	ss := createSession(t, p.a.sock, api.SessionCreateParams{Name: "alice", Argv: []string{"/bin/sh", "-c", fakeSender}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "DIR=" + p.a.work}})
	err = c.Call(ctx, "session.rename", api.SessionRenameParams{Session: ss.Session, Name: "bob"}, nil)
	if errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "in use on bee") {
		t.Fatalf("rename to bob on a: %v", err)
	}
	// One charset for names (mesh.md 6.5), at create and at rename.
	err = c.Call(ctx, "session.create", api.SessionCreateParams{Name: "a b", Argv: []string{"/bin/sh", "-c", fakeSender}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "DIR=" + p.a.work}}, &res)
	if errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("create %q: %v", "a b", err)
	}
	err = c.Call(ctx, "session.rename", api.SessionRenameParams{Session: ss.Session, Name: "x$(y)"}, nil)
	if errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("rename to %q: %v", "x$(y)", err)
	}
	// A name gff-hab picks skips names held on the peer.
	prog := filepath.Join(p.a.work, "bob")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nwhile [ ! -e \"$DIR/end\" ]; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	picked := createSession(t, p.a.sock, api.SessionCreateParams{Argv: []string{prog}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "DIR=" + p.a.work}})
	var sr api.StructureResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", p.a.sock, "structure.get")), &sr); err != nil {
		t.Fatal(err)
	}
	odd := filepath.Join(p.a.work, "my prog$x")
	if err := os.WriteFile(odd, []byte("#!/bin/sh\nwhile [ ! -e \"$DIR/end\" ]; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cleaned := createSession(t, p.a.sock, api.SessionCreateParams{Argv: []string{odd}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "DIR=" + p.a.work}})
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", p.a.sock, "structure.get")), &sr); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, s := range sr.Sessions {
		names[s.ID] = s.Name
	}
	if names[picked.Session] != "bob-2" || names[cleaned.Session] != "my-prog-x" {
		t.Fatalf("picked names %q and %q, want bob-2 and my-prog-x", names[picked.Session], names[cleaned.Session])
	}
}

// TestChannelsPaneIdentity: the pane door sends and waits as the pane's own session, whatever
// the params name.
func TestChannelsPaneIdentity(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	addr := startSender(t, e)
	startReceiver(t, e, "working") // bob holds what it gets
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var sent api.ChannelSent
	if err := hook.Call(ctx, addr, "channel.send", api.ChannelSendParams{To: "bob", From: "bob", Text: "who am i"}, &sent); err != nil {
		t.Fatal(err)
	}
	if r := channelStatus(t, e.sock, sent.ID, 0); r.From != "alice" {
		t.Fatalf("sent as %q, want alice", r.From)
	}
	var w api.ChannelWaitResult
	if err := hook.Call(ctx, addr, "channel.wait", api.ChannelWaitParams{Session: "bob", TimeoutMS: 300}, &w); err != nil {
		t.Fatal(err)
	}
	if !w.TimedOut || w.Message != nil {
		t.Fatalf("alice's pane took bob's message: %+v", w)
	}
	if r := channelStatus(t, e.sock, sent.ID, 0); r.State != api.ChannelStateHanded {
		t.Fatalf("record %+v", r)
	}
}

// TestChannelsWaitCommand: on one hab, gff-hab wait from the receiving pane takes the message
// that a working agent's PTY would hold, and the sender sees it delivered.
func TestChannelsWaitCommand(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	addr := startSender(t, e)
	// bob is working and never idle; its own pane runs the wait.
	startReceiver(t, e, "working")
	done := make(chan string, 1)
	go func() {
		out, err := exec.Command(bin, "wait", "-socket", e.sock, "-session", "bob", "-timeout", "20s").CombinedOutput()
		if err != nil {
			out = append(out, []byte(" err: "+err.Error())...)
		}
		done <- string(out)
	}()
	id, st := sendFromPane(t, addr, "bob", "over the wait")
	if st != api.ChannelStateHanded {
		t.Fatalf("local send state %s", st)
	}
	out := <-done
	if want := "from alice (" + id + "):\nover the wait\n"; out != want {
		t.Fatalf("wait printed %q, want %q", out, want)
	}
	if r := channelStatus(t, e.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.work, "msg-1")); err == nil {
		t.Fatal("also written to the pane")
	}
	// Outside a pane, send needs -from.
	if out, err := exec.Command(bin, "send", "-socket", e.sock, "bob", "x").CombinedOutput(); err == nil || !strings.Contains(string(out), "-from") {
		t.Fatalf("send with no -from: %v %s", err, out)
	}
	if out := run(t, "send", "-socket", e.sock, "-from", "alice", "bob", "x"); !strings.Contains(out, "handed") {
		t.Fatalf("send -from: %q", out)
	}
	// -from takes the session's id too.
	var s api.StructureResult
	call(t, e.control(), "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, &s)
	aliceID := ""
	for _, ss := range s.Structure.Sessions {
		if ss.Name == "alice" {
			aliceID = ss.ID
		}
	}
	if out := run(t, "send", "-socket", e.sock, "-from", aliceID, "bob", "x"); aliceID == "" || !strings.Contains(out, "handed") {
		t.Fatalf("send -from %q: %q", aliceID, out)
	}
}

func TestWaitTimeoutDefault(t *testing.T) {
	for _, c := range []struct {
		set, control, inPane bool
		want                 time.Duration
	}{
		{false, false, true, channels.PaneWait},
		{true, false, true, time.Second},
		{false, true, true, time.Second},
		{false, false, false, time.Second},
	} {
		if got := waitTimeout(time.Second, c.set, c.control, c.inPane); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

// fakeChannels records what the pane route asks of the channels service.
type fakeChannels struct {
	def  time.Duration
	p    api.ChannelWaitParams
	took chan bool
}

func (f *fakeChannels) Send(context.Context, api.ChannelSendParams) (api.ChannelSent, error) {
	return api.ChannelSent{}, nil
}

func (f *fakeChannels) Status(context.Context, api.ChannelStatusParams) (api.ChannelStatusResult, error) {
	return api.ChannelStatusResult{}, nil
}

func (f *fakeChannels) WaitTake(_ context.Context, p api.ChannelWaitParams, def time.Duration) (api.ChannelWaitResult, func(bool), error) {
	f.def, f.p = def, p
	return api.ChannelWaitResult{Message: &api.ChannelInbound{ID: "m"}}, func(ok bool) { f.took <- ok }, nil
}

// TestChannelHookWait (N-R6): a wait from a pane with no timeout uses the pane default (110 s,
// under a harness's 2 minute tool limit), as the pane's own session; the route's write result
// reaches the service.
func TestChannelHookWait(t *testing.T) {
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder(structure.HolderPane{ID: "p"})})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateSession(structure.SessionSpec{Name: "bob", Pane: "p", Index: -1}); err != nil {
		t.Fatal(err)
	}
	f := &fakeChannels{took: make(chan bool, 1)}
	_, written, err := channelHook(st, f)(context.Background(), "p", "channel.wait", json.RawMessage(`{"session":"alice"}`))
	if err != nil || written == nil {
		t.Fatalf("wait: %v", err)
	}
	if f.def != channels.PaneWait || f.p.Session != "bob" {
		t.Fatalf("waited %v as %q, want %v as bob", f.def, f.p.Session, channels.PaneWait)
	}
	written(errors.New("broken pipe"))
	if ok := <-f.took; ok {
		t.Fatal("a failed write counted as taken")
	}
}
