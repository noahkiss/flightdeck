package capcomthread

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Millisecond); return c.t }

func open(t *testing.T, dir string, c *clock) *Store {
	t.Helper()
	s, problems, err := Open(dir, c.now)
	if err != nil || len(problems) != 0 {
		t.Fatal(err, problems)
	}
	return s
}

func TestThreadAddFoldReopen(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)}
	s := open(t, dir, c)
	var seen []string
	s.Watch(func(it Item) { seen = append(seen, it.ID) })
	m, added, err := s.Add(Item{Kind: KindMessage, Author: Author{Kind: AuthorUser, Device: "dev1"}, Text: "hello", Key: "k1"})
	if err != nil || !added {
		t.Fatal(err)
	}
	if again, added, _ := s.Add(Item{Kind: KindMessage, Author: Author{Kind: AuthorUser}, Text: "hello", Key: "k1"}); added || again.ID != m.ID {
		t.Fatal("a repeated key stored a second item")
	}
	c.t = c.t.Add(2 * time.Second) // the next month
	r, _, _ := s.Add(Item{Kind: KindMessage, Author: Author{Kind: AuthorCapcom}, Turn: "t1", Part: 0, Text: "hi"})
	if dup, added, _ := s.Add(Item{Kind: KindMessage, Author: Author{Kind: AuthorCapcom}, Turn: "t1", Part: 0, Text: "hi"}); added || dup.ID != r.ID {
		t.Fatal("a repeated reply stored a second item")
	}
	if _, changed, _ := s.MarkRead(m.ID, c.now()); !changed {
		t.Fatal("not marked read")
	}
	if _, changed, _ := s.MarkRead(m.ID, c.now()); changed {
		t.Fatal("read twice")
	}
	s.AddTool("t1", ToolCall{Name: "transcript_search", Summary: "searched transcripts"}, false)
	tl, _ := s.AddTool("t1", ToolCall{Name: "WebSearch", Summary: "searched the web"}, false)
	if len(tl.Tools) != 2 {
		t.Fatalf("tools line %+v", tl)
	}
	card, _, _ := s.Add(Item{Kind: KindCard, Author: Author{Kind: AuthorFlightdeck}, Card: &Card{Type: "approval", Choices: []string{"allow", "deny"}}})
	if _, err := s.AnswerCard(card.ID, "allow", "dev1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AnswerCard(card.ID, "deny", "dev2"); !errors.Is(err, ErrConflict) || got.Answer.Choice != "allow" {
		t.Fatalf("second answer: %v %+v", err, got.Answer)
	}
	if _, err := s.AnswerCard(m.ID, "allow", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a message answered as a card: %v", err)
	}
	if len(seen) != 7 {
		t.Fatalf("watch saw %d changes: %v", len(seen), seen)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 2 || !strings.HasSuffix(files[0], "2026-10.jsonl") || !strings.HasSuffix(files[1], "2026-11.jsonl") {
		t.Fatalf("files %v", files)
	}
	for _, f := range files {
		if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, fi.Mode())
		}
	}
	// A reopen folds the same thread; ids keep rising.
	s2 := open(t, dir, c)
	page, more := s2.Page("", 0)
	if more || len(page) != 4 || page[0].ID != card.ID || page[3].ID != m.ID {
		t.Fatalf("page %+v %v", page, more)
	}
	if page[3].Read.IsZero() || len(page[1].Tools) != 2 || page[0].Answer == nil || page[0].Answer.Choice != "allow" {
		t.Fatalf("fold lost a change: %+v", page)
	}
	if got, ok := s2.ByKey("k1"); !ok || got.ID != m.ID {
		t.Fatal("key lost on reopen")
	}
	n, _, _ := s2.Add(Item{Kind: KindNote, Author: Author{Kind: AuthorFlightdeck}, Text: "Capcom restarted"})
	if n.ID <= card.ID || s2.Last() != n.ID {
		t.Fatalf("id %s not after %s", n.ID, card.ID)
	}
	if _, added, _ := s2.Add(Item{Kind: KindMessage, Author: Author{Kind: AuthorCapcom}, Turn: "t1", Part: 0}); added {
		t.Fatal("reply dedupe lost on reopen")
	}
}

func TestThreadPaging(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	var ids []string
	for i := 0; i < 250; i++ {
		it, _, err := s.Add(Item{Kind: KindNote, Author: Author{Kind: AuthorFlightdeck}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	p1, more := s.Page("", 1000)
	if len(p1) != PageMax || !more || p1[0].ID != ids[249] {
		t.Fatalf("page 1: %d %v", len(p1), more)
	}
	p2, more := s.Page(p1[len(p1)-1].ID, 100)
	if len(p2) != 50 || more || p2[49].ID != ids[0] {
		t.Fatalf("page 2: %d %v", len(p2), more)
	}
	if after := s.After(ids[247], nil); len(after) != 2 || after[0].ID != ids[248] {
		t.Fatalf("after: %v", after)
	}
}

func TestThreadDamagedAndCutLine(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	s := open(t, dir, c)
	s.Add(Item{Kind: KindNote, Author: Author{Kind: AuthorFlightdeck}, Text: "october"})
	c.t = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	s.Add(Item{Kind: KindNote, Author: Author{Kind: AuthorFlightdeck}, Text: "november"})
	nov := filepath.Join(dir, "2026-11.jsonl")
	oct := filepath.Join(dir, "2026-10.jsonl")
	// A crash mid-write leaves half a line: it is cut, and the file still loads.
	f, _ := os.OpenFile(nov, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"op":"add","item":{"id":"c00`)
	f.Close()
	s2 := open(t, dir, c)
	if p, _ := s2.Page("", 0); len(p) != 2 {
		t.Fatalf("after a cut line: %d items", len(p))
	}
	// A complete line that does not parse sets the month aside; October still loads.
	os.WriteFile(nov, []byte("not json\n"), 0o600)
	s3, problems, err := Open(dir, c.now)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0].SetAside, "2026-11.jsonl.bad-") {
		t.Fatalf("%v %+v", err, problems)
	}
	if p, _ := s3.Page("", 0); len(p) != 1 || p[0].Text != "october" {
		t.Fatalf("after a damaged month: %+v", p)
	}
	if _, err := os.Stat(nov); !os.IsNotExist(err) {
		t.Fatal("the damaged file was not moved")
	}
	if _, err := os.Stat(oct); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s3.Add(Item{Kind: KindNote, Author: Author{Kind: AuthorFlightdeck}, Text: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nov); err != nil {
		t.Fatal("no new month file:", err)
	}
}
