package main

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// TestScrollbackFlag: the cap comes from -scrollback-mib, else $GFF_HAB_SCROLLBACK_MIB, else the
// default; a value under 1 MiB is an error.
func TestScrollbackFlag(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "GFF_HAB_SCROLLBACK_MIB" {
				return v
			}
			return ""
		}
	}
	for _, c := range []struct {
		flag int
		env  string
		want int
		bad  bool
	}{
		{0, "", DefaultScrollbackMiB << 20, false},
		{0, "32", 32 << 20, false},
		{4, "32", 4 << 20, false},
		{-1, "", 0, true},
		{0, "0", 0, true},
		{0, "ten", 0, true},
		{0, "1048577", 0, true},
	} {
		got, err := scrollbackBytes(c.flag, env(c.env))
		if c.bad {
			if err == nil {
				t.Errorf("flag %d env %q: got %d, want an error", c.flag, c.env, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("flag %d env %q: got %d, %v; want %d", c.flag, c.env, got, err, c.want)
		}
	}
}

// TestScrollbackRestoreMatches: with gff-hab's default cap, a terminal restored from a
// checkpoint keeps the same scrollback as one that parsed every byte, well past libghostty's
// own default of about 10 kB. Under `make test` the parser is libghostty.
func TestScrollbackRestoreMatches(t *testing.T) {
	sb, err := scrollbackBytes(0, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	p, name := parser()
	t.Logf("parser %s, scrollback cap %d bytes", name, sb)
	cfg := vt.Config{Cols: 80, Rows: 24, ScrollbackBytes: sb}
	encs := p.Encodings()
	enc := encs[len(encs)-1] // the richest encoding, as checkpoints use

	cont, err := p.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cont.Close()
	var first, second bytes.Buffer
	for i := range 1500 {
		fmt.Fprintf(&first, "\x1b[3%dmline %05d %s\x1b[0m\r\n", i%8, i, bytes.Repeat([]byte{'x'}, 60))
	}
	for i := 1500; i < 3000; i++ {
		fmt.Fprintf(&second, "\x1b[3%dmline %05d %s\x1b[0m\r\n", i%8, i, bytes.Repeat([]byte{'x'}, 60))
	}
	cont.Write(first.Bytes())
	ck, err := cont.Snapshot(enc, vt.SnapshotOptions{Scrollback: vt.AllScrollback})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := p.Restore(enc, ck, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	cont.Write(second.Bytes())
	restored.Write(second.Bytes())

	all := vt.SnapshotOptions{Scrollback: vt.AllScrollback}
	want, err := cont.Snapshot(vt.EncodingVT, all)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restored.Snapshot(vt.EncodingVT, all)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored terminal differs from the continuous one: %d bytes vs %d", len(got), len(want))
	}
	// 3000 lines of about 80 bytes is far past 10 kB; the cap keeps them all.
	if !bytes.Contains(want, []byte("line 00000 ")) {
		t.Fatalf("the first line fell out of the scrollback (%d bytes kept)", len(want))
	}
}
