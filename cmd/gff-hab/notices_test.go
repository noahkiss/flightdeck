package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestNoticeStore: the saved store of recent notices (capcom.md 8.5) reads back after a
// reopen, keeps the last 500 within 7 days, cuts long text, and sets a broken file aside.
func TestNoticeStore(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	l := openNoticeLog(dir, clock, logf)
	l.add(api.Notice{ID: "old", Title: "Eight days ago", At: at(8 * 24 * time.Hour)})
	l.add(api.Notice{ID: "n1", Title: "Build failed", Body: "make test", Level: "active", Session: "s1", At: at(time.Hour)})
	l.add(api.Notice{ID: "long", Title: strings.Repeat("t", 2000), Body: strings.Repeat("é", 3000), At: at(time.Minute)})
	if _, ok := l.get("old"); ok {
		t.Error("a notice past 7 days reads back")
	}
	fi, err := os.Stat(filepath.Join(dir, noticesFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store file: %v %v", err, fi)
	}

	// A reopen (a gff-hab restart) reads every kept notice back.
	l = openNoticeLog(dir, clock, logf)
	n, ok := l.get("n1")
	if !ok || n.Title != "Build failed" || n.Body != "make test" || n.Session != "s1" || n.Level != "active" {
		t.Fatalf("after reopen: %v %+v", ok, n)
	}
	lg, ok := l.get("long")
	if !ok || len(lg.Title) != noticeTitleMax || len(lg.Body) > noticeBodyMax || !strings.HasSuffix(lg.Body, "é") {
		t.Errorf("long notice: %v title %d body %d", ok, len(lg.Title), len(lg.Body))
	}
	// Time passes: a notice ages out on read and at the next open.
	now = now.Add(7 * 24 * time.Hour)
	if _, ok := l.get("n1"); ok {
		t.Error("n1 reads back after 7 days")
	}
	now = now.Add(-7 * 24 * time.Hour)

	// The count: 500 kept, the oldest go first, and the file holds no more.
	for i := range noticeKeep + 20 {
		l.add(api.Notice{ID: fmt.Sprintf("c%d", i), Title: "x", At: at(time.Second)})
	}
	if _, ok := l.get("n1"); ok {
		t.Error("n1 kept past the count")
	}
	if _, ok := l.get("c19"); ok {
		t.Error("c19 kept past the count")
	}
	if _, ok := l.get("c20"); !ok {
		t.Error("c20 dropped within the count")
	}
	l = openNoticeLog(dir, clock, logf)
	if len(l.order) != noticeKeep {
		t.Errorf("reopened with %d notices, want %d", len(l.order), noticeKeep)
	}

	// The file size: escapes count, the oldest go first, and the newest stay. HTML characters
	// are written as they are.
	const big = 120 // 24 KiB each once escaped: past the file size
	for i := range big {
		l.add(api.Notice{ID: fmt.Sprintf("big%d", i), Title: "<b>", Body: strings.Repeat("\x01", noticeBodyMax), At: at(time.Second)})
	}
	b, err := os.ReadFile(filepath.Join(dir, noticesFile))
	if err != nil || len(b) > noticeFileMax || !strings.Contains(string(b), `"<b>"`) {
		t.Fatalf("file of %d bytes (max %d), err %v", len(b), noticeFileMax, err)
	}
	if _, ok := l.get(fmt.Sprintf("big%d", big-1)); !ok {
		t.Error("the newest notice left the store")
	}
	if _, ok := l.get("big0"); ok {
		t.Error("the oldest notice stayed past the file size")
	}
	if n := len(openNoticeLog(dir, clock, logf).order); n != len(l.order) || n >= big {
		t.Errorf("reopened with %d notices, memory has %d", n, len(l.order))
	}

	// A broken file is set aside, and the log starts empty and works.
	if err := os.WriteFile(filepath.Join(dir, noticesFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs = nil
	l = openNoticeLog(dir, clock, logf)
	if len(l.order) != 0 {
		t.Errorf("broken file: %d notices", len(l.order))
	}
	if b, err := os.ReadFile(filepath.Join(dir, noticesFile+".bad")); err != nil || string(b) != "{not json" {
		t.Errorf("broken file not kept aside: %v %q", err, b)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "kept as") {
		t.Errorf("logs %q", logs)
	}
	l.add(api.Notice{ID: "after", Title: "y", At: at(0)})
	if _, ok := openNoticeLog(dir, clock, logf).get("after"); !ok {
		t.Error("a notice after the broken file does not survive a reopen")
	}
}
