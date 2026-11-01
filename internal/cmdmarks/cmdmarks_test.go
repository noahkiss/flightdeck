package cmdmarks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// base is the offset every test stream starts at, so offsets are never confused with indexes.
const base = 1000

var at = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// marks feeds p to a new scanner in the given chunk sizes (the rest in one) and returns the marks.
func marks(p []byte, chunks []int) []Mark {
	s := NewScanner(base)
	var out []Mark
	emit := func(m Mark) { out = append(out, m) }
	for _, n := range chunks {
		n = min(n, len(p))
		s.Feed(p[:n], emit)
		p = p[n:]
	}
	s.Feed(p, emit)
	return out
}

func ones(n int) []int {
	c := make([]int, n)
	for i := range c {
		c[i] = 1
	}
	return c
}

func randomSplits(r *rand.Rand, n int) []int {
	var c []int
	for n > 0 {
		k := 1 + r.IntN(min(n, 17))
		c = append(c, k)
		n -= k
	}
	return c
}

func TestScannerForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Mark // Start and End relative to base
	}{
		{"BEL", "x\x1b]133;A\x07y", []Mark{{Kind: 'A', Start: 1, End: 9}}},
		{"ST", "\x1b]133;B\x1b\\", []Mark{{Kind: 'B', Start: 0, End: 9}}},
		{"params", "\x1b]133;D;127;aid=7\x07", []Mark{{Kind: 'D', Start: 0, End: 18, Params: []string{"127", "aid=7"}}}},
		{"empty param", "\x1b]133;C;\x07", []Mark{{Kind: 'C', Start: 0, End: 9, Params: []string{""}}}},
		{"unknown letter", "\x1b]133;E\x07\x1b]133;P;k=i\x07", nil},
		{"letter run on", "\x1b]133;Ab\x07", nil},
		{"no letter", "\x1b]133;\x07\x1b]133\x07", nil},
		{"other OSC", "\x1b]0;133;A\x07\x1b]1330;A\x07\x1b]2;title\x1b\\", nil},
		{"control byte inside", "\x1b]133;D;\x010\x07\x1b]133;D;\x7f\x07", nil},
		{"CAN aborts", "\x1b]133;A\x18\x07", nil},
		{"SUB aborts", "\x1b]133;\x1aA\x07", nil},
		{"ESC aborts, new mark", "\x1b]133;A\x1b]133;B\x07", []Mark{{Kind: 'B', Start: 7, End: 15}}},
		{"ESC aborts, other escape", "\x1b]133;A\x1b[0m\x07", nil},
		{"ESC ESC ]", "\x1b\x1b]133;C\x07", []Mark{{Kind: 'C', Start: 1, End: 9}}},
		{"C0 inside ESC", "\x1b\x07]133;A\x07", []Mark{{Kind: 'A', Start: 0, End: 9}}},
		{"CSI is not OSC", "\x1b[133;A\x07", nil},
		// Inside DCS, SOS, PM or APC only an ESC can start an OSC, and that ESC (not followed
		// by a backslash) aborts the string first, as in a terminal's parser and in the link's
		// seqScanner. So tmux passthrough, with its doubled ESC, carries a mark there too.
		{"DCS body", "\x1bP]133;A\x07\x1b\\", nil},
		{"tmux passthrough", "\x1bPtmux;\x1b\x1b]133;A\x07\x1b\\", []Mark{{Kind: 'A', Start: 8, End: 16}}},
		{"APC aborted", "\x1b_x\x1b]133;B\x07", []Mark{{Kind: 'B', Start: 3, End: 11}}},
		{"OSC 7", "\x1b]7;file://h/a%20b\x07", []Mark{{Kind: '7', Start: 0, End: 19, Params: []string{"file://h/a%20b"}}}},
		{"OSC 7 ST", "\x1b]7;file:///t\x1b\\", []Mark{{Kind: '7', Start: 0, End: 15, Params: []string{"file:///t"}}}},
		{"OSC 7 lookalikes", "\x1b]70;x\x07\x1b]7\x07\x1b]77;x\x07", nil},
	}
	for _, c := range cases {
		for _, split := range []string{"whole", "bytes"} {
			var chunks []int
			if split == "bytes" {
				chunks = ones(len(c.in))
			}
			got := marks([]byte(c.in), chunks)
			var want []Mark
			for _, m := range c.want {
				m.Start += base
				m.End += base
				want = append(want, m)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s (%s): got %v, want %v", c.name, split, got, want)
			}
		}
	}
}

// TestScannerBounded: an OSC 133 longer than MaxPayload is kept, cut at MaxPayload, and neither
// it nor a huge OSC of another kind makes the scanner hold more than MaxPayload bytes. The
// scanner follows both to their ends and finds the mark after them.
func TestScannerBounded(t *testing.T) {
	long := "\x1b]133;C;cmdline_url=" + strings.Repeat("a", 64<<10) + "\x07"
	huge := "\x1b]52;c;" + strings.Repeat("QUJD", 1<<18) + "\x07"
	in := []byte(long + huge + "\x1b]133;A\x07")
	s := NewScanner(base)
	var got []Mark
	for i := 0; i < len(in); i += 4096 {
		s.Feed(in[i:min(i+4096, len(in))], func(m Mark) { got = append(got, m) })
		if cap(s.buf) > 2*MaxPayload {
			t.Fatalf("scanner holds %d bytes", cap(s.buf))
		}
	}
	if len(got) != 2 || got[0].Kind != 'C' || !got[0].Truncated || got[0].End != base+uint64(len(long)) ||
		len(got[0].Params[0]) != MaxPayload-len("133;C;") ||
		got[1].Kind != 'A' || got[1].Truncated || got[1].Start != base+uint64(len(long)+len(huge)) {
		t.Fatalf("got %d marks, want a cut C then an A after the long OSCs", len(got))
	}
	// Exactly MaxPayload bytes is a whole mark.
	edge := "\x1b]133;C;" + strings.Repeat("b", MaxPayload-len("133;C;")) + "\x07"
	if got := marks([]byte(edge), nil); len(got) != 1 || got[0].Truncated {
		t.Fatalf("a payload of exactly MaxPayload bytes: got %v", got)
	}
	// A control byte past the cut still makes the mark malformed.
	bad := "\x1b]133;C;" + strings.Repeat("b", MaxPayload) + "\n\x07"
	if got := marks([]byte(bad), nil); len(got) != 0 {
		t.Fatalf("a control byte past the cut: got %d marks", len(got))
	}
}

// TestLongCommandLine: a command line longer than the payload cap keeps its command, which its
// D then ends. The text is cut at MaxText on a character boundary and says so; a short one
// is whole.
func TestLongCommandLine(t *testing.T) {
	line := strings.Repeat("é", 3<<10) // 6 KiB of text, 18 KiB as cmdline_url
	var s session
	s.mark("A", false)
	s.mark("B", false)
	s.mark("C;cmdline_url="+url.PathEscape(line), false)
	s.b.WriteString("out")
	s.mark("D;4", true)
	s.command("echo short", "short\r\n", 0, false)
	cs, _ := track(s.b.Bytes(), nil).List(nil, 10)
	if len(cs) != 2 {
		t.Fatalf("got %d commands, want 2: %s", len(cs), dump(cs))
	}
	c := cs[0]
	if c.State != Done || deref(c.Exit) != 4 || !c.TextTruncated || len(c.Text) != MaxText ||
		c.Text != line[:MaxText] || !utf8.ValidString(c.Text) {
		t.Fatalf("the long command: state %s exit %v truncated %v, %d bytes of text", c.State, deref(c.Exit),
			c.TextTruncated, len(c.Text))
	}
	if cs[1].Text != "echo short" || cs[1].TextTruncated {
		t.Fatalf("the short command: %q truncated %v", cs[1].Text, cs[1].TextTruncated)
	}
	// A cut that splits a percent escape, with a decoded line under MaxText: still truncated.
	m := Mark{Kind: 'C', Params: []string{"cmdline_url=ab%2"}, Truncated: true}
	if txt, cut := cmdline(m); txt != "ab" || !cut {
		t.Fatalf("a cut escape: %q, %v", txt, cut)
	}
	// An odd number of bytes, so MaxText falls inside a two-byte character.
	odd := "x" + line
	if txt, cut := cmdline(Mark{Params: []string{"cmdline_url=" + url.PathEscape(odd)}}); len(txt) != MaxText-1 || !cut || !utf8.ValidString(txt) {
		t.Fatalf("a cut inside a character: %d bytes, cut %v", len(txt), cut)
	}
}

// session builds a shell-like stream. It returns the bytes and, per command, its output.
type session struct {
	b   bytes.Buffer
	out []string
}

func (s *session) mark(k string, st bool) {
	s.b.WriteString("\x1b]133;" + k)
	if st {
		s.b.WriteString("\x1b\\")
	} else {
		s.b.WriteString("\x07")
	}
}

// command writes one prompt, typed line, output and end. exit < 0 sends D with no status.
func (s *session) command(line, output string, exit int, st bool) {
	s.mark("A", st)
	s.b.WriteString("\x1b[1m$ \x1b[0m")
	s.mark("B", st)
	s.b.WriteString(line + "\r\n")
	s.mark("C;cmdline_url="+strings.ReplaceAll(line, " ", "%20"), st)
	s.b.WriteString(output)
	s.out = append(s.out, output)
	if exit < 0 {
		s.mark("D", st)
	} else {
		s.mark(fmt.Sprintf("D;%d", exit), st)
	}
}

// cwd writes an OSC 7 report of the working directory, as a shell's precmd does.
func (s *session) cwd(u string, st bool) {
	s.b.WriteString("\x1b]7;" + u)
	if st {
		s.b.WriteString("\x1b\\")
	} else {
		s.b.WriteString("\x07")
	}
}

func threeCommands() *session {
	var s session
	s.mark("D", false) // a shell's precmd before the first prompt: no command runs
	s.cwd("file://box/home/u/a%20b", false)
	s.command("echo one", "one\r\n", 0, false)
	s.command("false", "", 1, true)
	s.cwd("file:///tmp", true)
	s.command("printf 'a\\033[31mb'", "a\x1b[31mb", -1, false)
	s.mark("A", false)
	s.b.WriteString("$ ")
	return &s
}

func track(p []byte, chunks []int) *Tracker {
	tr := New(base)
	off := uint64(base)
	for _, n := range chunks {
		n = min(n, len(p))
		tr.Feed(off, p[:n], at)
		off += uint64(n)
		p = p[n:]
	}
	tr.Feed(off, p, at)
	return tr
}

func dump(cs []Command) string {
	b, _ := json.Marshal(cs)
	return string(b)
}

// outputs returns each command's bytes from the stream.
func outputs(t *testing.T, stream []byte, cs []Command) []string {
	t.Helper()
	var out []string
	for _, c := range cs {
		if c.End == nil {
			t.Fatalf("command %d has no end", c.ID)
		}
		out = append(out, string(stream[c.Output-base:*c.End-base]))
	}
	return out
}

func TestThreeCommands(t *testing.T) {
	s := threeCommands()
	stream := s.b.Bytes()
	tr := track(stream, nil)
	cs, more := tr.List(nil, 100)
	if more || len(cs) != 3 {
		t.Fatalf("got %d commands (more %v), want 3: %s", len(cs), more, dump(cs))
	}
	if got := outputs(t, stream, cs); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", s.out) {
		t.Errorf("outputs %q, want %q", got, s.out)
	}
	for i, want := range []struct {
		exit  *int
		state State
		text  string
		cwd   string
	}{{ptr(0), Done, "echo one", "/home/u/a b"}, {ptr(1), Done, "false", "/home/u/a b"}, {nil, Done, "printf 'a\\033[31mb'", "/tmp"}} {
		c := cs[i]
		if fmt.Sprint(deref(c.Exit)) != fmt.Sprint(deref(want.exit)) || c.State != want.state || c.Text != want.text {
			t.Errorf("command %d: exit %v state %s text %q; want %v %s %q", i, deref(c.Exit), c.State, c.Text, deref(want.exit), want.state, want.text)
		}
		if c.Cwd != want.cwd {
			t.Errorf("command %d: cwd %q, want %q", i, c.Cwd, want.cwd)
		}
		if c.Prompt == nil || c.Input == nil || *c.Prompt >= *c.Input || *c.Input >= c.ID || c.ID >= c.Output {
			t.Errorf("command %d: offsets out of order: %s", i, dump(cs[i:i+1]))
		}
		if line := string(stream[*c.Input-base : c.ID-base]); line != want.text+"\r\n" {
			t.Errorf("command %d: typed line %q", i, line)
		}
		if !c.Started.Equal(at) || !c.Ended.Equal(at) {
			t.Errorf("command %d: times %v %v", i, c.Started, c.Ended)
		}
	}
}

func ptr(n int) *int { return &n }

// TestCwdReports: an OSC 7 report that is not an absolute file URL, is cut by the payload cap,
// or is longer than MaxText leaves the last good working directory in place.
func TestCwdReports(t *testing.T) {
	long := "file:///" + strings.Repeat("d", MaxText)
	huge := "file:///" + strings.Repeat("e", MaxPayload)
	for _, c := range []struct {
		name, report, want string
	}{
		{"good", "file:///srv/x", "/srv/x"},
		{"other scheme", "http://h/srv", "/start"},
		{"relative", "file:srv", "/start"},
		{"bad escape", "file:///a%zz", "/start"},
		{"bad UTF-8", "file:///a%ff", "/start"},
		{"too long", long, "/start"},
		{"cut by the cap", huge, "/start"},
	} {
		var s session
		s.cwd("file:///start", false)
		s.cwd(c.report, false)
		s.command("pwd", "x\r\n", 0, false)
		cs, _ := track(s.b.Bytes(), nil).List(nil, 10)
		if len(cs) != 1 || cs[0].Cwd != c.want {
			t.Errorf("%s: got %s, want cwd %q", c.name, dump(cs), c.want)
		}
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestSplits is the acceptance test for split marks: the same stream fed byte by byte, and at
// random splits, gives the same command list as one write.
func TestSplits(t *testing.T) {
	stream := threeCommands().b.Bytes()
	want := dump(must(track(stream, nil).List(nil, 100)))
	if got := dump(must(track(stream, ones(len(stream))).List(nil, 100))); got != want {
		t.Fatalf("byte by byte:\n got %s\nwant %s", got, want)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 200 {
		chunks := randomSplits(r, len(stream))
		if got := dump(must(track(stream, chunks).List(nil, 100))); got != want {
			t.Fatalf("split %d %v:\n got %s\nwant %s", i, chunks, got, want)
		}
	}
}

func must[T any](v T, _ bool) T { return v }

// TestNestedShell: a marked shell inside a marked command. The inner prompt closes the outer
// command as unknown; the inner commands are commands of their own; the outer shell's D after the
// inner shell exits is ignored.
func TestNestedShell(t *testing.T) {
	var s session
	s.mark("A", false)
	s.mark("B", false)
	s.b.WriteString("bash\r\n")
	s.mark("C", false)
	outerOut := len(s.b.Bytes())
	s.b.WriteString("inner banner\r\n")
	innerA := len(s.b.Bytes())
	s.command("ls", "file\r\n", 0, false)
	s.command("exit", "", 0, false) // the inner shell's last command
	s.mark("D;0", false)            // the outer shell: bash ended
	s.command("pwd", "/\r\n", 0, false)
	stream := s.b.Bytes()
	cs, _ := track(stream, ones(len(stream))).List(nil, 100)
	if len(cs) != 4 {
		t.Fatalf("got %d commands, want 4: %s", len(cs), dump(cs))
	}
	outer := cs[0]
	if outer.State != Unknown || outer.Exit != nil || outer.Output != base+uint64(outerOut) || *outer.End != base+uint64(innerA) {
		t.Errorf("outer command: %s", dump(cs[:1]))
	}
	if got := outputs(t, stream, cs[1:]); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", []string{"file\r\n", "", "/\r\n"}) {
		t.Errorf("inner and later outputs %q", got)
	}
	for _, c := range cs[1:] {
		if c.State != Done || deref(c.Exit) != 0 {
			t.Errorf("command %d: %s", c.ID, dump([]Command{c}))
		}
	}
}

// TestFakeAndStrayMarks: stray B and D marks change nothing; a second C while a command runs is
// ignored; a fake A printed by a program closes the running command as unknown, and the shell's
// real D after it is ignored. Offsets stay ordered and commands never overlap.
func TestFakeAndStrayMarks(t *testing.T) {
	var s session
	s.mark("D;5", false) // stray: nothing runs
	s.mark("A", false)
	s.mark("B", false)
	s.b.WriteString("cat marks.txt\r\n")
	s.mark("C", false)
	s.b.WriteString("text ")
	s.mark("C", false) // a fake C while running: ignored
	s.mark("B", false) // a fake B while running: ignored
	s.b.WriteString("more ")
	s.mark("A", false) // a fake A: closes cat as unknown
	s.b.WriteString("tail")
	s.mark("D;0", false) // the shell's real D: nothing runs now
	s.mark("D;0", false)
	s.command("true", "", 0, false)
	stream := s.b.Bytes()
	tr := track(stream, ones(len(stream)))
	cs, _ := tr.List(nil, 100)
	if len(cs) != 2 {
		t.Fatalf("got %d commands, want 2: %s", len(cs), dump(cs))
	}
	if cs[0].State != Unknown || cs[0].Exit != nil {
		t.Errorf("cat: %s", dump(cs[:1]))
	}
	if got := outputs(t, stream, cs[:1])[0]; got != "text \x1b]133;C\x07\x1b]133;B\x07more " {
		t.Errorf("cat output %q", got)
	}
	if cs[1].State != Done || deref(cs[1].Exit) != 0 {
		t.Errorf("true: %s", dump(cs[1:]))
	}
	checkOrdered(t, cs)
}

func checkOrdered(t *testing.T, cs []Command) {
	t.Helper()
	var last uint64
	for _, c := range cs {
		if c.ID < last || c.Output <= c.ID || (c.End != nil && *c.End < c.Output) {
			t.Fatalf("commands overlap or go back: %s", dump(cs))
		}
		if c.End != nil {
			last = *c.End
		}
	}
}

// TestMarkFlood: a program that prints marks in a loop leaves at most MaxCommands commands, the
// newest ones, in order.
func TestMarkFlood(t *testing.T) {
	var b bytes.Buffer
	n := MaxCommands*3 + 7
	for i := range n {
		fmt.Fprintf(&b, "\x1b]133;C\x07%d\x1b]133;D;%d\x07", i, i%256)
	}
	stream := b.Bytes()
	tr := track(stream, randomSplits(rand.New(rand.NewPCG(3, 4)), len(stream)))
	cs, more := tr.List(nil, MaxCommands+10)
	if len(cs) != MaxCommands || more {
		t.Fatalf("kept %d commands (more %v), want %d", len(cs), more, MaxCommands)
	}
	if got := string(stream[cs[len(cs)-1].Output-base : *cs[len(cs)-1].End-base]); got != fmt.Sprint(n-1) {
		t.Fatalf("the newest command's output is %q, want %d", got, n-1)
	}
	if len(tr.changed) > MaxCommands {
		t.Fatalf("%d changes held", len(tr.changed))
	}
	checkOrdered(t, cs)
}

// TestGap: bytes that never arrived close a running command as unknown at the gap's start, and a
// mark cut by the gap is not found.
func TestGap(t *testing.T) {
	tr := New(base)
	first := []byte("\x1b]133;A\x07\x1b]133;C\x07out\x1b]133;")
	tr.Feed(base, first, at)
	gapAt := uint64(base + len(first))
	tr.Feed(gapAt+50, []byte("D;0\x07\x1b]133;A\x07"), at)
	cs, _ := tr.List(nil, 10)
	if len(cs) != 1 || cs[0].State != Unknown || *cs[0].End != gapAt {
		t.Fatalf("after a gap: %s", dump(cs))
	}
	if tr.phase != phPrompt {
		t.Fatalf("phase %q after the A past the gap", tr.phase)
	}
}

// TestTrim: commands whose output left the ring are dropped; a running one is kept.
func TestTrim(t *testing.T) {
	stream := threeCommands().b.Bytes()
	tr := track(stream, nil)
	cs, _ := tr.List(nil, 10)
	tr.Trim(*cs[0].End)
	if got, _ := tr.List(nil, 10); len(got) != 2 || got[0].ID != cs[1].ID {
		t.Fatalf("after trimming the first: %s", dump(got))
	}
	tr.Feed(tr.Offset(), []byte("\x1b]133;C\x07running"), time.Time{})
	tr.Trim(tr.Offset())
	got, _ := tr.List(nil, 10)
	if len(got) != 1 || got[0].State != Running || !got[0].Started.IsZero() {
		t.Fatalf("after trimming all: %s", dump(got))
	}
}

// TestSaveRestore: a tracker saved and restored at any point between bytes, then fed the rest,
// ends with the same list as one fed every byte.
func TestSaveRestore(t *testing.T) {
	stream := threeCommands().b.Bytes()
	want := dump(must(track(stream, nil).List(nil, 100)))
	for cut := 0; cut <= len(stream); cut++ {
		tr := New(base)
		tr.Feed(base, stream[:cut], at)
		if !tr.sc.Idle() {
			continue // a checkpoint is only taken in the ground state
		}
		b, err := json.Marshal(tr.Save())
		if err != nil {
			t.Fatal(err)
		}
		var s Saved
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		r := Restore(base+uint64(cut), s)
		r.Feed(base+uint64(cut), stream[cut:], at)
		if got := dump(must(r.List(nil, 100))); got != want {
			t.Fatalf("cut at %d:\n got %s\nwant %s", cut, got, want)
		}
	}
}

func TestListPages(t *testing.T) {
	tr := track(threeCommands().b.Bytes(), nil)
	all, _ := tr.List(nil, 100)
	last, more := tr.List(nil, 2)
	if len(last) != 2 || !more || last[0].ID != all[1].ID {
		t.Fatalf("newest two: %s more %v", dump(last), more)
	}
	first, more := tr.List(&last[0].ID, 2)
	if len(first) != 1 || more || first[0].ID != all[0].ID {
		t.Fatalf("before %d: %s more %v", last[0].ID, dump(first), more)
	}
	if c, ok := tr.Get(all[2].ID); !ok || c.ID != all[2].ID {
		t.Fatal("Get missed a command")
	}
	if _, ok := tr.Get(all[2].ID + 1); ok {
		t.Fatal("Get found a command that is not there")
	}
}

func TestChanges(t *testing.T) {
	tr := New(base)
	tr.Feed(base, []byte("\x1b]133;A\x07\x1b]133;C\x07"), at)
	ch := tr.Changes()
	if len(ch) != 1 || ch[0].State != Running {
		t.Fatalf("after C: %s", dump(ch))
	}
	if tr.Changed() || tr.Changes() != nil {
		t.Fatal("changes not cleared")
	}
	tr.Feed(tr.Offset(), []byte("x\x1b]133;D;2\x07\x1b]133;A\x07"), at)
	ch = tr.Changes()
	if len(ch) != 1 || ch[0].State != Done || deref(ch[0].Exit) != 2 {
		t.Fatalf("after D: %s", dump(ch))
	}
}

// feedTime returns the shortest of five times to feed stream to a new tracker in 32 KiB reads.
func feedTime(stream []byte) time.Duration {
	best := time.Duration(math.MaxInt64)
	for range 5 {
		tr := New(base)
		start := time.Now()
		for off := 0; off < len(stream); off += 32 << 10 {
			tr.Feed(uint64(base+off), stream[off:min(off+32<<10, len(stream))], at)
		}
		best = min(best, time.Since(start))
	}
	return best
}

// TestMarkFloodCost: a stream of nothing but marks costs a small multiple of plain text to
// follow, at the command cap too: dropping the oldest command must not move the whole list.
func TestMarkFloodCost(t *testing.T) {
	const size = 4 << 20
	flood := bytes.Repeat([]byte("\x1b]133;C\x07\x1b]133;D\x07"), size/16)
	plain := bytes.Repeat([]byte("plain text without any mark\r\n"), size/29)
	tf, tp := feedTime(flood), feedTime(plain)
	// Measured on the dev box: 27x to 41x (14x with -race). Moving the list for each new
	// command at the cap measured about 200x. The bound sits between, clear of both.
	ratio := float64(tf) / float64(tp)
	t.Logf("marks %v, plain text %v: %.1fx", tf, tp, ratio)
	if ratio > 100 {
		t.Fatalf("a flood of marks costs %.0fx plain text, want at most 100x", ratio)
	}
}
