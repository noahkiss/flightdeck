package habkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{Private: priv}
}

// A proof verifies only for the key's own device id, the nonce it signed and the receiver it
// named (transport.md 6.3).
func TestProof(t *testing.T) {
	a, b := newKey(t), newKey(t)
	key, sig := a.Prove("nonce-1", "SHA256:receiver")
	if err := VerifyProof(key, sig, "nonce-1", "SHA256:receiver", a.Fingerprint()); err != nil {
		t.Fatalf("a good proof: %v", err)
	}
	for name, err := range map[string]error{
		"another device's id":   VerifyProof(key, sig, "nonce-1", "SHA256:receiver", b.Fingerprint()),
		"another nonce":         VerifyProof(key, sig, "nonce-2", "SHA256:receiver", a.Fingerprint()),
		"another receiver":      VerifyProof(key, sig, "nonce-1", "SHA256:other", a.Fingerprint()),
		"another key's sig":     VerifyProof(b.PublicKey(), sig, "nonce-1", "SHA256:receiver", b.Fingerprint()),
		"no nonce":              VerifyProof(key, sig, "", "SHA256:receiver", a.Fingerprint()),
		"a key of another type": VerifyProof("ssh-rsa "+key[len("ssh-ed25519 "):], sig, "nonce-1", "SHA256:receiver", a.Fingerprint()),
		"a short signature":     VerifyProof(key, sig[:20], "nonce-1", "SHA256:receiver", a.Fingerprint()),
	} {
		if err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}
