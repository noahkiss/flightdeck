package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/devicestore"
	"golang.org/x/crypto/ssh"
)

// TestDevicesListRevoke: devices.list reads the device list; devices.revoke by label removes the
// device's key line and keeps its record as a tombstone; an unknown device is not_found
// (transport.md 5, 6.6).
func TestDevicesListRevoke(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	keys := filepath.Join(dir, "authorized_keys")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sk, _ := ssh.NewPublicKey(pub)
	now := time.Now()
	line, err := devicestore.KeyLine(sk, "/usr/local/bin/gff-hab connect", "tablet", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys, []byte("# mine\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := devicestore.Open(state).Add(devicestore.Record{
		ID: ssh.FingerprintSHA256(sk), Label: "tablet", Kind: "tablet", PairedAt: now.UTC().Format(time.RFC3339),
		Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sk))), AuthorizedKeys: keys, State: devicestore.StatePaired,
	}); err != nil {
		t.Fatal(err)
	}
	svc := newDeviceService(state, nil)
	devs, err := svc.List()
	if err != nil || len(devs) != 1 || devs[0].Kind != "tablet" || devs[0].State != "paired" {
		t.Fatalf("list: %+v %v", devs, err)
	}
	if err := svc.Revoke("tablet"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(keys); string(b) != "# mine\n" {
		t.Errorf("authorized_keys after revoke: %q", b)
	}
	if devs, _ = svc.List(); len(devs) != 1 || devs[0].State != "revoked" {
		t.Errorf("after revoke: %+v", devs)
	}
	var ce *control.Error
	if err := svc.Revoke("no-such-device"); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("revoke unknown: %v", err)
	}
}

// TestDevicesConcurrentRevokes: two revokes at once, of two devices in one authorized_keys
// file, both land: two tombstones and neither key line left.
func TestDevicesConcurrentRevokes(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	keys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(keys, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := devicestore.Open(state)
	for _, label := range []string{"one", "two"} {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		sk, _ := ssh.NewPublicKey(pub)
		line, _ := devicestore.KeyLine(sk, "/usr/local/bin/gff-hab connect", label, time.Now())
		if _, err := devicestore.AppendAuthorizedKey(keys, sk, line); err != nil {
			t.Fatal(err)
		}
		if err := store.Add(devicestore.Record{ID: ssh.FingerprintSHA256(sk), Label: label, Kind: "phone",
			Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sk))), AuthorizedKeys: keys, State: devicestore.StatePaired}); err != nil {
			t.Fatal(err)
		}
	}
	svc := newDeviceService(state, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, label := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- svc.Revoke(label)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	devs, err := svc.List()
	if err != nil || len(devs) != 2 || devs[0].State != "revoked" || devs[1].State != "revoked" {
		t.Fatalf("after two revokes: %+v %v", devs, err)
	}
	if b, _ := os.ReadFile(keys); string(b) != "# mine\n" {
		t.Errorf("authorized_keys after two revokes: %q", b)
	}
}
