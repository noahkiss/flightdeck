package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/devicestore"
	"github.com/noahkiss/flightdeck/internal/pairing"
	"golang.org/x/crypto/ssh"
)

// TestConnectDeviceRevoked runs the forced command of a paired key, `gff-hab connect -device
// -devices`, as sshd would: it carries a session while the device is paired; a revoke, from the
// API or `gff-hab devices revoke`, ends that session within seconds; and a new channel for the
// revoked device, or for one the list does not hold, is refused (transport.md 6.6).
func TestConnectDeviceRevoked(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	state := filepath.Join(d, "state dir")
	keys := filepath.Join(d, "authorized_keys")
	if err := os.WriteFile(keys, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := control.New(control.Config{Build: "test", Host: api.HostInfo{ID: "test-host"}, DeviceID: "SHA256:x"})
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()

	// Pair the device as gff-hab does, then run the key line's forced command through the
	// shell, as sshd does. The state folder has a space, as the macOS default does.
	store := devicestore.Open(state)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sk, _ := ssh.NewPublicKey(pub)
	id := ssh.FingerprintSHA256(sk)
	self, err := shellWord(bin)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := shellWord(sock)
	if err != nil {
		t.Fatal(err)
	}
	a := admitter{store: store, authKeys: keys, command: self + " connect -socket " + sw, watch: store.Path}
	if _, err := a.admit(pairing.Device{Name: "phone", Kind: "phone", Key: sk, Fingerprint: id}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(keys)
	_, rest, _ := strings.Cut(string(b), `command="`)
	forced, _, ok := strings.Cut(rest, `"`)
	if !ok || !strings.Contains(forced, "'"+store.Path+"'") {
		t.Fatalf("authorized_keys:\n%s", b)
	}

	cmd := exec.Command("/bin/sh", "-c", forced)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	c := control.NewClient(pipeConn{Reader: out, WriteCloser: in})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "t", Version: "0", Kind: api.ClientKindApp}, "phone", ""); err != nil {
		t.Fatalf("hello while paired: %v (%s)", err, stderr.String())
	}

	if _, _, err := store.Revoke("phone", time.Now()); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	select {
	case err := <-ended:
		if err == nil || !strings.Contains(stderr.String(), "revoked") {
			t.Fatalf("connect ended with %v, stderr %q", err, stderr.String())
		}
	case <-time.After(8 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the revoked device's session is still open")
	}

	// A new channel runs the forced command again: refused for the revoked device, for one the
	// list does not hold, and for an unreadable list.
	if b, err := exec.Command("/bin/sh", "-c", forced).CombinedOutput(); err == nil || !strings.Contains(string(b), "revoked") {
		t.Errorf("a new channel through the key line: %v %q", err, b)
	}
	for _, who := range []string{id, "SHA256:someone-else"} {
		b, err := exec.Command(bin, "connect", "-socket", sock, "-device", who, "-devices", store.Path).CombinedOutput()
		if err == nil || !strings.Contains(string(b), who) {
			t.Errorf("a new channel for %s: %v %q", who, err, b)
		}
	}
	// A list that cannot be read fails closed.
	if b, err := exec.Command(bin, "connect", "-socket", sock, "-device", id, "-devices", filepath.Join(d, "missing", "devices.json")).CombinedOutput(); err == nil {
		t.Errorf("connect with no device list: %q", b)
	}
}
