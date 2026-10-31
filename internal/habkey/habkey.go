// Package habkey keeps the hab's own SSH host key and its device id (docs/specs/transport.md 5).
// The key is an Ed25519 key in the state directory, made on first start, owner-only. The device
// id is the key's SHA-256 fingerprint in the standard SSH form, "SHA256:" plus unpadded base64.
// The key file goes through internal/secretfile, as the hook key does.
package habkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"path/filepath"

	"github.com/noahkiss/flightdeck/internal/secretfile"
)

// FileName is the key file's name inside the state directory.
const FileName = "hab_ed25519_key"

// Key is the hab's host key.
type Key struct {
	Private ed25519.PrivateKey
}

// maxFile bounds the key file's size; a PEM Ed25519 key is about 120 bytes.
const maxFile = 4 << 10

// LoadOrCreate reads the key from dir, or makes and saves one if none exists. It refuses a key
// file that is a symlink or not a regular file, that another user owns, or that group or others
// may access, and never replaces such a file.
func LoadOrCreate(dir string) (*Key, error) {
	b, _, err := secretfile.LoadOrCreate(dir, FileName, maxFile, func() ([]byte, error) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
	})
	if err != nil {
		return nil, fmt.Errorf("habkey: %w", err)
	}
	return parse(filepath.Join(dir, FileName), b)
}

func parse(path string, b []byte) (*Key, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("habkey: %s holds no PKCS#8 private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("habkey: %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("habkey: %s is not an Ed25519 key", path)
	}
	return &Key{Private: priv}, nil
}

// Fingerprint returns the device id: the SHA-256 fingerprint of the public key in SSH wire form.
func (k *Key) Fingerprint() string {
	return Fingerprint(k.Private.Public().(ed25519.PublicKey))
}

// Fingerprint returns the SSH SHA-256 fingerprint of an Ed25519 public key.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(wire(pub))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// wire is the SSH public key blob: string "ssh-ed25519", then string key (RFC 8709).
func wire(pub ed25519.PublicKey) []byte {
	const name = "ssh-ed25519"
	b := make([]byte, 0, 4+len(name)+4+len(pub))
	b = binary.BigEndian.AppendUint32(b, uint32(len(name)))
	b = append(b, name...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(pub)))
	return append(b, pub...)
}
