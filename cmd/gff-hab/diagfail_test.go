package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/logfile"
)

// TestMarkerWriteFails: a run marker that cannot be written costs one try a minute and one log
// line, not a loop: the failure line flushes the log, and a flush moves last_seen (6.1).
func TestMarkerWriteFails(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only folder")
	}
	state := t.TempDir()
	var ref diagRef
	w := logfile.New(logfile.Config{OnFlush: ref.touch})
	lg := log.New(w, "", 0)
	d := startDiag(state, "", control.NewEpoch(), w, lg.Printf)
	ref.Store(d)
	run := filepath.Join(state, runDir)
	if err := os.Chmod(run, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(run, 0o700); d.cleanExit() })
	d.mu.Lock()
	d.lastWrite = time.Now().Add(-2 * markerEvery) // a minute and more since the last write
	d.mu.Unlock()
	lg.Print("a line")
	for range 5 {
		w.Flush()
	}
	b, _ := os.ReadFile(filepath.Join(logsDir(state), habLogFile))
	if n := strings.Count(string(b), "run marker:"); n != 1 {
		t.Fatalf("%d run marker lines, want 1; the log holds %d bytes", n, len(b))
	}
}

// TestSweepKeepsOnListError: when the holder registry cannot be listed, the sweep removes no
// holder's file, however long it has been idle.
func TestSweepKeepsOnListError(t *testing.T) {
	t.Parallel()
	d, _ := testDiag(t)
	notDir := filepath.Join(t.TempDir(), "holders")
	os.WriteFile(notDir, nil, 0o600) // a file where the registry folder should be
	d.holderDir = notDir
	idle := filepath.Join(logsDir(d.state), "gff-hab-holder.aaaa0003.log")
	os.WriteFile(idle, []byte("x"), 0o600)
	old := time.Now().Add(-goneHolderAge - time.Hour)
	os.Chtimes(idle, old, old)
	d.sweep()
	if _, err := os.Stat(idle); err != nil {
		t.Fatalf("the sweep removed a holder's file with no holder list: %v", err)
	}
}
