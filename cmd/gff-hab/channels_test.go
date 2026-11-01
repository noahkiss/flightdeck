package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
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
		x.e.env = []string{
			"PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GFF_HAB_PEERS=" + x.peers,
			"XDG_RUNTIME_DIR=" + shortDir(t),
		}
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

// pasted is what a message from sender looks like in the receiving pane.
func pasted(from, text string) string {
	return "\x1b[200~" + channels.Envelope(from, text) + "\x1b[201~\r"
}

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
	waitUntil(t, "the first message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-1"), m1))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered || r.From != "alice" || r.To != "bee/bob" {
		t.Fatalf("record on a: %+v", r)
	}

	waitCatalog(t, p.a.sock, "a's copy of b lists bob", func(l api.CatalogList) bool {
		return channels.ClashLabel(l, welcome(t, p.a.sock).DeviceID, "bob") == "bee"
	})
	id, _ = sendFromPane(t, addr, "bob", "second line\nby name")
	waitUntil(t, "the second message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-2"), m2))
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
	waitUntil(t, "the queued message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-1"), m1))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on a: %+v", r)
	}
	sendFromPane(t, addr, "bee/bob", "and after")
	waitUntil(t, "the next message in bob's pane", fileIs(filepath.Join(p.b.work, "msg-2"), m2))
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
	waitUntil(t, "the message after idle", fileIs(filepath.Join(p.b.work, "msg-1"), m1))
	if r := channelStatus(t, p.a.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record on a: %+v", r)
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
}
