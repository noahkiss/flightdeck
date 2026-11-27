package peers

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	proofs := OpenProofs(filepath.Join(dir, name+".proofs"), t.Logf)
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
	again := OpenProofs(filepath.Join(d, "a.proofs"), t.Logf)
	if !again.Proved(b.id) || again.Proved(a.id) {
		t.Fatal("the proof file does not hold b alone")
	}
}

// A proof file the hab cannot read in full does not stop it: it logs, and keeps the ids read
// before the problem. A line longer than bufio's default 64 KiB is a bad id, not an error
// (transport.md 6.3).
func TestProofsReadProblems(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }

	long := filepath.Join(d, "long")
	content := "SHA256:first\n" + strings.Repeat("x", 100<<10) + "\nSHA256:after\n"
	if err := os.WriteFile(long, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := OpenProofs(long, logf)
	if !p.Proved("SHA256:first") || !p.Proved("SHA256:after") || len(logged) != 0 {
		t.Fatalf("a long line: first %v, after %v, logged %q", p.Proved("SHA256:first"), p.Proved("SHA256:after"), logged)
	}

	over := filepath.Join(d, "over")
	content = "SHA256:first\n" + strings.Repeat("y", maxProofFile) + "\nSHA256:late\n"
	if err := os.WriteFile(over, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p = OpenProofs(over, logf)
	if !p.Proved("SHA256:first") || p.Proved("SHA256:late") || len(logged) != 1 {
		t.Fatalf("a file over the cap: first %v, late %v, logged %q", p.Proved("SHA256:first"), p.Proved("SHA256:late"), logged)
	}

	// A folder opens but does not read: an error after open.
	dir := filepath.Join(d, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p = OpenProofs(dir, logf)
	if p.Proved("SHA256:first") || len(logged) != 2 {
		t.Fatalf("an unreadable file: logged %q", logged)
	}
	if err := p.Record("SHA256:new"); err == nil {
		t.Fatal("Record into a folder worked")
	}
}
