package habkey

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vector is a throwaway key; ssh-keygen -lf printed the fingerprint.
const (
	vectorBlob = "AAAAC3NzaC1lZDI1NTE5AAAAIJOC+phHPRbstZI8r/1OhXthhMlhAMU1TiRPPAyXm4+h"
	vectorFP   = "SHA256:2xvoC5tcGEUi7zeRyp4eoa96OYm8wGB1bhw8Q7elVEc"
)

func TestFingerprintMatchesSSH(t *testing.T) {
	blob, err := base64.StdEncoding.DecodeString(vectorBlob)
	if err != nil {
		t.Fatal(err)
	}
	pub := ed25519.PublicKey(blob[len(blob)-ed25519.PublicKeySize:])
	if string(wire(pub)) != string(blob) {
		t.Fatalf("wire form differs from the ssh-keygen blob")
	}
	if got := Fingerprint(pub); got != vectorFP {
		t.Fatalf("fingerprint %s, want %s", got, vectorFP)
	}
}

func TestLoadOrCreateKeepsTheKey(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v, want 0600", fi.Mode().Perm())
	}
	b, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint() != b.Fingerprint() || !strings.HasPrefix(a.Fingerprint(), "SHA256:") {
		t.Fatalf("fingerprints %s and %s", a.Fingerprint(), b.Fingerprint())
	}
}

func TestRefusesAnOpenKeyFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, FileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("a key readable by others was accepted")
	}
}
