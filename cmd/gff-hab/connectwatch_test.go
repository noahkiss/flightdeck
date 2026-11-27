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
	state := filepath.Join(d, "state")
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

	store := devicestore.Open(state)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sk, _ := ssh.NewPublicKey(pub)
	id := ssh.FingerprintSHA256(sk)
	line, _ := devicestore.KeyLine(sk, "gff-hab connect -device "+id+" -devices "+store.Path, "phone", time.Now())
	if _, err := devicestore.AppendAuthorizedKey(keys, sk, line); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(devicestore.Record{ID: id, Label: "phone", Kind: "phone", Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sk))),
		AuthorizedKeys: keys, State: devicestore.StatePaired}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "connect", "-socket", sock, "-device", id, "-devices", store.Path)
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
