// Package tallies keeps gff-hab's local usage figures (docs/specs/diagnostics.md 7): counts of
// API calls and errors per method and client kind, events published, hook envelopes, features,
// settings and health, per local day. Counts only, kept on this machine, never sent anywhere.
//
// The key sets are closed. A Tallies is built from a Surface (the method table, the event
// types, the hook events, the declared features, the settings registry); a key outside it is
// counted under Other, never stored by name. A count is one atomic add into a table built at
// start: no lock, no allocation, no disk write. Flush moves the counts to the day file.
package tallies

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Other is the bucket of every key outside the surface.
const Other = "other"

// Feature is one entry of the declared feature list (7.2).
type Feature int

// The declared features, in the order of Features.
const (
	FeatureSend Feature = iota
	FeatureSessionCreate
	FeatureSessionRestart
	FeatureSessionResume
	FeatureRoundRun
	FeatureCapcomJob
	FeatureApprovalAnswer
	FeatureChannelSend
	FeatureChannelExpired
	FeatureFriction
	numFeatures
)

// Features is the one declared feature list: the word of each Feature (7.2).
var Features = [numFeatures]string{
	FeatureSend:           "send",
	FeatureSessionCreate:  "session.create",
	FeatureSessionRestart: "session.restart",
	FeatureSessionResume:  "session.resume",
	FeatureRoundRun:       "round.run",
	FeatureCapcomJob:      "capcom.job",
	FeatureApprovalAnswer: "approval.answer",
	FeatureChannelSend:    "channel.send",
	FeatureChannelExpired: "channel.expired",
	FeatureFriction:       "friction",
}

// methodFeatures are the features a method counts when it succeeds (7.2).
var methodFeatures = map[string]Feature{
	"session.send":    FeatureSend,
	"session.create":  FeatureSessionCreate,
	"session.restart": FeatureSessionRestart,
	"session.resume":  FeatureSessionResume,
	"approval.answer": FeatureApprovalAnswer,
	"diag.friction":   FeatureFriction,
}

// The health counts per process kind (7.1).
const (
	healthStart = iota
	healthCrash
	healthUnclean
	numHealth
)

// Health record kinds Health counts; they match internal/health.
const (
	KindStart   = "start"
	KindCrash   = "crash"
	KindUnclean = "unclean"
)

// Tallies holds the counters. A nil *Tallies counts nothing: tallies are off.
type Tallies struct {
	surface Surface

	methods  map[string]int
	clients  []string
	errKinds []string
	events   map[string]int
	harness  []string
	hookIdx  []map[string]int // per harness: event → index into hooks
	settings map[string]int
	procs    []string
	feature  []int // per method index: the feature it counts on success, or -1

	nm, nc, ne int // methods, clients, error kinds, each with its Other slot

	calls       []atomic.Uint64 // [method][client]
	errs        []atomic.Uint64 // [method][client][error kind]
	eventN      []atomic.Uint64
	hookN       []atomic.Uint64
	hookRefused []atomic.Uint64
	featureN    [numFeatures]atomic.Uint64
	settingSet  []atomic.Uint64
	health      []atomic.Uint64 // [proc][numHealth]

	dir      string
	now      func() time.Time
	inConfig func() []string
	logf     func(string, ...any)

	flushMu sync.Mutex
	pending *Day // counts taken from the counters and not yet in a file
	pruned  string
	stop    chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

// Config is what New needs.
type Config struct {
	Dir     string // <state>/tallies
	Surface Surface
	// InConfig returns the setting ids the hab's config or a flag locks now; read at each flush.
	InConfig func() []string
	Now      func() time.Time
	Logf     func(string, ...any)
}

func index(names []string) map[string]int {
	m := make(map[string]int, len(names))
	for i, n := range names {
		m[n] = i
	}
	return m
}

// New builds the counter table from the surface.
func New(cfg Config) *Tallies {
	s := cfg.Surface
	t := &Tallies{surface: s, dir: cfg.Dir, now: cfg.Now, inConfig: cfg.InConfig, logf: cfg.Logf,
		stop: make(chan struct{})}
	if t.now == nil {
		t.now = time.Now
	}
	if t.logf == nil {
		t.logf = func(string, ...any) {}
	}
	t.methods = index(s.Methods)
	t.clients = s.Clients
	t.errKinds = s.Errors
	t.events = index(s.Events)
	t.settings = index(s.Settings)
	t.procs = s.Procs
	hooks := 0
	for _, h := range s.Hooks {
		t.harness = append(t.harness, h.Harness)
		m := make(map[string]int, len(h.Events))
		for _, e := range h.Events {
			m[e] = hooks
			hooks++
		}
		t.hookIdx = append(t.hookIdx, m)
	}
	t.nm, t.nc, t.ne = len(s.Methods)+1, len(s.Clients)+1, len(s.Errors)+1
	t.feature = make([]int, t.nm)
	for i := range t.feature {
		t.feature[i] = -1
	}
	for name, f := range methodFeatures {
		if i, ok := t.methods[name]; ok {
			t.feature[i] = int(f)
		}
	}
	t.calls = make([]atomic.Uint64, t.nm*t.nc)
	t.errs = make([]atomic.Uint64, t.nm*t.nc*t.ne)
	t.eventN = make([]atomic.Uint64, len(s.Events)+1)
	t.hookN = make([]atomic.Uint64, hooks+1)
	t.hookRefused = make([]atomic.Uint64, hooks+1)
	t.settingSet = make([]atomic.Uint64, len(s.Settings)+1)
	t.health = make([]atomic.Uint64, (len(s.Procs)+1)*numHealth)
	return t
}

// slot returns the index of name in names, or len(names): the Other slot.
func slot(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return len(names)
}

func lookup(m map[string]int, name string) int {
	if i, ok := m[name]; ok {
		return i
	}
	return len(m)
}

// Call counts one reply: its method, the client kind, and the error kind when failed. A method
// outside the table counts under Other. A successful call of a feature's method counts it.
func (t *Tallies) Call(method, client, errKind string, failed bool) {
	if t == nil {
		return
	}
	m := lookup(t.methods, method)
	c := slot(t.clients, client)
	i := m*t.nc + c
	t.calls[i].Add(1)
	if failed {
		t.errs[i*t.ne+slot(t.errKinds, errKind)].Add(1)
		return
	}
	if f := t.feature[m]; f >= 0 {
		t.featureN[f].Add(1)
	}
}

// Event counts one event published to clients.
func (t *Tallies) Event(typ string) {
	if t == nil {
		return
	}
	t.eventN[lookup(t.events, typ)].Add(1)
}

// Hook counts one hook envelope by harness and native event; refused when the hook endpoint
// turned it away.
func (t *Tallies) Hook(harness, event string, refused bool) {
	if t == nil {
		return
	}
	i := len(t.hookN) - 1
	if h := slot(t.harness, harness); h < len(t.harness) {
		if j, ok := t.hookIdx[h][event]; ok {
			i = j
		}
	}
	if refused {
		t.hookRefused[i].Add(1)
	} else {
		t.hookN[i].Add(1)
	}
}

// Feature counts one use of a declared feature.
func (t *Tallies) Feature(f Feature) {
	if t == nil || f < 0 || f >= numFeatures {
		return
	}
	t.featureN[f].Add(1)
}

// SettingSet counts one row written by settings.set.
func (t *Tallies) SettingSet(id string) {
	if t == nil {
		return
	}
	t.settingSet[lookup(t.settings, id)].Add(1)
}

// Health counts a start, a crash or an unclean end of a process kind.
func (t *Tallies) Health(proc, kind string) {
	if t == nil {
		return
	}
	k := -1
	switch kind {
	case KindStart:
		k = healthStart
	case KindCrash:
		k = healthCrash
	case KindUnclean:
		k = healthUnclean
	}
	if k < 0 {
		return
	}
	t.health[slot(t.procs, proc)*numHealth+k].Add(1)
}

// take moves every counter to a Day and zeroes it. Each Swap keeps an add that races it: the
// add lands in this Day or in the next.
func (t *Tallies) take() *Day {
	d := &Day{}
	name := func(names []string, i int) string {
		if i < len(names) {
			return names[i]
		}
		return Other
	}
	for m := 0; m < t.nm; m++ {
		for c := 0; c < t.nc; c++ {
			i := m*t.nc + c
			n := t.calls[i].Swap(0)
			var errs map[string]uint64
			for e := 0; e < t.ne; e++ {
				if x := t.errs[i*t.ne+e].Swap(0); x > 0 {
					if errs == nil {
						errs = map[string]uint64{}
					}
					errs[name(t.errKinds, e)] += x
				}
			}
			if n == 0 && errs == nil {
				continue
			}
			d.addCall(name(t.surface.Methods, m), name(t.clients, c), Call{N: n, Errors: errs})
		}
	}
	for i := range t.eventN {
		if n := t.eventN[i].Swap(0); n > 0 {
			d.Events = add(d.Events, name(t.surface.Events, i), n)
		}
	}
	keys := t.surface.HookKeys()
	for i := range t.hookN {
		n, r := t.hookN[i].Swap(0), t.hookRefused[i].Swap(0)
		if n > 0 || r > 0 {
			d.addHook(name(keys, i), Hook{N: n, Refused: r})
		}
	}
	for f := range t.featureN {
		if n := t.featureN[f].Swap(0); n > 0 {
			d.Features = add(d.Features, Features[f], n)
		}
	}
	for i := range t.settingSet {
		if n := t.settingSet[i].Swap(0); n > 0 {
			d.addSetting(name(t.surface.Settings, i), Setting{Set: n})
		}
	}
	for p := 0; p <= len(t.procs); p++ {
		var h Health
		h.Starts = t.health[p*numHealth+healthStart].Swap(0)
		h.Crashes = t.health[p*numHealth+healthCrash].Swap(0)
		h.Unclean = t.health[p*numHealth+healthUnclean].Swap(0)
		if h != (Health{}) {
			d.addHealth(name(t.procs, p), h)
		}
	}
	return d
}

// inConfigIDs is InConfig kept to the settings registry.
func (t *Tallies) inConfigIDs() []string {
	if t.inConfig == nil {
		return nil
	}
	var out []string
	for _, id := range t.inConfig() {
		if _, ok := t.settings[id]; ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
