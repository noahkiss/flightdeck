package link

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// rowsScript prints "row 0000" to "row 0299", one per line, then waits.
const rowsScript = `i=0; while [ $i -lt 300 ]; do printf 'row %04d\n' $i; i=$((i+1)); done; exec sleep 1000`

func hasRows(t *testing.T, b []byte, rows ...int) {
	t.Helper()
	for _, r := range rows {
		if !bytes.Contains(b, fmt.Appendf(nil, "row %04d", r)) {
			t.Errorf("row %04d missing from\n%q", r, b)
		}
	}
}

func lacksRows(t *testing.T, b []byte, rows ...int) {
	t.Helper()
	for _, r := range rows {
		if bytes.Contains(b, fmt.Appendf(nil, "row %04d", r)) {
			t.Errorf("row %04d present, want it left out", r)
		}
	}
}

// TestSubscribeScrollbackBound: a subscription's snapshot carries the screen and SnapshotScrollback
// rows of scrollback, and Scrollback returns the older rows page by page. An 80x24 pane that
// printed rows 0 to 299 shows rows 277 to 299 above the cursor's blank line; its scrollback holds
// rows 0 to 276.
func TestSubscribeScrollbackBound(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "sb", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour, SnapshotScrollback: 50})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(70)
	spawn(t, l, u, rowsScript)
	waitFor(t, "the last row", func() bool { return screenHas(t, l, u, "row 0299") })

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	s, err := l.Subscribe(ctx, u, FromNow)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ch := <-s.C
	if ch.Kind != ChunkSnapshot {
		t.Fatalf("first chunk kind %d, want a snapshot", ch.Kind)
	}
	hasRows(t, ch.Data, 227, 276, 277, 299)
	lacksRows(t, ch.Data, 0, 100, 226)

	sc, err := l.Screen(u)
	if err != nil {
		t.Fatal(err)
	}
	hasRows(t, sc.VT, 227, 299)
	lacksRows(t, sc.VT, 226)

	// The client holds 50 scrollback rows: it asks for the 100 before them, and so on.
	pg, err := l.Scrollback(u, 50, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Rows != 100 || pg.Total != 277 || pg.Offset != sc.Offset {
		t.Fatalf("page: %d rows of %d at offset %d, want 100 of 277 at %d", pg.Rows, pg.Total, pg.Offset, sc.Offset)
	}
	hasRows(t, pg.VT, 127, 226)
	lacksRows(t, pg.VT, 126, 227)
	if i, j := bytes.Index(pg.VT, []byte("row 0127")), bytes.Index(pg.VT, []byte("row 0226")); i > j {
		t.Error("page rows are not oldest first")
	}
	pg, err = l.Scrollback(u, 200, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Rows != 77 {
		t.Fatalf("last page: %d rows, want 77", pg.Rows)
	}
	hasRows(t, pg.VT, 0, 76)
	if pg, _ = l.Scrollback(u, 277, 100); pg.Rows != 0 {
		t.Errorf("past the oldest row: %d rows, want 0", pg.Rows)
	}

	// A page draws rows and nothing else: replayed into an empty terminal it shows them.
	r, err := testParser().Restore(vt.EncodingVT, mustPage(t, l, u, 0, 3).VT, vt.Config{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, _ := r.PlainText(); got != "row 0274\nrow 0275\nrow 0276" {
		t.Errorf("page replayed: %q", got)
	}
}

func mustPage(t *testing.T, l *Link, u hp.UUID, skip, n int) ScrollbackPage {
	t.Helper()
	pg, err := l.Scrollback(u, skip, n)
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

// TestScrollbackShare: each parser gets ScrollbackBytes until the budget's equal share is smaller,
// and never less than MinScrollbackShare.
func TestScrollbackShare(t *testing.T) {
	l := &Link{cfg: Config{ScrollbackBytes: 10 << 20, ScrollbackBudget: 256 << 20}}
	for _, c := range []struct{ panes, want int }{
		{0, 10 << 20}, {1, 10 << 20}, {25, 10 << 20}, {32, 8 << 20}, {64, 4 << 20}, {1000, MinScrollbackShare},
	} {
		if got := l.scrollbackShare(c.panes); got != c.want {
			t.Errorf("%d panes: share %d, want %d", c.panes, got, c.want)
		}
	}
	l.cfg.ScrollbackBudget = 0
	if got := l.scrollbackShare(1000); got != 10<<20 {
		t.Errorf("no budget: share %d, want the per-pane cap", got)
	}
}

// TestScrollbackBudgetRebalance: a second pane halves each parser's cap under a budget of two
// shares, and the cap comes back once the link drops that pane.
func TestScrollbackBudgetRebalance(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "budget", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour,
		ScrollbackBytes: 4 << 20, ScrollbackBudget: 4 << 20})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	if got := l.sbCap.Load(); got != 4<<20 {
		t.Fatalf("no panes: cap %d", got)
	}
	spawn(t, l, uuidN(71), "exec sleep 1000")
	if got := l.sbCap.Load(); got != 4<<20 {
		t.Fatalf("one pane: cap %d, want 4 MiB", got)
	}
	spawn(t, l, uuidN(72), "exec sleep 1000")
	if got := l.sbCap.Load(); got != 2<<20 {
		t.Fatalf("two panes: cap %d, want 2 MiB", got)
	}
	l.run(func() {
		l.mu.Lock()
		delete(l.panes, uuidN(72))
		l.mu.Unlock()
		l.rebalance()
	})
	if got := l.sbCap.Load(); got != 4<<20 {
		t.Fatalf("back to one pane: cap %d, want 4 MiB", got)
	}
}
