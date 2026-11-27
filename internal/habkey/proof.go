package habkey

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// proofLabel starts every peer proof message, so a signature made for a proof is never valid
// for anything else the hab key signs.
const proofLabel = "flightdeck-peer-proof-v1"

// ProofMessage is the message a dialing hab signs in hab.prove (docs/specs/transport.md 6.3):
// the label, the receiving hab's nonce, the receiving hab's device id and the dialer's own
// device id, each ended by a NUL byte. The receiver's id stops a hab in the middle from relaying
// a proof meant for itself to a third hab.
func ProofMessage(nonce, receiver, dialer string) []byte {
	var b bytes.Buffer
	for _, s := range []string{proofLabel, nonce, receiver, dialer} {
		b.WriteString(s)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// PublicKey returns the public key in authorized_keys form: "ssh-ed25519 <base64 blob>".
func (k *Key) PublicKey() string {
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(wire(k.Private.Public().(ed25519.PublicKey)))
}

// Prove signs the proof message for the receiver's nonce and device id. It returns the public
// key in authorized_keys form and the base64 signature: the params of hab.prove.
func (k *Key) Prove(nonce, receiver string) (key, signature string) {
	sig := ed25519.Sign(k.Private, ProofMessage(nonce, receiver, k.Fingerprint()))
	return k.PublicKey(), base64.StdEncoding.EncodeToString(sig)
}

// VerifyProof checks a hab.prove call. key must be an Ed25519 key whose fingerprint is claimed,
// and signature must sign the proof message for nonce, receiver and claimed.
func VerifyProof(key, signature, nonce, receiver, claimed string) error {
	if nonce == "" || receiver == "" || claimed == "" {
		return errors.New("a proof needs a nonce, the receiver's id and the claimed id")
	}
	pub, err := parsePublic(key)
	if err != nil {
		return err
	}
	if got := Fingerprint(pub); got != claimed {
		return fmt.Errorf("the key is device %s, the hello claimed %s", got, claimed)
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("the signature is not a base64 Ed25519 signature")
	}
	if !ed25519.Verify(pub, ProofMessage(nonce, receiver, claimed), sig) {
		return errors.New("the signature does not verify")
	}
	return nil
}

// parsePublic reads "ssh-ed25519 <base64 blob> [comment]" and checks the blob's own type name.
func parsePublic(s string) (ed25519.PublicKey, error) {
	f := strings.Fields(s)
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return nil, errors.New("the key is not an ssh-ed25519 key in authorized_keys form")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return nil, errors.New("the key blob is not base64")
	}
	if len(blob) != 4+len("ssh-ed25519")+4+ed25519.PublicKeySize {
		return nil, errors.New("the key blob has the wrong length")
	}
	pub := ed25519.PublicKey(blob[len(blob)-ed25519.PublicKeySize:])
	if !bytes.Equal(wire(pub), blob) {
		return nil, errors.New("the key blob is not an ssh-ed25519 blob")
	}
	return pub, nil
}
