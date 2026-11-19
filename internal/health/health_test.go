package health

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrevious(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	m := Marker{Proc: "hab", Run: "e1", Build: "b1", Version: "v1", CrashFile: "hab-e1.txt"}
	if _, ok := Previous(Marker{Clean: true}, nil, now); ok {
		t.Fatal("a clean end gave a record")
	}
	r, ok := Previous(m, nil, now)
	if !ok || r.Kind != KindUnclean || r.Cause != CauseKilled || r.CrashFile != "" || r.Run != "e1" {
		t.Fatalf("no crash output: %+v", r)
	}
	long := strings.Repeat("é", 100)
	crash := "goroutine 1:\npanic: " + long + "\n\ngoroutine 2 [running]:\n"
	r, _ = Previous(m, []byte(crash), now)
	if r.Kind != KindCrash || r.CrashFile != "hab-e1.txt" || !strings.HasPrefix(r.Cause, "panic: é") ||
		len(r.Cause) > len("panic: ")+CauseMax || !strings.HasSuffix(r.Cause, "é") {
		t.Fatalf("a panic: %+v", r)
	}
	r, _ = Previous(m, []byte("fatal error: concurrent map writes\n"), now)
	if r.Kind != KindCrash || r.Cause != "fatal error: concurrent map writes" {
		t.Fatalf("a fatal error: %+v", r)
	}
}

func TestAppendKeeps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.jsonl")
	for i := range Keep + 5 {
		if err := Append(path, Record{Kind: KindUnclean, Cause: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := Read(path)
	if err != nil || len(rs) != Keep || rs[0].Cause != "5" || rs[Keep-1].Cause != fmt.Sprint(Keep+4) {
		t.Fatalf("%d records, first %+v, err %v", len(rs), rs[0], err)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "hab.json")
	if _, ok, err := ReadMarker(path); ok || err != nil {
		t.Fatalf("no marker: %v %v", ok, err)
	}
	m := Marker{Proc: "hab", Run: "e", Pid: 7, Started: time.Unix(5, 0).UTC(), Hook: "up"}
	if err := WriteMarker(path, m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadMarker(path)
	if !ok || err != nil || got != m {
		t.Fatalf("read %+v %v %v", got, ok, err)
	}
}

func TestNoticeDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notice.json")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if !NoticeDue(path, now, time.Hour) {
		t.Fatal("the first notice was not due")
	}
	if NoticeDue(path, now.Add(59*time.Minute), time.Hour) {
		t.Fatal("a second notice within the hour")
	}
	if !NoticeDue(path, now.Add(61*time.Minute), time.Hour) {
		t.Fatal("no notice after the hour")
	}
}

// TestAppendConcurrent: Appends from many goroutines, as the link and the hook server make,
// keep every record.
func TestAppendConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.jsonl")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Append(path, Record{Kind: KindUnclean, Cause: fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if rs, err := Read(path); err != nil || len(rs) != 20 {
		t.Fatalf("%d records, want 20: %v", len(rs), err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*")); len(m) != 0 {
		t.Fatalf("temporary files left: %v", m)
	}
}
