package hook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/noahkiss/flightdeck/internal/secretfile"
)

// KeyFile is the hook secret's file name inside the state directory.
const KeyFile = "hook.key"

// keySize is the secret's length in bytes.
const keySize = 32

// tokenLen is a token's length in hex digits: the 16-byte pane uuid, then the 32-byte HMAC.
const tokenLen = 2 * (16 + sha256.Size)

// tokenLabel separates hook tokens from any other use of the same secret.
const tokenLabel = "gff-hab hook token v1\x00"

// Tokens makes and checks pane tokens. A token is the pane uuid in hex followed by
// HMAC-SHA256(secret, label || uuid) in hex. It needs no registry: the same secret gives the same
// token after a gff-hab restart. A token is accepted only when its hash is right and Known says
// gff-hab follows the pane.
type Tokens struct {
	key []byte
	// Known reports whether the pane with this uuid is live, and its pane id. Nil knows no pane.
	known func(uuid [16]byte) (pane string, ok bool)
}

// NewTokens returns tokens under secret. known decides which panes are live.
func NewTokens(secret []byte, known func(uuid [16]byte) (pane string, ok bool)) *Tokens {
	return &Tokens{key: append([]byte(nil), secret...), known: known}
}

func (t *Tokens) mac(uuid [16]byte) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(tokenLabel))
	m.Write(uuid[:])
	return m.Sum(nil)
}

// Token returns the token of the pane with this uuid.
func (t *Tokens) Token(uuid [16]byte) string {
	return hex.EncodeToString(uuid[:]) + hex.EncodeToString(t.mac(uuid))
}

// Pane resolves a token to its pane id. It refuses a malformed token, a wrong hash (compared in
// constant time), and a pane gff-hab does not follow.
func (t *Tokens) Pane(token string) (string, bool) {
	if len(token) != tokenLen || !validToken(token) {
		return "", false
	}
	raw, err := hex.DecodeString(token)
	if err != nil {
		return "", false
	}
	var uuid [16]byte
	copy(uuid[:], raw[:16])
	if !hmac.Equal(raw[16:], t.mac(uuid)) {
		return "", false
	}
	if t.known == nil {
		return "", false
	}
	return t.known(uuid)
}

// LoadOrCreateKey reads the hook secret from dir, or makes one if none exists, and reports
// whether it made one. The file is created with mode 0600 and appears complete or not at all. It
// refuses a key file that is a symlink or not a regular file, that another user owns, that group
// or others may access, or that has the wrong size, and never replaces such a file. A new secret
// changes every pane's token, so panes that outlived the old one can no longer post.
func LoadOrCreateKey(dir string) (key []byte, created bool, err error) {
	key, created, err = secretfile.LoadOrCreate(dir, KeyFile, keySize, func() ([]byte, error) {
		k := make([]byte, keySize)
		_, err := rand.Read(k)
		return k, err
	})
	if err != nil {
		return nil, false, fmt.Errorf("hook: %w", err)
	}
	if len(key) != keySize {
		return nil, false, fmt.Errorf("hook: %s holds %d bytes, want %d", filepath.Join(dir, KeyFile), len(key), keySize)
	}
	return key, created, nil
}
