package heavy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// clock is a fake clock the tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// disk is a fake volume reader: one free figure for every volume.
type disk struct {
	mu   sync.Mutex
	free uint64
}

func (d *disk) set(b uint64) {
	d.mu.Lock()
	d.free = b
	d.mu.Unlock()
}

func (d *disk) read(string) (uint64, uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.free, 500 * GiB, true
}

// rec records notices and messages.
type rec struct {
	mu       sync.Mutex
	notices  []string
	messages []string
	changed  int
}

func (r *rec) notice(title, body string) {
	r.mu.Lock()
	r.notices = append(r.notices, title+": "+body)
	r.mu.Unlock()
}

func (r *rec) message(session, text string) error {
	r.mu.Lock()
	r.messages = append(r.messages, session+": "+text)
	r.mu.Unlock()
	return nil
}

func (r *rec) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notices), len(r.messages)
}

func newLock(t *testing.T, c *clock, cfg Config) *Lock {
	t.Helper()
	cfg.Now = c.Now
	cfg.Every = -1
	if cfg.Free == nil {
		d := &disk{free: 400 * GiB}
		cfg.Free = d.read
	}
	if cfg.Volumes == nil {
		cfg.Volumes = []string{"/state", "/worktrees"}
	}
	l := New(cfg)
	t.Cleanup(l.Close)
	return l
}

func take(t *testing.T, l *Lock, owner context.Context, p api.HeavyTakeParams) api.HeavyTaken {
	t.Helper()
	res, err := l.Take(owner, p)
	if err != nil {
		t.Fatalf("Take(%+v): %v", p, err)
	}
	return res
}

func holders(l *Lock) []string {
	var out []string
	for _, h := range l.Status().Holders {
		out = append(out, h.Command)
	}
	return out
}

func waiters(l *Lock) []string {
	var out []string
	for _, h := range l.Status().Waiters {
		out = append(out, h.Command)
	}
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// TestArrivalOrder: three runs take the lock first come, first served; each waiter knows how
// many are ahead of it; a release grants the next.
func TestArrivalOrder(t *testing.T) {
	c := newClock()
	l := newLock(t, c, Config{})
	a := take(t, l, nil, api.HeavyTakeParams{Command: "a"})
	c.add(time.Millisecond)
	b := take(t, l, nil, api.HeavyTakeParams{Command: "b"})
	c.add(time.Millisecond)
	cc := take(t, l, nil, api.HeavyTakeParams{Command: "c"})
	if a.State != "holding" || b.State != "waiting" || b.Ahead != 1 || cc.State != "waiting" || cc.Ahead != 2 {
		t.Fatalf("a %+v, b %+v, c %+v", a, b, cc)
	}
	if !eq(holders(l), []string{"a"}) || !eq(waiters(l), []string{"b", "c"}) {
		t.Fatalf("holders %v waiters %v", holders(l), waiters(l))
	}
	l.Release(api.HeavyReleaseParams{Ticket: a.Ticket})
	if !eq(holders(l), []string{"b"}) || !eq(waiters(l), []string{"c"}) {
		t.Fatalf("after a: holders %v waiters %v", holders(l), waiters(l))
	}
	l.Release(api.HeavyReleaseParams{Ticket: b.Ticket})
	if !eq(holders(l), []string{"c"}) {
		t.Fatalf("after b: holders %v", holders(l))
	}
	// An unknown ticket is no error.
	if err := l.Release(api.HeavyReleaseParams{Ticket: a.Ticket}); err != nil {
		t.Fatal(err)
	}
}

// TestSlots: heavy.slots 2 lets two runs hold at once; a raised value grants at the next check.
func TestSlots(t *testing.T) {
	c := newClock()
	slots := 1
	var mu sync.Mutex
	l := newLock(t, c, Config{Slots: func() int { mu.Lock(); defer mu.Unlock(); return slots }})
	take(t, l, nil, api.HeavyTakeParams{Command: "a"})
	take(t, l, nil, api.HeavyTakeParams{Command: "b"})
	take(t, l, nil, api.HeavyTakeParams{Command: "c"})
	mu.Lock()
	slots = 2
	mu.Unlock()
	l.Check()
	if !eq(holders(l), []string{"a", "b"}) || !eq(waiters(l), []string{"c"}) || l.Status().Slots != 2 {
		t.Fatalf("holders %v waiters %v", holders(l), waiters(l))
	}
}

// TestOwnerDrop: when the connection that took a ticket ends, the ticket goes and the next
// waiter is granted. A ticket another connection took over stays when the first one ends.
func TestOwnerDrop(t *testing.T) {
	c := newClock()
	l := newLock(t, c, Config{})
	conn1, end1 := context.WithCancel(context.Background())
	a := take(t, l, conn1, api.HeavyTakeParams{Command: "a"})
	take(t, l, nil, api.HeavyTakeParams{Command: "b"})
	end1()
	waitFor(t, "b holds", func() bool { return eq(holders(l), []string{"b"}) })
	if _, err := l.Wait(context.Background(), a.Ticket); !isKind(err, api.ErrorKindNotFound) {
		t.Fatalf("Wait on a dropped ticket: %v", err)
	}

	conn2, end2 := context.WithCancel(context.Background())
	conn3, end3 := context.WithCancel(context.Background())
	defer end3()
	d := take(t, l, conn2, api.HeavyTakeParams{Command: "d"})
	take(t, l, conn3, api.HeavyTakeParams{Ticket: d.Ticket})
	end2()
	time.Sleep(20 * time.Millisecond)
	if !eq(waiters(l), []string{"d"}) {
		t.Fatalf("a taken-over ticket went with its old connection: waiters %v", waiters(l))
	}
	end3()
	waitFor(t, "d goes with its new connection", func() bool { return len(waiters(l)) == 0 })
}

// TestPane: a pane token names the pane and its session; the pane's close releases its runs;
// an unknown token is not_found.
func TestPane(t *testing.T) {
	c := newClock()
	l := newLock(t, c, Config{PaneOf: func(tok string) (string, string, bool) {
		if tok == "tok1" {
			return "pane-1", "builder", true
		}
		return "", "", false
	}})
	take(t, l, nil, api.HeavyTakeParams{Command: "a", PaneToken: "tok1"})
	take(t, l, nil, api.HeavyTakeParams{Command: "b"})
	if h := l.Status().Holders[0]; h.Pane != "pane-1" || h.Session != "builder" {
		t.Fatalf("holder %+v", h)
	}
	if _, err := l.Take(nil, api.HeavyTakeParams{PaneToken: "nope"}); !isKind(err, api.ErrorKindNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	l.ReleasePane("pane-1")
	if !eq(holders(l), []string{"b"}) {
		t.Fatalf("after the pane closed: holders %v", holders(l))
	}
}

// TestRefusals: a ticket the hab never gives and a wait past the limit are invalid.
func TestRefusals(t *testing.T) {
	l := newLock(t, newClock(), Config{})
	if _, err := l.Take(nil, api.HeavyTakeParams{Ticket: "../x"}); !isKind(err, api.ErrorKindInvalid) {
		t.Fatalf("bad ticket: %v", err)
	}
	if _, err := l.Take(nil, api.HeavyTakeParams{WaitMS: 600001}); !isKind(err, api.ErrorKindInvalid) {
		t.Fatalf("long wait: %v", err)
	}
	long := strings.Repeat("é", 150)
	r := take(t, l, nil, api.HeavyTakeParams{Command: long})
	if got := l.Status().Holders[0].Command; len(got) > maxCommand || !strings.HasPrefix(long, got) {
		t.Fatalf("command kept as %d bytes", len(got))
	}
	_ = r
}

// TestWait: Wait returns when the ticket is granted, or with its waiting state when its context
// ends.
func TestWait(t *testing.T) {
	c := newClock()
	l := newLock(t, c, Config{})
	a := take(t, l, nil, api.HeavyTakeParams{Command: "a"})
	b := take(t, l, nil, api.HeavyTakeParams{Command: "b"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	res, err := l.Wait(ctx, b.Ticket)
	if err != nil || res.State != "waiting" || res.Ahead != 1 {
		t.Fatalf("Wait past its context: %+v %v", res, err)
	}
	got := make(chan api.HeavyTaken, 1)
	go func() {
		r, _ := l.Wait(context.Background(), b.Ticket)
		got <- r
	}()
	time.Sleep(10 * time.Millisecond)
	l.Release(api.HeavyReleaseParams{Ticket: a.Ticket})
	select {
	case r := <-got:
		if r.State != "holding" || r.Granted == "" {
			t.Fatalf("Wait answered %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not end at the grant")
	}
}

// TestRestartKeepsPlace: after a restart the lock is empty; runs come back with their tickets.
// During the restart hold nothing is granted, a holder holds again at once, waiters take their
// old places before a run that asked after the restart, and the hold's end grants the first.
func TestRestartKeepsPlace(t *testing.T) {
	c := newClock()
	old := newLock(t, c, Config{})
	a := take(t, old, nil, api.HeavyTakeParams{Command: "a"})
	c.add(time.Second)
	b := take(t, old, nil, api.HeavyTakeParams{Command: "b"})
	c.add(time.Second)
	cc := take(t, old, nil, api.HeavyTakeParams{Command: "c"})
	old.Close()

	c.add(time.Minute)
	l := newLock(t, c, Config{RestartHold: 10 * time.Second})
	d := take(t, l, nil, api.HeavyTakeParams{Command: "d"})
	if d.State != "waiting" || !strings.Contains(d.Reason, "started") {
		t.Fatalf("a new run during the hold: %+v", d)
	}
	cBack := take(t, l, nil, api.HeavyTakeParams{Ticket: cc.Ticket, Since: cc.Since, Command: "c"})
	aBack := take(t, l, nil, api.HeavyTakeParams{Ticket: a.Ticket, Holding: true, Since: a.Since, Granted: a.Granted, Command: "a"})
	bBack := take(t, l, nil, api.HeavyTakeParams{Ticket: b.Ticket, Since: b.Since, Command: "b"})
	if aBack.State != "holding" || aBack.Granted != a.Granted || aBack.Ticket != a.Ticket {
		t.Fatalf("the holder back: %+v (was %+v)", aBack, a)
	}
	if bBack.Ahead != 1 || cBack.State != "waiting" {
		t.Fatalf("b back %+v, c back %+v", bBack, cBack)
	}
	if !eq(holders(l), []string{"a"}) || !eq(waiters(l), []string{"b", "c", "d"}) {
		t.Fatalf("holders %v waiters %v", holders(l), waiters(l))
	}
	if l.Status().HoldUntil == "" {
		t.Fatal("status lacks the hold")
	}
	l.Release(api.HeavyReleaseParams{Ticket: a.Ticket})
	if len(holders(l)) != 0 {
		t.Fatalf("a grant during the hold: %v", holders(l))
	}
	c.add(10 * time.Second)
	l.Check()
	if !eq(holders(l), []string{"b"}) || !eq(waiters(l), []string{"c", "d"}) || l.Status().HoldUntil != "" {
		t.Fatalf("after the hold: holders %v waiters %v", holders(l), waiters(l))
	}
}

// TestRestartHoldZero: [heavy] restart_hold 0 grants at once after a start.
func TestRestartHoldZero(t *testing.T) {
	l := newLock(t, newClock(), Config{RestartHold: 0})
	if r := take(t, l, nil, api.HeavyTakeParams{Command: "a"}); r.State != "holding" {
		t.Fatalf("with no hold: %+v", r)
	}
}

// TestLongHold: a hold past heavy.long_hold raises one notice, and no more for that hold; with
// the setting at 5 minutes, at 5 minutes. A later hold gets its own notice.
func TestLongHold(t *testing.T) {
	for _, limit := range []time.Duration{0, 5 * time.Minute} {
		c := newClock()
		r := &rec{}
		cfg := Config{Notice: r.notice, Host: "box"}
		want := DefaultLongHold
		if limit > 0 {
			cfg.LongHold = func() time.Duration { return limit }
			want = limit
		}
		l := newLock(t, c, cfg)
		a := take(t, l, nil, api.HeavyTakeParams{Command: "make test"})
		c.add(want - time.Second)
		l.Check()
		if n, _ := r.counts(); n != 0 {
			t.Fatalf("limit %v: a notice before it: %v", want, r.notices)
		}
		c.add(time.Second)
		l.Check()
		c.add(3 * want)
		l.Check()
		if n, _ := r.counts(); n != 1 || !strings.Contains(r.notices[0], "make test") || !strings.Contains(r.notices[0], "box") {
			t.Fatalf("limit %v: notices %v", want, r.notices)
		}
		l.Release(api.HeavyReleaseParams{Ticket: a.Ticket})
		take(t, l, nil, api.HeavyTakeParams{Command: "again"})
		c.add(want)
		l.Check()
		if n, _ := r.counts(); n != 2 {
			t.Fatalf("limit %v: a second hold's notice: %v", want, r.notices)
		}
	}
}

// TestDiskWatch: below heavy.disk_min_free a run waits with the reason while the holder goes on;
// the episode sends each coordinator exactly one message and raises no notice; free space
// between the threshold and the margin keeps the hold; above it grants again; a second episode
// sends a second message. With no coordinator the episode raises one notice.
func TestDiskWatch(t *testing.T) {
	c := newClock()
	d := &disk{free: 50 * GiB}
	r := &rec{}
	coords := []string{"lead"}
	l := newLock(t, c, Config{Free: d.read, ResumeMargin: DefaultResumeMargin, Notice: r.notice, Message: r.message,
		Coordinators: func() []string { return coords }, Host: "box"})
	a := take(t, l, nil, api.HeavyTakeParams{Command: "a"})
	d.set(9 * GiB)
	c.add(DefaultDiskEvery)
	l.Check()
	b := take(t, l, nil, api.HeavyTakeParams{Command: "b"})
	if b.State != "waiting" || !strings.Contains(b.Reason, "disk low") {
		t.Fatalf("b during low disk: %+v", b)
	}
	if !eq(holders(l), []string{"a"}) || !l.Status().DiskLow || !l.Section().DiskLow {
		t.Fatalf("holders %v status %+v", holders(l), l.Status())
	}
	l.Release(api.HeavyReleaseParams{Ticket: a.Ticket})
	for range 3 {
		c.add(DefaultDiskEvery)
		l.Check()
	}
	if len(holders(l)) != 0 {
		t.Fatalf("a grant during low disk: %v", holders(l))
	}
	if n, m := r.counts(); n != 0 || m != 1 || !strings.HasPrefix(r.messages[0], "lead: Disk low on box") {
		t.Fatalf("notices %v messages %v", r.notices, r.messages)
	}
	d.set(11 * GiB) // above the threshold, below threshold + margin
	c.add(DefaultDiskEvery)
	l.Check()
	if len(holders(l)) != 0 {
		t.Fatal("granted inside the margin")
	}
	d.set(12 * GiB)
	c.add(DefaultDiskEvery)
	l.Check()
	if !eq(holders(l), []string{"b"}) || l.Status().DiskLow {
		t.Fatalf("after recovery: holders %v", holders(l))
	}
	d.set(1 * GiB)
	c.add(DefaultDiskEvery)
	l.Check()
	if _, m := r.counts(); m != 2 {
		t.Fatalf("a second episode: messages %v", r.messages)
	}
	coords = nil
	d.set(20 * GiB)
	c.add(DefaultDiskEvery)
	l.Check()
	d.set(1 * GiB)
	c.add(DefaultDiskEvery)
	l.Check()
	if n, m := r.counts(); n != 1 || m != 2 {
		t.Fatalf("no coordinator: notices %v messages %v", r.notices, r.messages)
	}
}

// TestDiskMarginZero: [heavy] disk_resume_margin 0 resumes grants at the threshold.
func TestDiskMarginZero(t *testing.T) {
	c := newClock()
	d := &disk{free: 5 * GiB}
	l := newLock(t, c, Config{Free: d.read, ResumeMargin: 0})
	if r := take(t, l, nil, api.HeavyTakeParams{Command: "a"}); r.State != "waiting" {
		t.Fatalf("low at start: %+v", r)
	}
	d.set(DefaultMinFree)
	c.add(DefaultDiskEvery)
	l.Check()
	if !eq(holders(l), []string{"a"}) {
		t.Fatalf("at the threshold: holders %v", holders(l))
	}
}

// TestDiskReadEvery: the volumes are read once per DiskEvery, not at every check.
func TestDiskReadEvery(t *testing.T) {
	c := newClock()
	reads := 0
	l := newLock(t, c, Config{Volumes: []string{"/v"}, Free: func(string) (uint64, uint64, bool) {
		reads++
		return 100 * GiB, 200 * GiB, true
	}})
	start := reads
	l.Check()
	c.add(time.Second)
	l.Check()
	if reads != start {
		t.Fatalf("read %d times before it was due", reads-start)
	}
	c.add(DefaultDiskEvery)
	l.Check()
	if reads != start+1 {
		t.Fatalf("read %d times when due", reads-start)
	}
}

// TestSectionChanges: the catalog section changes with the holders, the waiters and the low
// state, and with free space only when it moved by 1 GiB or more.
func TestSectionChanges(t *testing.T) {
	c := newClock()
	d := &disk{free: 100 * GiB}
	r := &rec{}
	var mu sync.Mutex
	l := newLock(t, c, Config{Free: d.read, Changed: func() { mu.Lock(); r.changed++; mu.Unlock() }})
	count := func() int { mu.Lock(); defer mu.Unlock(); return r.changed }
	base := count()
	a := take(t, l, nil, api.HeavyTakeParams{Command: "a"})
	if count() != base+1 {
		t.Fatalf("a take: %d changes", count()-base)
	}
	base = count()
	d.set(100*GiB - 512<<20)
	c.add(DefaultDiskEvery)
	l.Check()
	if count() != base {
		t.Fatal("half a GiB changed the section")
	}
	if v := l.Section().Volumes[0]; v.FreeBytes != 100*GiB {
		t.Fatalf("published %d", v.FreeBytes)
	}
	d.set(98 * GiB)
	c.add(DefaultDiskEvery)
	l.Check()
	if count() != base+1 || l.Section().Volumes[0].FreeBytes != 98*GiB {
		t.Fatalf("two GiB: %d changes, %+v", count()-base, l.Section())
	}
	if l.Status().Volumes[0].FreeBytes != 98*GiB {
		t.Fatal("status lacks the reading")
	}
	l.Release(api.HeavyReleaseParams{Ticket: a.Ticket})
	if s := l.Section(); s.Holding != 0 || s.Slots != 1 {
		t.Fatalf("section %+v", s)
	}
}

func isKind(err error, k api.ErrorKind) bool {
	var ce *control.Error
	return errors.As(err, &ce) && ce.Data.Kind == k
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
