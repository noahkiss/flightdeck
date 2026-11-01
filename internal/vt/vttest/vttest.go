// Package vttest holds the shared tests that every vt.Parser must pass, and the fixture they use.
package vttest

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
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
// Queries is one of every terminal query form a program may send. No snapshot may contain one.
var Queries = []string{
	"\x1b[c", "\x1b[0c", "\x1b[>c", "\x1b[>0c", "\x1b[=c", // device attributes
	"\x1b[5n", "\x1b[6n", "\x1b[?6n", // device status, cursor position
	"\x1b[?2004$p", "\x1b[4$p", // mode queries
	"\x1b[>q", "\x1b[>0q", // terminal version
	"\x1b]10;?\x07", "\x1b]11;?\x1b\\", "\x1b]12;?\x07", "\x1b]4;1;?\x07", // colour queries
	"\x1b[?u",                             // keyboard protocol query
	"\x1bP+q544e\x1b\\", "\x1bP$qm\x1b\\", // termcap and setting queries
	"\x1b[14t", "\x1b[16t", "\x1b[18t", // window size reports
	"\x1b[x", // terminal parameters
}

var all = vt.SnapshotOptions{Scrollback: vt.AllScrollback}

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

	t.Run("cursor and modes", func(t *testing.T) {
		// gff-hab answers terminal queries from these (spec 5.6).
		term := newTerm(t)
		term.Write([]byte("ab\r\ncde"))
		if c, r := term.Cursor(); c != 3 || r != 1 {
			t.Errorf("cursor = %d,%d, want 3,1", c, r)
		}
		if set, known := term.Mode(1049, false); !known || set {
			t.Errorf("mode 1049 before = %v,%v, want reset and known", set, known)
		}
		term.Write([]byte("\x1b[?1049h"))
		if set, known := term.Mode(1049, false); !known || !set {
			t.Errorf("mode 1049 after = %v,%v, want set and known", set, known)
		}
	})

	t.Run("read screen", func(t *testing.T) {
		// gff's composed view draws panes from these cells (gff-client.md 3.2).
		term := newTerm(t)
		term.Write([]byte("ab\r\n界x\x1b[?1049h\x1b[H\x1b[2Jalt"))
		var s vt.Screen
		if err := term.ReadScreen(&s); err != nil {
			t.Fatal(err)
		}
		if s.Cols != FixtureCols || s.Rows != FixtureRows || len(s.Cells) != s.Cols*s.Rows {
			t.Fatalf("screen %dx%d with %d cells", s.Cols, s.Rows, len(s.Cells))
		}
		if got := s.Cells[0].Text + s.Cells[1].Text + s.Cells[2].Text; got != "alt" {
			t.Errorf("alternate screen row 0 = %q, want alt", got)
		}
		if s.CursorX != 3 || s.CursorY != 0 || !s.CursorVisible {
			t.Errorf("cursor = %d,%d visible %v, want 3,0 visible", s.CursorX, s.CursorY, s.CursorVisible)
		}
		term.Write([]byte("\x1b[?1049l"))
		if err := term.ReadScreen(&s); err != nil {
			t.Fatal(err)
		}
		row1 := s.Cells[s.Cols : 2*s.Cols]
		if row1[0].Text != "界" || !row1[0].Wide || !row1[1].Spacer || row1[2].Text != "x" || row1[3].Text != "" {
			t.Errorf("row 1 = %+v, want a wide 界, its spacer, x, blank", row1[:4])
		}
	})

	for _, enc := range p.Encodings() {
		t.Run("restore "+string(enc), func(t *testing.T) {
			term := newTerm(t)
			term.Write(fixture)
			want := dump(t, term)
			snap, err := term.Snapshot(enc, all)
			if err != nil {
				t.Fatal(err)
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
			snap, err := term.Snapshot(enc, all)
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
			// Leaving the alternate screen must show the primary screen from before.
			restored.Write([]byte("\x1b[?1049l"))
			if got := dump(t, restored); got != "primary" {
				t.Errorf("primary after leaving alt = %q, want %q", got, "primary")
			}
		})
	}

	for _, enc := range p.Encodings() {
		t.Run("snapshot holds no query, palette or directory "+string(enc), func(t *testing.T) {
			term := newTerm(t)
			term.Write([]byte("before\r\n"))
			for _, q := range Queries {
				term.Write([]byte(q))
			}
			// A palette change and a working directory report from the program.
			term.Write([]byte("\x1b]4;1;rgb:ff/00/00\x07\x1b]7;file://host/tmp\x07after"))
			snap, err := term.Snapshot(enc, all)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range Queries {
				if bytes.Contains(snap, []byte(q)) {
					t.Errorf("snapshot contains the query %q (spec 5.5)", q)
				}
			}
			if enc == vt.EncodingVT {
				for _, bad := range []string{"\x1b]4;", "\x1b]7;", "\x1b]104"} {
					if bytes.Contains(snap, []byte(bad)) {
						t.Errorf("snapshot-vt contains %q: it would change the client's own terminal", bad)
					}
				}
			}
		})
	}

	t.Run("scrollback bound", func(t *testing.T) {
		term := newTerm(t)
		for i := range FixtureRows + 20 {
			fmt.Fprintf(term, "line %d\r\n", i)
		}
		full := dump(t, term)
		for _, n := range []int{0, 5} {
			snap, err := term.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: n})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := p.Restore(vt.EncodingVT, snap, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := dump(t, restored)
			restored.Close()
			lines := strings.Split(full, "\n")
			// The bounded dump is the last n scrollback lines plus the screen.
			want := strings.Join(lines[len(lines)-(FixtureRows-1)-n:], "\n")
			if got != want {
				t.Errorf("scrollback %d: dump\n%s\nwant\n%s", n, got, want)
			}
		}
	})

	t.Run("scrollback pages", func(t *testing.T) {
		term := newTerm(t)
		for i := range FixtureRows + 20 {
			fmt.Fprintf(term, "\x1b[3%dmline %d\x1b[0m\r\n", i%8, i)
		}
		lines := strings.Split(dump(t, term), "\n")
		check := func(t *testing.T, term vt.Terminal, skip, n int) {
			t.Helper()
			page, err := term.Scrollback(skip, n)
			if err != nil {
				t.Fatal(err)
			}
			end := max(page.Total-skip, 0)
			start := max(end-n, 0)
			if page.Total < 20 || page.Rows != end-start {
				t.Fatalf("skip %d n %d: %d rows of %d, want %d", skip, n, page.Rows, page.Total, end-start)
			}
			if page.Rows == 0 {
				return
			}
			for _, bad := range []string{"\x1b[?", "\x1b[H", "\x1b]"} {
				if bytes.Contains(page.VT, []byte(bad)) {
					t.Errorf("skip %d n %d: page holds %q; it must hold rows only", skip, n, bad)
				}
			}
			r, err := p.Restore(vt.EncodingVT, page.VT, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if got, want := dump(t, r), strings.Join(lines[start:end], "\n"); got != want {
				t.Errorf("skip %d n %d: page\n%s\nwant\n%s", skip, n, got, want)
			}
		}
		for _, c := range [][2]int{{0, 5}, {5, 5}, {0, 0}, {18, 5}, {100, 5}} {
			check(t, term, c[0], c[1])
		}
		// On the alternate screen the pages come from the primary screen's scrollback.
		term.Write([]byte("\x1b[?1049hfull screen app"))
		check(t, term, 3, 4)
	})

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
		if _, err := term.Snapshot("snapshot-nope", all); err == nil {
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
