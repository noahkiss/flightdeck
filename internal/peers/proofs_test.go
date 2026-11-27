package peers

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/habkey"
)

// startProvingHab is startHab with a real hab key: its links prove the key, and its server
// remembers the devices that proved theirs.
func startProvingHab(t *testing.T, dir, name, peers string) (*hab, *Proofs) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := &habkey.Key{Private: priv}
	h := &hab{sock: filepath.Join(dir, name+".sock"), id: key.Fingerprint(), file: filepath.Join(dir, name+".toml")}
	write(t, h.file, peers)
	m, err := Open(Config{Path: h.file, Self: h.id, Host: name, Build: "test", Poll: 50 * time.Millisecond, Prove: key.Prove,
		Conn: func(c *habconn.Config) {
			c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 100*time.Millisecond
			c.PingInterval, c.DeadAfter = 200*time.Millisecond, 2*time.Second
		}})
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	proofs, err := OpenProofs(filepath.Join(dir, name+".proofs"))
	if err != nil {
		t.Fatal(err)
	}
	h.srv = control.New(control.Config{Build: "test", Host: api.HostInfo{ID: name}, DeviceID: h.id, Capabilities: []string{Capability},
		Source: emptySource{}, Peers: m, PeerProofs: proofs})
	l, err := control.Listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	go h.srv.Serve(l)
	t.Cleanup(h.stop)
	return h, proofs
}

// Two habs link and each proves its hab key to the other through habconn; the proof survives in
// the file (transport.md 6.3).
func TestLinksProve(t *testing.T) {
	d := shortDir(t)
	file := `[[peers]]
label = "a"
endpoints = ["` + filepath.Join(d, "a.sock") + `"]

[[peers]]
label = "b"
endpoints = ["` + filepath.Join(d, "b.sock") + `"]
`
	a, pa := startProvingHab(t, d, "a", file)
	b, pb := startProvingHab(t, d, "b", file)
	wait(t, "each proved its key to the other", func() bool { return pa.Proved(b.id) && pb.Proved(a.id) })
	wait(t, "both online", func() bool {
		x, ok1 := peer(a.m.List(), "b")
		y, ok2 := peer(b.m.List(), "a")
		return ok1 && ok2 && x.State == api.PeerStateOnline && y.State == api.PeerStateOnline && x.Inbound && y.Inbound
	})
	again, err := OpenProofs(filepath.Join(d, "a.proofs"))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Proved(b.id) || again.Proved(a.id) {
		t.Fatal("the proof file does not hold b alone")
	}
}
