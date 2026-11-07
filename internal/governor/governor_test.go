package governor

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// clock is a fake clock with one pending timer, as the governor keeps one.
type clock struct {
	mu  sync.Mutex
	now time.Time
	at  time.Time
	f   func()
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) After(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.f = c.now.Add(d), f
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.f = nil
		return true
	}
}

// advance moves the clock and fires the timer when it is due.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var f func()
	if c.f != nil && !c.now.Before(c.at) {
		f, c.f = c.f, nil
	}
	c.mu.Unlock()
	if f != nil {
		f()
	}
}

// accounts are hand-written usage readings by key.
type accounts struct {
	mu sync.Mutex
	m  map[string]api.UsageAccount
}

func (a *accounts) get(k string) (api.UsageAccount, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.m[k]
	return v, ok
}

// week sets account k's weekly window: started at start, used pct.
func (a *accounts) week(k string, start time.Time, pct int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]api.UsageAccount{}
	}
	a.m[k] = api.UsageAccount{Key: k, Harness: "claude", Windows: []api.UsageWindow{
		{Name: "5h", UsedPct: 90, ResetsAt: start.Add(5 * time.Hour).Format(time.RFC3339)},
		{Name: Window, UsedPct: pct, ResetsAt: start.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			StartsAt: start.Format(time.RFC3339), LengthS: 7 * 24 * 3600},
	}}
}

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) // the window's start

type heard struct {
	mu sync.Mutex
	l  []string
}

func (h *heard) on(id string, a Answer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.l = append(h.l, id+" "+string(a.Pace)+"/"+string(a.EventPace))
}

func (h *heard) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.l
	h.l = nil
	return l
}

func setup(t *testing.T, state string) (*Governor, *clock, *accounts, *heard) {
	t.Helper()
	c := &clock{now: t0.Add(2 * time.Hour)}
	acc := &accounts{}
	h := &heard{}
	g := New(Config{Account: acc.get, OnChange: h.on, Now: c.Now, After: c.After, Location: time.UTC, StatePath: state})
	t.Cleanup(g.Close)
	return g, c, acc, h
}

func capcom(p Policy) Consumer {
	return Consumer{ID: "capcom", Policy: func() Policy { return p }, Account: func() string { return "acct" }}
}

func check(t *testing.T, g *Governor, id string, pace, event Pace) Answer {
	t.Helper()
	a, ok := g.Check(id)
	if !ok {
		t.Fatalf("%s: no answer", id)
	}
	if a.Pace != pace || a.EventPace != event {
		t.Fatalf("%s: pace %s/%s, want %s/%s (%+v)", id, a.Pace, a.EventPace, pace, event, a)
	}
	return a
}

func TestShareAccumulates(t *testing.T) {
	g, c, acc, h := setup(t, "")
	acc.week("acct", t0, 13)
	g.Register(capcom(DefaultPolicy))
	if got := h.take(); len(got) != 1 {
		t.Fatalf("Config.OnChange heard %v for a new consumer, want its first answer", got)
	}
	// Day 1: the share is 15%; 13% is 86.7% of it, past event_stop.
	a := check(t, g, "capcom", Ok, Wait)
	if a.Basis != BasisPlan || a.SharePct != 15 || a.UsedPct != 13 {
		t.Fatalf("answer %+v", a)
	}
	if !a.EventUntil.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("event until %v, want the second day", a.EventUntil)
	}
	// The second window day: the share is 30%, and event turns run again.
	c.advance(22 * time.Hour)
	a = check(t, g, "capcom", Ok, Ok)
	if a.SharePct != 30 {
		t.Fatalf("share %v on day 2, want 30", a.SharePct)
	}
	if got := h.take(); len(got) != 1 || got[0] != "capcom ok/ok" {
		t.Fatalf("heard %v", got)
	}
	// The share stops at 100%.
	c.advance(6 * 24 * time.Hour)
	acc.week("acct", t0.Add(7*24*time.Hour), 0) // a new window, read
	c.advance(5 * 24 * time.Hour)
	g.Recheck()
	if a, _ := g.Check("capcom"); a.SharePct != 90 {
		t.Fatalf("share %v on day 6 of a window, want 90", a.SharePct)
	}
	c.advance(24 * time.Hour)
	if a, _ := g.Check("capcom"); a.SharePct != 100 {
		t.Fatalf("share %v on day 7, want at most 100", a.SharePct)
	}
}

func TestStepsAndUntil(t *testing.T) {
	g, _, acc, h := setup(t, "")
	acc.week("acct", t0, 15)
	g.Register(capcom(DefaultPolicy))
	h.take()
	check(t, g, "capcom", Small, Wait) // 100% of the share: the small model
	acc.week("acct", t0, 18)
	g.Recheck()
	a := check(t, g, "capcom", Wait, Wait) // 120%: wait
	if !a.Until.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("until %v, want the second day", a.Until)
	}
	// 40% on day 1 waits two days: 40/30 is still past 120%, 40/45 is not.
	acc.week("acct", t0, 40)
	g.Recheck()
	a = check(t, g, "capcom", Wait, Wait)
	if !a.Until.Equal(t0.Add(48*time.Hour)) || !a.EventUntil.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("until %v, event until %v", a.Until, a.EventUntil)
	}
	// A window used up: user turns wait until 100% is under 120% of the share (day 6, 90%),
	// event turns until the reset, as 100% never falls under 80% of a share of at most 100%.
	acc.week("acct", t0, 100)
	g.Recheck()
	a = check(t, g, "capcom", Wait, Wait)
	if !a.Until.Equal(t0.Add(5*24*time.Hour)) || !a.EventUntil.Equal(t0.Add(7*24*time.Hour)) {
		t.Fatalf("until %v, event until %v, want day 6 and the reset", a.Until, a.EventUntil)
	}
	if got := h.take(); len(got) != 3 {
		t.Fatalf("heard %v, want 3 changes (an unchanged answer is not heard)", got)
	}
}

func TestWaitEndsOnItsOwn(t *testing.T) {
	g, c, acc, h := setup(t, "")
	acc.week("acct", t0, 18)
	g.Register(capcom(DefaultPolicy))
	h.take()
	a := check(t, g, "capcom", Wait, Wait)
	c.advance(a.Until.Sub(c.Now()))
	check(t, g, "capcom", Ok, Ok)
	if got := h.take(); len(got) != 1 {
		t.Fatalf("heard %v", got)
	}
}

func TestResetWithNoReading(t *testing.T) {
	g, c, acc, _ := setup(t, "")
	acc.week("acct", t0, 100)
	g.Register(capcom(DefaultPolicy))
	check(t, g, "capcom", Wait, Wait)
	c.advance(7 * 24 * time.Hour) // past the reset, no reading since
	a := check(t, g, "capcom", Ok, Ok)
	if a.UsedPct != 0 || a.SharePct != 15 {
		t.Fatalf("after the reset: %+v", a)
	}
}

func TestPolicySteps(t *testing.T) {
	g, _, acc, _ := setup(t, "")
	p := Policy{DailyShare: 10, EventStop: 50, SmallAt: 60, WaitAt: 70}
	g.Register(capcom(p))
	for _, c := range []struct {
		used        int
		pace, event Pace
	}{{4, Ok, Ok}, {5, Ok, Wait}, {6, Small, Wait}, {7, Wait, Wait}} {
		acc.week("acct", t0, c.used)
		g.Recheck()
		check(t, g, "capcom", c.pace, c.event)
	}
}

func TestUnpaced(t *testing.T) {
	g, _, acc, _ := setup(t, "")
	g.Register(capcom(DefaultPolicy))
	a := check(t, g, "capcom", Ok, Ok)
	if a.Basis != BasisNone {
		t.Fatalf("basis %s with no reading", a.Basis)
	}
	g.Spent("capcom", 1200, 300, 0)
	g.Spent("capcom", 100, 50, 0)
	a = check(t, g, "capcom", Ok, Ok)
	if a.InputTok != 1300 || a.OutputTok != 350 {
		t.Fatalf("tokens %d/%d", a.InputTok, a.OutputTok)
	}
	// A reading with no weekly window paces nothing either.
	acc.mu.Lock()
	acc.m = map[string]api.UsageAccount{"acct": {Key: "acct", Windows: []api.UsageWindow{{Name: "5h", UsedPct: 99}}}}
	acc.mu.Unlock()
	g.Recheck()
	if a := check(t, g, "capcom", Ok, Ok); a.Basis != BasisNone {
		t.Fatalf("basis %s with only a 5h window", a.Basis)
	}
	s := g.Section()
	if s == nil || len(s.Consumers) != 1 || s.Consumers[0].Basis != "none" || s.Consumers[0].InputTokens != 1300 {
		t.Fatalf("section %+v", s)
	}
}

func TestDailyUSD(t *testing.T) {
	g, c, _, h := setup(t, "")
	p := DefaultPolicy
	p.DailyUSD = 1
	g.Register(capcom(p))
	check(t, g, "capcom", Ok, Ok)
	g.Spent("capcom", 10, 10, 0.85)
	check(t, g, "capcom", Ok, Wait)
	g.Spent("capcom", 10, 10, 0.15)
	check(t, g, "capcom", Small, Wait)
	g.Spent("capcom", 10, 10, 0.25)
	a := check(t, g, "capcom", Wait, Wait)
	if want := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC); !a.Until.Equal(want) {
		t.Fatalf("until %v, want the next local midnight", a.Until)
	}
	if cs := a.Catalog("capcom"); cs.SpentCents != 125 || cs.CapCents != 100 || cs.Basis != "usd" {
		t.Fatalf("catalog %+v", cs)
	}
	h.take()
	c.advance(a.Until.Sub(c.Now()))
	a = check(t, g, "capcom", Ok, Ok)
	if a.SpentUSD != 0 || a.InputTok != 0 {
		t.Fatalf("a new day kept the counters: %+v", a)
	}
	if got := h.take(); len(got) != 1 {
		t.Fatalf("heard %v", got)
	}
}

func TestCountersAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governor.json")
	g, c, _, _ := setup(t, path)
	p := DefaultPolicy
	p.DailyUSD = 1
	g.Register(capcom(p))
	g.Spent("capcom", 5, 6, 1.1)
	g.Close()

	g2 := New(Config{Now: c.Now, After: c.After, Location: time.UTC, StatePath: path})
	defer g2.Close()
	g2.Register(capcom(p))
	if a := check(t, g2, "capcom", Small, Wait); a.SpentUSD != 1.1 || a.InputTok != 5 {
		t.Fatalf("after a restart: %+v", a)
	}
	// The next day starts at 0, also after a restart.
	g2.Close()
	c.advance(24 * time.Hour)
	g3 := New(Config{Now: c.Now, After: c.After, Location: time.UTC, StatePath: path})
	defer g3.Close()
	g3.Register(capcom(p))
	check(t, g3, "capcom", Ok, Ok)
}

// A group registers as its own consumer, waits at its cap and resumes when the window moves
// (groups.md 10): the interface Part 15 builds on.
func TestGroupWaitsAndResumes(t *testing.T) {
	g, _, acc, h := setup(t, "")
	acc.week("acct", t0, 5)
	acc.week("member", t0, 50)
	g.Register(capcom(DefaultPolicy))
	grp := Consumer{ID: "group:g1", Policy: func() Policy { return Policy{DailyShare: 20, EventStop: 100, SmallAt: 100, WaitAt: 100} },
		Account: func() string { return "member" }}
	if a := g.Register(grp); a.Pace != Wait || a.Until.IsZero() {
		t.Fatalf("group answer %+v", a)
	}
	check(t, g, "capcom", Ok, Ok) // one consumer's cap is not another's
	h.take()
	acc.week("member", t0.Add(time.Hour), 3) // the window moved: a new reading
	g.Recheck()
	check(t, g, "group:g1", Ok, Ok)
	if got := h.take(); len(got) != 1 || got[0] != "group:g1 ok/ok" {
		t.Fatalf("heard %v", got)
	}
	g.Unregister("group:g1")
	if _, ok := g.Check("group:g1"); ok {
		t.Fatal("an unregistered consumer still answers")
	}
	if got := h.take(); len(got) != 1 || got[0] != "group:g1 /" {
		t.Fatalf("heard %v at the removal", got)
	}
}

// OnChange may call back into the governor, and hears the changes in order.
func TestOnChangeReentrant(t *testing.T) {
	c := &clock{now: t0}
	acc := &accounts{}
	acc.week("acct", t0, 18)
	var g *Governor
	var got []Pace
	g = New(Config{Account: acc.get, Now: c.Now, After: c.After, Location: time.UTC, OnChange: func(id string, a Answer) {
		got = append(got, a.Pace)
		g.Spent(id, 1, 1, 0) // rechecks inside the callback
	}})
	defer g.Close()
	g.Register(capcom(DefaultPolicy))
	acc.week("acct", t0, 0)
	g.Recheck()
	acc.week("acct", t0, 15)
	g.Recheck()
	if len(got) != 3 || got[0] != Wait || got[1] != Ok || got[2] != Small {
		t.Fatalf("heard %v", got)
	}
}
