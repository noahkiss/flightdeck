package panetext

import (
	"slices"
	"strings"
	"testing"
)

// TestText removes every kind of sequence the spec names and keeps the text (hab-logic-api.md
// 3.11).
func TestText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain rows", "one\r\ntwo\r\n\r\nfour", []string{"one", "two", "", "four"}},
		{"SGR and reset", "\x1b[1;31mred\x1b[0m text  \r\nnext\x1b[0m", []string{"red text", "next"}},
		{"cursor moves dropped", "a\x1b[10;5Hb\x1b[2Kc", []string{"abc"}},
		{"OSC by BEL and ST", "\x1b]0;title\x07x\x1b]8;;https://example.org\x1b\\link\x1b]8;;\x1b\\y", []string{"xlinky"}},
		{"DCS APC PM SOS", "\x1bPq#0;1\x1b\\a\x1b_apc\x1b\\b\x1b^pm\x07c\x1bXsos\x1b\\d", []string{"abcd"}},
		{"charset and keypad", "\x1b(Bx\x1b=y\x1b>z\x1b#8w", []string{"xyzw"}},
		{"C0 dropped, tab kept", "a\x00b\x07c\td\x08e\x7ff", []string{"abc\tdef"}},
		{"C1 dropped", "a\u0085b\u009bc", []string{"abc"}},
		{"wide and combining", "日本語 é 👍🏽", []string{"日本語 é 👍🏽"}},
		{"lone CR ends a line", "50%\r100%\r\ndone", []string{"50%", "100%", "done"}},
		{"bad UTF-8", "a\xffb", []string{"a�b"}},
		{"ESC inside a string starts a new sequence", "\x1b]0;t\x1b[31mx", []string{"x"}},
		{"CAN ends a sequence", "\x1b[12\x18x", []string{"x"}},
	}
	for _, c := range cases {
		if got := Text([]byte(c.in)); !slices.Equal(got, c.want) {
			t.Errorf("%s: Text(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestFeedSplitAnywhere gives the same lines whatever the chunk boundaries: an escape sequence,
// a UTF-8 character or a CR LF split across two writes.
func TestFeedSplitAnywhere(t *testing.T) {
	in := []byte("\x1b[1mhé\x1b]0;t\x1b\\llo\r\nwörld\x1b[0m\r\n日本\r\nlast")
	want := []string{"héllo", "wörld", "日本"}
	for cut := 0; cut <= len(in); cut++ {
		var l Lines
		var got []string
		f := func(line string) bool {
			got = append(got, line)
			return false
		}
		l.Feed(in[:cut], f)
		l.Feed(in[cut:], f)
		// A UTF-8 character cut in two shows as U+FFFD in the open line only, never in a line
		// that ended.
		if !slices.Equal(got, want) || l.Open() != "last" {
			t.Fatalf("cut %d: lines %q open %q, want %q and last", cut, got, l.Open(), want)
		}
	}
}

// TestFeedOpenLine keeps the open line for the caller to read, changes Gen only when it
// changes, and stops as soon as fn says so.
func TestFeedOpenLine(t *testing.T) {
	var l Lines
	var seen []string
	f := func(line string) bool {
		seen = append(seen, line)
		return line == "stop"
	}
	l.Feed([]byte("out\nuser"), f)
	if want := []string{"out"}; !slices.Equal(seen, want) || l.Open() != "user" || string(l.OpenBytes()) != "user" {
		t.Fatalf("seen %q open %q", seen, l.Open())
	}
	l.Feed([]byte("@host $ "), f)
	if l.Open() != "user@host $ " {
		t.Fatalf("open %q", l.Open())
	}
	g := l.Gen()
	l.Feed([]byte("\x1b[0m"), f)
	if l.Gen() != g {
		t.Fatal("an escape sequence changed Gen")
	}
	if !l.Feed([]byte("\nstop\nmore\n"), f) || seen[len(seen)-1] != "stop" {
		t.Fatalf("no stop: %q", seen)
	}
}

// TestCleanControls keeps text, tab and LF, and drops every other control; StripBidi drops
// the bidi controls only.
func TestCleanControls(t *testing.T) {
	in := "a\tb\nc\x1b]52;c;eA==\x07d\re\x7ff\u0085g\u009bh\xffi"
	if got, want := CleanControls(in), "a\tb\nc]52;c;eA==de"+"fgh\uFFFDi"; got != want {
		t.Fatalf("CleanControls = %q, want %q", got, want)
	}
	if got := StripBidi("a\u202eb\u2066c\u2069d\u200fe"); got != "abcd\u200fe" {
		t.Fatalf("StripBidi = %q", got)
	}
}

// TestFeedLongLine hands on a line past MaxLine in parts.
func TestFeedLongLine(t *testing.T) {
	var l Lines
	var parts []int
	l.Feed([]byte(strings.Repeat("x", MaxLine*2+5)+"\n"), func(line string) bool {
		parts = append(parts, len(line))
		return false
	})
	if want := []int{MaxLine, MaxLine, 5}; !slices.Equal(parts, want) {
		t.Fatalf("parts %v, want %v", parts, want)
	}
}
