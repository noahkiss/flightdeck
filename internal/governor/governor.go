// Package governor is the usage governor (capcom-service.md 10.1, groups.md 10; task-k0ydtgae):
// it paces consumers of a plan's usage, the Capcom service and later each agent group, from the
// usage readings. Each consumer has a policy: a daily share of the plan's weekly window,
// cumulative, and three steps measured against that share. The governor answers, for each
// consumer, the pace of its next turn: ok, small (the small model) or wait until a time.
//
// The rule (built in stage G):
//
//   - Plan basis. The consumer's usage account has a weekly window (7d) with a start. The
//     allowance on day d of the window (0 on the first day) is share × (d + 1), at most 100%.
//     The use is the window's used percentage, the whole account's. The use as a percentage of
//     the allowance is the level.
//   - Dollar basis. A policy with a daily dollar cap paces by the result cost the consumer
//     reported today, by the hab's local day, against the cap.
//   - No basis. With neither, the consumer is unpaced: every answer is ok, and its tokens are
//     counted.
//
// User turns go to the small model at small_at and wait at wait_at; event turns wait at
// event_stop. A wait lasts until the first window day whose allowance takes the level under the
// step, else until the window resets; under the dollar cap, until the next local midnight.
//
// Pure Go; the clock and timers are injected, so tests run on a fake clock.
package governor

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Pace is the answer for a consumer's next turn.
type Pace string

// The paces (capcom-service.md 4.4).
const (
	Ok    Pace = "ok"
	Small Pace = "small"
	Wait  Pace = "wait"
)

// Basis is what a consumer is paced by.
type Basis string

// The bases.
const (
	BasisPlan Basis = "plan" // the plan's weekly window
	BasisUSD  Basis = "usd"  // result cost against a daily dollar cap
	BasisNone Basis = "none" // unpaced: tokens counted
)

// Window is the name of the window the governor paces by: the plan's weekly window.
const Window = "7d"

// Policy is one consumer's pacing settings. Shares and steps are percentages.
type Policy struct {
	DailyShare float64 // percent of the weekly window a day, cumulative
	EventStop  float64 // percent of the share: event turns wait
	SmallAt    float64 // percent of the share: user turns go to the small model
	WaitAt     float64 // percent of the share: user turns wait
	DailyUSD   float64 // above 0: pace by result cost against this cap a day, not by the window
}

// DefaultPolicy is Capcom's default (capcom-service.md 19.2).
var DefaultPolicy = Policy{DailyShare: 15, EventStop: 80, SmallAt: 100, WaitAt: 120}

// Consumer is one user of the plan's usage: capcom, or group:<id>.
type Consumer struct {
	ID string
	// Policy returns the consumer's settings now. Nil: DefaultPolicy.
	Policy func() Policy
	// Account returns the usage account key whose weekly window paces the consumer; "" when it
	// has none. Nil: none.
	Account func() string
	// OnChange hears each change of the consumer's answer, in order, with no lock held; it may
	// call back into the governor. Nil: none.
	OnChange func(Answer)
}

// Answer is the governor's answer for one consumer.
type Answer struct {
	Pace       Pace      // for a user turn
	EventPace  Pace      // for an event turn: ok or wait
	Until      time.Time // when Pace is wait: when it ends; zero otherwise
	EventUntil time.Time // when EventPace is wait: when it ends; zero otherwise
	Basis      Basis
	UsedPct    float64 // plan: the window's used percentage; usd: the spend as a percentage of the cap
	SharePct   float64 // plan: the allowance so far, a percentage of the window; usd: 100
	Level      float64 // the use as a percentage of the share
	SpentUSD   float64 // today's reported cost
	CapUSD     float64 // the daily dollar cap; 0: none
	InputTok   int64   // today's tokens
	OutputTok  int64
}

// same reports whether two answers pace alike: what a consumer acts on.
func (a Answer) same(b Answer) bool {
	return a.Pace == b.Pace && a.EventPace == b.EventPace && a.Until.Equal(b.Until) &&
		a.EventUntil.Equal(b.EventUntil) && a.Basis == b.Basis
}

// Config wires a governor.
type Config struct {
	// Account returns a usage account by key. Nil: no account has readings.
	Account func(key string) (api.UsageAccount, bool)
	// OnChange hears each consumer whose answer changed, after the consumer's own OnChange,
	// with no lock held. Nil: none.
	OnChange func(id string, a Answer)
	// StatePath keeps today's counters across a restart. "": in memory only.
	StatePath string
	Now       func() time.Time
	// After runs f after d; the returned stop cancels it. Nil: time.AfterFunc.
	After func(d time.Duration, f func()) (stop func() bool)
	// Location is the local day's zone for the dollar cap and the counters. Nil: time.Local.
	Location *time.Location
	Logf     func(string, ...any)
}

// Governor paces its consumers. It is safe for concurrent use.
type Governor struct {
	cfg Config

	mu        sync.Mutex
	consumers map[string]Consumer
	answers   map[string]Answer
	day       string // the counters' local day, 2006-01-02
	counts    map[string]*count
	stop      func() bool
	closed    bool
	// queue holds changes for OnChange in the order they were found; one caller at a time
	// delivers them, so OnChange hears them in order and may call back into the governor.
	queue      []change
	delivering bool
}

type change struct {
	id string
	a  Answer
}

type count struct {
	USD    float64 `json:"usd"`
	Input  int64   `json:"input_tokens"`
	Output int64   `json:"output_tokens"`
}

type stateFile struct {
	Day       string            `json:"day"`
	Consumers map[string]*count `json:"consumers"`
}

// New returns a governor with no consumers. It reads today's counters from StatePath.
func New(cfg Config) *Governor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Account == nil {
		cfg.Account = func(string) (api.UsageAccount, bool) { return api.UsageAccount{}, false }
	}
	g := &Governor{cfg: cfg, consumers: map[string]Consumer{}, answers: map[string]Answer{}, counts: map[string]*count{}}
	g.load()
	return g
}

func (g *Governor) load() {
	if g.cfg.StatePath == "" {
		return
	}
	b, err := os.ReadFile(g.cfg.StatePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			g.cfg.Logf("governor: read %s: %v", g.cfg.StatePath, err)
		}
		return
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		g.cfg.Logf("governor: %s is not readable; today's counters start at 0: %v", g.cfg.StatePath, err)
		return
	}
	if f.Day != g.today() {
		return
	}
	g.day = f.Day
	for id, c := range f.Consumers {
		if c != nil && validCount(*c) {
			g.counts[id] = c
		}
	}
}

func validCount(c count) bool {
	return c.USD >= 0 && !math.IsInf(c.USD, 0) && !math.IsNaN(c.USD) && c.Input >= 0 && c.Output >= 0
}

// saveLocked writes today's counters. The caller holds g.mu.
func (g *Governor) saveLocked() {
	if g.cfg.StatePath == "" {
		return
	}
	b, _ := json.Marshal(stateFile{Day: g.day, Consumers: g.counts})
	dir := filepath.Dir(g.cfg.StatePath)
	tmp, err := os.CreateTemp(dir, ".governor-*")
	if err == nil {
		_, err = tmp.Write(b)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), g.cfg.StatePath)
		}
		if err != nil {
			os.Remove(tmp.Name())
		}
	}
	if err != nil {
		g.cfg.Logf("governor: save %s: %v", g.cfg.StatePath, err)
	}
}

func (g *Governor) today() string { return g.cfg.Now().In(g.cfg.Location).Format(time.DateOnly) }

// rollLocked starts new counters on a new local day. The caller holds g.mu.
func (g *Governor) rollLocked() {
	if d := g.today(); d != g.day {
		g.day = d
		g.counts = map[string]*count{}
	}
}

// Register adds a consumer, or replaces one of the same id, and returns its answer. OnChange
// does not hear the first answer.
func (g *Governor) Register(c Consumer) Answer {
	g.mu.Lock()
	g.consumers[c.ID] = c
	g.mu.Unlock()
	g.Recheck()
	a, _ := g.Check(c.ID)
	return a
}

// Unregister removes a consumer. Its counters stay for the rest of the local day, so a
// consumer registered again the same day starts from what it spent.
func (g *Governor) Unregister(id string) {
	g.mu.Lock()
	delete(g.consumers, id)
	delete(g.answers, id)
	g.mu.Unlock()
	g.Recheck()
}

// Check returns a consumer's answer as of the last recheck.
func (g *Governor) Check(id string) (Answer, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.answers[id]
	return a, ok
}

// Spent records what one turn of a consumer used: its tokens and its reported cost. It
// rechecks, since a dollar cap may now be reached.
func (g *Governor) Spent(id string, input, output int64, usd float64) {
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	if !(usd >= 0) || math.IsInf(usd, 0) {
		usd = 0
	}
	g.mu.Lock()
	g.rollLocked()
	c := g.counts[id]
	if c == nil {
		c = &count{}
		g.counts[id] = c
	}
	c.USD += usd
	c.Input += input
	c.Output += output
	g.saveLocked()
	g.mu.Unlock()
	g.Recheck()
}

// Recheck computes every answer again: call it when a reading or a policy changed. Each
// changed answer goes to OnChange; the next recheck is set for the next time an answer can
// change on its own (a window day, a reset, a local midnight).
func (g *Governor) Recheck() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	now := g.cfg.Now()
	g.rollLocked()
	next := g.nextMidnight(now)
	ids := make([]string, 0, len(g.consumers))
	for id := range g.consumers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, at := g.answer(g.consumers[id], now)
		if !at.IsZero() && at.Before(next) {
			next = at
		}
		old, had := g.answers[id]
		g.answers[id] = a
		if had && !old.same(a) {
			g.queue = append(g.queue, change{id, a})
		}
	}
	if g.stop != nil {
		g.stop()
	}
	d := next.Sub(now)
	if d < time.Second {
		d = time.Second
	}
	g.stop = g.cfg.After(d, g.Recheck)
	g.deliverLocked()
}

// deliverLocked hands the queued changes to OnChange, in order, with no lock held while it
// runs; a caller that finds another delivering leaves its changes to it. The caller holds g.mu;
// it is released on return.
func (g *Governor) deliverLocked() {
	if g.delivering || len(g.queue) == 0 {
		g.mu.Unlock()
		return
	}
	g.delivering = true
	for len(g.queue) > 0 {
		c := g.queue[0]
		g.queue = g.queue[1:]
		own := g.consumers[c.id].OnChange
		g.mu.Unlock()
		if own != nil {
			own(c.a)
		}
		if g.cfg.OnChange != nil {
			g.cfg.OnChange(c.id, c.a)
		}
		g.mu.Lock()
	}
	g.delivering = false
	g.mu.Unlock()
}

func (g *Governor) nextMidnight(now time.Time) time.Time {
	l := now.In(g.cfg.Location)
	y, m, d := l.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, g.cfg.Location)
}

// answer computes one consumer's answer, and the time it can next change on its own (zero:
// only at a new reading). The caller holds g.mu.
func (g *Governor) answer(c Consumer, now time.Time) (Answer, time.Time) {
	p := DefaultPolicy
	if c.Policy != nil {
		p = c.Policy()
	}
	a := Answer{Pace: Ok, EventPace: Ok, Basis: BasisNone}
	if n := g.counts[c.ID]; n != nil {
		a.SpentUSD, a.InputTok, a.OutputTok = n.USD, n.Input, n.Output
	}
	if p.DailyUSD > 0 {
		a.Basis, a.CapUSD = BasisUSD, p.DailyUSD
		a.UsedPct = a.SpentUSD / p.DailyUSD * 100
		a.SharePct = 100
		a.Level = a.UsedPct
		mid := g.nextMidnight(now)
		a.Pace, a.EventPace = steps(a.Level, p)
		if a.Pace == Wait {
			a.Until = mid
		}
		if a.EventPace == Wait {
			a.EventUntil = mid
		}
		return a, time.Time{} // midnight is always a recheck
	}
	key := ""
	if c.Account != nil {
		key = c.Account()
	}
	if key == "" {
		return a, time.Time{}
	}
	acct, ok := g.cfg.Account(key)
	if !ok {
		return a, time.Time{}
	}
	i := slices.IndexFunc(acct.Windows, func(w api.UsageWindow) bool { return w.Name == Window })
	if i < 0 {
		return a, time.Time{}
	}
	w := acct.Windows[i]
	start, err1 := time.Parse(time.RFC3339, w.StartsAt)
	reset, err2 := time.Parse(time.RFC3339, w.ResetsAt)
	length := time.Duration(w.LengthS) * time.Second
	if err1 != nil || err2 != nil || length < 24*time.Hour || !reset.After(start) {
		return a, time.Time{}
	}
	used := float64(w.UsedPct)
	// A window that reset with no reading since starts again at 0.
	for !now.Before(reset) {
		start, reset, used = reset, reset.Add(length), 0
	}
	days := int(length / (24 * time.Hour))
	day := int(now.Sub(start) / (24 * time.Hour))
	day = max(0, min(day, days-1))
	allow := func(d int) float64 { return math.Min(100, p.DailyShare*float64(d+1)) }
	a.Basis = BasisPlan
	a.UsedPct = used
	a.SharePct = allow(day)
	a.Level = used / a.SharePct * 100
	a.Pace, a.EventPace = steps(a.Level, p)
	// until is the start of the first later window day whose allowance takes the level under
	// step, else the reset.
	until := func(step float64) time.Time {
		for d := day + 1; d < days; d++ {
			if used/allow(d)*100 < step {
				return start.Add(time.Duration(d) * 24 * time.Hour)
			}
		}
		return reset
	}
	if a.Pace == Wait {
		a.Until = until(p.WaitAt)
	}
	if a.EventPace == Wait {
		a.EventUntil = until(p.EventStop)
	}
	next := reset
	if day+1 < days {
		next = start.Add(time.Duration(day+1) * 24 * time.Hour)
	}
	return a, next
}

// steps turns a level (percent of the share) into the user and event paces.
func steps(level float64, p Policy) (user, event Pace) {
	user, event = Ok, Ok
	switch {
	case level >= p.WaitAt:
		user = Wait
	case level >= p.SmallAt:
		user = Small
	}
	if level >= p.EventStop {
		event = Wait
	}
	return user, event
}

// Section is the governor's catalog section: every consumer's answer, by id. Nil with no
// consumer.
func (g *Governor) Section() *api.CatalogGovernor {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.answers) == 0 {
		return nil
	}
	s := &api.CatalogGovernor{Consumers: []api.GovernorConsumer{}}
	for id, a := range g.answers {
		s.Consumers = append(s.Consumers, a.Catalog(id))
	}
	sort.Slice(s.Consumers, func(i, j int) bool { return s.Consumers[i].ID < s.Consumers[j].ID })
	return s
}

// Catalog is the answer as one consumer of the catalog section.
func (a Answer) Catalog(id string) api.GovernorConsumer {
	c := api.GovernorConsumer{ID: id, Basis: string(a.Basis), Pace: string(a.Pace), EventPace: string(a.EventPace),
		InputTokens: a.InputTok, OutputTokens: a.OutputTok}
	if !a.Until.IsZero() {
		c.Until = a.Until.UTC().Format(time.RFC3339)
	}
	if !a.EventUntil.IsZero() {
		c.EventUntil = a.EventUntil.UTC().Format(time.RFC3339)
	}
	switch a.Basis {
	case BasisPlan:
		c.UsedPct, c.SharePct = int(math.Floor(a.UsedPct)), int(math.Floor(a.SharePct))
	case BasisUSD:
		c.SpentCents, c.CapCents = int64(math.Round(a.SpentUSD*100)), int64(math.Round(a.CapUSD*100))
	}
	return c
}

// Close stops the recheck timer.
func (g *Governor) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.stop != nil {
		g.stop()
		g.stop = nil
	}
}
