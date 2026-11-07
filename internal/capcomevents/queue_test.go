package capcomevents

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// clock is a fake clock with one pending timer at a time, as the queue keeps.
type clock struct {
	now   time.Time
	fire  time.Time
	f     func()
	fires int
}

func (c *clock) Now() time.Time { return c.now }

func (c *clock) After(d time.Duration, f func()) func() bool {
	c.fire, c.f = c.now.Add(d), f
	return func() bool { c.f = nil; return true }
}

// advance moves the clock to t, firing each timer due on the way.
func (c *clock) advance(t time.Time) {
	for c.f != nil && !c.fire.After(t) {
		c.now = c.fire
		f := c.f
		c.f = nil
		c.fires++
		f()
	}
	c.now = t
}

type rig struct {
	t        *testing.T
	c        *clock
	q        *Queue
	path     string
	lim      Limits
	host     *time.Location
	notes    []string
	outcomes []string
	released int
	n        int
}

func newRig(t *testing.T, start time.Time) *rig {
	t.Helper()
	r := &rig{t: t, c: &clock{now: start}, path: filepath.Join(t.TempDir(), File), lim: DefaultLimits, host: start.Location()}
	r.open()
	return r
}

func (r *rig) open() {
	r.t.Helper()
	q, err := Open(Config{Path: r.path, Now: r.c.Now, After: r.c.After, Limits: func() Limits { return r.lim }, Host: r.host,
		Released: func() { r.released++ },
		Note:     func(s string) { r.notes = append(r.notes, s) },
		Outcome:  func(round, word string, at time.Time) { r.outcomes = append(r.outcomes, round+" "+word) },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.q = q
	q.Start()
}

func (r *rig) add(urgent bool, round string) string {
	r.t.Helper()
	r.n++
	id := fmt.Sprintf("c%016x", r.n)
	if err := r.q.Add(Event{Item: id, At: r.c.now, Urgent: urgent, Round: round}); err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *rig) batches() [][]string {
	var out [][]string
	for _, b := range r.q.Released() {
		out = append(out, b.Items)
	}
	return out
}

var ny, _ = time.LoadLocation("America/New_York")

func day(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, ny) }

func TestBatchWindow(t *testing.T) {
	r := newRig(t, day(12, 0))
	for i := range 10 {
		r.c.advance(day(12, 0).Add(time.Duration(i) * 12 * time.Second))
		r.add(false, "")
	}
	if len(r.batches()) != 0 {
		t.Fatalf("released before the window ended: %v", r.batches())
	}
	r.c.advance(day(12, 2))
	if b := r.batches(); len(b) != 1 || len(b[0]) != 10 {
		t.Fatalf("ten events in 2 minutes: %v", b)
	}
	if n, _ := r.q.Turns(); n != 1 {
		t.Fatalf("turns %d", n)
	}
}

func TestBatchMaxAndSettings(t *testing.T) {
	r := newRig(t, day(12, 0))
	r.lim.BatchMax, r.lim.BatchWindow = 3, 10*time.Second
	for range 4 {
		r.add(false, "")
	}
	if b := r.batches(); len(b) != 1 || len(b[0]) != 3 {
		t.Fatalf("a full batch goes at once: %v", b)
	}
	r.c.advance(day(12, 0).Add(9 * time.Second))
	if len(r.batches()) != 1 {
		t.Fatal("the fourth went before its window")
	}
	r.c.advance(day(12, 0).Add(10 * time.Second))
	if b := r.batches(); len(b) != 2 || len(b[1]) != 1 {
		t.Fatalf("the fourth after 10 s: %v", b)
	}
}

func TestQuietHours(t *testing.T) {
	r := newRig(t, day(23, 0))
	r.add(false, "")
	r.c.advance(day(23, 30))
	r.add(false, "")
	r.c.advance(day(23, 59))
	if len(r.batches()) != 0 {
		t.Fatal("an event ran in quiet hours")
	}
	// Urgent: at once, alone; the others still wait.
	urgent := r.add(true, "")
	if b := r.batches(); len(b) != 1 || !slices.Equal(b[0], []string{urgent}) {
		t.Fatalf("urgent in quiet hours: %v", b)
	}
	r.c.advance(day(23, 59).Add(7*time.Hour + time.Minute)) // 07:00 the next day
	if b := r.batches(); len(b) != 2 || len(b[1]) != 2 {
		t.Fatalf("one batch at the end of quiet hours: %v", b)
	}
	// Equal start and end: no quiet hours.
	r.lim.QuietStart, r.lim.QuietEnd = 0, 0
	if q, _ := Quiet(day(3, 0), ny, r.lim); q {
		t.Fatal("equal start and end are quiet")
	}
}

func TestDailyBudget(t *testing.T) {
	r := newRig(t, day(8, 0))
	r.lim.DailyTurns = 20
	for i := range 21 {
		r.c.advance(day(8, 0).Add(time.Duration(i) * 3 * time.Minute))
		r.add(false, "")
		r.c.advance(r.c.now.Add(2 * time.Minute))
	}
	if n := len(r.batches()); n != 20 {
		t.Fatalf("batches %d, want 20", n)
	}
	if len(r.q.Waiting()) != 1 || len(r.notes) != 1 || !strings.Contains(r.notes[0], "20 event batches") {
		t.Fatalf("the 21st: waiting %v notes %q", r.q.Waiting(), r.notes)
	}
	// A time-sensitive event waits too, outside quiet hours; one note a day.
	r.add(true, "")
	if len(r.batches()) != 20 || len(r.notes) != 1 {
		t.Fatalf("urgent past the budget: %d batches, notes %q", len(r.batches()), r.notes)
	}
	// Midnight: the next day's first batch; in quiet hours only the urgent one.
	r.c.advance(time.Date(2026, 11, 8, 0, 0, 30, 0, ny))
	if b := r.batches(); len(b) != 21 || len(b[20]) != 1 {
		t.Fatalf("at midnight: %v", b[20:])
	}
	r.c.advance(time.Date(2026, 11, 8, 7, 0, 0, 0, ny))
	if b := r.batches(); len(b) != 22 {
		t.Fatalf("at 07:00: %d batches", len(b))
	}
	// A lower budget moves the limit.
	r.lim.DailyTurns = 2
	r.add(false, "")
	r.c.advance(r.c.now.Add(3 * time.Minute))
	if len(r.batches()) != 22 || len(r.q.Waiting()) != 1 {
		t.Fatalf("budget 2: %d batches", len(r.batches()))
	}
}

func TestSavedTimeZone(t *testing.T) {
	// The host is UTC; a device shared New York. 03:30 UTC is 22:30 in New York: quiet there.
	r := newRig(t, time.Date(2026, 11, 8, 3, 30, 0, 0, time.UTC))
	r.host = time.UTC
	r.open()
	r.q.SetTZ("America/New_York")
	r.open() // a gff-hab restart
	if r.q.TZ().String() != "America/New_York" {
		t.Fatalf("tz after a restart %s", r.q.TZ())
	}
	r.add(false, "")
	r.c.advance(r.c.now.Add(10 * time.Minute))
	if len(r.batches()) != 0 {
		t.Fatal("ran in New York's quiet hours")
	}
	r.q.SetTZ("Not/AZone")
	if r.q.TZ().String() != "America/New_York" {
		t.Fatal("an unknown zone replaced the saved one")
	}
}

func TestOutcomes(t *testing.T) {
	r := newRig(t, day(12, 0))
	r.add(false, "m1")
	r.add(false, "m2")
	r.c.advance(day(12, 2))
	b := r.q.Released()
	if len(b) != 1 || !slices.Equal(b[0].Rounds, []string{"m1", "m2"}) {
		t.Fatalf("batch %+v", b)
	}
	if _, ok := r.q.Taken(b[0].ID, "t1"); !ok {
		t.Fatal("taken")
	}
	if _, ok := r.q.Taken(b[0].ID, "t1"); ok {
		t.Fatal("taken twice")
	}
	r.q.Wrote("t1")
	r.q.TurnEnded("t1", day(12, 3))
	if !slices.Equal(r.outcomes, []string{"m1 told", "m2 told"}) {
		t.Fatalf("outcomes %q", r.outcomes)
	}
	// A quiet turn.
	r.outcomes = nil
	r.add(false, "m3")
	r.c.advance(day(12, 6))
	b = r.q.Released()
	r.q.Taken(b[0].ID, "t2")
	r.q.TurnEnded("t2", day(12, 7))
	if !slices.Equal(r.outcomes, []string{"m3 quiet"}) {
		t.Fatalf("outcomes %q", r.outcomes)
	}
	// A turn that ended while gff-hab was down: settled at the next ready.
	r.outcomes = nil
	r.add(false, "m4")
	r.c.advance(day(12, 10))
	b = r.q.Released()
	r.q.Taken(b[0].ID, "t3")
	r.open()
	r.q.Running("")
	if !slices.Equal(r.outcomes, []string{"m4 quiet"}) {
		t.Fatalf("outcomes %q", r.outcomes)
	}
	// The mark moves: untaken ones are unseen, a taken one waits for its turn.
	r.outcomes = nil
	r.add(false, "m5")
	r.c.advance(day(12, 13))
	r.add(false, "m6")
	b = r.q.Released()
	r.q.Taken(b[0].ID, "t4")
	r.add(false, "m7")
	r.q.Drop()
	if !slices.Equal(r.outcomes, []string{"m6 unseen", "m7 unseen"}) {
		t.Fatalf("outcomes %q", r.outcomes)
	}
	r.q.TurnEnded("t4", day(12, 14))
	if !slices.Contains(r.outcomes, "m5 quiet") {
		t.Fatalf("outcomes %q", r.outcomes)
	}
}

func TestStoppedHoldsAll(t *testing.T) {
	r := newRig(t, day(12, 0))
	r.q.Stop()
	r.add(true, "")
	if len(r.batches()) != 0 {
		t.Fatal("released while stopped")
	}
	r.q.Start()
	if len(r.batches()) != 1 {
		t.Fatal("not released at start")
	}
}
