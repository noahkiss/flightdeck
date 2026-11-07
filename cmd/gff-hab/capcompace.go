package main

// Capcom as a consumer of the usage governor (capcom-service.md 10.1): its policy from the
// [capcom] pace_* keys, its readings from the engine's usage events under its own account key,
// the pace sent to the engine before each turn, and the "Pacing usage" note.

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/governor"
	habusage "github.com/noahkiss/flightdeck/internal/usage"
)

// capcomConsumer is Capcom's consumer id; each group is group:<id> (groups.md 10).
const capcomConsumer = "capcom"

// capcomPacer paces one Capcom engine.
type capcomPacer struct {
	gov    *governor.Governor
	usage  *habusage.Store
	key    string // Capcom's own usage account key (app.md 14.1)
	config func() capcom.ServiceConfig
	link   *capcomengine.Link
	thread *capcomthread.Store
	now    func() time.Time
	logf   func(string, ...any)

	sendMu sync.Mutex // one answer read and sent at a time
	mu     sync.Mutex
	on     bool
	ans    governor.Answer
	noted  time.Time // the wait the last note named; zero: none since the wait began
	drifts map[string]bool
}

// policy is Capcom's policy now: the pace_* keys, and under an API key the daily dollar cap.
func (p *capcomPacer) policy() governor.Policy {
	cfg := p.config()
	pol := governor.Policy{DailyShare: cfg.Pace.DailyShare, EventStop: cfg.Pace.EventStop, SmallAt: cfg.Pace.SmallAt, WaitAt: cfg.Pace.WaitAt}
	if cfg.Auth == capcom.AuthAPIKey {
		pol.DailyUSD = cfg.Limits.DailyUSD
	}
	return pol
}

// account is the account whose weekly window paces Capcom: its own, under the OAuth token. An
// API key has no plan window.
func (p *capcomPacer) account() string {
	if p.config().Auth == capcom.AuthAPIKey {
		return ""
	}
	return p.key
}

// start registers Capcom with the governor: this hab holds the mark.
func (p *capcomPacer) start() {
	if p.gov == nil {
		return
	}
	p.mu.Lock()
	if p.on {
		p.mu.Unlock()
		return
	}
	p.on = true
	p.mu.Unlock()
	a := p.gov.Register(governor.Consumer{ID: capcomConsumer, Policy: p.policy, Account: p.account, OnChange: p.changed})
	p.changed(a)
}

// stop takes Capcom off the governor: the mark moved.
func (p *capcomPacer) stop() {
	if p.gov == nil {
		return
	}
	p.mu.Lock()
	on := p.on
	p.on = false
	p.mu.Unlock()
	if on {
		p.gov.Unregister(capcomConsumer)
	}
}

// changed sends the governor's answer to the engine. It reads the answer again, so a call that
// lost a race with a newer change still sends the newest.
func (p *capcomPacer) changed(governor.Answer) {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	a, ok := p.gov.Check(capcomConsumer)
	p.mu.Lock()
	if !p.on || !ok {
		p.mu.Unlock()
		return
	}
	if a.Pace != governor.Wait || !a.Until.Equal(p.ans.Until) {
		p.noted = time.Time{}
	}
	p.ans = a
	p.mu.Unlock()
	p.link.SetPace(string(a.Pace), paceTime(a.Until), string(a.EventPace), paceTime(a.EventUntil))
}

func paceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// reading stores one usage event of the engine under Capcom's account, then rechecks.
func (p *capcomPacer) reading(raw json.RawMessage) {
	if p.usage == nil {
		return
	}
	r := habusage.SDKReading(p.key, raw, p.now())
	if len(r.Drift) > 0 {
		line := strings.Join(r.Drift, "; ")
		p.mu.Lock()
		if p.drifts == nil || len(p.drifts) > 100 {
			p.drifts = map[string]bool{}
		}
		first := !p.drifts[line]
		p.drifts[line] = true
		p.mu.Unlock()
		if first {
			p.logf("capcom: usage reading: %s", line)
		}
	}
	p.usage.Put(r)
	if p.gov != nil {
		p.gov.Recheck()
	}
}

// turn records one turn's tokens and cost.
func (p *capcomPacer) turn(origin string, input, output int64, usd float64) {
	if p.gov != nil {
		p.gov.Spent(capcomConsumer, input, output, usd)
	}
}

// waiting adds the "Pacing usage until <time>" note when a user message arrives while user
// turns wait: once per wait, not once per message. A send-now message gets none: it runs.
func (p *capcomPacer) waiting(it capcomthread.Item) {
	if it.Kind != capcomthread.KindMessage || it.Author.Kind != capcomthread.AuthorUser || it.SendNow || !it.Read.IsZero() {
		return
	}
	p.mu.Lock()
	a := p.ans
	note := p.on && a.Pace == governor.Wait && !a.Until.IsZero() && !p.noted.Equal(a.Until)
	if note {
		p.noted = a.Until
	}
	p.mu.Unlock()
	if !note {
		return
	}
	loc := time.Local
	if it.TZ != "" {
		if l, err := time.LoadLocation(it.TZ); err == nil {
			loc = l
		}
	}
	if _, _, err := p.thread.Add(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Text: "Pacing usage until " + untilText(a.Until.In(loc), p.now().In(loc))}); err != nil {
		p.logf("capcom: the pacing note: %v", err)
	}
}

// untilText names a time: the clock time today, else the weekday and clock time within a week,
// else the date.
func untilText(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04")
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("2 Jan 15:04")
}
