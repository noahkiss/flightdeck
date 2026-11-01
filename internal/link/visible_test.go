package link

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// TestVisibleRows: VisibleRows returns the active screen's rows and nothing of the scrollback.
// An 80x24 pane that printed rows 0 to 299 shows rows 277 to 299 and the cursor's blank line.
func TestVisibleRows(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "vr", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(71)
	spawn(t, l, u, rowsScript)
	waitFor(t, "the last row", func() bool { return screenHas(t, l, u, "row 0299") })

	rows, cols, err := l.VisibleRows(u)
	if err != nil {
		t.Fatal(err)
	}
	if cols != 80 || len(rows) != 24 {
		t.Fatalf("%d columns, %d rows; want 80, 24", cols, len(rows))
	}
	for i := 0; i < 23; i++ {
		if want := fmt.Sprintf("row %04d", 277+i); rows[i] != want {
			t.Errorf("row %d = %q, want %q", i, rows[i], want)
		}
	}
	if rows[23] != "" {
		t.Errorf("last row %q, want the cursor's blank line", rows[23])
	}
	if _, _, err := l.VisibleRows(uuidN(72)); !errors.Is(err, ErrNoPane) {
		t.Errorf("unknown pane: %v", err)
	}
}

func TestScreenRows(t *testing.T) {
	var s vt.Screen
	s.Reset(4, 2)
	s.Cells[0] = vt.Cell{Text: "a"}
	s.Cells[2] = vt.Cell{Text: "b"} // a blank cell between a and b
	s.Cells[4] = vt.Cell{Text: "界", Wide: true}
	s.Cells[5] = vt.Cell{Spacer: true}
	s.Cells[6] = vt.Cell{Text: "c"}
	if got, want := ScreenRows(&s), []string{"a b", "界c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
