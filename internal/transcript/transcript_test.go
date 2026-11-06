package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

var update = flag.Bool("update", false, "write the expected chat files of the fixtures")

// logs collects a reader's warnings.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// readAll pages back from the tail to the start and returns the whole conversation.
func readAll(t *testing.T, r *Reader, limit int) []api.ChatMessage {
	t.Helper()
	var all []api.ChatMessage
	before := ""
	for {
		msgs, more, _, err := r.Page(context.Background(), before, limit)
		if err != nil {
			t.Fatal(err)
		}
		all = append(append([]api.ChatMessage{}, msgs...), all...)
		if !more {
			return all
		}
		before = msgs[0].ID
	}
}

// TestFixtures reads each hand-written fixture and compares the chat messages with the expected
// file beside it (.chat.json). Each fixture states the harness version its shape follows:
// claude-<version>.jsonl for Claude Code, codex-<version>.jsonl for a Codex rollout.
func TestFixtures(t *testing.T) {
	claude, _ := filepath.Glob("testdata/claude-*.jsonl")
	codex, _ := filepath.Glob("testdata/codex-*.jsonl")
	if len(claude) == 0 || len(codex) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range append(claude, codex...) {
		t.Run(filepath.Base(f), func(t *testing.T) {
			var l logs
			open := NewClaude
			if strings.HasPrefix(filepath.Base(f), "codex-") {
				open = NewCodex
			}
			r := open(Config{Path: f, Session: "s1", Logf: l.logf})
			got := readAll(t, r, 7)
			if full := readAll(t, r, 200); !reflect.DeepEqual(got, full) {
				t.Fatal("paging in pages of 7 gives other messages than one page")
			}
			want := strings.TrimSuffix(f, ".jsonl") + ".chat.json"
			b, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, '\n')
			if *update {
				if err := os.WriteFile(want, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			exp, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, exp) {
				t.Errorf("messages differ from %s:\n%s", want, b)
			}
			for _, m := range got {
				if m.ID == "" || m.At == "" || m.Session != "s1" || m.Kind == "" {
					t.Errorf("message missing id, time, session or kind: %+v", m)
				}
			}
		})
	}
}

// TestUnknownRecord: an unknown record type yields a placeholder message and a logged warning,
// never a silent drop. So do an unknown system subtype and an unknown content block.
func TestUnknownRecord(t *testing.T) {
	var l logs
	r := NewClaude(Config{Path: "testdata/claude-2.1.310.jsonl", Session: "s1", Logf: l.logf})
	msgs := readAll(t, r, 200)
	for _, want := range []string{"future-record", "future_subtype", "server_tool_use"} {
		found := false
		for _, m := range msgs {
			if m.Kind == api.ChatKindEvent && m.Event == api.ChatEventUnknown && strings.Contains(m.Text, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no placeholder for %s", want)
		}
		if !l.has(want) {
			t.Errorf("no warning for %s; logs: %q", want, l.lines)
		}
	}
	if r.Drift() != "" {
		t.Errorf("drift on a tested version: %s", r.Drift())
	}
}

// TestDrift: a version outside the tested range reads in tolerant mode and is reported; an
// unreadable line is a placeholder.
func TestDrift(t *testing.T) {
	var l logs
	r := NewClaude(Config{Path: "testdata/claude-drift.jsonl", Session: "s1", Logf: l.logf})
	msgs := readAll(t, r, 200)
	if len(msgs) != 3 || msgs[0].Kind != api.ChatKindUser || msgs[1].Kind != api.ChatKindAgent || msgs[2].Event != api.ChatEventUnknown {
		t.Fatalf("messages: %+v", msgs)
	}
	if !strings.Contains(r.Drift(), "2.9.0") || !l.has("outside the tested range") || !l.has("unreadable") {
		t.Errorf("drift %q, logs %q", r.Drift(), l.lines)
	}
}

// TestSessionOwner: Claude Code 2.1.319 writes session-owner records (account and organization
// ids) several times a turn. They are known bookkeeping: no chat message, no warning, no drift.
func TestSessionOwner(t *testing.T) {
	var l logs
	r := NewClaude(Config{Path: "testdata/claude-2.1.319.jsonl", Session: "s1", Logf: l.logf})
	msgs := readAll(t, r, 200)
	for _, m := range msgs {
		if m.Event == api.ChatEventUnknown {
			t.Errorf("placeholder: %+v", m)
		}
	}
	if len(msgs) != 3 {
		t.Errorf("%d messages, want 3: %+v", len(msgs), msgs)
	}
	if r.Drift() != "" || len(l.lines) != 0 {
		t.Errorf("drift %q, logs %q", r.Drift(), l.lines)
	}
}

// fixtureLines returns the fixture's lines, each with its newline.
func fixtureLines(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/claude-2.1.310.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.SplitAfter(b, []byte("\n"))
}

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

// TestTruncatedLastLine: while the writer is mid-append, the half-written last line yields no
// message; once its newline lands, the message appears once and is reported as a change. A
// file that did not exist when a client paged it reports what it holds once it appears.
func TestTruncatedLastLine(t *testing.T) {
	lines := fixtureLines(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var changes [][]api.ChatMessage
	r := NewClaude(Config{Path: path, Session: "s1", OnChange: func(m []api.ChatMessage) { changes = append(changes, m) }})
	// Not written yet: an empty conversation, not an error.
	if msgs, more, cur, err := r.Page(context.Background(), "", 50); err != nil || len(msgs) != 0 || more || cur != "" {
		t.Fatalf("missing file: %v %v %q %v", msgs, more, cur, err)
	}
	appendFile(t, path, bytes.Join(lines[:4], nil)) // up to and with the first prompt
	before := readAll(t, r, 200)
	if len(before) != 1 || before[0].Text != "Add a greeting to the demo script." {
		t.Fatalf("first read: %+v", before)
	}
	text := lines[5] // the agent text record
	for _, cutAt := range []int{1, len(text) / 2, len(text) - 1} {
		path2 := filepath.Join(t.TempDir(), "s.jsonl")
		appendFile(t, path2, bytes.Join(lines[:5], nil))
		var got [][]api.ChatMessage
		r2 := NewClaude(Config{Path: path2, Session: "s1", OnChange: func(m []api.ChatMessage) { got = append(got, m) }})
		n0 := len(readAll(t, r2, 200))
		appendFile(t, path2, text[:cutAt])
		if msgs := readAll(t, r2, 200); len(msgs) != n0 || len(got) != 0 {
			t.Fatalf("cut at %d: a half line made a message: %+v %+v", cutAt, msgs, got)
		}
		appendFile(t, path2, text[cutAt:])
		msgs := readAll(t, r2, 200)
		if len(msgs) != n0+1 || msgs[n0].Text != "I will read the script, then edit it." {
			t.Fatalf("cut at %d: after the rest: %+v", cutAt, msgs)
		}
		if len(got) != 1 || len(got[0]) != 1 || got[0][0].ID != msgs[n0].ID {
			t.Fatalf("cut at %d: changes %+v", cutAt, got)
		}
	}
	if len(changes) != 1 || len(changes[0]) != 1 || changes[0][0].ID != before[0].ID {
		t.Fatalf("the file appeared after an empty page; changes %+v, want its first prompt", changes)
	}
}

// client is what a chat view keeps: messages in order, replaced by id.
type client struct {
	msgs   []api.ChatMessage
	cursor string
}

func (c *client) apply(msgs []api.ChatMessage) {
	for _, m := range msgs {
		found := false
		for i := range c.msgs {
			if c.msgs[i].ID == m.ID {
				c.msgs[i], found = m, true
			}
		}
		if !found {
			c.msgs = append(c.msgs, m)
		}
	}
}

// TestCursorResume: a client that keeps a cursor and asks only for changes ends with the same
// conversation as a fresh full read, whatever the page size and wherever the writer paused. A
// tool result changes its call in place, before the cursor.
func TestCursorResume(t *testing.T) {
	lines := fixtureLines(t)
	for _, limit := range []int{1, 2, 3, 5, 200} {
		for _, pause := range []int{4, 7, 9, 12, 20, len(lines)} {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			appendFile(t, path, bytes.Join(lines[:pause], nil))
			r := NewClaude(Config{Path: path, Session: "s1"})
			var c client
			msgs, _, cur, err := r.Page(context.Background(), "", 200)
			if err != nil {
				t.Fatal(err)
			}
			c.apply(msgs)
			c.cursor = cur
			appendFile(t, path, bytes.Join(lines[pause:], nil))
			for i := 0; ; i++ {
				msgs, more, next, err := r.Since(context.Background(), c.cursor, limit)
				if err != nil {
					t.Fatal(err)
				}
				c.apply(msgs)
				c.cursor = next
				if !more {
					break
				}
				if i > 1000 {
					t.Fatalf("limit %d pause %d: paging does not end", limit, pause)
				}
			}
			full := readAll(t, NewClaude(Config{Path: path, Session: "s1"}), 200)
			if !reflect.DeepEqual(c.msgs, full) {
				t.Fatalf("limit %d pause %d: the resumed conversation differs:\n%+v\nwant\n%+v", limit, pause, c.msgs, full)
			}
			// A cursor at the end gives nothing new; a fresh reader (a restarted gff-hab) takes it.
			msgs, more, _, err := NewClaude(Config{Path: path, Session: "s1"}).Since(context.Background(), c.cursor, limit)
			if err != nil || len(msgs) != 0 || more {
				t.Fatalf("limit %d pause %d: cursor at the end: %+v %v %v", limit, pause, msgs, more, err)
			}
		}
	}
}

// TestStaleCursor: a cursor from other contents or past the end is refused, and a replaced file
// is read again from the start.
func TestStaleCursor(t *testing.T) {
	lines := fixtureLines(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	appendFile(t, path, bytes.Join(lines[:10], nil))
	var l logs
	r := NewClaude(Config{Path: path, Session: "s1", Logf: l.logf})
	_, _, cur, err := r.Page(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	gen, _, _ := strings.Cut(cur, ":")
	for _, bad := range []string{"x", gen + ":999999", "000000000000:10", gen + ":-1"} {
		if _, _, _, err := r.Since(context.Background(), bad, 50); !errors.Is(err, ErrStaleCursor) {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
	if _, _, _, err := r.Page(context.Background(), "no-such-id", 50); !errors.Is(err, ErrUnknownMessage) {
		t.Errorf("unknown before: %v", err)
	}
	// Replace the file with other contents.
	tmp := filepath.Join(dir, "new.jsonl")
	appendFile(t, tmp, bytes.Join(lines[3:6], nil))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.Since(context.Background(), cur, 50); !errors.Is(err, ErrStaleCursor) {
		t.Errorf("cursor after a replacement: %v", err)
	}
	msgs := readAll(t, r, 50)
	if len(msgs) != 2 || !l.has("replaced") {
		t.Errorf("after a replacement: %+v, logs %q", msgs, l.lines)
	}
}

func TestFindClaude(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	if _, err := FindClaude(dir, id); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("empty dir: %v", err)
	}
	p := filepath.Join(dir, "projects", "-work-demo", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, []byte("{}\n"))
	if got, err := FindClaude(dir, id); err != nil || got != p {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"", "../x", "a/b", "*", ".hidden"} {
		if _, err := FindClaude(dir, bad); err == nil {
			t.Errorf("id %q was accepted", bad)
		}
	}
	got, err := ClaudeConfigDir(func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "/cfg"
		}
		return ""
	})
	if err != nil || got != "/cfg" {
		t.Errorf("config dir %q %v", got, err)
	}
}

// TestCodexUnknown: an unknown rollout line type and an unknown item type become placeholders
// with a warning; an unknown event_msg type is only logged, since Codex has many UI events. The
// generated image's bytes never reach a message.
func TestCodexUnknown(t *testing.T) {
	var l logs
	r := NewCodex(Config{Path: "testdata/codex-0.166.0.jsonl", Session: "s1", Logf: l.logf})
	msgs := readAll(t, r, 200)
	for _, want := range []string{"future_line", "FutureItem"} {
		found := false
		for _, m := range msgs {
			if m.Event == api.ChatEventUnknown && strings.Contains(m.Text, want) {
				found = true
			}
		}
		if !found || !l.has(want) {
			t.Errorf("no placeholder or warning for %s; logs %q", want, l.lines)
		}
	}
	if !l.has("future_event") {
		t.Errorf("no warning for an unknown event; logs %q", l.lines)
	}
	b, _ := json.Marshal(msgs)
	for _, leak := range []string{"future_event", "iVBORw0K", "environment_context", "opaque"} {
		if bytes.Contains(b, []byte(leak)) {
			t.Errorf("messages contain %q", leak)
		}
	}
	if r.Drift() != "" {
		t.Errorf("drift on a tested version: %s", r.Drift())
	}
}

// TestCodexDrift: a newer Codex reads in tolerant mode and is reported; an unreadable line is a
// placeholder.
func TestCodexDrift(t *testing.T) {
	var l logs
	r := NewCodex(Config{Path: "testdata/codex-drift.jsonl", Session: "s1", Logf: l.logf})
	msgs := readAll(t, r, 200)
	if len(msgs) != 3 || msgs[0].Kind != api.ChatKindUser || msgs[1].Kind != api.ChatKindAgent || msgs[2].Event != api.ChatEventUnknown {
		t.Fatalf("messages: %+v", msgs)
	}
	if !strings.Contains(r.Drift(), "Codex 0.170.0") || !l.has("outside the tested range") || !l.has("unreadable") {
		t.Errorf("drift %q, logs %q", r.Drift(), l.lines)
	}
}

// TestCodexTail: a rollout read while Codex appends to it reports each new item once, and a
// half-written last line waits for its newline.
func TestCodexTail(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-0.166.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	appendFile(t, path, bytes.Join(lines[:6], nil))
	var changes []api.ChatMessage
	r := NewCodex(Config{Path: path, Session: "s1", OnChange: func(m []api.ChatMessage) { changes = append(changes, m...) }})
	msgs, _, _, err := r.Page(context.Background(), "", 50)
	if err != nil || len(msgs) != 1 || msgs[0].Kind != api.ChatKindUser {
		t.Fatalf("first page: %+v %v", msgs, err)
	}
	half := lines[8][:len(lines[8])/2]
	appendFile(t, path, bytes.Join(lines[6:8], nil))
	appendFile(t, path, half)
	if err := r.Refresh(); err != nil || len(changes) != 0 {
		t.Fatalf("half line: %+v %v", changes, err)
	}
	appendFile(t, path, lines[8][len(half):])
	if err := r.Refresh(); err != nil || len(changes) != 1 || changes[0].ID != "msg_a1" {
		t.Fatalf("after the newline: %+v %v", changes, err)
	}
}

func TestFindCodex(t *testing.T) {
	dir := t.TempDir()
	id := "019a0000-0000-7000-8000-000000000001"
	if _, err := FindCodex(dir, id); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("empty dir: %v", err)
	}
	p := filepath.Join(dir, "sessions", "2026", "01", "01", "rollout-2026-01-01T10-00-00-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, []byte("{}\n"))
	if got, err := FindCodex(dir, id); err != nil || got != p {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"", "../x", "a/b", "*", ".hidden"} {
		if _, err := FindCodex(dir, bad); err == nil {
			t.Errorf("id %q was accepted", bad)
		}
	}
	got, err := CodexHome(func(k string) string {
		if k == "CODEX_HOME" {
			return "/codex"
		}
		return ""
	})
	if err != nil || got != "/codex" {
		t.Errorf("codex home %q %v", got, err)
	}
}

// TestFixturesClean: fixtures hold no home paths and no record from a real session.
func TestFixturesClean(t *testing.T) {
	files, _ := filepath.Glob("testdata/*")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"/Users/", "/home/", "/root/", ".ts.net"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s contains %q", f, bad)
			}
		}
	}
}

// TestIdlePollAllocatesLittle: reading a file that has not grown allocates no read buffer.
func TestIdlePollAllocatesLittle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendFile(t, path, bytes.Join(fixtureLines(t), nil))
	r := NewClaude(Config{Path: path, Session: "s1"})
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	for range 100 {
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&b)
	if got := b.TotalAlloc - a.TotalAlloc; got > 1<<20 {
		t.Errorf("100 idle reads allocated %d bytes", got)
	}
}

// TestRewriteSeen: a file truncated and written again past the old size, and a file replaced,
// are read again from the start; each time a subscriber gets one reset event, never the
// messages read again, and the log names each replacement.
func TestRewriteSeen(t *testing.T) {
	lines := fixtureLines(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	appendFile(t, path, bytes.Join(lines[:6], nil))
	var l logs
	var changes []api.ChatMessage
	r := NewClaude(Config{Path: path, Session: "s1", Logf: l.logf, OnChange: func(m []api.ChatMessage) { changes = append(changes, m...) }})
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	// Same file, truncated, then other contents longer than what was read.
	if err := os.WriteFile(path, bytes.Join(append([][]byte{lines[5]}, lines[:10]...), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Event != api.ChatEventReset || changes[0].Session != "s1" || changes[0].ID != "reset-1" {
		t.Fatalf("after a rewrite: changes %+v", changes)
	}
	if msgs := readAll(t, r, 200); len(msgs) != 5 || msgs[0].Kind != api.ChatKindAgent {
		t.Fatalf("after a rewrite the conversation was not read again: %+v", msgs)
	}
	tmp := filepath.Join(dir, "new.jsonl")
	appendFile(t, tmp, bytes.Join(lines[:4], nil))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[1].ID != "reset-2" || !l.has("2 times") {
		t.Fatalf("after a replacement: changes %+v, logs %q", changes, l.lines)
	}
}

// TestPageHonoursContext: a page whose context has ended returns its error, and the next page
// reads the whole file with no change reported.
func TestPageHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendFile(t, path, bytes.Join(fixtureLines(t), nil))
	var changes int
	r := NewClaude(Config{Path: path, Session: "s1", OnChange: func(m []api.ChatMessage) { changes += len(m) }})
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := r.Page(done, "", 50); !errors.Is(err, context.Canceled) {
		t.Fatalf("page with an ended context: %v", err)
	}
	if msgs := readAll(t, r, 200); len(msgs) == 0 || changes != 0 {
		t.Fatalf("after: %d messages, %d changes reported", len(msgs), changes)
	}
}

// TestNoSilentDrops: a known record that yields nothing to show becomes a placeholder with a
// warning, and a tool result whose call id now names a placeholder does not crash the reader.
// A version inside the tested range after one outside it clears the drift.
func TestNoSilentDrops(t *testing.T) {
	recs := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T10:00:00Z","version":"2.9.0"}`,
		`{"type":"system","subtype":"informational","uuid":"s1","timestamp":"2026-01-01T10:00:01Z","content":{"x":1}}`,
		`{"type":"system","subtype":"api_error","uuid":"s2","timestamp":"2026-01-01T10:00:02Z","content":""}`,
		`{"type":"system","subtype":"local_command","uuid":"s3","timestamp":"2026-01-01T10:00:03Z","content":["x"]}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T10:00:04Z","version":"2.1.310","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`,
		`{"type":"future-record","uuid":"a1","timestamp":"2026-01-01T10:00:05Z"}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-01-01T10:00:06Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendFile(t, path, []byte(strings.Join(recs, "\n")+"\n"))
	var l logs
	r := NewClaude(Config{Path: path, Session: "s1", Logf: l.logf})
	msgs := readAll(t, r, 200)
	unknown := map[string]bool{}
	for _, m := range msgs {
		if m.Event == api.ChatEventUnknown {
			unknown[m.ID] = true
		}
	}
	for _, id := range []string{"u1", "s1", "s2", "s3"} {
		if !unknown[id] {
			t.Errorf("record %s left no placeholder: %+v", id, msgs)
		}
	}
	if !l.has("u1 has no message") || !l.has("s1 (informational)") || !l.has("s2 (api_error)") || !l.has("s3 (local_command)") {
		t.Errorf("logs %q", l.lines)
	}
	if r.Drift() != "" {
		t.Errorf("drift %q after a record in the tested range", r.Drift())
	}
}

// TestCwdAndAll: the reader keeps the folder each format records and returns the whole
// conversation, which rich content derives touched files from.
func TestCwdAndAll(t *testing.T) {
	for path, open := range map[string]func(Config) *Reader{
		"testdata/claude-2.1.310.jsonl": NewClaude,
		"testdata/codex-0.166.0.jsonl":  NewCodex,
	} {
		r := open(Config{Path: path, Session: "s1"})
		if r.Cwd() != "" {
			t.Errorf("%s: cwd before a read", path)
		}
		all, err := r.All(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		page, _, _, _ := r.Page(context.Background(), "", 200)
		if len(all) == 0 || len(all) != len(page) || all[0].Session != "s1" {
			t.Errorf("%s: all %d, page %d", path, len(all), len(page))
		}
		if r.Cwd() != "/work/demo" {
			t.Errorf("%s: cwd %q", path, r.Cwd())
		}
	}
}
