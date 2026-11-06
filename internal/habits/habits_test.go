package habits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)} }
func newTracker(c *clock) *Tracker   { return New(c.now) }
func prompts(tr *Tracker, k string, n int) (reminded int) {
	for range n {
		if tr.Prompt(k) {
			reminded++
		}
	}
	return reminded
}

// TestReminderTurns: 20 prompts after a change with no write bring one reminder, and no other
// within the hour; a write starts the count again.
func TestReminderTurns(t *testing.T) {
	c := newClock()
	tr := newTracker(c)
	if prompts(tr, "s1", 50) != 0 {
		t.Fatal("a reminder with no change")
	}
	tr.Changed("s1")
	if prompts(tr, "s1", 19) != 0 {
		t.Fatal("a reminder before 20 prompts")
	}
	if !tr.Prompt("s1") {
		t.Fatal("no reminder at the 20th prompt")
	}
	if prompts(tr, "s1", 40) != 0 {
		t.Fatal("a second reminder within the hour")
	}
	c.add(ReminderEvery)
	if !tr.Prompt("s1") {
		t.Fatal("no reminder an hour later")
	}
	tr.Wrote("s1")
	c.add(2 * ReminderEvery)
	if prompts(tr, "s1", 30) != 0 {
		t.Fatal("a reminder after a write with no change since")
	}
	tr.Changed("s1")
	if prompts(tr, "s1", 19) != 0 || !tr.Prompt("s1") {
		t.Fatal("the count did not start again at the change after the write")
	}
	if tr.Prompt("s2") {
		t.Fatal("another session reminded")
	}
}

// TestReminderTime: files changed 30 minutes ago with no write bring the reminder at the next
// prompt.
func TestReminderTime(t *testing.T) {
	c := newClock()
	tr := newTracker(c)
	tr.Changed("s1")
	c.add(ReminderAfter - time.Second)
	if tr.Prompt("s1") {
		t.Fatal("a reminder before 30 minutes")
	}
	c.add(time.Second)
	tr.Changed("s1") // a later change does not move the start
	if !tr.Prompt("s1") {
		t.Fatal("no reminder at 30 minutes")
	}
}

// TestCloseout: the first stop after a change with no write closes out, once per session; a
// session that wrote, or changed nothing, never does.
func TestCloseout(t *testing.T) {
	tr := newTracker(newClock())
	if tr.Stop("s1") {
		t.Fatal("a closeout with no change")
	}
	tr.Changed("s1")
	if !tr.Stop("s1") {
		t.Fatal("no closeout after a change")
	}
	for range 5 {
		tr.Changed("s1")
		if tr.Stop("s1") {
			t.Fatal("a second closeout")
		}
	}
	tr.Changed("s2")
	tr.Wrote("s2")
	tr.Changed("s2")
	if tr.Stop("s2") {
		t.Fatal("a closeout for a session that wrote")
	}
}

func TestChanges(t *testing.T) {
	for _, c := range []struct {
		payload string
		want    bool
	}{
		{`{"tool_name":"Edit","tool_input":{"file_path":"/work/r/a.go"}}`, true},
		{`{"tool_name":"Write","tool_input":{}}`, true},
		{`{"tool_name":"MultiEdit"}`, true},
		{`{"tool_name":"NotebookEdit"}`, true},
		{`{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch"}}`, true},
		{`{"tool_name":"Read","tool_input":{"file_path":"/work/r/a.go"}}`, false},
		{`{"tool_name":"Bash","tool_input":{"command":"git commit -m 'x'"}}`, true},
		{`{"tool_name":"Bash","tool_input":{"command":"make && git -C /work/r commit -am x"}}`, true},
		{`{"tool_name":"exec_command","tool_input":{"command":["git","commit","-m","x"]}}`, true},
		{`{"tool_name":"Bash","tool_input":{"command":"git status"}}`, false},
		{`{"tool_name":"Bash","tool_input":{"command":"echo legit commit"}}`, false},
		{`{"tool_name":"Bash","tool_input":{"command":"git log --grep commit"}}`, false},
		{`not json`, false},
	} {
		if got := Changes(json.RawMessage(c.payload)); got != c.want {
			t.Errorf("%s: %v, want %v", c.payload, got, c.want)
		}
	}
}

// TestOff: [logbook.habits_off] turns habits off per project; a missing file is no table, an
// unknown habit is refused by name, and OffFile keeps the last good table.
func TestOff(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hab.toml")
	if off, err := LoadOff(p); err != nil || len(off) != 0 {
		t.Fatalf("missing file: %v %v", off, err)
	}
	if err := os.WriteFile(p, []byte("[catalog]\nroots = []\n\n[logbook.habits_off]\n\"example.com/o/app\" = [\"closeout\", \"reminder\"]\n\"proj-aaaaaaaa\" = [\"brief\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &OffFile{Path: p}
	off := f.Get()
	if !off.Has(Closeout, "proj-x", "example.com/o/app") || !off.Has(Reminder, "", "example.com/o/app") || off.Has(Brief, "", "example.com/o/app") {
		t.Fatalf("by identity: %v", off)
	}
	if !off.Has(Brief, "proj-aaaaaaaa") || off.Has(Closeout, "proj-aaaaaaaa") || off.Has(Brief, "") {
		t.Fatalf("by id: %v", off)
	}
	if err := os.WriteFile(p, []byte("[logbook.habits_off]\n\"proj-aaaaaaaa\" = [\"nag\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(p, future, future)
	if _, err := LoadOff(p); err == nil || !strings.Contains(err.Error(), `"nag"`) {
		t.Fatalf("an unknown habit: %v", err)
	}
	if off := f.Get(); !off.Has(Brief, "proj-aaaaaaaa") || f.Err() == nil {
		t.Fatalf("a bad file replaced the last good table: %v %v", off, f.Err())
	}
}

// TestOffExample: the example config's table parses and turns nothing off.
func TestOffExample(t *testing.T) {
	off, err := LoadOff(filepath.Join("..", "..", "docs", "examples", "hab.toml"))
	if err != nil || len(off) != 0 {
		t.Fatalf("example: %v %v", off, err)
	}
}
