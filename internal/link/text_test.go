package link

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// textScript prints 30 numbered rows, so some scroll into the scrollback, then a plain row, a
// coloured row, a row with wide characters and a 100-column line that wraps at 80.
const textScript = `i=0; while [ $i -lt 30 ]; do printf 'row %04d\n' $i; i=$((i+1)); done
printf 'plain row\n\033[1;31mred\033[0m text\033]8;;https://example.org\033\\link\033]8;;\033\\\n'
printf '\346\227\245\346\234\254\350\252\236 wide\n'
x=''; i=0; while [ $i -lt 100 ]; do x="${x}x"; i=$((i+1)); done; printf '%s\n' "$x"
exec sleep 1000`

// TestReadText reads a pane's screen and scrollback as plain text (hab-logic-api.md 3.11): no
// escape bytes, a wide character once, a wrapped line as two rows, scrollback rows oldest first
// ending just above the screen, and the offset the text was read at.
func TestReadText(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "tx", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(80)
	spawn(t, l, u, textScript)
	x100 := strings.Repeat("x", 100)
	var tx Text
	waitFor(t, "the last row on the screen", func() bool {
		var err error
		tx, err = l.ReadText(u, 5)
		return err == nil && slices.Contains(tx.Screen, x100[:20])
	})
	if tx.Cols != 80 || tx.Rows != 24 || len(tx.Screen) != 24 {
		t.Fatalf("size %dx%d with %d rows, want 80x24 with 24", tx.Cols, tx.Rows, len(tx.Screen))
	}
	i := slices.Index(tx.Screen, "plain row")
	if i < 0 {
		t.Fatalf("no plain row on the screen:\n%s", strings.Join(tx.Screen, "\n"))
	}
	want := []string{"plain row", "red textlink", "日本語 wide", x100[:80], x100[80:]}
	if got := tx.Screen[i : i+len(want)]; !slices.Equal(got, want) {
		t.Errorf("screen rows %q, want %q", got, want)
	}
	for _, row := range tx.Screen {
		if strings.ContainsAny(row, "\x1b\r\n\a") {
			t.Errorf("control byte in row %q", row)
		}
	}
	if tx.CursorCol != 0 || tx.CursorRow != i+len(want) {
		t.Errorf("cursor at %d,%d, want 0,%d", tx.CursorCol, tx.CursorRow, i+len(want))
	}
	if tx.Alternate {
		t.Error("alternate screen reported on the primary screen")
	}
	// The scrollback: the 5 rows just above the screen's first row, oldest first.
	var first int
	if _, err := fmt.Sscanf(tx.Screen[0], "row %04d", &first); err != nil {
		t.Fatalf("first screen row %q: %v", tx.Screen[0], err)
	}
	if tx.ScrollbackTotal != first {
		t.Errorf("scrollback total %d, want %d", tx.ScrollbackTotal, first)
	}
	var sb []string
	for n := first - 5; n < first; n++ {
		sb = append(sb, fmt.Sprintf("row %04d", n))
	}
	if !slices.Equal(tx.Scrollback, sb) {
		t.Errorf("scrollback %q, want %q", tx.Scrollback, sb)
	}
	if st, _ := paneStatus(l, u); tx.Offset != st.Offset {
		t.Errorf("offset %d, want the pane's %d", tx.Offset, st.Offset)
	}
	// None asked, none read; more than there is gives all of it.
	if tx, _ := l.ReadText(u, 0); len(tx.Scrollback) != 0 || tx.ScrollbackTotal != first {
		t.Errorf("scrollback 0: %d rows of %d", len(tx.Scrollback), tx.ScrollbackTotal)
	}
	if tx, _ := l.ReadText(u, 100000); len(tx.Scrollback) != first || tx.Scrollback[0] != "row 0000" {
		t.Errorf("all scrollback: %d rows, want %d from row 0000", len(tx.Scrollback), first)
	}
}

// TestReadTextWrappedScrollback keeps a wrapped line as two rows in the scrollback too, as the
// screen shows it.
func TestReadTextWrappedScrollback(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "tw", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(82)
	spawn(t, l, u, `x=''; i=0; while [ $i -lt 100 ]; do x="${x}y"; i=$((i+1)); done; printf '%s\n' "$x"; i=0; while [ $i -lt 30 ]; do printf 'row %04d\n' $i; i=$((i+1)); done; exec sleep 1000`)
	var tx Text
	waitFor(t, "the last row", func() bool {
		var err error
		tx, err = l.ReadText(u, 100)
		return err == nil && slices.Contains(tx.Screen, "row 0029")
	})
	y := strings.Repeat("y", 100)
	if len(tx.Scrollback) < 3 || tx.Scrollback[0] != y[:80] || tx.Scrollback[1] != y[80:] || tx.Scrollback[2] != "row 0000" {
		t.Fatalf("scrollback %q, want the wrapped line as two rows, then row 0000", tx.Scrollback)
	}
	if len(tx.Scrollback) != tx.ScrollbackTotal {
		t.Errorf("%d scrollback rows, total %d", len(tx.Scrollback), tx.ScrollbackTotal)
	}
}

// TestReadTextAlternate reads what a full-screen program shows, and says it is the alternate
// screen.
func TestReadTextAlternate(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "ta", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(81)
	spawn(t, l, u, `printf 'shell line\n\033[?1049h\033[H\033[2J\033[5;3Hfull screen'; exec sleep 1000`)
	var tx Text
	waitFor(t, "the full-screen text", func() bool {
		var err error
		tx, err = l.ReadText(u, 10)
		return err == nil && len(tx.Screen) > 4 && tx.Screen[4] == "  full screen"
	})
	if !tx.Alternate {
		t.Error("alternate screen not reported")
	}
	if slices.Contains(tx.Screen, "shell line") {
		t.Error("the primary screen shows through the alternate one")
	}
	if tx.CursorRow != 4 || tx.CursorCol != 13 {
		t.Errorf("cursor at %d,%d, want 13,4", tx.CursorCol, tx.CursorRow)
	}
}
