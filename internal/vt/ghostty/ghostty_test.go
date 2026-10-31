//go:build ghostty

package ghostty

import (
	"bytes"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/vttest"
)

func TestParser(t *testing.T) {
	vttest.Run(t, Parser{}, "fixture.golden")
}

// A ghostty snapshot keeps colours and styles: the VT rendering of the restored terminal matches
// the original byte for byte.
func TestGhosttySnapshotKeepsStyles(t *testing.T) {
	cfg := vt.Config{Cols: vttest.FixtureCols, Rows: vttest.FixtureRows}
	p := Parser{}
	term, err := p.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.Write(vttest.Fixture(t))
	want, err := term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(want, []byte("38;2;10;20;30")) {
		t.Fatalf("VT snapshot lost the truecolour run: %q", want)
	}
	snap, err := term.Snapshot(vt.EncodingGhostty, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := p.Restore(vt.EncodingGhostty, snap, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("VT rendering differs after ghostty restore\n got %q\nwant %q", got, want)
	}
}
