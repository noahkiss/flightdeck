package tallies

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Storage (7.3).
const (
	DirName    = "tallies"   // under gff-hab's state directory
	SeenFile   = "seen.json" // first and last day per name, and since
	KeepDays   = 90          // day files older than this are deleted
	FlushEvery = 5 * time.Minute
	dayLayout  = "2006-01-02"
)

// Day is one day file: <yyyy-mm-dd>.json. Keys are names from the surface or Other.
type Day struct {
	Day      string                      `json:"day"`
	Calls    map[string]map[string]*Call `json:"calls,omitempty"` // method → client kind
	Events   map[string]uint64           `json:"events,omitempty"`
	Hooks    map[string]*Hook            `json:"hooks,omitempty"` // harness/event
	Features map[string]uint64           `json:"features,omitempty"`
	Settings map[string]*Setting         `json:"settings,omitempty"`
	Health   map[string]*Health          `json:"health,omitempty"` // process kind
}

// Call is the calls of one method by one client kind: all of them, and the failed ones per
// error kind.
type Call struct {
	N      uint64            `json:"n"`
	Errors map[string]uint64 `json:"errors,omitempty"`
}

// Hook is the envelopes of one hook event: taken, and refused by the hook endpoint.
type Hook struct {
	N       uint64 `json:"n"`
	Refused uint64 `json:"refused,omitempty"`
}

// Setting is one setting's day: rows written by settings.set, and whether the hab's config or
// a flag locked it at a flush.
type Setting struct {
	Set      uint64 `json:"set,omitempty"`
	InConfig bool   `json:"in_config,omitempty"`
}

// Health is one process kind's starts, crashes and unclean ends.
type Health struct {
	Starts  uint64 `json:"starts,omitempty"`
	Crashes uint64 `json:"crashes,omitempty"`
	Unclean uint64 `json:"unclean,omitempty"`
}

func add(m map[string]uint64, k string, n uint64) map[string]uint64 {
	if m == nil {
		m = map[string]uint64{}
	}
	m[k] += n
	return m
}

func (d *Day) addCall(method, client string, c Call) {
	if d.Calls == nil {
		d.Calls = map[string]map[string]*Call{}
	}
	if d.Calls[method] == nil {
		d.Calls[method] = map[string]*Call{}
	}
	x := d.Calls[method][client]
	if x == nil {
		x = &Call{}
		d.Calls[method][client] = x
	}
	x.N += c.N
	for k, n := range c.Errors {
		x.Errors = add(x.Errors, k, n)
	}
}

func (d *Day) addHook(key string, h Hook) {
	if d.Hooks == nil {
		d.Hooks = map[string]*Hook{}
	}
	x := d.Hooks[key]
	if x == nil {
		x = &Hook{}
		d.Hooks[key] = x
	}
	x.N += h.N
	x.Refused += h.Refused
}

func (d *Day) addSetting(id string, s Setting) {
	if d.Settings == nil {
		d.Settings = map[string]*Setting{}
	}
	x := d.Settings[id]
	if x == nil {
		x = &Setting{}
		d.Settings[id] = x
	}
	x.Set += s.Set
	x.InConfig = x.InConfig || s.InConfig
}

func (d *Day) addHealth(proc string, h Health) {
	if d.Health == nil {
		d.Health = map[string]*Health{}
	}
	x := d.Health[proc]
	if x == nil {
		x = &Health{}
		d.Health[proc] = x
	}
	x.Starts += h.Starts
	x.Crashes += h.Crashes
	x.Unclean += h.Unclean
}

// Merge adds o's counts to d.
func (d *Day) Merge(o *Day) {
	if o == nil {
		return
	}
	for m, cs := range o.Calls {
		for c, x := range cs {
			d.addCall(m, c, *x)
		}
	}
	for k, n := range o.Events {
		d.Events = add(d.Events, k, n)
	}
	for k, h := range o.Hooks {
		d.addHook(k, *h)
	}
	for k, n := range o.Features {
		d.Features = add(d.Features, k, n)
	}
	for k, s := range o.Settings {
		d.addSetting(k, *s)
	}
	for k, h := range o.Health {
		d.addHealth(k, *h)
	}
}

func (d *Day) empty() bool {
	return len(d.Calls) == 0 && len(d.Events) == 0 && len(d.Hooks) == 0 && len(d.Features) == 0 &&
		len(d.Settings) == 0 && len(d.Health) == 0
}

// Span is when a name was first and last used, and how often in all.
type Span struct {
	First string `json:"first"`
	Last  string `json:"last"`
	Total uint64 `json:"total"`
}

// Seen is seen.json: per method, event, hook event, feature and setting, its span; and the day
// tallying started. It outlives the day files.
type Seen struct {
	Since    string           `json:"since"`
	Methods  map[string]*Span `json:"methods,omitempty"`
	Events   map[string]*Span `json:"events,omitempty"`
	Hooks    map[string]*Span `json:"hooks,omitempty"`
	Features map[string]*Span `json:"features,omitempty"`
	Settings map[string]*Span `json:"settings,omitempty"`
}

func see(m map[string]*Span, name, day string, n uint64) map[string]*Span {
	if name == Other || n == 0 {
		return m
	}
	if m == nil {
		m = map[string]*Span{}
	}
	s := m[name]
	if s == nil {
		s = &Span{First: day}
		m[name] = s
	}
	if day < s.First {
		s.First = day
	}
	if day > s.Last {
		s.Last = day
	}
	s.Total += n
	return m
}

// note records the names d used on day.
func (s *Seen) note(day string, d *Day) {
	if s.Since == "" || day < s.Since {
		s.Since = day
	}
	for m, cs := range d.Calls {
		var n uint64
		for _, c := range cs {
			n += c.N
		}
		s.Methods = see(s.Methods, m, day, n)
	}
	for k, n := range d.Events {
		s.Events = see(s.Events, k, day, n)
	}
	for k, h := range d.Hooks {
		s.Hooks = see(s.Hooks, k, day, h.N+h.Refused)
	}
	for k, n := range d.Features {
		s.Features = see(s.Features, k, day, n)
	}
	for k, x := range d.Settings {
		n := x.Set
		if n == 0 && x.InConfig {
			n = 1 // locked by config: in use, counted once per flush that saw it
		}
		s.Settings = see(s.Settings, k, day, n)
	}
}

// Start flushes every FlushEvery until Close.
func (t *Tallies) Start() {
	if t == nil {
		return
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		tk := time.NewTicker(FlushEvery)
		defer tk.Stop()
		failing := false
		for {
			select {
			case <-tk.C:
				err := t.Flush()
				if err != nil && !failing {
					t.logf("tallies: %v; retrying every %v", err, FlushEvery)
				} else if err == nil && failing {
					t.logf("tallies: written again")
				}
				failing = err != nil
			case <-t.stop:
				return
			}
		}
	}()
}

// Close stops the timer and flushes, at a clean exit.
func (t *Tallies) Close() {
	if t == nil {
		return
	}
	t.flushMu.Lock()
	if t.stopped {
		t.flushMu.Unlock()
		return
	}
	t.stopped = true
	t.flushMu.Unlock()
	close(t.stop)
	t.wg.Wait()
	if err := t.Flush(); err != nil {
		t.logf("tallies: %v", err)
	}
}

// Flush moves the counts into today's day file and seen.json. With no count it writes nothing:
// an idle hab never wakes the disk. Counts a failed write held stay for the next flush.
func (t *Tallies) Flush() error {
	if t == nil {
		return nil
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	now := t.now()
	day := now.Format(dayLayout)
	d := t.take()
	if t.pending != nil {
		t.pending.Merge(d)
		d, t.pending = t.pending, nil
	}
	if !d.empty() {
		for _, id := range t.inConfigIDs() {
			d.addSetting(id, Setting{InConfig: true})
		}
		if err := t.write(day, d); err != nil {
			t.pending = d
			return err
		}
	}
	if t.pruned != day {
		t.pruned = day
		Prune(t.dir, now)
	}
	return nil
}

// write adds d to the day file and to seen.json. Once the day file holds d, a failure of
// seen.json leaves d out of it rather than count d twice.
func (t *Tallies) write(day string, d *Day) error {
	path := filepath.Join(t.dir, day+".json")
	cur, err := readDay(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.logf("tallies: %s: %v; starting the day again", filepath.Base(path), err)
	}
	if cur == nil {
		cur = &Day{}
	}
	cur.Day = day
	cur.Merge(d)
	if err := writeJSON(path, cur); err != nil {
		return err
	}
	seen, err := ReadSeen(t.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.logf("tallies: %s: %v; starting it again", SeenFile, err)
	}
	seen.note(day, d)
	if err := writeJSON(filepath.Join(t.dir, SeenFile), &seen); err != nil {
		t.logf("tallies: %s: %v", SeenFile, err)
	}
	return nil
}

func readDay(path string) (*Day, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Day
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ReadSeen reads seen.json. A missing file is an empty Seen and fs.ErrNotExist.
func ReadSeen(dir string) (Seen, error) {
	var s Seen
	b, err := os.ReadFile(filepath.Join(dir, SeenFile))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return Seen{}, err
	}
	return s, nil
}

// writeJSON writes v to path in one rename, from a temporary file of its own, so a reader in
// another process never sees half a file.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// dayOf returns the day of a day file's name.
func dayOf(name string) (time.Time, bool) {
	s, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(dayLayout, s, time.Local)
	return t, err == nil
}

// Prune deletes the day files older than KeepDays. seen.json stays.
func Prune(dir string, now time.Time) {
	es, _ := os.ReadDir(dir)
	cut := startOfDay(now).AddDate(0, 0, -KeepDays)
	for _, e := range es {
		if t, ok := dayOf(e.Name()); ok && t.Before(cut) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// Window is the sum of the day files of the last days, today included.
type Window struct {
	From, To string // first and last day of the window
	Files    int    // day files found in it
	Sum      Day
}

// ReadWindow sums the day files of the days days up to now.
func ReadWindow(dir string, now time.Time, days int) (Window, error) {
	if days < 1 {
		return Window{}, fmt.Errorf("days must be 1 or more")
	}
	to := startOfDay(now)
	from := to.AddDate(0, 0, -(days - 1))
	w := Window{From: from.Format(dayLayout), To: to.Format(dayLayout)}
	es, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return w, err
	}
	for _, e := range es {
		t, ok := dayOf(e.Name())
		if !ok || t.Before(from) || t.After(to) {
			continue
		}
		d, err := readDay(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // a file another tool broke; the rest still count
		}
		w.Files++
		w.Sum.Merge(d)
	}
	w.Sum.Day = ""
	return w, nil
}

// Unused is one name of the surface with no use in the window: Last is its last day of use, or
// "" for never since Seen.Since.
type Unused struct {
	Kind string `json:"kind"` // method, event, hook, feature, setting
	Name string `json:"name"`
	Last string `json:"last,omitempty"`
}

// FindUnused lists the names of the surface with no use in the days days up to now. It lists
// against this build's surface, so a name that left the surface is never listed.
func FindUnused(s Surface, seen Seen, now time.Time, days int) []Unused {
	from := startOfDay(now).AddDate(0, 0, -(days - 1)).Format(dayLayout)
	var out []Unused
	check := func(kind string, names []string, spans map[string]*Span) {
		for _, n := range names {
			sp := spans[n]
			switch {
			case sp == nil:
				out = append(out, Unused{Kind: kind, Name: n})
			case sp.Last < from:
				out = append(out, Unused{Kind: kind, Name: n, Last: sp.Last})
			}
		}
	}
	check("method", s.Methods, seen.Methods)
	check("event", s.Events, seen.Events)
	check("hook", s.HookKeys(), seen.Hooks)
	check("feature", Features[:], seen.Features)
	check("setting", s.Settings, seen.Settings)
	return out
}

// Clear deletes the tallies folder.
func Clear(dir string) error { return os.RemoveAll(dir) }

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
