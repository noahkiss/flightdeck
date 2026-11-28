package diaglog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/trace"
)

// lines is a File that keeps each Write.
type lines struct {
	mu sync.Mutex
	b  []string
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, string(p))
	return len(p), nil
}

func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.b...)
}

// setup points the sink at a fresh file and stderr for one test. Tests that use it do not run
// in parallel: the sink is the process's.
func setup(t *testing.T, cfg Config) (*lines, *bytes.Buffer) {
	t.Helper()
	f, e := &lines{}, &bytes.Buffer{}
	cfg.File, cfg.Stderr = f, e
	if cfg.Proc == "" {
		cfg.Proc = "hab"
	}
	Setup(cfg)
	t.Cleanup(Reset)
	return f, e
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("line %q: %v", s, err)
	}
	return m
}

// TestLineFields: one line is one JSON object with t (UTC, milliseconds), lvl, proc, epoch,
// comp, msg, the trace of the context and each value under its key (diagnostics.md 4.2).
func TestLineFields(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 891_234_567, time.FixedZone("x", 3600))
	f, e := setup(t, Config{Epoch: "ep1", Now: func() time.Time { return at }, Prefix: "gff-hab: "})
	tr := trace.New(trace.Gff)
	ctx := trace.With(context.Background(), tr)
	For("peers").Ctx(ctx).Warn("peer offline", Device("dev-1"), Count("tries", 3),
		Dur("wait", 1500*time.Millisecond), Err(errors.New("refused")), Session("has space"))
	got := f.all()
	if len(got) != 1 || !strings.HasSuffix(got[0], "\n") {
		t.Fatalf("lines %q", got)
	}
	m := decode(t, got[0])
	want := map[string]any{"t": "2026-03-04T04:06:07.891Z", "lvl": "warn", "proc": "hab", "epoch": "ep1",
		"comp": "peers", "msg": "peer offline", "trace": tr, "device": "dev-1", "tries": 3.0, "wait_ms": 1500.0,
		"err": "refused", "session_text": "has space"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("keys %v, want %v", m, want)
	}
	// A warn line reaches stderr as text, in the standard log package's form.
	if s := e.String(); !strings.HasPrefix(s, "gff-hab: 2026/03/04 04:06:07.891234 warn peers: peer offline device=dev-1") ||
		!strings.HasSuffix(s, ", trace "+tr+"\n") {
		t.Errorf("stderr %q", s)
	}
}

// TestLevels: the floor drops lower lines; the debug switch lets debug lines through; info goes
// to stderr only while CopyAll says so (4.2, 4.5).
func TestLevels(t *testing.T) {
	copyAll := false
	f, e := setup(t, Config{CopyAll: func() bool { return copyAll }})
	l := For("x")
	l.Debug("d1")
	l.Info("i1")
	if n := len(f.all()); n != 1 || e.Len() != 0 {
		t.Fatalf("after debug and info: %d lines, stderr %q", n, e.String())
	}
	on := true
	SetDebug(func() bool { return on })
	l.Debug("d2")
	on = false
	l.Debug("d3")
	SetLevel(LevelWarn)
	l.Info("i2")
	l.Error("e1")
	copyAll = true
	SetLevel(LevelInfo)
	l.Info("i3")
	var msgs []string
	for _, s := range f.all() {
		msgs = append(msgs, decode(t, s)["msg"].(string))
	}
	if strings.Join(msgs, " ") != "i1 d2 e1 i3" {
		t.Errorf("file messages %v", msgs)
	}
	if s := e.String(); strings.Count(s, "\n") != 2 || !strings.Contains(s, "error x: e1") || !strings.Contains(s, "x: i3") {
		t.Errorf("stderr %q", s)
	}
	for _, w := range []string{"debug", "info", "warn", "error"} {
		if l, ok := ParseLevel(w); !ok || l.String() != w {
			t.Errorf("ParseLevel(%q) = %v %v", w, l, ok)
		}
	}
	if _, ok := ParseLevel("loud"); ok {
		t.Error("ParseLevel took loud")
	}
}

// TestRepeat: a line with the same component, message and identity is written once per
// RepeatEvery; when the window ends, the count left out is a line of its own. Legacy lines are
// never limited (4.5).
func TestRepeat(t *testing.T) {
	now := time.Unix(1000, 0)
	f, _ := setup(t, Config{Now: func() time.Time { return now }})
	l := For("peers")
	for i := range 50 {
		l.Warn("reconnect failed", Label("a"), Count("try", int64(i)), Err(errors.New("refused")))
	}
	For("other").Warn("reconnect failed", Label("a"))
	lg := log.New(Legacy(LevelInfo), "", 0)
	lg.Printf("same text")
	lg.Printf("same text")
	if got := f.all(); len(got) != 4 {
		t.Fatalf("lines in the window %q", got)
	}
	now = now.Add(RepeatEvery)
	current.Load().sweep()
	l.Warn("reconnect failed", Label("a"))
	got := f.all()
	if len(got) != 6 {
		t.Fatalf("lines %q", got)
	}
	count := decode(t, got[4])
	if count["suppressed"] != 49.0 || count["msg"] != "reconnect failed" || count["label"] != "a" || count["lvl"] != "warn" ||
		count["try"] != nil || count["err"] != nil {
		t.Errorf("the count line: %v", count)
	}
	if last := decode(t, got[5]); last["suppressed"] != nil {
		t.Errorf("the line after the window carries a count: %v", last)
	}
	if decode(t, got[2])["text"] != "same text" || decode(t, got[3])["msg"] != MsgLegacy {
		t.Errorf("legacy lines %q", got[2:4])
	}
}

// TestRepeatTimer: the first line left out in a window starts a timer for the window's end; the
// timer writes the count when no other line follows (4.5).
func TestRepeatTimer(t *testing.T) {
	var waits []time.Duration
	var fire []func()
	old := afterFunc
	afterFunc = func(d time.Duration, f func()) { waits, fire = append(waits, d), append(fire, f) }
	t.Cleanup(func() { afterFunc = old })
	now := time.Unix(1000, 0)
	f, _ := setup(t, Config{Now: func() time.Time { return now }})
	l := For("speech")
	l.Info("helper stderr", Text("a"))
	now = now.Add(3 * time.Second)
	l.Info("helper stderr", Text("b"))
	l.Info("helper stderr", Text("c"))
	if len(fire) != 1 || waits[0] != RepeatEvery-3*time.Second+time.Millisecond {
		t.Fatalf("timers %v", waits)
	}
	fire[0]() // early: the window is still open
	if n := len(f.all()); n != 1 {
		t.Fatalf("%d lines after an early timer", n)
	}
	now = now.Add(7 * time.Second)
	fire[0]()
	got := f.all()
	if len(got) != 2 {
		t.Fatalf("lines %q", got)
	}
	if c := decode(t, got[1]); c["suppressed"] != 2.0 || c["msg"] != "helper stderr" || c["text"] != nil {
		t.Errorf("count %v", c)
	}
}

// TestRepeatIdentity: lines of one message for different entities are each written, and a count
// is written only for the entity whose lines were left out (4.5, Q10).
func TestRepeatIdentity(t *testing.T) {
	now := time.Unix(1000, 0)
	f, _ := setup(t, Config{Now: func() time.Time { return now }})
	l := For("peers")
	l.Info("peer pinned", Label("a"), Device("dev-1"))
	now = now.Add(time.Second)
	l.Info("peer pinned", Label("b"), Device("dev-2"))
	l.Info("peer pinned", Label("b"), Device("dev-2"))
	For("trust").Info("trust prompt answered", Pane("00000000-0000-4000-8000-000000000001"), Path("/x"), Word("choice", "trust"))
	For("trust").Info("trust prompt answered", Pane("00000000-0000-4000-8000-000000000002"), Path("/y"), Word("choice", "trust"))
	got := f.all()
	if len(got) != 4 {
		t.Fatalf("lines %q", got)
	}
	if decode(t, got[0])["device"] != "dev-1" || decode(t, got[1])["device"] != "dev-2" {
		t.Errorf("pins %q", got[:2])
	}
	// The next line, of another key, ends every window. Only b's left a line out, so only b's
	// writes a count.
	now = now.Add(RepeatEvery)
	For("x").Info("later")
	got = f.all()
	if len(got) != 6 {
		t.Fatalf("lines after the windows %q", got)
	}
	if c := decode(t, got[4]); c["suppressed"] != 1.0 || c["label"] != "b" || c["device"] != "dev-2" {
		t.Errorf("count %v", c)
	}
}

// TestRepeatBound: the limiter keeps at most maxRepeat windows; the oldest ends early and writes
// its count.
func TestRepeatBound(t *testing.T) {
	now := time.Unix(1000, 0)
	f, _ := setup(t, Config{Now: func() time.Time { return now }})
	l := For("x")
	l.Info("each", Session("s-ffffffffffffffffffffffff"))
	l.Info("each", Session("s-ffffffffffffffffffffffff"))
	for i := range maxRepeat {
		l.Info("each", Session(fmt.Sprintf("s-%024x", i)))
	}
	s := current.Load()
	s.mu.Lock()
	n := len(s.repeat)
	s.mu.Unlock()
	if n != maxRepeat {
		t.Errorf("%d windows, want %d", n, maxRepeat)
	}
	got := f.all()
	if len(got) != maxRepeat+2 {
		t.Fatalf("%d lines, want %d", len(got), maxRepeat+2)
	}
	if c := decode(t, got[len(got)-2]); c["suppressed"] != 1.0 || c["session"] != "s-ffffffffffffffffffffffff" {
		t.Errorf("the evicted window's count %v", c)
	}
}

// TestReservedKeys: Word, Count and Dur write a value under a reserved key as <key>_text, so it
// passes as no line key and no fixed key's class (10.2).
func TestReservedKeys(t *testing.T) {
	f, _ := setup(t, Config{})
	For("x").Info("real", Word("msg", "fake"), Count("t", 1), Word("host", "box"), Count("session", 2),
		Word("state", "ok"), Dur("took", time.Second))
	got := f.all()
	if len(got) != 1 {
		t.Fatalf("lines %q", got)
	}
	l, ok := Parse(got[0])
	if !ok || l.Msg != "real" {
		t.Fatalf("Parse %q: %+v %v", got[0], l, ok)
	}
	m := decode(t, got[0])
	want := map[string]any{"msg_text": "fake", "t_text": 1.0, "host_text": "box", "session_text": 2.0, "state": "ok", "took_ms": 1000.0}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	for _, k := range []string{"t", "lvl", "msg", "proc", "epoch", "comp", "trace", "suppressed", "session", "host", "text", "path", "client_id"} {
		if !Reserved(k) {
			t.Errorf("%s is not reserved", k)
		}
	}
	if Reserved("state") || Word("msg", "x").class != ClassText {
		t.Error("Reserved(state), or Word under msg kept as safe")
	}
}

// TestLegacyFloor: a floor above info keeps legacy lines at info, which are not typed at their
// level yet; a legacy debug line keeps the debug rule (4.5).
func TestLegacyFloor(t *testing.T) {
	f, _ := setup(t, Config{Level: LevelWarn})
	For("x").Info("typed info")
	log.New(Legacy(LevelInfo), "", 0).Print("old info")
	log.New(Legacy(LevelDebug), "", 0).Print("old debug")
	got := f.all()
	if len(got) != 1 || decode(t, got[0])["text"] != "old info" {
		t.Errorf("lines %q", got)
	}
}

// TestCut: a line stays within MaxLine; a long value is cut and marked (4.2).
func TestCut(t *testing.T) {
	f, _ := setup(t, Config{})
	long := strings.Repeat("é", MaxLine) // two bytes each
	quotes := strings.Repeat(`"`, MaxLine)
	For("x").Info("long", Text(long), Path(quotes), Count("n", 1))
	got := f.all()
	if len(got) != 1 || len(got[0]) > MaxLine {
		t.Fatalf("line of %d bytes", len(got[0]))
	}
	m := decode(t, got[0])
	if s := m["text"].(string); !strings.HasSuffix(s, Cut) || !strings.HasPrefix(s, "éé") {
		t.Errorf("text %q", s[:20])
	}
	if m["n"] != 1.0 {
		t.Errorf("n = %v", m["n"])
	}
	// The legacy shim cuts the same way.
	log.New(Legacy(LevelInfo), "", 0).Print(long)
	if got := f.all(); len(got[1]) > MaxLine {
		t.Fatalf("legacy line of %d bytes", len(got[1]))
	}
}

// TestSafeForms: a safe constructor keeps a value of the right form, and keeps any other as text
// under key_text (10.2); KeyClass gives each fixed key its class (8.3).
func TestSafeForms(t *testing.T) {
	cases := []struct {
		a    Attr
		key  string
		cl   Class
		text bool
	}{
		{Session("s-0123456789abcdef01234567"), "session", ClassSafe, false},
		{Pane("0a1b2c3d-4e5f-4a6b-8c7d-0e1f2a3b4c5d"), "pane", ClassSafe, false},
		{Method("session.send"), "method", ClassSafe, false},
		{Method("rm -rf"), "method_text", ClassText, true},
		{Kind("not_found"), "kind", ClassSafe, false},
		{Event("session.updated"), "event", ClassSafe, false},
		{Harness("claude"), "harness", ClassSafe, false},
		{ClientKind("app"), "client", ClassSafe, false},
		{Trace("gK3V9QX2M7TA"), "trace", ClassSafe, false},
		{Trace("gk3v9qx2m7ta"), "trace_text", ClassText, true},
		{Event("PostToolUse"), "event", ClassSafe, false},
		{ClientID("0a1b2c3d4e5f6789"), "client_id", ClassSafe, false},
		{ClientID("app"), "client_id_text", ClassText, true},
		{Word("state", "online"), "state", ClassSafe, false},
		{Word("state", "a b"), "state_text", ClassText, true},
		{Host("box"), "host", ClassName, false},
		{Group("g"), "group", ClassName, false},
		{Path("/x"), "path", ClassPath, false},
		{Text("t"), "text", ClassText, false},
	}
	for _, c := range cases {
		if c.a.key != c.key || c.a.class != c.cl {
			t.Errorf("%+v: want key %s class %v", c.a, c.key, c.cl)
		}
		if !c.text && KeyClass(c.key) != c.cl && c.key != "state" {
			t.Errorf("KeyClass(%s) = %v, want %v", c.key, KeyClass(c.key), c.cl)
		}
		if c.text && KeyClass(c.a.key) != ClassText {
			t.Errorf("KeyClass(%s) = %v", c.a.key, KeyClass(c.a.key))
		}
	}
	// The safe forms are the minted id grammars and the API's word sets: a path, user@host or a
	// host name passes none of them (review L3).
	for _, v := range []string{"/srv/work/notes.txt", "jdoe@box.example.org", "printer.local", "box.example.org",
		"p:2", "s-1", "S-0123456789ABCDEF01234567", "0A1B2C3D-4E5F-4A6B-8C7D-0E1F2A3B4C5D", ""} {
		for _, a := range []Attr{Session(v), Pane(v), Method(v), Kind(v), Event(v), Harness(v), ClientKind(v)} {
			if a.class != ClassText || !strings.HasSuffix(a.key, textSuffix) {
				t.Errorf("%q passed as safe under %s", v, a.key)
			}
		}
	}
	if KeyClass("msg") != ClassSafe || KeyClass("anything") != ClassText {
		t.Error("KeyClass of the line's own keys or of an unknown key")
	}
	if a := Err(nil); a.key != "" {
		t.Errorf("Err(nil) = %+v", a)
	}
}

// TestParse reads a written line back, and Text renders it; a text line is no structured line.
func TestParse(t *testing.T) {
	f, _ := setup(t, Config{Epoch: "e"})
	tr := trace.New(trace.Hab)
	For("control").Ctx(trace.With(context.Background(), tr)).Info("request failed", Method("pane.read"),
		Kind("not_found"), Dur("took", 3*time.Millisecond), Text(`say "hi"`))
	log.New(Legacy(LevelDebug), "", 0).Printf("old line %d", 7)
	SetDebug(func() bool { return true })
	log.New(Legacy(LevelDebug), "", 0).Printf("old line %d", 8)
	got := f.all()
	if len(got) != 2 {
		t.Fatalf("lines %q", got)
	}
	l, ok := Parse(got[0])
	if !ok || l.Comp != "control" || l.Trace != tr || l.Lvl != "info" || l.T.IsZero() || l.Epoch != "e" {
		t.Fatalf("Parse: %+v %v", l, ok)
	}
	want := `control: request failed method=pane.read kind=not_found took_ms=3 text="say \"hi\"", trace ` + tr
	if l.Text() != want {
		t.Errorf("Text\n got %s\nwant %s", l.Text(), want)
	}
	l2, ok := Parse(got[1])
	if !ok || l2.Text() != "debug old line 8" || l2.Msg != MsgLegacy {
		t.Errorf("legacy: %+v %q", l2, l2.Text())
	}
	for _, s := range []string{"gff-hab: 2026/01/02 03:04:05.000001 text", `{"msg":"no time"}`, `{"t":"x","msg":"m"}`, "{"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) ok", s)
		}
	}
}

// TestNoSink: before Setup, and on a nil Logger, nothing is written and nothing fails.
func TestNoSink(t *testing.T) {
	Reset()
	For("x").Error("nothing")
	var l *Logger
	l.Info("nothing")
	l.Ctx(context.Background()).Info("nothing")
	log.New(Legacy(LevelInfo), "", 0).Print("nothing")
	if Format(LevelInfo, "x", "y") != nil || Enabled(LevelError) {
		t.Error("a line with no sink")
	}
}
