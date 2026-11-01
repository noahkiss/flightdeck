package progstatus

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Hand-written byte fixtures in the shape Claude Code 2.1 sends (OSC 7501 with app=claude),
// mixed with ordinary output.
const fixture = "\x1b[2J\x1b[Hwelcome\r\n" +
	"\x1b]7501;?\x1b\\\x1b[c" + // the query and its DA1 fence: not reports
	"\x1b]7501;state=idle:app=claude\x1b\\" +
	"\x1b]0;a title\x07" +
	"\x1b]7501;state=working:app=claude:title=Rml4IHRoZSBidWc=\x1b\\" +
	"thinking\r\n" +
	"\x1b]7501;state=blocked:kind=permission:app=claude:msg=QWxsb3cgQmFzaD8\x07" +
	"\x1b]7501;state=working:app=claude:progress=40\x1b\\" +
	"\x1b]7501;state=done:app=claude\x1b\\"

func scanAll(p []byte, chunk int) []Report {
	var s Scanner
	var out []Report
	for len(p) > 0 {
		n := min(chunk, len(p))
		s.Feed(p[:n], func(r Report) { out = append(out, r) })
		p = p[n:]
	}
	return out
}

func TestFixture(t *testing.T) {
	want := []Report{
		{State: "idle", App: "claude", Progress: -1},
		{State: "working", App: "claude", Progress: -1, Title: "Fix the bug"},
		{State: "blocked", Kind: "permission", App: "claude", Progress: -1, Msg: "Allow Bash?"},
		{State: "working", App: "claude", Progress: 40},
		{State: "done", App: "claude", Progress: -1},
	}
	// Every chunk size gives the same reports: a report may be cut anywhere.
	for chunk := 1; chunk <= len(fixture); chunk++ {
		got := scanAll([]byte(fixture), chunk)
		if len(got) != len(want) {
			t.Fatalf("chunk %d: %d reports %+v, want %d", chunk, len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("chunk %d: report %d = %+v, want %+v", chunk, i, got[i], want[i])
			}
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		body string
		ok   bool
		want Report
	}{
		{"state=working", true, Report{State: "working", Progress: -1}},
		{" state = idle : app = claude ", true, Report{State: "idle", App: "claude", Progress: -1}},
		{"state=idle:state=done", true, Report{State: "done", Progress: -1}},          // last value wins
		{"bogus:state=error:x=1:Upper=2", true, Report{State: "error", Progress: -1}}, // malformed and unknown pairs skipped
		{"state=blocked:kind=nope", true, Report{State: "blocked", Progress: -1}},     // unknown kind is absent
		{"state=idle:kind=permission", true, Report{State: "idle", Progress: -1}},     // kind only with blocked
		{"state=working:progress=101", true, Report{State: "working", Progress: -1}},
		{"state=done:progress=50", true, Report{State: "done", Progress: -1}},
		{"state=working:app=" + strings.Repeat("a", 33), true, Report{State: "working", Progress: -1}},
		{"state=clear:id=a/b", true, Report{State: "clear", ID: "a/b", Progress: -1}},
		{"state=idle:msg=SGk", true, Report{State: "idle", Msg: "Hi", Progress: -1}}, // padding optional
		{"?", false, Report{}},
		{"?:states=idle,working", false, Report{}},
		{"app=claude", false, Report{}},
		{"state=sleeping", false, Report{}},
		{"state=idle:msg=!!!", true, Report{State: "idle", Progress: -1}}, // ! is outside the value set: pair skipped
		{"state=idle:msg=@@@@", true, Report{State: "idle", Progress: -1}},
		{"state=idle:msg=Zm9v-", false, Report{}},                        // bad base64 discards the whole report
		{"state=idle:msg=YQph", false, Report{}},                         // "a\na": a control character
		{"state=idle:msg=Yf9i", false, Report{}},                         // "a\xffb": invalid UTF-8
		{"state=idle:id=a//b", false, Report{}},                          // empty segment
		{"state=idle:id=a b", true, Report{State: "idle", Progress: -1}}, // space is outside the value set: pair skipped
		{"state=idle:title=" + b64(strings.Repeat("x", 193)), false, Report{}},
	}
	for _, c := range cases {
		got, ok := Parse([]byte(c.body))
		if ok != c.ok || got != c.want {
			t.Errorf("%q: got %+v %v, want %+v %v", c.body, got, ok, c.want, c.ok)
		}
	}
}

func TestStreamEdges(t *testing.T) {
	for _, c := range []struct {
		in string
		n  int
	}{
		{"\x1b]7501;state=idle\x1b\x1b]7501;state=done\x07", 1},                                       // ESC ends the first; the second counts
		{"\x1b]7501;state=idle\x18\x1b]7501;state=done\x07", 1},                                       // CAN aborts
		{"\x1b]75012;state=idle\x07", 0},                                                              // another OSC number
		{"\x1b]2;x\x1b]7501;state=idle\x07", 1},                                                       // a title cut short by ESC, then a report
		{"\x1b]7501;state=idle:msg=" + strings.Repeat("A", 4100) + "\x07\x1b]7501;state=done\x07", 1}, // too long
		{"\x1b\x1b]7501;state=idle\x07", 1},
		{"\x1b[7501;state=idle\x07", 0},
	} {
		if got := scanAll([]byte(c.in), 3); len(got) != c.n {
			t.Errorf("%q: %d reports %+v, want %d", c.in[:min(40, len(c.in))], len(got), got, c.n)
		}
	}
	var s Scanner
	s.Feed([]byte("\x1b]7501;state=wor"), nil)
	s.Reset()
	var got []Report
	s.Feed([]byte("king\x07\x1b]7501;state=idle\x07"), func(r Report) { got = append(got, r) })
	if len(got) != 1 || got[0].State != "idle" {
		t.Errorf("after Reset: %+v", got)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// FuzzScanner: any stream gives the same reports however it is cut, and never panics.
func FuzzScanner(f *testing.F) {
	f.Add([]byte(fixture), 3)
	f.Add([]byte("\x1b]7501;state=idle:msg=\x1b\x1b]7501;state=done\x07"), 1)
	f.Fuzz(func(t *testing.T, p []byte, chunk int) {
		if chunk < 1 {
			chunk = 1
		}
		whole := scanAll(p, len(p)+1)
		cut := scanAll(p, chunk)
		if len(whole) != len(cut) {
			t.Fatalf("%d reports whole, %d cut", len(whole), len(cut))
		}
		for i := range whole {
			if whole[i] != cut[i] {
				t.Fatalf("report %d: %+v whole, %+v cut", i, whole[i], cut[i])
			}
		}
	})
}
