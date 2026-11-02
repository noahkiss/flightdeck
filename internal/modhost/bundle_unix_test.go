//go:build unix

package modhost

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLoadRefusesFIFO: a FIFO named like a source file is refused without being opened, which
// would block Load until a writer came.
func TestLoadRefusesFIFO(t *testing.T) {
	b := writeBundle(t, "fifo", "", `exports.render = function () {};`)
	if err := syscall.Mkfifo(filepath.Join(b.Dir, "pipe.js"), 0o600); err != nil {
		t.Skip("no FIFO:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := Load(b.Dir); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}
