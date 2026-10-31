package link

import (
	"errors"
	"os"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

func TestSeqScanner(t *testing.T) {
	for _, c := range []struct {
		in     string
		ground bool
	}{
		{"", true},
		{"hello", true},
		{"hello\x1b", false},
		{"hello\x1b[", false},
		{"hello\x1b[2;5", false},
		{"hello\x1b[2;5H", true},
		{"\x1b[?1049h", true},
		{"\x1b[38;2;1;2;3m", true},
		{"\x1b[2\x1b[3", false},     // ESC inside CSI starts a new sequence
		{"\x1b[2\x18", true},        // CAN aborts
		{"\x1b(", false},            // designate: waits for the final byte
		{"\x1b(B", true},            // ...
		{"\x1b7", true},             // DECSC
		{"\x1b]0;title", false},     // OSC
		{"\x1b]0;title\x07", true},  // ended by BEL
		{"\x1b]0;title\x1b", false}, // ESC: ST or a new sequence?
		{"\x1b]0;title\x1b\\", true},
		{"\x1b]0;a\x1b[1m", true}, // the ESC aborted the OSC and a CSI followed
		{"\x1bPq#0\x07", false},   // BEL does not end a DCS
		{"\x1bPq#0\x1b\\", true},
		{"\x1b_apc\x1b\\", true},
		{"a\xe4", false},           // first byte of a 3-byte character
		{"a\xe4\xb8", false},       // ...
		{"a\xe4\xb8\x96", true},    // 世
		{"\xf0\x9f\x98", false},    // first 3 of a 4-byte character
		{"\xf0\x9f\x98\x80", true}, // 😀
		{"\xe4a", true},            // a broken character ends at the next ASCII byte
		{"\xe4\x1b[", false},       // ...or at an ESC, which starts a sequence
		{"\x80\x9c", true},         // stray continuation bytes
		{"\x1b[1;31mred\x1b[0m\r\n", true},
	} {
		var s seqScanner
		s.write([]byte(c.in))
		if s.ground() != c.ground {
			t.Errorf("%q: ground %v, want %v", c.in, s.ground(), c.ground)
		}
		// Byte by byte must agree with the whole write.
		var b seqScanner
		for i := 0; i < len(c.in); i++ {
			b.write([]byte{c.in[i]})
		}
		if b != s {
			t.Errorf("%q: byte by byte %+v, whole %+v", c.in, b, s)
		}
	}
}

// TestCheckpointMidSequence is the review probe: output that ends inside an escape sequence or a
// UTF-8 character gets no checkpoint, timed or forced; the older checkpoint stays; and a
// checkpoint taken once the sequence ends restores to the same screen. It runs with vt.Fake, and
// with libghostty under the ghostty tag.
func TestCheckpointMidSequence(t *testing.T) {
	dir := t.TempDir()
	parser := testParser()
	l := &Link{
		cfg: Config{StateDir: dir, Parser: parser, IdleAfter: -1, CheckpointEvery: -1, BufferBytes: 1 << 20, Logf: t.Logf},
		enc: checkpointEncoding(parser.Encodings()),
	}
	u := uuidN(9)
	p := newPane(u)
	term, err := parser.New(vt.Config{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	p.term = term
	p.markReady()
	defer p.close()

	off := uint64(0)
	feed := func(s string) {
		p.feed(l, off, []byte(s), true)
		off += uint64(len(s))
	}
	ckOffset := func() (uint64, bool) {
		ck, err := readCheckpoint(dir, u)
		if errors.Is(err, os.ErrNotExist) {
			return 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return ck.Offset, true
	}

	feed("hello\x1b[")
	if err := p.checkpoint(l, true); err != nil {
		t.Fatal(err)
	}
	if o, ok := ckOffset(); ok {
		t.Fatalf("checkpoint written mid-CSI at %d", o)
	}
	feed("2;5Hworld")
	if err := p.checkpoint(l, true); err != nil {
		t.Fatal(err)
	}
	good, ok := ckOffset()
	if !ok || good != off {
		t.Fatalf("checkpoint at %d (%v), want %d", good, ok, off)
	}

	// A split UTF-8 character, then an unterminated OSC: the checkpoint stays where it was.
	feed("\r\nwide \xe4\xb8")
	p.checkpoint(l, true)
	if o, _ := ckOffset(); o != good {
		t.Fatalf("checkpoint moved to %d mid-character; want it kept at %d", o, good)
	}
	feed("\x96 \x1b]2;title")
	p.checkpoint(l, true)
	if o, _ := ckOffset(); o != good {
		t.Fatalf("checkpoint moved to %d mid-OSC; want it kept at %d", o, good)
	}
	feed("\x07 end")
	if err := p.checkpoint(l, true); err != nil {
		t.Fatal(err)
	}
	if o, _ := ckOffset(); o != off {
		t.Fatalf("checkpoint at %d, want %d", o, off)
	}

	// The checkpoint restores to the screen the pane holds, and both take more output alike.
	ck, err := readCheckpoint(dir, u)
	if err != nil {
		t.Fatal(err)
	}
	r, err := parser.Restore(ck.Encoding, ck.Snapshot, vt.Config{Cols: ck.Cols, Rows: ck.Rows})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	more := "\x1b[3;1Hthird \xe4\xb8\x96\x1b[1;31mred\x1b[0m"
	feed(more)
	r.Write([]byte(more))
	want, _ := p.term.PlainText()
	got, _ := r.PlainText()
	if got != want {
		t.Fatalf("restored screen differs\n--- pane ---\n%s\n--- restored ---\n%s", want, got)
	}
	wantVT, _ := p.term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	gotVT, _ := r.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if string(gotVT) != string(wantVT) {
		t.Fatalf("restored snapshot-vt differs\n%q\n%q", wantVT, gotVT)
	}
}

// TestCheckpointCorrupt checks that a checkpoint whose snapshot disagrees with its header's
// length or CRC is refused.
func TestCheckpointCorrupt(t *testing.T) {
	dir := t.TempDir()
	u := uuidN(10)
	if err := writeCheckpoint(dir, u, &checkpoint{Version: checkpointVersion, Pane: u.String(), Encoding: vt.EncodingVT,
		Cols: 80, Rows: 24, Snapshot: []byte("screen bytes")}); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheckpoint(dir, u); err != nil {
		t.Fatal(err)
	}
	path := checkpointPath(dir, u)
	b, _ := os.ReadFile(path)
	for name, bad := range map[string][]byte{
		"flipped byte": append(append([]byte(nil), b[:len(b)-1]...), b[len(b)-1]^1),
		"cut short":    b[:len(b)-2],
		"extra byte":   append(append([]byte(nil), b...), 'x'),
	} {
		os.WriteFile(path, bad, 0o600)
		if _, err := readCheckpoint(dir, u); err == nil {
			t.Errorf("%s: checkpoint accepted", name)
		}
	}
}
