// Package vttest holds the shared tests that every vt.Parser must pass, and the fixture they use.
package vttest

import (
	"bufio"
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/vt"
)

var update = flag.Bool("update", false, "rewrite golden files")

// FixtureCols and FixtureRows are the terminal size the fixture is written for.
const FixtureCols, FixtureRows = 40, 10

// testdata returns the path of a file in this package's testdata directory.
func testdata(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

// Fixture returns the fixture byte stream from testdata/fixture.vt.txt.
func Fixture(t testing.TB) []byte {
	t.Helper()
	f, err := os.Open(testdata("fixture.vt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := strconv.Unquote(line)
		if err != nil {
			t.Fatalf("fixture.vt.txt:%d: %v", n, err)
		}
		out.WriteString(s)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Golden compares got with testdata/<name>, or rewrites the file when -update is set.
func Golden(t testing.TB, name, got string) {
	t.Helper()
	path := testdata(name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func dump(t testing.TB, term vt.Terminal) string {
	t.Helper()
	s, err := term.PlainText()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Run runs the shared parser tests. golden names the file the fixture's plain-text dump must match.
func Run(t *testing.T, p vt.Parser, golden string) {
	cfg := vt.Config{Cols: FixtureCols, Rows: FixtureRows}
	fixture := Fixture(t)

	newTerm := func(t *testing.T) vt.Terminal {
		t.Helper()
		term, err := p.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { term.Close() })
		return term
	}

	t.Run("golden", func(t *testing.T) {
		term := newTerm(t)
		if _, err := term.Write(fixture); err != nil {
			t.Fatal(err)
		}
		Golden(t, golden, dump(t, term))
	})

	t.Run("split writes", func(t *testing.T) {
		// One byte per Write must parse the same as one Write: UTF-8 and escape sequences can be
		// cut anywhere by the pane reader.
		whole, split := newTerm(t), newTerm(t)
		whole.Write(fixture)
		for i := range fixture {
			split.Write(fixture[i : i+1])
		}
		if a, b := dump(t, whole), dump(t, split); a != b {
			t.Errorf("split writes differ\n--- whole ---\n%s\n--- split ---\n%s", a, b)
		}
	})

	for _, enc := range p.Encodings() {
		t.Run("restore "+string(enc), func(t *testing.T) {
			term := newTerm(t)
			term.Write(fixture)
			want := dump(t, term)
			snap, err := term.Snapshot(enc)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(snap, []byte("\x1b[6n")) || bytes.Contains(snap, []byte("\x1b[c")) {
				t.Error("snapshot contains a terminal query (spec 5.5)")
			}
			restored, err := p.Restore(enc, snap, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if got := dump(t, restored); got != want {
				t.Errorf("restored dump differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			// The restored terminal keeps working.
			restored.Write([]byte("\r\nafter restore"))
			if !strings.Contains(dump(t, restored), "after restore") {
				t.Error("restored terminal ignores new bytes")
			}
		})
	}

	for _, enc := range p.Encodings() {
		t.Run("restore on alternate screen "+string(enc), func(t *testing.T) {
			term := newTerm(t)
			term.Write([]byte("primary\r\n\x1b[?1049h\x1b[H\x1b[2Jon alt"))
			snap, err := term.Snapshot(enc)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := p.Restore(enc, snap, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if got := dump(t, restored); got != "on alt" {
				t.Errorf("restored dump = %q, want %q", got, "on alt")
			}
		})
	}

	t.Run("alternate screen", func(t *testing.T) {
		term := newTerm(t)
		term.Write([]byte("primary\r\n\x1b[?1049h\x1b[H\x1b[2Jon alt"))
		if got := dump(t, term); got != "on alt" {
			t.Errorf("alt screen dump = %q, want %q", got, "on alt")
		}
		term.Write([]byte("\x1b[?1049l"))
		if got := dump(t, term); got != "primary" {
			t.Errorf("primary dump = %q, want %q", got, "primary")
		}
	})

	t.Run("resize", func(t *testing.T) {
		term := newTerm(t)
		term.Write([]byte("hello"))
		if err := term.Resize(60, 20); err != nil {
			t.Fatal(err)
		}
		if c, r := term.Size(); c != 60 || r != 20 {
			t.Errorf("size = %dx%d, want 60x20", c, r)
		}
		if got := dump(t, term); got != "hello" {
			t.Errorf("dump after resize = %q", got)
		}
		if err := term.Resize(0, 5); err == nil {
			t.Error("Resize(0, 5) succeeded")
		}
	})

	t.Run("unsupported encoding", func(t *testing.T) {
		term := newTerm(t)
		if _, err := term.Snapshot("snapshot-nope"); err == nil {
			t.Error("Snapshot of an unknown encoding succeeded")
		}
	})

	t.Run("close", func(t *testing.T) {
		term, err := p.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		term.Close()
		if err := term.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
		if _, err := term.Write([]byte("x")); err == nil {
			t.Error("Write after Close succeeded")
		}
	})
}
