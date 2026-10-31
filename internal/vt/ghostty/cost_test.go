//go:build ghostty

package ghostty

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// TestScrollbackCost fills a 120x40 terminal past several scrollback caps and logs what one pane
// costs at each: the parser's resident memory, the checkpoint (snapshot-ghostty: encode, then
// write and fsync to a file), and snapshot-vt with all scrollback and with 1000 lines. The numbers
// in spec 2.3 come from it. Set GFF_MEASURE_DIR to a directory on a real disk to time the fsync
// there; the default is the test's temporary directory, which may be in memory. Run with -v.
func TestScrollbackCost(t *testing.T) {
	const cols, rows = 120, 40
	dir := os.Getenv("GFF_MEASURE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	text := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 4)[:cols-7]
	for _, capMiB := range []int{1, 2, 4, 10} {
		term, err := Parser{}.New(vt.Config{Cols: cols, Rows: rows, ScrollbackBytes: capMiB << 20})
		if err != nil {
			t.Fatal(err)
		}
		// Twice the lines the cap holds (about 1000 per MiB), so the scrollback is full.
		var buf bytes.Buffer
		lines := capMiB * 2000
		for i := range lines {
			fmt.Fprintf(&buf, "\x1b[3%dm%06d\x1b[0m %s\r\n", i%8, i, text)
		}
		term.Write(buf.Bytes())
		sb, err := term.(*terminal).t.ScrollbackRows()
		if err != nil {
			t.Fatal(err)
		}
		resident, _, err := term.(*terminal).MemoryUsage()
		if err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		ck, err := term.Snapshot(vt.EncodingGhostty, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
		if err != nil {
			t.Fatal(err)
		}
		encode := time.Since(start)
		start = time.Now()
		if err := writeSynced(filepath.Join(dir, "measure.ckpt"), ck); err != nil {
			t.Fatal(err)
		}
		write := time.Since(start)

		start = time.Now()
		all, err := term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
		if err != nil {
			t.Fatal(err)
		}
		allTime := time.Since(start)
		start = time.Now()
		bounded, err := term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: 1000})
		if err != nil {
			t.Fatal(err)
		}
		boundTime := time.Since(start)
		start = time.Now()
		page, err := term.Scrollback(0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		pageTime := time.Since(start)
		term.Close()

		t.Logf("cap %2d MiB: %5d scrollback rows, resident %4.1f MiB; checkpoint %4.1f MiB, encode %v, write+fsync %v; "+
			"snapshot-vt all %4.2f MiB in %v, 1000 lines %4.2f MiB in %v; history 1000 rows %4.2f MiB in %v",
			capMiB, sb, mib(resident), mib(uint64(len(ck))), encode.Round(10*time.Microsecond), write.Round(10*time.Microsecond),
			mib(uint64(len(all))), allTime.Round(10*time.Microsecond),
			mib(uint64(len(bounded))), boundTime.Round(10*time.Microsecond),
			mib(uint64(len(page.VT))), pageTime.Round(10*time.Microsecond))
	}
	os.Remove(filepath.Join(dir, "measure.ckpt"))
}

func writeSynced(path string, b []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
