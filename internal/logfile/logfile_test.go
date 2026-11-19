package logfile

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/testtmp"
)

const habFlags = log.LstdFlags | log.Lmicroseconds | log.LUTC

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// TestWriterRotates: the file is renamed at its cap, Keep old files stay, the newest line is in
// the current file, and every file is mode 0600 in a folder of mode 0700 (spec 4.3, test 1).
func TestWriterRotates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "hab.log")
	w := New(Config{Max: 400, Keep: 2})
	defer w.Close()
	if err := w.Open(path); err != nil {
		t.Fatal(err)
	}
	l := log.New(w, "gff-hab: ", habFlags)
	for i := range 60 {
		l.Printf("line %03d", i)
	}
	w.Flush()
	for _, p := range []string{path, Old(path, 1), Old(path, 2)} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 400 {
			t.Errorf("%s is %d bytes, over the cap", p, st.Size())
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", p, st.Mode().Perm())
		}
	}
	if _, err := os.Stat(Old(path, 3)); !os.IsNotExist(err) {
		t.Errorf("a third old file: %v", err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v", st.Mode().Perm())
	}
	got := lines(t, path)
	if !strings.HasSuffix(got[len(got)-1], "line 059") {
		t.Errorf("the newest line is %q", got[len(got)-1])
	}
	// The old files hold the lines just before, in order.
	var all []string
	for _, p := range []string{Old(path, 2), Old(path, 1), path} {
		all = append(all, lines(t, p)...)
	}
	for i := 1; i < len(all); i++ {
		if all[i][len(all[i])-3:] <= all[i-1][len(all[i-1])-3:] {
			t.Fatalf("lines out of order at %d: %q after %q", i, all[i], all[i-1])
		}
	}
}

// TestWriterReopens: a removed folder and file come back at the next check, and the lines after
// it land in the new file (spec 4.3 inode check, 12).
func TestWriterReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "hab.log")
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	w := New(Config{Now: func() time.Time { return time.Unix(0, now.Load()) }})
	defer w.Close()
	if err := w.Open(path); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(w, "before")
	w.Flush()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	now.Add(int64(checkEvery + time.Second))
	fmt.Fprintln(w, "after")
	w.Flush()
	if got := lines(t, path); len(got) != 1 || got[0] != "after" {
		t.Fatalf("after the folder was removed: %q", got)
	}
}

// TestWriterNeverBlocks: with its queue full, Write returns at once and counts the lines it
// dropped; the count is written before the next line that reaches the file (spec 4.3, 11).
func TestWriterNeverBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hab.log")
	w := New(Config{Queue: 10, Dropped: func(n int64) []byte { return fmt.Appendf(nil, "dropped %d\n", n) }})
	defer w.Close()
	done := make(chan struct{})
	go func() {
		for i := range 100 {
			fmt.Fprintf(w, "line %d\n", i) // nothing reads the queue before Open
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Write blocked on a full queue")
	}
	if n := w.Dropped(); n != 90 {
		t.Fatalf("dropped %d, want 90", n)
	}
	if err := w.Open(path); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	got := lines(t, path)
	if len(got) != 11 || got[0] != "dropped 90" || got[1] != "line 0" || got[10] != "line 9" {
		t.Fatalf("file: %q", got)
	}
}

// TestWriterEarlyAndTee: before Open a line goes to Early at once; with Tee every line does.
func TestWriterEarlyAndTee(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hab.log")
	var early bytes.Buffer
	w := New(Config{Early: &early})
	fmt.Fprintln(w, "one")
	w.Open(path)
	fmt.Fprintln(w, "two")
	w.Close()
	if early.String() != "one\n" {
		t.Errorf("early got %q", early.String())
	}
	if got := lines(t, path); len(got) != 2 {
		t.Errorf("file: %q", got)
	}
	var tee bytes.Buffer
	w = New(Config{Tee: &tee, Early: &early})
	fmt.Fprintln(w, "three")
	w.Close()
	if tee.String() != "three\n" || early.String() != "one\n" {
		t.Errorf("tee %q early %q", tee.String(), early.String())
	}
}

// TestCopyTruncateAppender: a file another process appends to is copied to .1 and truncated in
// place; that process's later lines land at the start of the truncated file, with no hole of
// zero bytes (spec 4.3, test 2). The shell's >> opens the file for append, as launchd and
// systemd's append: do; internal/service's TestCopyTruncateUnderInit checks the real ones.
func TestCopyTruncateAppender(t *testing.T) {
	dir := testtmp.Dir(t, "gfflog") // the shell runs from it, so a leak is found and stopped
	path := filepath.Join(dir, "holder.log")
	cmd := exec.Command("/bin/sh", "-c", `pad=$(printf '%0100d' 0); i=0; while :; do i=$((i+1)); echo "line $i $pad"; sleep 0.01; done >>"$1"`, "sh", path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	waitFor(t, "the file to pass 2 KiB", func() bool { st, err := os.Stat(path); return err == nil && st.Size() > 2048 })
	did, err := CopyTruncate(path, 1024, 2)
	if err != nil || !did {
		t.Fatalf("CopyTruncate: %v %v", did, err)
	}
	old, err := os.ReadFile(Old(path, 1))
	if err != nil || len(old) <= 2048 {
		t.Fatalf("path.1: %d bytes, %v", len(old), err)
	}
	waitFor(t, "new lines after the truncate", func() bool { st, err := os.Stat(path); return err == nil && st.Size() > 300 })
	b, _ := os.ReadFile(path)
	if b[0] == 0 || bytes.IndexByte(b, 0) >= 0 {
		t.Fatalf("the truncated file has a hole of zero bytes: %q", b[:min(len(b), 64)])
	}
	if len(b) >= len(old) {
		t.Fatalf("the file is %d bytes after the truncate, %d before: the writer did not append", len(b), len(old))
	}
	// Under the cap nothing happens.
	if did, err := CopyTruncate(path, 1<<20, 2); did || err != nil {
		t.Fatalf("under the cap: %v %v", did, err)
	}
}

// TestRenameCap: a file over its cap is renamed, and its old file shifts.
func TestRenameCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.log")
	os.WriteFile(path, []byte("first\n"), 0o600)
	if did, _ := RenameCap(path, 3, 1); !did {
		t.Fatal("not renamed")
	}
	os.WriteFile(path, []byte("second\n"), 0o600)
	RenameCap(path, 3, 1)
	if b, _ := os.ReadFile(Old(path, 1)); string(b) != "second\n" {
		t.Fatalf("path.1 = %q", b)
	}
	if _, err := os.Stat(Old(path, 2)); !os.IsNotExist(err) {
		t.Fatal("a second old file stayed")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseLine(t *testing.T) {
	for _, c := range []struct {
		line, msg string
		ok        bool
		loc       *time.Location
		want      time.Time
	}{
		{"gff-hab: 2026/11/19 12:34:56.123456 hello world", "hello world", true, time.UTC,
			time.Date(2026, 11, 19, 12, 34, 56, 123456000, time.UTC)},
		{"gff-hab-holder: 2026/11/19 07:00:01 up", "up", true, time.FixedZone("x", -5*3600),
			time.Date(2026, 11, 19, 12, 0, 1, 0, time.UTC)},
		{"2026/11/19 12:34:56 bare", "bare", true, time.UTC, time.Date(2026, 11, 19, 12, 34, 56, 0, time.UTC)},
		{"goroutine 1 [running]:", "goroutine 1 [running]:", false, nil, time.Time{}},
		{"panic: x: 2026/11/19", "panic: x: 2026/11/19", false, nil, time.Time{}},
	} {
		tm, msg, ok := ParseLine(c.line, c.loc)
		if ok != c.ok || msg != c.msg || !tm.Equal(c.want) {
			t.Errorf("ParseLine(%q) = %v %q %v", c.line, tm, msg, ok)
		}
	}
}

// TestMergeAndFollow: hab and holder files merge in time order across their old files, with a
// stack trace kept with its line; the filter selects by kind, text and time; Follow prints lines
// added after it starts, across a rename rotation and a copy-truncate (spec 4.8, test 5).
func TestMergeAndFollow(t *testing.T) {
	dir := t.TempDir()
	hab := filepath.Join(dir, "hab.log")
	holder := filepath.Join(dir, "gff-hab-holder.aaaa0001.log")
	local := time.FixedZone("local", 2*3600)
	os.WriteFile(Old(hab, 1), []byte("gff-hab: 2026/11/19 10:00:00.000001 h1\n"), 0o600)
	os.WriteFile(hab, []byte("gff-hab: 2026/11/19 10:00:02.000000 h3\ngff-hab: 2026/11/19 10:00:04.000000 h5\ngoroutine 7 [running]:\n\tmain.go:1\n"), 0o600)
	os.WriteFile(holder, []byte("gff-hab-holder: 2026/11/19 12:00:01 o2\ngff-hab-holder: 2026/11/19 12:00:03 o4\n"), 0o600)
	srcs := []Source{{Kind: "hab", Name: "hab", Path: hab}, {Kind: "holder", Name: "holder aaaa0001", Path: holder, Loc: local}}
	var got []string
	if err := Merge(srcs, Filter{}, func(e Entry) { got = append(got, e.Text) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"h1", "o2", "h3", "o4", "h5\ngoroutine 7 [running]:\n\tmain.go:1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("merged %q, want %q", got, want)
	}
	got = nil
	Merge(srcs, Filter{Kinds: []string{"holder"}, Since: time.Date(2026, 11, 19, 10, 0, 2, 0, time.UTC)},
		func(e Entry) { got = append(got, e.Text) })
	if strings.Join(got, "|") != "o4" {
		t.Fatalf("holder since 10:00:02: %q", got)
	}
	got = nil
	Merge(srcs, Filter{Grep: "goroutine"}, func(e Entry) { got = append(got, e.Text) })
	if len(got) != 1 || !strings.HasPrefix(got[0], "h5") {
		t.Fatalf("grep: %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var followed []string
	done := make(chan struct{})
	fw := NewFollower(func() []Source { return srcs }, Filter{})
	go func() {
		fw.Run(ctx, 10*time.Millisecond, func(e Entry) {
			mu.Lock()
			followed = append(followed, e.Text)
			mu.Unlock()
		})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	has := func(s string) func() bool {
		return func() bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(strings.Join(followed, "|"), s) }
	}
	appendLine(t, hab, "gff-hab: 2026/11/19 10:00:06.000000 f1")
	waitFor(t, "f1", has("f1"))
	os.Rename(hab, Old(hab, 1)) // a rename rotation
	appendLine(t, hab, "gff-hab: 2026/11/19 10:00:07.000000 f2")
	waitFor(t, "f2", has("f2"))
	appendLine(t, holder, "gff-hab-holder: 2026/11/19 12:00:08 f3")
	waitFor(t, "f3", has("f3"))
	os.Truncate(holder, 0) // a copy-truncate
	appendLine(t, holder, "gff-hab-holder: 2026/11/19 12:00:09 f4")
	waitFor(t, "f4", has("f4"))
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(strings.Join(followed, "|"), "h1") || strings.Contains(strings.Join(followed, "|"), "o2") {
		t.Fatalf("Follow printed lines from before it started: %q", followed)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, line)
	f.Close()
}
