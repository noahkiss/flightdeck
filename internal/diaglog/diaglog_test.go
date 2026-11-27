package diaglog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// TestRepeat: a line with the same message and component is written once per RepeatEvery; the
// next one carries the count left out. Legacy lines are never limited (4.5).
func TestRepeat(t *testing.T) {
	now := time.Unix(1000, 0)
	f, _ := setup(t, Config{Now: func() time.Time { return now }})
	l := For("peers")
	for range 5 {
		l.Warn("reconnect failed")
	}
	For("other").Warn("reconnect failed")
	lg := log.New(Legacy(LevelInfo), "", 0)
	lg.Printf("same text")
	lg.Printf("same text")
	now = now.Add(RepeatEvery)
	l.Warn("reconnect failed")
	got := f.all()
	if len(got) != 5 {
		t.Fatalf("lines %q", got)
	}
	last := decode(t, got[4])
	if last["suppressed"] != 4.0 {
		t.Errorf("the line after the window: %v", last)
	}
	if decode(t, got[2])["text"] != "same text" || decode(t, got[3])["msg"] != MsgLegacy {
		t.Errorf("legacy lines %q", got[2:4])
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
		{Session("s-1"), "session", ClassSafe, false},
		{Pane("p:2"), "pane", ClassSafe, false},
		{Method("session.send"), "method", ClassSafe, false},
		{Method("rm -rf"), "method_text", ClassText, true},
		{Kind("not_found"), "kind", ClassSafe, false},
		{Trace("gK3V9QX2M7TA"), "trace", ClassSafe, false},
		{Trace("gk3v9qx2m7ta"), "trace_text", ClassText, true},
		{Event("PostToolUse"), "event", ClassSafe, false},
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
