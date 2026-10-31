package secretfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func fixed(b string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(b), nil }
}

// TestLoadOrCreate: a new file is made once with mode 0600 and read back after.
func TestLoadOrCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	b, created, err := LoadOrCreate(dir, "s", 64, fixed("one"))
	if err != nil || !created || string(b) != "one" {
		t.Fatalf("first: %q, %v, %v", b, created, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "s"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file %v, %v", fi.Mode(), err)
	}
	b, created, err = LoadOrCreate(dir, "s", 64, fixed("two"))
	if err != nil || created || string(b) != "one" {
		t.Fatalf("second: %q, %v, %v", b, created, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "s.tmp-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// TestRefused: a symlink, a directory, a file open to others and a file over the bound are
// refused, and never replaced.
func TestRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s")

	os.WriteFile(path, []byte("secret"), 0o644)
	if _, _, err := LoadOrCreate(dir, "s", 64, fixed("new")); err == nil {
		t.Error("a file others can read was used")
	}
	os.Chmod(path, 0o600)
	if _, err := Read(path, 3); err == nil {
		t.Error("a file over the bound was used")
	}
	if b, _ := os.ReadFile(path); string(b) != "secret" {
		t.Error("a refused file was replaced")
	}

	real := filepath.Join(dir, "real")
	os.WriteFile(real, []byte("secret"), 0o600)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)
	if _, err := Read(link, 64); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("a symlink: %v", err)
	}
	dangling := filepath.Join(dir, "dangling")
	os.Symlink(filepath.Join(dir, "nowhere"), dangling)
	if _, _, err := LoadOrCreate(dir, "dangling", 64, fixed("new")); err == nil {
		t.Error("a dangling symlink was followed or replaced")
	}

	os.Mkdir(filepath.Join(dir, "d"), 0o700)
	if _, err := Read(filepath.Join(dir, "d"), 64); err == nil {
		t.Error("a directory was used")
	}
}

// TestRace: creators starting at once agree on one secret.
func TestRace(t *testing.T) {
	dir := t.TempDir()
	got := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _, err := LoadOrCreate(dir, "s", 64, fixed(string(rune('a'+i))))
			if err != nil {
				t.Error(err)
			}
			got[i] = b
		}()
	}
	wg.Wait()
	for _, b := range got[1:] {
		if !bytes.Equal(b, got[0]) {
			t.Fatalf("racing creators got %q and %q", got[0], b)
		}
	}
}
