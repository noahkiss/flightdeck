package hoststats

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestVolume: a real folder gives a reading with free at most total, a folder not made yet is
// read at its nearest parent and makes nothing, and an empty path gives none.
func TestVolume(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no volume reader on", runtime.GOOS)
	}
	d := t.TempDir()
	free, total, ok := Volume(d)
	if !ok || total == 0 || free > total {
		t.Fatalf("Volume(%s) = %d, %d, %v", d, free, total, ok)
	}
	_, total2, ok := Volume(filepath.Join(d, "not", "made", "yet"))
	if !ok || total2 != total {
		t.Fatalf("a folder not made yet: total %d, %v; want %d", total2, ok, total)
	}
	if _, err := os.Stat(filepath.Join(d, "not")); err == nil {
		t.Fatal("Volume made a folder")
	}
	if _, _, ok := Volume(""); ok {
		t.Fatal("an empty path gave a reading")
	}
}
