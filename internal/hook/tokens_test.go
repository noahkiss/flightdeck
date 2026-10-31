package hook

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testPanes is a Tokens whose live panes are the names minted so far.
type testPanes struct {
	*Tokens
	mu     sync.Mutex
	byUUID map[[16]byte]string
}

func newTestPanes() *testPanes {
	p := &testPanes{byUUID: map[[16]byte]string{}}
	p.Tokens = NewTokens(bytes.Repeat([]byte{7}, keySize), p.known)
	return p
}

func (p *testPanes) known(u [16]byte) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	name, ok := p.byUUID[u]
	return name, ok
}

func uuidOf(name string) [16]byte {
	sum := sha256.Sum256([]byte(name))
	return [16]byte(sum[:16])
}

// mint makes name a live pane and returns its token.
func (p *testPanes) mint(name string) string {
	u := uuidOf(name)
	p.mu.Lock()
	p.byUUID[u] = name
	p.mu.Unlock()
	return p.Token(u)
}

func (p *testPanes) forget(name string) {
	p.mu.Lock()
	delete(p.byUUID, uuidOf(name))
	p.mu.Unlock()
}

// TestTokens: a token resolves to its live pane, survives a restart with the same secret, and is
// refused when forged, spliced from two panes, made under another secret, or for a pane gff-hab
// does not follow.
func TestTokens(t *testing.T) {
	p := newTestPanes()
	a, b := p.mint("pane-a"), p.mint("pane-b")
	if len(a) != tokenLen || !validToken(a) {
		t.Fatalf("token %q does not fit a GFF_HAB address", a)
	}
	if got, ok := p.Pane(a); !ok || got != "pane-a" {
		t.Fatalf("Pane(a) = %q, %v", got, ok)
	}
	again := NewTokens(bytes.Repeat([]byte{7}, keySize), p.known) // a gff-hab restart
	if again.Token(uuidOf("pane-a")) != a {
		t.Fatal("the same secret gave another token")
	}
	other := NewTokens(bytes.Repeat([]byte{8}, keySize), p.known)
	for name, tok := range map[string]string{
		"forged":        strings.Repeat("ab", tokenLen/2),
		"spliced":       b[:32] + a[32:],
		"other secret":  other.Token(uuidOf("pane-a")),
		"short":         a[:len(a)-2],
		"upper case":    strings.ToUpper(a),
		"unknown pane":  p.Token(uuidOf("pane-c")),
		"empty":         "",
		"one digit off": a[:len(a)-1] + string("0123456789abcdef"[(strings.IndexByte("0123456789abcdef", a[len(a)-1])+1)%16]),
	} {
		if pane, ok := p.Pane(tok); ok {
			t.Errorf("%s token resolved to %q", name, pane)
		}
	}
	p.forget("pane-a")
	if _, ok := p.Pane(a); ok {
		t.Error("the token of a pane that is gone still resolves")
	}
	if _, ok := p.Pane(b); !ok {
		t.Error("pane b's token stopped resolving")
	}
}

// TestKeyFile: the secret is made once with mode 0600 and read back; a file with the wrong mode,
// size or type is refused, not replaced.
func TestKeyFile(t *testing.T) {
	dir := filepath.Join(shortDir(t), "state")
	k1, created, err := LoadOrCreateKey(dir)
	if err != nil || !created {
		t.Fatalf("first load: %v, created %v", err, created)
	}
	path := filepath.Join(dir, KeyFile)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 || len(k1) != keySize {
		t.Fatalf("key file %v, %v, %d bytes", fi.Mode(), err, len(k1))
	}
	k2, created, err := LoadOrCreateKey(dir)
	if err != nil || !bytes.Equal(k1, k2) || created {
		t.Fatalf("second load: %v, same key %v, created %v", err, bytes.Equal(k1, k2), created)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, KeyFile+".tmp-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	os.Chmod(path, 0o644)
	if _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Error("a key file others can read was used")
	}
	os.Chmod(path, 0o600)
	os.WriteFile(path, []byte("short"), 0o600)
	if _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Error("a key file of the wrong size was used")
	}
	if b, _ := os.ReadFile(path); string(b) != "short" {
		t.Error("a bad key file was replaced")
	}

	os.Remove(path)
	real := filepath.Join(dir, "elsewhere")
	os.WriteFile(real, k1, 0o600)
	os.Symlink(real, path)
	if _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Error("a symlinked key file was used")
	}
}

// TestKeyFileRace: processes starting at once agree on one secret.
func TestKeyFileRace(t *testing.T) {
	dir := filepath.Join(shortDir(t), "state")
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, _, err := LoadOrCreateKey(dir)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}()
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) {
			t.Fatal("racing starts made different secrets")
		}
	}
}
