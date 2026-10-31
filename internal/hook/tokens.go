package hook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// LoadOrCreateKey reads the hook secret from dir, or makes one if none exists. The file is
// created with mode 0600 and appears complete or not at all. It refuses a key file that is not a
// regular file, that another user owns, that group or others may access, or that has the wrong
// size.
func LoadOrCreateKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, KeyFile)
	k, err := loadKey(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return k, err
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// Write a complete temp file, then hard-link it into place. The link fails if the key
	// exists, so a racing process never sees a half-written key, and the first one wins.
	f, err := os.CreateTemp(dir, KeyFile+".tmp-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(key)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Link(tmp, path); errors.Is(err, os.ErrExist) {
		return loadKey(path) // another process made it first
	} else if err != nil {
		return nil, err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return key, nil
}

// loadKey opens the key without following a symlink and checks the open file itself, so the
// checks and the read see the same file.
func loadKey(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("hook: %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("hook: %s is open to group or others (mode %v); want 0600", path, fi.Mode().Perm())
	}
	if err := checkOwner(fi); err != nil {
		return nil, fmt.Errorf("hook: %s: %w", path, err)
	}
	if fi.Size() != keySize {
		return nil, fmt.Errorf("hook: %s holds %d bytes, want %d", path, fi.Size(), keySize)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("hook: %s: %w", path, err)
	}
	return key, nil
}
