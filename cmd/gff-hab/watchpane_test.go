package main

// The watch-pane wrapper's queue (rounds.md 4.4, 4.7): the rate bucket, the buffer kept while
// gff-hab does not answer, the 64 KiB line ceiling, the paced drop line and safe pane text.

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/rounds"
)

func testWatchPane(rate, burst, lines, size int) (*watchPane, *bytes.Buffer, *time.Time) {
	out := &bytes.Buffer{}
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	w := &watchPane{round: "round-x", out: out, maxLines: lines, maxBytes: size,
		rate: float64(rate), burst: float64(burst), now: func() time.Time { return now }}
	w.cond = sync.NewCond(&w.mu)
	w.tokens, w.filled = w.burst, w.now()
	return w, out, &now
}

func TestWatchPaneRate(t *testing.T) {
	w, out, now := testWatchPane(60, 5, 100, 1<<20)
	for i := 0; i < 6; i++ {
		w.add(`{"title":"e"}`)
	}
	if len(w.q) != 5 || w.dropped != 1 {
		t.Fatalf("queue %d, dropped %d: want 5 and 1", len(w.q), w.dropped)
	}
	if !strings.Contains(out.String(), "dropped 1 lines; the last: over the rate") {
		t.Fatalf("pane %q", out.String())
	}
	// A second refills one token at 60 a minute; a second drop within watchDropEvery is counted
	// but its line waits for the pace.
	*now = now.Add(time.Second)
	w.add(`{"title":"f"}`)
	w.add(`{"title":"g"}`)
	if len(w.q) != 6 || w.dropped != 2 || strings.Count(out.String(), "dropped") != 1 {
		t.Fatalf("queue %d, dropped %d, pane %q", len(w.q), w.dropped, out.String())
	}
	w.flushNote(true)
	if !strings.Contains(out.String(), "dropped 1 lines; the last: over the rate\nwatch-pane: dropped 1 lines") {
		t.Fatalf("pane %q", out.String())
	}
	// Blank lines are no events and take no token.
	w.add("   ")
	if len(w.q) != 6 {
		t.Fatal("a blank line was queued")
	}
}

// While gff-hab does not answer the queue holds buffer_lines and buffer_bytes; the oldest lines
// go first.
func TestWatchPaneBuffer(t *testing.T) {
	w, _, _ := testWatchPane(6000, 6000, 3, 1<<20)
	for _, l := range []string{"1", "2", "3", "4", "5"} {
		w.add(l)
	}
	if strings.Join(w.q, ",") != "3,4,5" || w.dropped != 2 || w.qBytes != 3 {
		t.Fatalf("queue %v, dropped %d, bytes %d", w.q, w.dropped, w.qBytes)
	}
	w, _, _ = testWatchPane(6000, 6000, 100, 10)
	for _, l := range []string{"aaaa", "bbbb", "cccc"} {
		w.add(l)
	}
	if strings.Join(w.q, ",") != "bbbb,cccc" || w.dropped != 1 {
		t.Fatalf("by bytes: queue %v, dropped %d", w.q, w.dropped)
	}
}

// A line over the 64 KiB ceiling is dropped whole, up to its newline; the next line is read.
func TestWatchPaneLongLine(t *testing.T) {
	w, out, _ := testWatchPane(6000, 6000, 100, 1<<20)
	in := strings.Repeat("x", rounds.MaxWatchLine+10) + "\n" + `{"title":"next"}` + "\r\n" + `{"title":"last, no newline"}`
	w.read(strings.NewReader(in))
	if len(w.q) != 2 || w.q[0] != `{"title":"next"}` || w.q[1] != `{"title":"last, no newline"}` || w.dropped != 1 || !w.ended {
		t.Fatalf("queue %q, dropped %d, ended %v", w.q, w.dropped, w.ended)
	}
	if !strings.Contains(out.String(), "the last: line over 64 KiB") {
		t.Fatalf("pane %q", out.String())
	}
}

func TestPaneSafe(t *testing.T) {
	if got := paneSafe("Re: \x1b]52;c;aGk=\x07 bill\x9b2J ok é"); got != "Re: �]52;c;aGk=� bill�2J ok é" {
		t.Fatalf("%q", got)
	}
}

// The page being sent is never trimmed by the buffer bound, so its answer removes exactly it.
func TestWatchPaneInflight(t *testing.T) {
	w, _, _ := testWatchPane(6000, 6000, 2, 1<<20)
	w.add("a")
	w.add("b")
	w.inflight = 1 // "a" is in the page being sent
	w.add("c")
	if strings.Join(w.q, ",") != "a,c" || w.dropped != 1 {
		t.Fatalf("queue %v, dropped %d", w.q, w.dropped)
	}
	w.q, w.inflight = w.q[1:], 0 // the page's answer
	if strings.Join(w.q, ",") != "c" {
		t.Fatalf("after the answer %v", w.q)
	}
}

// Cursor lines take no rate token.
func TestWatchPaneCursorRate(t *testing.T) {
	w, _, _ := testWatchPane(60, 2, 100, 1<<20)
	for _, l := range []string{`{"title":"1"}`, `{"cursor":"c1"}`, `{"title":"2"}`, `{"cursor":"c2"}`, `{"title":"3"}`} {
		w.add(l)
	}
	if len(w.q) != 4 || w.dropped != 1 {
		t.Fatalf("queue %v, dropped %d: want the two events and two cursors", w.q, w.dropped)
	}
	if watchCursorLine(`{"title":"says \"cursor\" in it"}`) || !watchCursorLine(`{ "cursor" : "x" }`) {
		t.Fatal("watchCursorLine")
	}
}
