//go:build ghostty

package ghostty

import (
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// Kitty goldens: libghostty encodes a key gff decoded at each kitty flag level a program can
// push (gff-client.md 8.2), from the program's own flags and modes.
func TestEncodeKeyGoldens(t *testing.T) {
	keys := []vt.Key{
		{Code: 'a', Text: "a"},
		{Code: 'a', Mods: 1, Text: "A"},
		{Code: 'c', Mods: 4},
		{Code: 'x', Mods: 2},
		{Code: 27},
		{Code: 13},
		{Code: 13, Mods: 1},
		{Code: 127},
		{Code: 57399},
		{Code: 57352},
		{Code: 'a', Event: 3},
	}
	for _, c := range []struct {
		setup string
		want  []string
	}{
		{"\x1b[>1u", []string{"a", "A", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\r", "\x1b[13;2u", "\x7f", "\x1b[57399u", "\x1b[A", ""}},
		{"\x1b[>3u", []string{"a", "A", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\r", "\x1b[13;2u", "\x7f", "\x1b[57399u", "\x1b[1;1:1A", "\x1b[97;1:3u"}},
		{"\x1b[>5u", []string{"a", "A", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\r", "\x1b[13;2u", "\x7f", "\x1b[57399::48u", "\x1b[A", ""}},
		{"\x1b[>9u", []string{"\x1b[97u", "\x1b[97;2u", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\x1b[13u", "\x1b[13;2u", "\x1b[127u", "\x1b[57399u", "\x1b[A", ""}},
		{"\x1b[>25u", []string{"\x1b[97;;97u", "\x1b[97;2;65u", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\x1b[13u", "\x1b[13;2u", "\x1b[127u", "\x1b[57399u", "\x1b[A", ""}},
		{"\x1b[>31u", []string{"\x1b[97;;97u", "\x1b[97:65;2;65u", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\x1b[13u", "\x1b[13;2u", "\x1b[127u", "\x1b[57399::48u", "\x1b[1;1:1A", "\x1b[97;1:3u"}},
		{"\x1b[?1h\x1b[>1u", []string{"a", "A", "\x1b[99;5u", "\x1b[120;3u", "\x1b[27u", "\r", "\x1b[13;2u", "\x7f", "\x1b[57399u", "\x1b[A", ""}}, // kitty arrows ignore DECCKM
	} {
		term, err := Parser{}.New(vt.Config{Cols: 10, Rows: 2})
		if err != nil {
			t.Fatal(err)
		}
		term.Write([]byte(c.setup))
		enc := term.(vt.KeyEncoder)
		for i, k := range keys {
			if got := string(enc.EncodeKey(k)); got != c.want[i] {
				t.Errorf("%q %+v: %q, want %q", c.setup, k, got, c.want[i])
			}
		}
		term.Close()
	}
}

// A program's kitty flags survive a snapshot-vt, so gff's parser knows them for a program that
// pushed them before gff opened the pane.
func TestKeyboardFlagsInSnapshot(t *testing.T) {
	p := Parser{}
	cfg := vt.Config{Cols: 10, Rows: 2}
	term, err := p.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.Write([]byte("\x1b[>9u"))
	snap, err := term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Restore(vt.EncodingVT, snap, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if f := r.KeyboardFlags(); f != 9 {
		t.Errorf("flags after a snapshot-vt restore: %d, want 9", f)
	}
}
