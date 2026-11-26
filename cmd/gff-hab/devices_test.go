package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/devicestore"
	"github.com/noahkiss/flightdeck/internal/pairing"
	"golang.org/x/crypto/ssh"
)

// devicesFixture is a device service on loopback with temporary files.
type devicesFixture struct {
	svc      *deviceService
	authKeys string
}

func newDevicesFixture(t *testing.T) devicesFixture {
	t.Helper()
	dir := t.TempDir()
	keys := filepath.Join(dir, "ssh", "authorized_keys")
	os.MkdirAll(filepath.Dir(keys), 0o700)
	os.WriteFile(keys, []byte("# mine\n"), 0o600)
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	svc := newDeviceService(deviceConfig{
		state: filepath.Join(dir, "state"), hostKey: hk, label: "devbox", authKeys: keys,
		command: "/usr/local/bin/gff-hab connect", user: "owner", sshdKeys: filepath.Join(dir, "no-sshd"),
		listen:   func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil },
		lifetime: 30 * time.Second,
	})
	t.Cleanup(svc.Close)
	return devicesFixture{svc: svc, authKeys: keys}
}

// phone plays the device: it dials the offer's first endpoint, pins the hab's key, presents the
// token and its name, and sends each answer line it reads.
func phone(t *testing.T, o api.PairingOffer, signer ssh.Signer) <-chan pairing.Answer {
	t.Helper()
	c, err := ssh.Dial("tcp", o.Endpoints[0], &ssh.ClientConfig{
		User: "u", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != o.DeviceID {
				return fmt.Errorf("host key %s, offer says %s", ssh.FingerprintSHA256(key), o.DeviceID)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{pairing.Subsystem})); !ok || err != nil {
		t.Fatalf("subsystem %v %v", ok, err)
	}
	b, _ := json.Marshal(map[string]any{"v": 1, "token": o.Token, "name": "Owner's iPad", "kind": "tablet"})
	fmt.Fprintf(ch, "%s\n", b)
	out := make(chan pairing.Answer, 4)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(ch)
		for sc.Scan() {
			var a pairing.Answer
			json.Unmarshal(sc.Bytes(), &a)
			out <- a
		}
	}()
	return out
}

func newPhoneKey(t *testing.T) (ssh.Signer, string) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s, _ := ssh.NewSignerFromKey(k)
	fp := ssh.FingerprintSHA256(s.PublicKey())
	return s, fp
}

func lastAnswer(t *testing.T, ch <-chan pairing.Answer) pairing.Answer {
	t.Helper()
	var last pairing.Answer
	deadline := time.After(10 * time.Second)
	for {
		select {
		case a, ok := <-ch:
			if !ok {
				return last
			}
			last = a
		case <-deadline:
			t.Fatal("the device heard no final answer")
		}
	}
}

// TestDevicesPairConfirm: devices.pair opens the window in the running hab; devices.confirm
// shows the waiting device with its code hidden, refuses a wrong code with the tries left, and
// pairs it on the right one: the key line lands and devices.list shows the device. Only the
// client that opened the window answers it, and one window is open at a time
// (transport.md 6.1.7).
func TestDevicesPairConfirm(t *testing.T) {
	f := newDevicesFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	o, err := f.svc.Pair("c1", api.DevicesPairParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Endpoints) != 1 || !strings.HasPrefix(o.Endpoints[0], "127.0.0.1:") || o.ExpiresInMS != 30000 || len(o.Token) != 22 {
		t.Fatalf("offer %+v", o)
	}
	if _, err := f.svc.Pair("c1", api.DevicesPairParams{}); errKindOf(err) != api.ErrorKindConflict {
		t.Fatalf("a second window: %v", err)
	}
	if _, err := f.svc.Confirm(ctx, "c2", api.DevicesConfirmParams{}); errKindOf(err) != api.ErrorKindDenied {
		t.Fatalf("another client's confirm: %v", err)
	}
	signer, fp := newPhoneKey(t)
	answers := phone(t, o, signer)
	w, err := f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{})
	if err != nil || w.State != "waiting" || w.Device == nil || w.Device.Name != "Owner's iPad" || w.Device.Kind != "tablet" ||
		w.Device.Fingerprint != fp[:len(fp)-4]+"????" || w.Device.From != "127.0.0.1" {
		t.Fatalf("waiting: %+v %v", w, err)
	}
	if strings.Contains(fmt.Sprint(w), fp) {
		t.Fatal("the answer carries the code the owner must read off the device")
	}
	wrong := "zzzz"
	if fp[len(fp)-4:] == wrong {
		wrong = "yyyy"
	}
	if w, err = f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{Code: wrong}); err != nil || w.State != "waiting" || w.TriesLeft != 2 {
		t.Fatalf("a wrong code: %+v %v", w, err)
	}
	p, err := f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{Code: fp[len(fp)-4:]})
	if err != nil || p.State != "paired" || p.Paired == nil || p.Paired.ID != fp || p.Paired.Kind != "tablet" {
		t.Fatalf("paired: %+v %v", p, err)
	}
	if a := lastAnswer(t, answers); a.State != "paired" || a.User != "owner" || !a.ForcedCommand {
		t.Fatalf("the device heard %+v", a)
	}
	b, _ := os.ReadFile(f.authKeys)
	if !strings.HasPrefix(string(b), "# mine\nrestrict,command=\"/usr/local/bin/gff-hab connect\" ecdsa-sha2-nistp256 ") || !strings.Contains(string(b), "flightdeck: Owner's iPad") {
		t.Fatalf("authorized_keys:\n%s", b)
	}
	devs, err := f.svc.List()
	if err != nil || len(devs) != 1 || devs[0].ID != fp || devs[0].State != "paired" {
		t.Fatalf("list %+v %v", devs, err)
	}
	// The window is closed: a later confirm reads the outcome; a new window may open.
	if p, err := f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{}); err != nil || p.State != "paired" {
		t.Fatalf("after: %+v %v", p, err)
	}

	// Revoke: the line goes, the record stays as a tombstone.
	if err := f.svc.Revoke("no-such"); errKindOf(err) != api.ErrorKindNotFound {
		t.Fatalf("revoke an unknown device: %v", err)
	}
	if err := f.svc.Revoke(fp); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.authKeys); string(b) != "# mine\n" {
		t.Fatalf("after revoke:\n%s", b)
	}
	if devs, _ := f.svc.List(); len(devs) != 1 || devs[0].State != "revoked" {
		t.Fatalf("after revoke: %+v", devs)
	}
}

// TestDevicesConfirmDeclines: three wrong codes decline the device, as does decline, and a
// decline with no device waiting closes the window; nothing lands in authorized_keys.
func TestDevicesConfirmDeclines(t *testing.T) {
	for _, how := range []string{"three wrong codes", "decline", "close"} {
		t.Run(how, func(t *testing.T) {
			f := newDevicesFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			o, err := f.svc.Pair("c1", api.DevicesPairParams{})
			if err != nil {
				t.Fatal(err)
			}
			var answers <-chan pairing.Answer
			var p api.DevicePairing
			switch how {
			case "three wrong codes":
				signer, _ := newPhoneKey(t)
				answers = phone(t, o, signer)
				for i := 0; i < 3; i++ {
					if p, err = f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{Code: "!!!!"}); err != nil {
						t.Fatal(err)
					}
				}
			case "decline":
				signer, _ := newPhoneKey(t)
				answers = phone(t, o, signer)
				if p, err = f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{}); err != nil || p.State != "waiting" {
					t.Fatalf("waiting: %+v %v", p, err)
				}
				p, err = f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{Decline: true})
			case "close":
				p, err = f.svc.Confirm(ctx, "c1", api.DevicesConfirmParams{Decline: true})
			}
			if err != nil || p.State != "declined" {
				t.Fatalf("%s: %+v %v", how, p, err)
			}
			if answers != nil {
				if a := lastAnswer(t, answers); a.State != "refused" || a.Reason != "declined" {
					t.Fatalf("the device heard %+v", a)
				}
			}
			if b, _ := os.ReadFile(f.authKeys); string(b) != "# mine\n" {
				t.Fatalf("authorized_keys:\n%s", b)
			}
			if _, err := f.svc.Pair("c1", api.DevicesPairParams{}); err != nil {
				t.Fatalf("a new window after the decline: %v", err)
			}
		})
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
	svc := newDeviceService(deviceConfig{state: state})
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
