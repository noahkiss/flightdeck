package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/devicestore"
	"github.com/noahkiss/flightdeck/internal/habkey"
	"github.com/noahkiss/flightdeck/internal/pairing"
	"golang.org/x/crypto/ssh"
)

// noKeyLeft fails when the device left a key line or a record behind.
func noKeyLeft(t *testing.T, f devicesFixture) {
	t.Helper()
	if b, _ := os.ReadFile(f.authKeys); string(b) != "# mine\n" {
		t.Fatalf("authorized_keys:\n%s", b)
	}
	if devs, err := f.svc.List(); err != nil || len(devs) != 0 {
		t.Fatalf("device list: %+v %v", devs, err)
	}
}

// TestDevicesPairOff: with [pairing] api off, the default, devices.pair and devices.confirm
// answer unavailable and open nothing; list and revoke still work (transport.md 6.1.7).
func TestDevicesPairOff(t *testing.T) {
	f := newDevicesFixture(t, func(c *deviceConfig) { c.pair = false })
	if _, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{}); errKindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "[pairing]") {
		t.Fatalf("devices.pair with pairing off: %v", err)
	}
	if _, err := f.svc.Confirm(context.Background(), byConn(context.Background(), 1), api.DevicesConfirmParams{}); errKindOf(err) != api.ErrorKindUnavailable {
		t.Fatalf("devices.confirm with pairing off: %v", err)
	}
	if devs, err := f.svc.List(); err != nil || len(devs) != 0 {
		t.Fatalf("list: %+v %v", devs, err)
	}
}

// TestDevicesPairOpenerGone: the connection that opened the window ends while a device waits:
// the window closes, the device is refused, and no key or record is left (the orphan window).
func TestDevicesPairOpenerGone(t *testing.T) {
	f := newDevicesFixture(t)
	opener, hangUp := context.WithCancel(context.Background())
	o, err := f.svc.Pair(byConn(opener, 1), api.DevicesPairParams{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	signer, _ := newPhoneKey(t)
	answers := phone(t, o, signer)
	if p, err := f.svc.Confirm(ctx, byConn(context.Background(), 1), api.DevicesConfirmParams{}); err != nil || p.State != "waiting" {
		t.Fatalf("waiting: %+v %v", p, err)
	}
	hangUp()
	if a := lastAnswer(t, answers); a.State == "paired" {
		t.Fatalf("the device heard %+v", a)
	}
	p, err := f.svc.Confirm(ctx, byConn(context.Background(), 1), api.DevicesConfirmParams{})
	if err != nil || p.State != "closed" || !strings.Contains(p.Reason, "disconnected") {
		t.Fatalf("after the opener left: %+v %v", p, err)
	}
	noKeyLeft(t, f)
	if _, err := f.svc.Pair(byConn(context.Background(), 2), api.DevicesPairParams{}); err != nil {
		t.Fatalf("a new window after the orphan closed: %v", err)
	}

	// An opener already gone when the window opens closes it at once.
	g := newDevicesFixture(t)
	gone, end := context.WithCancel(context.Background())
	end()
	if _, err := g.svc.Pair(byConn(gone, 1), api.DevicesPairParams{}); err != nil {
		t.Fatal(err)
	}
	if p, err := g.svc.Confirm(ctx, byConn(context.Background(), 1), api.DevicesConfirmParams{}); err != nil || p.State != "closed" {
		t.Fatalf("a window whose opener was gone: %+v %v", p, err)
	}
}

// TestDevicesPairExpires: a device presents the token and nobody answers: the window expires
// and leaves no key or record behind.
func TestDevicesPairExpires(t *testing.T) {
	f := newDevicesFixture(t, func(c *deviceConfig) { c.lifetime = 2 * time.Second })
	o, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	signer, _ := newPhoneKey(t)
	answers := phone(t, o, signer)
	// On a slow machine the window may expire before the device is shown: both are fine.
	if p, err := f.svc.Confirm(ctx, byConn(context.Background(), 1), api.DevicesConfirmParams{}); err != nil || (p.State != "waiting" && p.State != "expired") {
		t.Fatalf("waiting: %+v %v", p, err)
	}
	if a := lastAnswer(t, answers); a.State == "paired" {
		t.Fatalf("the device heard %+v", a)
	}
	if p, err := f.svc.Confirm(ctx, byConn(context.Background(), 1), api.DevicesConfirmParams{}); err != nil || p.State != "expired" {
		t.Fatalf("after the lifetime: %+v %v", p, err)
	}
	noKeyLeft(t, f)
}

// TestDevicesPairStateTooOpen: a state folder or device list that group or others can write, or
// a host key they can read, keeps the window closed, as sshd's StrictModes refuses keys. An
// authorized_keys folder that StrictModes would refuse is a warning in the offer.
func TestDevicesPairStateTooOpen(t *testing.T) {
	for _, c := range []struct {
		name string
		file string
		mode os.FileMode
	}{
		{"state folder", "", 0o770},
		{"device list", devicestore.FileName, 0o620},
		{"host key", habkey.FileName, 0o640},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDevicesFixture(t)
			state := f.svc.cfg.state
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			path := state
			if c.file != "" {
				path = filepath.Join(state, c.file)
				if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(path, c.mode); err != nil {
				t.Fatal(err)
			}
			_, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{})
			if errKindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), path) {
				t.Fatalf("%s at %04o: %v", c.name, c.mode, err)
			}
		})
	}

	f := newDevicesFixture(t)
	dir := filepath.Dir(f.authKeys)
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	o, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], "chmod go-w "+dir) {
		t.Fatalf("warnings %q", o.Warnings)
	}
}

// TestPickPairAddrs: with no address named, the window takes every LAN and tailnet address;
// on a host whose firmware names a cloud provider only the tailnet ones, and with none it
// refuses and names listen (transport.md 6.1.3).
func TestPickPairAddrs(t *testing.T) {
	ifs := []pairing.IfAddr{
		{Name: "eth0", Prefix: netip.MustParsePrefix("10.0.0.5/24")},
		{Name: "tailscale0", Prefix: netip.MustParsePrefix("100.101.102.103/32")},
		{Name: "tailscale0", Prefix: netip.MustParsePrefix("fd7a:115c:a1e0::1/128")},
		{Name: "eth1", Prefix: netip.MustParsePrefix("fd00::5/64")},
	}
	got, err := pickPairAddrs(ifs, "")
	if err != nil || len(got) != 4 {
		t.Fatalf("LAN host: %v %v", got, err)
	}
	got, err = pickPairAddrs(ifs, "Amazon EC2")
	if err != nil || len(got) != 2 || got[0].String() != "100.101.102.103" || got[1].String() != "fd7a:115c:a1e0::1" {
		t.Fatalf("cloud host: %v %v", got, err)
	}
	if _, err := pickPairAddrs(ifs[:1], "Amazon EC2"); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("cloud host with no tailnet: %v", err)
	}
	if _, err := pickPairAddrs(nil, ""); err == nil {
		t.Fatal("no address: no error")
	}
}

// TestDevicesPairCloudRefuses: devices.pair on a cloud host with no tailnet address and no
// listen opens nothing and says why.
func TestDevicesPairCloudRefuses(t *testing.T) {
	f := newDevicesFixture(t, func(c *deviceConfig) {
		c.listen = nil
		c.auto = func() ([]netip.Addr, error) {
			return pickPairAddrs([]pairing.IfAddr{{Name: "eth0", Prefix: netip.MustParsePrefix("172.31.5.6/20")}}, "Amazon EC2")
		}
	})
	if _, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{}); errKindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "Amazon EC2") {
		t.Fatalf("devices.pair on a cloud host: %v", err)
	}

	// Addresses gff-hab chose itself come with the cloud note as a warning: the API caller has
	// no terminal to print it on.
	g := newDevicesFixture(t, func(c *deviceConfig) {
		c.listen = nil
		c.auto = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil }
	})
	o, err := g.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{})
	if err != nil || len(o.Warnings) != 1 || !strings.Contains(o.Warnings[0], "set listen") {
		t.Fatalf("auto-chosen addresses: %+v %v", o.Warnings, err)
	}
}

// TestLoadPairSettings: the [pairing] section; a missing file or section turns pairing over the
// API off; a public or unspecified listen address and a bad port are refused.
func TestLoadPairSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "hab.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if s, err := loadPairSettings(filepath.Join(dir, "none.toml")); err != nil || s.API {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	if s, err := loadPairSettings("../../docs/examples/hab.toml"); err != nil || s.API || len(s.Listen) != 0 {
		t.Fatalf("the example: %+v %v", s, err)
	}
	if s, err := loadPairSettings(write("[speech]\nthreads = 2\n")); err != nil || s.API {
		t.Fatalf("no section: %+v %v", s, err)
	}
	s, err := loadPairSettings(write("[pairing]\napi = true\nlisten = [\"100.101.102.103\"]\nssh_port = 2222\nsshd_keys = \"/opt/ssh\"\n"))
	if err != nil || !s.API || len(s.Listen) != 1 || s.Listen[0].String() != "100.101.102.103" || s.SSHPort != 2222 || s.SSHDKeys != "/opt/ssh" {
		t.Fatalf("settings: %+v %v", s, err)
	}
	for _, bad := range []string{
		"[pairing]\nlisten = [\"0.0.0.0\"]\n",
		"[pairing]\nlisten = [\"203.0.113.9\"]\n",
		"[pairing]\nlisten = [\"::\"]\n",
		"[pairing]\nssh_port = 70000\n",
	} {
		if _, err := loadPairSettings(write(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}

	// authorized_keys is absolute, or starts at ~; a relative path would land in the job's
	// working folder.
	home, _ := os.UserHomeDir()
	if s, err := loadPairSettings(write("[pairing]\nauthorized_keys = \"~/.ssh/other_keys\"\n")); err != nil || s.AuthorizedKeys != filepath.Join(home, ".ssh", "other_keys") {
		t.Errorf("~ path: %q %v", s.AuthorizedKeys, err)
	}
	if s, err := loadPairSettings(write("[pairing]\nauthorized_keys = \"/etc/ssh/keys/owner\"\n")); err != nil || s.AuthorizedKeys != "/etc/ssh/keys/owner" {
		t.Errorf("absolute path: %q %v", s.AuthorizedKeys, err)
	}
	if _, err := loadPairSettings(write("[pairing]\nauthorized_keys = \".ssh/authorized_keys\"\n")); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Errorf("relative path: %v", err)
	}

	// The file turns pairing on: with api = true it must not be writable by group or others.
	on := write("[pairing]\napi = true\n")
	if err := os.Chmod(on, 0o620); err != nil {
		t.Fatal(err)
	}
	if s, err := loadPairSettings(on); err == nil || s.API || !strings.Contains(err.Error(), "chmod go-w") {
		t.Errorf("a group-writable file with api = true: %+v %v", s, err)
	}
	if err := os.WriteFile(on, []byte("[pairing]\napi = false\n"), 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPairSettings(on); err != nil {
		t.Errorf("a group-writable file with pairing off: %v", err)
	}
	os.Chmod(on, 0o600)
}

// TestDevicesCloseRacesPair: Close and Pair at once never call the WaitGroup's Add while its
// Wait runs; a Pair after Close answers unavailable. Run it with -race.
func TestDevicesCloseRacesPair(t *testing.T) {
	for range 20 {
		f := newDevicesFixture(t)
		done := make(chan error, 1)
		go func() {
			_, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{})
			done <- err
		}()
		f.svc.Close()
		if err := <-done; err != nil && errKindOf(err) != api.ErrorKindUnavailable {
			t.Fatalf("Pair during Close: %v", err)
		}
		if _, err := f.svc.Pair(byConn(context.Background(), 1), api.DevicesPairParams{}); errKindOf(err) != api.ErrorKindUnavailable {
			t.Fatalf("Pair after Close: %v", err)
		}
	}
}

// TestAdmitRollsBack: when the device list cannot take the record, the key line just added is
// taken out again: no key without its record, which no revoke could find.
func TestAdmitRollsBack(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(keys, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "state")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil { // a file where the state folder goes
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sk, _ := ssh.NewPublicKey(pub)
	a := admitter{store: devicestore.Open(blocked), authKeys: keys, command: "/usr/local/bin/gff-hab connect"}
	_, err := a.admit(pairing.Device{Name: "phone", Kind: "phone", Key: sk, Fingerprint: ssh.FingerprintSHA256(sk)})
	if err == nil || !strings.Contains(err.Error(), "taken out") {
		t.Fatalf("admit: %v", err)
	}
	if b, _ := os.ReadFile(keys); string(b) != "# mine\n" {
		t.Fatalf("authorized_keys:\n%s", b)
	}
}
