package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeMessages stands in for an opencode server's message list: oldest first, served newest
// first in pages, as GET /api/session/{id}/message does.
type fakeMessages struct {
	mu    sync.Mutex
	msgs  []json.RawMessage
	page  int
	calls int
	err   error
}

func (f *fakeMessages) fetch(ctx context.Context, cursor string) ([]json.RawMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, "", f.err
	}
	end := len(f.msgs) // the page holds msgs[start:end], newest first
	if cursor != "" {
		var err error
		if end, err = strconv.Atoi(cursor); err != nil {
			return nil, "", err
		}
	}
	start := max(0, end-f.page)
	var out []json.RawMessage
	for i := end - 1; i >= start; i-- {
		out = append(out, f.msgs[i])
	}
	next := ""
	if start > 0 {
		next = strconv.Itoa(start)
	}
	return out, next, nil
}

func (f *fakeMessages) set(i int, raw string) {
	f.mu.Lock()
	f.msgs[i] = json.RawMessage(raw)
	f.mu.Unlock()
}

func (f *fakeMessages) add(raw string) {
	f.mu.Lock()
	f.msgs = append(f.msgs, json.RawMessage(raw))
	f.mu.Unlock()
}

func (f *fakeMessages) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func loadOpencodeFixture(t *testing.T) (*fakeMessages, string) {
	t.Helper()
	b, err := os.ReadFile("testdata/opencode-2.2.5.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Version  string            `json:"version"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Version == "" {
		t.Fatal("fixture does not state the opencode version its shape follows")
	}
	return &fakeMessages{msgs: fx.Messages, page: 4}, fx.Version
}

func readAllOpencode(t *testing.T, r *OpencodeReader, limit int) []api.ChatMessage {
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

// TestOpencodeFixture reads the hand-written message list and compares the chat messages with
// opencode-2.2.5.chat.json. Unknown types and blocks are placeholders with one warning each.
func TestOpencodeFixture(t *testing.T) {
	f, version := loadOpencodeFixture(t)
	var l logs
	r := NewOpencode(OpencodeConfig{Session: "s1", Fetch: f.fetch, Logf: l.logf})
	r.SetVersion(version)
	if r.Drift() != "" {
		t.Fatalf("drift %q on the tested version", r.Drift())
	}
	got := readAllOpencode(t, r, 7)
	if full := readAllOpencode(t, r, 200); !reflect.DeepEqual(got, full) {
		t.Fatal("paging in pages of 7 gives other messages than one page")
	}
	want := "testdata/opencode-2.2.5.chat.json"
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
	if !l.has(`unknown opencode message type "future-message"`) || !l.has(`unknown opencode content block "future-block"`) {
		t.Errorf("logs %q", l.lines)
	}
}

// TestOpencodeIncremental: a read that nothing marked dirty fetches nothing; a dirty one stops
// at the first message held in its final form once every changing one was fetched again; a
// changed message and a new one reach OnChange and Since.
func TestOpencodeIncremental(t *testing.T) {
	f, _ := loadOpencodeFixture(t)
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var changes []api.ChatMessage
	r := NewOpencode(OpencodeConfig{Session: "s1", Fetch: f.fetch, Now: func() time.Time { return clock },
		OnChange: func(m []api.ChatMessage) { mu.Lock(); changes = append(changes, m...); mu.Unlock() }})
	_, _, cursor, err := r.Page(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 5 || len(changes) != 0 {
		t.Fatalf("first read: %d fetches, %d changes; want every page and no changes", f.count(), len(changes))
	}
	if err := r.Refresh(); err != nil || f.count() != 5 {
		t.Fatalf("a clean read fetched (%d) or failed: %v", f.count(), err)
	}
	clock = clock.Add(opencodeStale)
	if err := r.Refresh(); err != nil || f.count() != 5+3 {
		t.Fatalf("a stale read fetched %d pages, want back to the running shell (3): %v", f.count()-5, err)
	}

	// The running shell ends, and a new message comes.
	f.set(8, `{"id":"msg_09","type":"shell","time":{"created":1793550005600,"completed":1793550006000},"command":"sleep 100","status":"killed","output":{"output":""}}`)
	f.add(`{"id":"msg_19","type":"user","time":{"created":1793550010000},"text":"Next."}`)
	r.MarkDirty()
	msgs, more, next, err := r.Since(context.Background(), cursor, 50)
	if err != nil || more {
		t.Fatal(err, more)
	}
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "msg_09,msg_19" || msgs[0].Tool.Status != api.ToolStatusError {
		t.Fatalf("since: %v %+v", ids, msgs)
	}
	mu.Lock()
	n := len(changes)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("OnChange got %d messages, want 2", n)
	}
	// Nothing ran there now: the next dirty read stops at the newest page.
	before := f.count()
	r.MarkDirty()
	if msgs, _, _, err := r.Since(context.Background(), next, 50); err != nil || len(msgs) != 0 {
		t.Fatalf("since after nothing changed: %+v %v", msgs, err)
	}
	if f.count()-before != 1 {
		t.Fatalf("a dirty read with nothing open fetched %d pages, want 1", f.count()-before)
	}
}

func TestOpencodeCursors(t *testing.T) {
	f, _ := loadOpencodeFixture(t)
	r1 := NewOpencode(OpencodeConfig{Session: "s1", Fetch: f.fetch})
	r2 := NewOpencode(OpencodeConfig{Session: "s1", Fetch: f.fetch})
	_, _, c1, err := r1.Page(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r2.Since(context.Background(), c1, 10); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("another reader's cursor: %v", err)
	}
	if _, _, _, err := r1.Since(context.Background(), "nonsense", 10); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("malformed cursor: %v", err)
	}
	if _, _, _, err := r1.Page(context.Background(), "msg_nope", 10); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("unknown before: %v", err)
	}
}

func TestOpencodeDriftAndErrors(t *testing.T) {
	var l logs
	f := &fakeMessages{page: 10, msgs: []json.RawMessage{json.RawMessage(`{"type":"user"}`), json.RawMessage(`[1]`)}}
	r := NewOpencode(OpencodeConfig{Session: "s1", Fetch: f.fetch, Logf: l.logf})
	r.SetVersion("2.9.0")
	if !strings.Contains(r.Drift(), "outside the tested range") {
		t.Fatalf("drift %q", r.Drift())
	}
	r.SetVersion("2.2.5")
	if r.Drift() != "" {
		t.Fatalf("drift %q in range", r.Drift())
	}
	r.SetVersion("nightly")
	if !strings.Contains(r.Drift(), "unreadable") {
		t.Fatalf("drift %q", r.Drift())
	}
	msgs := readAllOpencode(t, r, 50)
	if len(msgs) != 2 || msgs[0].Event != api.ChatEventUnknown || msgs[1].Event != api.ChatEventUnknown || !l.has("unknown shape") {
		t.Fatalf("unreadable messages: %+v %q", msgs, l.lines)
	}
	f.err = errors.New("connection refused")
	r.MarkDirty()
	if _, _, _, err := r.Page(context.Background(), "", 10); err == nil {
		t.Fatal("a failed fetch read as success")
	}
}
