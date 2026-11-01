package cmdmarks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
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

// TestScannerBounded: an OSC 133 longer than MaxPayload is dropped, and neither it nor a huge
// OSC of another kind makes the scanner hold more than MaxPayload bytes. The scanner follows
// both to their ends and finds the mark after them.
func TestScannerBounded(t *testing.T) {
	long := "\x1b]133;C;cmdline_url=" + strings.Repeat("a", MaxPayload) + "\x07"
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
	if len(got) != 1 || got[0].Kind != 'A' || got[0].Start != base+uint64(len(long)+len(huge)) {
		t.Fatalf("got %v, want one A after the long OSCs", got)
	}
	// Exactly MaxPayload bytes is still a mark.
	edge := "\x1b]133;C;" + strings.Repeat("b", MaxPayload-len("133;C;")) + "\x07"
	if got := marks([]byte(edge), nil); len(got) != 1 {
		t.Fatalf("a payload of exactly MaxPayload bytes: got %d marks", len(got))
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

func threeCommands() *session {
	var s session
	s.mark("D", false) // a shell's precmd before the first prompt: no command runs
	s.command("echo one", "one\r\n", 0, false)
	s.command("false", "", 1, true)
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
	}{{ptr(0), Done, "echo one"}, {ptr(1), Done, "false"}, {nil, Done, "printf 'a\\033[31mb'"}} {
		c := cs[i]
		if fmt.Sprint(deref(c.Exit)) != fmt.Sprint(deref(want.exit)) || c.State != want.state || c.Text != want.text {
			t.Errorf("command %d: exit %v state %s text %q; want %v %s %q", i, deref(c.Exit), c.State, c.Text, deref(want.exit), want.state, want.text)
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
