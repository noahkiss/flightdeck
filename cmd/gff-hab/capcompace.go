package main

// Capcom as a consumer of the usage governor (capcom-service.md 10.1): its policy from the
// [capcom] pace_* keys, its readings from the engine's usage events under its own account key,
// the pace sent to the engine before each turn, and the "Pacing usage" note.

import (
	"encoding/json"
	"os"
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
	gov   *governor.Governor
	usage *habusage.Store
	// host and dir name Capcom's own usage account (app.md 14.1), one per engine kind: the key
	// usage.Key(host, <engine>, dir), so a Codex engine's plan windows are a Codex account.
	host, dir string
	config    func() capcom.ServiceConfig
	// configPath is the hab's config file: a pace_* edit rechecks once it held for one poll.
	// "": no watch.
	configPath string
	poll       time.Duration // the config file check; 0: 2 s
	link       *capcomengine.Link
	thread     *capcomthread.Store
	now        func() time.Time
	logf       func(string, ...any)

	sendMu sync.Mutex // one answer read and sent at a time
	mu     sync.Mutex
	on     bool
	ans    governor.Answer
	noted  time.Time // the wait the last note named; zero: none since the wait began
	drifts map[string]bool
	// The settings from the last config read: the governor asks for them at each recheck,
	// and the config file is read only at start, on a change and on a user message.
	pol   governor.Policy
	acct  string
	done  chan struct{} // closed by stop: ends the config watch
	stamp fileStamp     // the config file as last read
}

// fileStamp tells a changed file: its size and modification time.
type fileStamp struct {
	size int64
	mod  time.Time
	ok   bool
}

func stampFile(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime(), ok: true}
}

// refresh reads the config once: Capcom's policy, the pace_* keys and under an API key the
// daily dollar cap; and its account, its own under the OAuth token (an API key has no plan
// window). It reports whether either changed.
func (p *capcomPacer) refresh() bool {
	st := stampFile(p.configPath)
	cfg := p.config()
	pol := governor.Policy{DailyShare: cfg.Pace.DailyShare, EventStop: cfg.Pace.EventStop, SmallAt: cfg.Pace.SmallAt, WaitAt: cfg.Pace.WaitAt}
	acct := p.key(cfg.Engine)
	if cfg.Auth == capcom.AuthAPIKey {
		pol.DailyUSD = cfg.Limits.DailyUSD
		acct = ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stamp = st
	changed := pol != p.pol || acct != p.acct
	p.pol, p.acct = pol, acct
	return changed
}

// policy is Capcom's policy as of the last config read.
func (p *capcomPacer) policy() governor.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pol
}

// account is the account whose weekly window paces Capcom, as of the last config read.
func (p *capcomPacer) account() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acct
}

// watch rechecks once the config file changed and then held for one poll, so a pace_* edit
// applies at once, also during a wait, and a file in the middle of a write is not read.
func (p *capcomPacer) watch(done chan struct{}) {
	d := p.poll
	if d <= 0 {
		d = 2 * time.Second
	}
	t := time.NewTicker(d)
	defer t.Stop()
	var pending fileStamp
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		st := stampFile(p.configPath)
		p.mu.Lock()
		same := st == p.stamp
		p.mu.Unlock()
		switch {
		case same:
			pending = fileStamp{}
		case st != pending:
			pending = st
		default:
			pending = fileStamp{}
			if p.refresh() {
				p.gov.Recheck()
			}
		}
	}
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
	var done chan struct{}
	if p.configPath != "" {
		done = make(chan struct{})
		p.done = done
	}
	p.mu.Unlock()
	p.refresh()
	if done != nil {
		go p.watch(done)
	}
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
	if p.done != nil {
		close(p.done)
		p.done = nil
	}
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

// key is Capcom's usage account key for an engine kind; the Claude engine's for any other.
func (p *capcomPacer) key(engine string) string {
	if engine != capcom.EngineCodex {
		engine = capcom.EngineClaude
	}
	return habusage.Key(p.host, engine, p.dir)
}

// reading stores one usage event of the engine under Capcom's account for kind, the engine its
// connection's ready named, then rechecks: the Claude SDK's rate_limit_info, or the Codex
// rollout's token_count records.
func (p *capcomPacer) reading(kind string, raw json.RawMessage) {
	if p.usage == nil {
		return
	}
	var r habusage.Reading
	if kind == capcom.EngineCodex {
		r = habusage.CodexEngineReading(p.key(kind), raw)
		if len(r.Windows) == 0 && len(r.Drift) == 0 {
			return // no plan window: nothing to store
		}
	} else {
		r = habusage.SDKReading(p.key(kind), raw, p.now())
	}
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

// waiting rechecks when a user message arrives, with the config read again, so the message
// meets the pace of now. Then it adds the "Pacing usage until <time>" note while user turns
// wait: once per wait, not once per message. A send-now message gets none: it runs.
func (p *capcomPacer) waiting(it capcomthread.Item) {
	if it.Kind != capcomthread.KindMessage || it.Author.Kind != capcomthread.AuthorUser || !it.Read.IsZero() {
		return
	}
	p.mu.Lock()
	on := p.on
	p.mu.Unlock()
	if on && p.gov != nil {
		p.refresh()
		p.gov.Recheck()
	}
	if it.SendNow {
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
