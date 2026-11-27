package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habkey"
)

// peerPID reads the pid of the process that connected: this test process (spec 4.1).
func TestPeerPID(t *testing.T) {
	path := socketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan int, 1)
	go func() {
		nc, err := l.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer nc.Close()
		raw, _ := nc.(*net.UnixConn).SyscallConn()
		pid := -1
		raw.Control(func(fd uintptr) { pid, _ = peerPID(int(fd)) })
		got <- pid
	}()
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if pid := <-got; pid != os.Getpid() {
		t.Fatalf("peer pid %d, want %d", pid, os.Getpid())
	}
}

// localHab serves a socket whose every caller LocalCaller marks as a pane's, or not.
func localHab(t *testing.T, pane bool, tweak func(*Config)) (path string, pids chan int) {
	t.Helper()
	path = socketPath(t)
	pids = make(chan int, 16)
	startHab(t, "", path, func(c *Config) {
		c.LocalCaller = func(pid int, at time.Time) (bool, string) {
			pids <- pid
			return pane, "test"
		}
		if tweak != nil {
			tweak(c)
		}
	})
	return path, pids
}

var habClient = api.ClientInfo{Name: "gff-hab", Version: "0", Kind: api.ClientKindHab}

// Serve hands LocalCaller the pid the socket gave: this process.
func TestLocalCallerPID(t *testing.T) {
	path, pids := localHab(t, false, nil)
	hello(t, dial(t, path), "")
	if pid := <-pids; pid != os.Getpid() {
		t.Fatalf("LocalCaller got pid %d, want %d", pid, os.Getpid())
	}
}

// personOnlyCalls are the person-only methods (spec 4.1), with params that pass decoding.
var personOnlyCalls = []struct {
	method string
	params any
}{
	{"confirm.answer", map[string]any{"id": "c1", "answer": "confirm"}},
	{"trust.answer", map[string]any{"pane": "p1", "trust": true}},
	{"approval.answer", map[string]any{"pane": "p1", "request": "r1", "decision": "allow"}},
	{"capcom.card.answer", map[string]any{"id": "k1"}},
	{"settings.set", map[string]any{"id": "capcom.event_actions", "value": "all"}},
	{"peers.accept", map[string]any{"label": "desk"}},
	{"push.register", map[string]any{}},
	{"devices.list", map[string]any{}},
	{"devices.revoke", map[string]any{"id": "abcdef"}},
}

// A process inside a pane cannot call a person-only method, whatever its environment holds: the
// hab judges the process, not GFF_HAB. Other calls work. A caller outside every pane reaches
// each method's own handler (here unavailable or invalid, since the test hab has no services).
func TestPaneCallerPersonOnly(t *testing.T) {
	for _, pane := range []bool{true, false} {
		path, _ := localHab(t, pane, nil)
		c := dial(t, path)
		hello(t, c, "")
		for _, call := range personOnlyCalls {
			err := c.Call(ctx(t), call.method, call.params, nil)
			denied := errKind(err) == api.ErrorKindDenied && strings.Contains(err.Error(), "inside a Flightdeck pane")
			if denied != pane {
				t.Errorf("pane %v: %s: %v", pane, call.method, err)
			}
		}
		var st api.StructureResult
		if err := c.Call(ctx(t), "structure.get", api.StructureGetParams{}, &st); err != nil {
			t.Errorf("pane %v: structure.get: %v", pane, err)
		}
	}
}

// A process inside a pane cannot create a Capcom round that carries action words.
func TestPaneCallerRoundWords(t *testing.T) {
	path, _ := localHab(t, true, nil)
	c := dial(t, path)
	hello(t, c, "")
	err := c.Call(ctx(t), "round.create", api.RoundCreateParams{Round: api.RoundSpec{Name: "job", Actions: []api.RoundAction{"hand_off"}}}, nil)
	if errKind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "action words") {
		t.Fatalf("round.create with words from a pane: %v", err)
	}
}

// A process inside a pane cannot say hello as a hab, so it never reaches the peer-only methods;
// a caller outside every pane that says the same hello passes the gate (a legacy link).
func TestPaneCallerHelloAsHab(t *testing.T) {
	for _, pane := range []bool{true, false} {
		path, _ := localHab(t, pane, nil)
		c := dial(t, path)
		_, err := c.HelloAs(ctx(t), api.HelloParams{Client: habClient, Peer: &api.PeerHello{DeviceID: "SHA256:peer", Host: "laptop"}})
		if pane {
			if errKind(err) != api.ErrorKindDenied {
				t.Fatalf("hello as a hab from a pane: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Call(ctx(t), "group.tell", map[string]any{}, nil); gateRefused(err) {
			t.Fatalf("a legacy peer link outside every pane: %v", err)
		}
	}
}

// gateRefused says whether err is the peer-only gate's refusal.
func gateRefused(err error) bool {
	return errKind(err) == api.ErrorKindDenied && strings.Contains(err.Error(), "peer link only")
}

// fakeProofs is a PeerProofs in memory.
type fakeProofs struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (f *fakeProofs) Proved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ids[id] }
func (f *fakeProofs) Record(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[id] = true
	return nil
}

// callerPanes records the PaneCaller of each pane.read.
type callerPanes struct {
	Panes
	got chan PaneCaller
}

func (p callerPanes) Read(ctx context.Context, from PaneCaller, _ api.PaneReadParams) (api.PaneText, error) {
	p.got <- from
	return api.PaneText{}, nil
}

func testKey(t *testing.T) *habkey.Key {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &habkey.Key{Private: priv}
}

// proofHab serves a socket with a proof store and a pane reader that records its callers.
func proofHab(t *testing.T, proved ...string) (string, *fakeProofs, callerPanes) {
	t.Helper()
	path := socketPath(t)
	fp := &fakeProofs{ids: map[string]bool{}}
	for _, id := range proved {
		fp.ids[id] = true
	}
	panes := callerPanes{got: make(chan PaneCaller, 4)}
	startHab(t, "", path, func(c *Config) { c.PeerProofs = fp; c.Panes = panes })
	return path, fp, panes
}

func peerHello(t *testing.T, c *Client, device string) api.Welcome {
	t.Helper()
	w, err := c.HelloAs(ctx(t), api.HelloParams{Client: habClient, Peer: &api.PeerHello{DeviceID: device, Host: "laptop"}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func readCaller(t *testing.T, c *Client, panes callerPanes) PaneCaller {
	t.Helper()
	if err := c.Call(ctx(t), "pane.read", api.PaneReadParams{Pane: "p1"}, nil); err != nil {
		t.Fatal(err)
	}
	return <-panes.got
}

// A peer link that proves its hab key carries the key's device id, and the hab remembers the
// device. One that does not prove stays a legacy link with no device id for the Capcom mark.
func TestPeerProof(t *testing.T) {
	a := testKey(t)
	path, fp, panes := proofHab(t)

	legacy := dial(t, path)
	if w := peerHello(t, legacy, a.Fingerprint()); w.PeerNonce == "" {
		t.Fatal("no peer_nonce in a peer hello's welcome")
	}
	if pc := readCaller(t, legacy, panes); !pc.Peer || pc.Device != "" {
		t.Fatalf("an unproven link's caller: %+v", pc)
	}

	c := dial(t, path)
	w := peerHello(t, c, a.Fingerprint())
	key, sig := a.Prove(w.PeerNonce, w.DeviceID)
	r, err := api.Client{Caller: c}.HabProve(ctx(t), api.HabProveParams{Key: key, Signature: sig})
	if err != nil || r.DeviceID != a.Fingerprint() {
		t.Fatalf("hab.prove: %+v %v", r, err)
	}
	if pc := readCaller(t, c, panes); pc.Device != a.Fingerprint() {
		t.Fatalf("a proven link's caller: %+v", pc)
	}
	if !fp.Proved(a.Fingerprint()) {
		t.Fatal("the proof was not remembered")
	}
	// The nonce serves one attempt.
	if _, err := (api.Client{Caller: c}).HabProve(ctx(t), api.HabProveParams{Key: key, Signature: sig}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a second hab.prove: %v", err)
	}
}

// A peer's hello cannot claim another device than its key: a proof by another key, or one
// made for another hab, closes the connection.
func TestPeerProofWrongKey(t *testing.T) {
	a, b := testKey(t), testKey(t)
	path, fp, _ := proofHab(t)
	for name, prove := range map[string]func(w api.Welcome) (string, string){
		"another key":      func(w api.Welcome) (string, string) { return b.Prove(w.PeerNonce, w.DeviceID) },
		"another receiver": func(w api.Welcome) (string, string) { return a.Prove(w.PeerNonce, "SHA256:another-hab") },
		"another nonce":    func(w api.Welcome) (string, string) { return a.Prove("bm9uY2U=", w.DeviceID) },
	} {
		c := dial(t, path)
		w := peerHello(t, c, a.Fingerprint())
		key, sig := prove(w)
		if _, err := (api.Client{Caller: c}).HabProve(ctx(t), api.HabProveParams{Key: key, Signature: sig}); errKind(err) != api.ErrorKindDenied {
			t.Fatalf("%s: hab.prove: %v", name, err)
		}
		if err := c.Call(ctx(t), "hab.ping", api.PingParams{}, nil); err == nil {
			t.Fatalf("%s: the connection stayed open", name)
		}
	}
	if fp.Proved(a.Fingerprint()) {
		t.Fatal("a failed proof was remembered")
	}
}

// Once a device proved its key here, a hello that only claims its id does not pass the
// peer-only gate, from a process outside every pane too, until the link proves the key.
func TestPeerProofHeld(t *testing.T) {
	a := testKey(t)
	path, _, panes := proofHab(t, a.Fingerprint())
	c := dial(t, path)
	w := peerHello(t, c, a.Fingerprint())
	if err := c.Call(ctx(t), "group.tell", map[string]any{}, nil); !gateRefused(err) {
		t.Fatalf("a claim of a proven device passed the peer-only gate: %v", err)
	}
	if pc := readCaller(t, c, panes); pc.Device != "" {
		t.Fatalf("a held claim's caller: %+v", pc)
	}
	key, sig := a.Prove(w.PeerNonce, w.DeviceID)
	if _, err := (api.Client{Caller: c}).HabProve(ctx(t), api.HabProveParams{Key: key, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx(t), "group.tell", map[string]any{}, nil); gateRefused(err) {
		t.Fatalf("a proven link was refused by the gate: %v", err)
	}
}
