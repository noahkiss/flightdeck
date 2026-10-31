//go:build ghostty

package ghostty

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// TestMemoryLongScrollback fills a 120x40 terminal with 10,000 full, coloured lines and logs the
// memory libghostty reports for it. Run with -v to see the numbers (spec 2.3).
func TestMemoryLongScrollback(t *testing.T) {
	const cols, rows, lines = 120, 40, 10000
	term, err := Parser{}.New(vt.Config{Cols: cols, Rows: rows, ScrollbackBytes: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	var buf bytes.Buffer
	text := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 4)
	for i := range lines {
		fmt.Fprintf(&buf, "\x1b[3%dm%06d\x1b[0m %s\r\n", i%8, i, text[:cols-7])
	}
	term.Write(buf.Bytes())

	dump, err := term.PlainText()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(dump, "\n") + 1; n < lines {
		t.Fatalf("kept %d lines, want at least %d", n, lines)
	}
	resident, virtual, err := term.(*terminal).MemoryUsage()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := term.Snapshot(vt.EncodingGhostty)
	if err != nil {
		t.Fatal(err)
	}
	vtSnap, err := term.Snapshot(vt.EncodingVT)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d lines x %d cols: resident %.1f MiB, virtual %.1f MiB, input %.1f MiB, "+
		"snapshot-ghostty %.1f MiB, snapshot-vt %.1f MiB",
		lines, cols, mib(resident), mib(virtual), mib(uint64(buf.Len())),
		mib(uint64(len(snap))), mib(uint64(len(vtSnap))))
}

func mib(n uint64) float64 { return float64(n) / (1 << 20) }
