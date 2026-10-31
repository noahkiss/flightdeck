package link

import (
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// TestGapSendsSnapshot: when output jumps past bytes that never arrived, a subscriber gets a
// snapshot at the new offset before the bytes, so its stream stays contiguous.
func TestGapSendsSnapshot(t *testing.T) {
	parser := testParser()
	l := &Link{cfg: Config{StateDir: t.TempDir(), Parser: parser, BufferBytes: 1 << 20, Logf: t.Logf}}
	p := newPane(uuidN(60), DefaultSnapshotScrollback)
	term, err := parser.New(vt.Config{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	p.term = term
	p.markReady()
	defer p.close()
	p.feed(l, 0, []byte("before"), true)
	p.mu.Lock()
	s, err := p.subscribe(l, p.off, nil)
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	p.feed(l, 100, []byte(" after"), true)
	snap := <-s.C
	if snap.Kind != ChunkSnapshot || snap.Offset != 100 {
		t.Fatalf("first chunk after the gap: kind %d at %d, want a snapshot at 100", snap.Kind, snap.Offset)
	}
	b := <-s.C
	if b.Kind != ChunkBytes || b.Offset != 100 || string(b.Data) != " after" {
		t.Fatalf("second chunk: kind %d at %d %q", b.Kind, b.Offset, b.Data)
	}
	if st := p.status(); !st.Suspect || st.Offset != 106 {
		t.Errorf("after the gap: suspect %v, offset %d", st.Suspect, st.Offset)
	}
}
