package holderproto_test

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenText formats a frame as hex: the header on the first line, the payload 32 bytes a line.
func goldenText(b []byte) string {
	var sb strings.Builder
	sb.WriteString(hex.EncodeToString(b[:hp.HeaderSize]))
	sb.WriteByte('\n')
	for p := b[hp.HeaderSize:]; len(p) > 0; {
		n := min(32, len(p))
		sb.WriteString(hex.EncodeToString(p[:n]))
		sb.WriteByte('\n')
		p = p[n:]
	}
	return sb.String()
}

func parseGolden(t *testing.T, text string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(text, "\n", ""))
	if err != nil {
		t.Fatalf("bad golden hex: %v", err)
	}
	return b
}

func TestGolden(t *testing.T) {
	versions := []struct {
		v       hp.Version
		reg     *hp.Registry
		samples []sample
	}{
		{1, hp.V1(), v1Samples()},
		{2, v2Registry(), v2Samples()},
	}
	for _, tc := range versions {
		// Every kind the version carries must have a sample, so every kind has a golden.
		have := map[hp.Kind]bool{}
		for _, s := range tc.samples {
			have[s.f.Header().Kind] = true
		}
		for _, k := range tc.reg.Kinds() {
			if since, _ := tc.reg.Since(k); since <= tc.v && !have[k] {
				t.Errorf("v%d: kind %s has no golden sample", tc.v, k)
			}
		}
		for _, s := range tc.samples {
			t.Run(fmt.Sprintf("v%d/%s", tc.v, s.name), func(t *testing.T) {
				enc, err := hp.AppendFrame(nil, s.f, tc.v)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join("testdata", fmt.Sprintf("v%d", tc.v), s.name+".hex")
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(goldenText(enc)), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				text, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run go test -update to create it)", err)
				}
				golden := parseGolden(t, string(text))
				if !bytes.Equal(enc, golden) {
					t.Errorf("encoding changed\n got: %s\nwant: %s", goldenText(enc), text)
				}
				f, n, err := tc.reg.DecodeFrame(tc.v, golden)
				if err != nil {
					t.Fatal(err)
				}
				if n != len(golden) {
					t.Errorf("decode used %d of %d bytes", n, len(golden))
				}
				if !reflect.DeepEqual(f, s.f) {
					t.Errorf("decoded golden differs\n got: %#v\nwant: %#v", f, s.f)
				}
			})
		}
	}
}

// FuzzDecodeFrame checks that no input makes the decoder panic, and that a decoded frame
// re-encodes to a frame that decodes the same way.
func FuzzDecodeFrame(f *testing.F) {
	for _, s := range v1Samples() {
		b, _ := hp.AppendFrame(nil, s.f, 1)
		f.Add(b)
	}
	reg := hp.V1()
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, _, err := reg.DecodeFrame(1, b)
		if err != nil || fr == nil {
			return
		}
		again, err := hp.AppendFrame(nil, fr, 1)
		if err != nil {
			t.Fatal(err)
		}
		fr2, _, err := reg.DecodeFrame(1, again)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fr, fr2) {
			t.Fatalf("re-encode changed the frame: %#v vs %#v", fr, fr2)
		}
	})
}

// FuzzReadFrame feeds a byte stream, after a handshake at version 2, to Conn.ReadFrame with the
// test version 2 registry. Any input must end in a frame sequence and an error, never a panic.
func FuzzReadFrame(f *testing.F) {
	var seed []byte
	for _, s := range v2Samples() {
		seed, _ = hp.AppendFrame(seed, s.f, 2)
	}
	f.Add(seed)
	f.Add([]byte{0x7e, 0xee, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 1, 'x'})
	welcome, _ := hp.AppendFrame(nil, &hp.Welcome{Version: 2}, 2)
	f.Fuzz(func(t *testing.T, b []byte) {
		c := hp.NewConn(rawConn{bytes.NewReader(append(append([]byte{}, welcome...), b...))}, v2Registry())
		c.OnSkip = func(hp.Header) {}
		if _, err := c.Hello("fuzz"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 1000; i++ {
			if _, err := c.ReadFrame(); err != nil {
				return
			}
		}
	})
}
