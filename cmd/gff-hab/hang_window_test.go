package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/health"
)

// A window ends by the wall clock: after a sleep the monotonic timer has not fired, yet printf
// writes nothing past until, and an end timer that fires early (a clock that went back) waits
// for the rest (diagnostics.md 4.5).
func TestDebugWindowWallClock(t *testing.T) {
	state := t.TempDir()
	base := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := base
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
	var lines []string
	logf := func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	count := func(s string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range lines {
			if l == s {
				n++
			}
		}
		return n
	}
	d := openDebug(state, false, logf, clock, func(bool, time.Time) {})
	defer d.Close()
	until := base.Add(time.Hour)
	if _, err := d.Set(until); err != nil {
		t.Fatal(err)
	}
	d.printf("inside")
	set(base.Add(10 * time.Hour)) // the lid closed: the wall clock moved on, the timer did not fire
	d.printf("after")
	if count("inside") != 1 || count("after") != 0 {
		t.Fatalf("lines: %q", lines)
	}
	// The end timer fires while the wall clock is short of until: the window stays open.
	set(base.Add(30 * time.Minute))
	d.end(until)
	if d.untilNs.Load() == 0 || count("debug off: the window ended") != 0 {
		t.Fatal("an early end closed the window")
	}
	d.printf("still")
	if count("still") != 1 {
		t.Fatalf("lines: %q", lines)
	}
	set(until.Add(time.Second))
	d.end(until)
	if d.untilNs.Load() != 0 || count("debug off: the window ended") != 1 {
		t.Fatalf("the window did not end: %q", lines)
	}
}

// status reads a window whose end has passed as off, and GFF_HAB_DEBUG only with no window.
func TestDebugWord(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		m    health.Marker
		want string
	}{
		{health.Marker{}, "off"},
		{health.Marker{Debug: true}, "on (GFF_HAB_DEBUG)"},
		{health.Marker{Debug: true, DebugUntil: now.Add(time.Hour)}, "on until "},
		{health.Marker{Debug: true, DebugUntil: now.Add(-time.Hour)}, "off"},
	} {
		got := debugWord(c.m, now)
		if !strings.HasPrefix(got, c.want) || (c.want == "off" && got != "off") {
			t.Errorf("%+v: %q, want %q", c.m, got, c.want)
		}
	}
}

// A window more than 24 hours ahead in debug.json is not one diag.debug wrote: it is removed
// and logged (4.5).
func TestDebugFileTooFar(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, runDir, debugFile)
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.Marshal(debugRecord{Until: time.Now().Add(48 * time.Hour)})
	os.WriteFile(path, b, 0o600)
	var lines []string
	d := openDebug(state, false, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }, time.Now, func(bool, time.Time) {})
	defer d.Close()
	if d.on.Load() || d.untilNs.Load() != 0 {
		t.Fatal("a window 48 h ahead turned debug on")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file stays: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "more than 24h0m0s ahead") {
		t.Fatalf("lines: %q", lines)
	}
}

// Stack dumps keep their own newest stacksKeep: many dumps never push out a crash trace (4.7).
func TestStackDumpsKeepApart(t *testing.T) {
	d, _ := testDiag(t)
	dir := filepath.Join(d.state, crashDir)
	old := time.Now().Add(-time.Hour)
	for i := range 3 {
		p := filepath.Join(dir, fmt.Sprintf("hab-%016d.txt", i))
		os.WriteFile(p, []byte("panic: x\n"), 0o600)
		os.Chtimes(p, old, old)
	}
	for range stacksKeep + 4 {
		if _, err := d.writeStacks(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct names and times
	}
	var crashes, dumps int
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		switch {
		case strings.Contains(e.Name(), stacksInfix):
			dumps++
		case strings.HasPrefix(e.Name(), "hab-"):
			crashes++
		}
	}
	if dumps != stacksKeep || crashes < 3 {
		t.Fatalf("%d dumps (want %d), %d crash files (want 3 and this run's)", dumps, stacksKeep, crashes)
	}
}
