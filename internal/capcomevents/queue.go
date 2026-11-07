// Package capcomevents is the hab's queue of the events Capcom looks at first
// (docs/specs/capcom-service.md 9.2, 9.3, 9.5): notices, round run ends and round messages wait
// here, as thread items, until a batch may go to the engine. A batch goes when its window ends
// or it is full, never in quiet hours unless an event in it is urgent, and never past the day's
// event turn budget. The queue also keeps what the second answer to a round message needs: the
// turn that took its batch and whether that turn wrote to the user. Its state is one file,
// <state>/capcom/events.json, with the time zone the day is counted in. Pure Go.
package capcomevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// File is the queue's state file in the Capcom folder.
const File = "events.json"

// Outcomes of a round message (9.5).
const (
	OutcomeTold   = "told"
	OutcomeQuiet  = "quiet"
	OutcomeUnseen = "unseen"
)

// maxTimer bounds one wait of the timer: each fire compares wall time, so a sleep ends no wait
// late and a clock change is seen within it.
const maxTimer = 5 * time.Minute

// Event is one event that waits: a thread item.
type Event struct {
	Item   string    `json:"item"` // the thread item id
	At     time.Time `json:"at"`
	Urgent bool      `json:"urgent,omitempty"` // a time-sensitive notice or round message
	// Round is the channel message id of a round message, for its second answer.
	Round string `json:"round,omitempty"`
}

// Batch is a set of events released as one input (one event turn).
type Batch struct {
	ID     string    `json:"id"` // the input's id, "b" and 16 hex digits
	Items  []string  `json:"items"`
	Rounds []string  `json:"rounds,omitempty"`
	Urgent bool      `json:"urgent,omitempty"`
	At     time.Time `json:"at"` // released
	// Turn is the engine turn that took the batch; "" until it is taken.
	Turn  string `json:"turn,omitempty"`
	Wrote bool   `json:"wrote,omitempty"` // that turn wrote to the user
}

// Limits are the hab-side settings of 9.2 and 9.3, read at each check.
type Limits struct {
	// QuietStart and QuietEnd are minutes after local midnight; equal: no quiet hours.
	QuietStart, QuietEnd int
	DailyTurns           int
	BatchWindow          time.Duration
	BatchMax             int
}

// DefaultLimits are 19.1's defaults: 22:00 to 07:00, 20 turns a day, 2 minutes, 20 events.
var DefaultLimits = Limits{QuietStart: 22 * 60, QuietEnd: 7 * 60, DailyTurns: 20, BatchWindow: 2 * time.Minute, BatchMax: 20}

// Config wires a queue.
type Config struct {
	Path string // the state file
	Now  func() time.Time
	// After runs f after d; the returned stop cancels it. Nil: time.AfterFunc.
	After  func(d time.Duration, f func()) (stop func() bool)
	Limits func() Limits // nil: DefaultLimits
	// Host is the host's time zone, used until a device shares one. Nil: time.Local.
	Host *time.Location
	// Released hears that a batch is ready to send, with no lock held.
	Released func()
	// Note writes a Flightdeck note to the thread, with no lock held.
	Note func(text string)
	// Outcome hears the second answer of a round message, with no lock held.
	Outcome func(round, word string, at time.Time)
	Logf    func(string, ...any)
}

type state struct {
	// TZ is the time zone the day and quiet hours are counted in: the one a device last shared,
	// "" for the host's (9.2, 9.3).
	TZ       string    `json:"tz,omitempty"`
	Day      string    `json:"day,omitempty"` // the local date Turns counts
	Turns    int       `json:"turns"`
	Noted    string    `json:"noted,omitempty"` // the day the budget note went to the thread
	Waiting  []Event   `json:"waiting,omitempty"`
	Released []Batch   `json:"released,omitempty"`
	Last     uint64    `json:"last,omitempty"` // the newest batch id's number
	Saved    time.Time `json:"saved,omitzero"`
}

// Queue is the event queue of one home host.
type Queue struct {
	cfg Config

	mu    sync.Mutex
	st    state
	stop  func() bool
	on    bool // the home host runs the service: batches may go
	notes []string
	outs  []outcome
	rel   bool
}

type outcome struct {
	round, word string
	at          time.Time
}

// Open loads the queue's file. A file that does not parse is set aside as <file>.bad-<unix
// seconds> and the queue starts empty; the error says so.
func Open(cfg Config) (*Queue, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if cfg.Limits == nil {
		cfg.Limits = func() Limits { return DefaultLimits }
	}
	if cfg.Host == nil {
		cfg.Host = time.Local
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	q := &Queue{cfg: cfg}
	b, err := os.ReadFile(cfg.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return q, nil
	case err != nil:
		return nil, err
	}
	if jerr := json.Unmarshal(b, &q.st); jerr != nil {
		q.st = state{}
		bad := fmt.Sprintf("%s.bad-%d", cfg.Path, cfg.Now().Unix())
		if rerr := os.Rename(cfg.Path, bad); rerr != nil {
			return nil, rerr
		}
		return q, fmt.Errorf("capcom events: %s does not parse (%v); set aside as %s", filepath.Base(cfg.Path), jerr, filepath.Base(bad))
	}
	return q, nil
}

// Start lets batches go: this hab runs the service. It checks at once.
func (q *Queue) Start() {
	q.mu.Lock()
	q.on = true
	q.mu.Unlock()
	q.Check()
}

// Stop holds every batch and stops the timer.
func (q *Queue) Stop() {
	q.mu.Lock()
	q.on = false
	if q.stop != nil {
		q.stop()
		q.stop = nil
	}
	q.mu.Unlock()
}

// Add queues one event and checks.
func (q *Queue) Add(ev Event) error {
	q.mu.Lock()
	for _, w := range q.st.Waiting {
		if w.Item == ev.Item {
			q.mu.Unlock()
			return nil
		}
	}
	if ev.At.IsZero() {
		ev.At = q.cfg.Now()
	}
	q.st.Waiting = append(q.st.Waiting, ev)
	err := q.saveLocked()
	q.mu.Unlock()
	q.Check()
	return err
}

// SetTZ keeps the time zone a device shared (an IANA name). An unknown name is ignored.
func (q *Queue) SetTZ(name string) {
	if name == "" {
		return
	}
	if _, err := time.LoadLocation(name); err != nil {
		return
	}
	q.mu.Lock()
	if q.st.TZ == name {
		q.mu.Unlock()
		return
	}
	q.st.TZ = name
	if err := q.saveLocked(); err != nil {
		q.cfg.Logf("capcom events: save: %v", err)
	}
	q.mu.Unlock()
	q.Check()
}

// TZ is the time zone the queue counts in.
func (q *Queue) TZ() *time.Location {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.locLocked()
}

func (q *Queue) locLocked() *time.Location {
	if q.st.TZ != "" {
		if l, err := time.LoadLocation(q.st.TZ); err == nil {
			return l
		}
	}
	return q.cfg.Host
}

// Holds reports whether a thread item waits or is in a released batch.
func (q *Queue) Holds(item string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, w := range q.st.Waiting {
		if w.Item == item {
			return true
		}
	}
	for _, b := range q.st.Released {
		if slices.Contains(b.Items, item) {
			return true
		}
	}
	return false
}

// Waiting returns the events that wait, oldest first.
func (q *Queue) Waiting() []Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.st.Waiting)
}

// Turns returns the event turns released today and the day they count.
func (q *Queue) Turns() (int, string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dayLocked(q.cfg.Now())
	return q.st.Turns, q.st.Day
}

// Released returns the batches no turn took yet, oldest first: the link sends them.
func (q *Queue) Released() []Batch {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Batch
	for _, b := range q.st.Released {
		if b.Turn == "" {
			out = append(out, cloneBatch(b))
		}
	}
	return out
}

// Batch returns a released batch by id.
func (q *Queue) Batch(id string) (Batch, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, b := range q.st.Released {
		if b.ID == id {
			return cloneBatch(b), true
		}
	}
	return Batch{}, false
}

// Taken records that turn took the batch. ok false: no such batch, or one taken already.
func (q *Queue) Taken(id, turn string) (Batch, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.st.Released {
		b := &q.st.Released[i]
		if b.ID != id {
			continue
		}
		if b.Turn != "" {
			return cloneBatch(*b), false
		}
		b.Turn = turn
		if turn == "" {
			b.Turn = "?" // taken outside a known turn: settled at the next ready
		}
		if err := q.saveLocked(); err != nil {
			q.cfg.Logf("capcom events: save: %v", err)
		}
		return cloneBatch(*b), true
	}
	return Batch{}, false
}

// Wrote records that turn wrote a message to the user.
func (q *Queue) Wrote(turn string) {
	if turn == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	changed := false
	for i := range q.st.Released {
		if b := &q.st.Released[i]; b.Turn == turn && !b.Wrote {
			b.Wrote, changed = true, true
		}
	}
	if changed {
		if err := q.saveLocked(); err != nil {
			q.cfg.Logf("capcom events: save: %v", err)
		}
	}
}

// UrgentTurn reports whether turn took a batch with an urgent event: its messages push
// time-sensitive (9.2).
func (q *Queue) UrgentTurn(turn string) bool {
	if turn == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, b := range q.st.Released {
		if b.Turn == turn && b.Urgent {
			return true
		}
	}
	return false
}

// TurnEnded settles the batches turn took: each round message in them gets the turn's outcome,
// told when it wrote to the user, else quiet, at the turn's end (9.5).
func (q *Queue) TurnEnded(turn string, at time.Time) {
	q.settle(func(b Batch) bool { return b.Turn != "" && b.Turn == turn }, at)
}

// Running settles the batches taken by any turn other than the running one ("" for none): the
// engine restarted, and their turn ended while this hab did not hear it.
func (q *Queue) Running(turn string) {
	q.settle(func(b Batch) bool { return b.Turn != "" && b.Turn != turn }, q.cfg.Now())
}

func (q *Queue) settle(match func(Batch) bool, at time.Time) {
	q.mu.Lock()
	kept := q.st.Released[:0:0]
	for _, b := range q.st.Released {
		if !match(b) {
			kept = append(kept, b)
			continue
		}
		word := OutcomeQuiet
		if b.Wrote {
			word = OutcomeTold
		}
		for _, r := range b.Rounds {
			q.outs = append(q.outs, outcome{round: r, word: word, at: at})
		}
	}
	changed := len(kept) != len(q.st.Released)
	q.st.Released = kept
	if changed {
		if err := q.saveLocked(); err != nil {
			q.cfg.Logf("capcom events: save: %v", err)
		}
	}
	q.mu.Unlock()
	q.flush()
}

// Drop ends every event no turn took: this hab lost the mark (9.5 unseen). Each round message
// among them gets the outcome unseen. Batches a running turn took stay, for its end.
func (q *Queue) Drop() {
	now := q.cfg.Now()
	q.mu.Lock()
	for _, w := range q.st.Waiting {
		if w.Round != "" {
			q.outs = append(q.outs, outcome{round: w.Round, word: OutcomeUnseen, at: now})
		}
	}
	q.st.Waiting = nil
	kept := q.st.Released[:0:0]
	for _, b := range q.st.Released {
		if b.Turn != "" {
			kept = append(kept, b)
			continue
		}
		for _, r := range b.Rounds {
			q.outs = append(q.outs, outcome{round: r, word: OutcomeUnseen, at: now})
		}
	}
	q.st.Released = kept
	if err := q.saveLocked(); err != nil {
		q.cfg.Logf("capcom events: save: %v", err)
	}
	q.mu.Unlock()
	q.flush()
}

// Check releases the batches that may go now and sets the timer for the next check.
func (q *Queue) Check() {
	q.mu.Lock()
	q.checkLocked()
	q.mu.Unlock()
	q.flush()
}

// flush hands out what the last change left, with no lock held.
func (q *Queue) flush() {
	q.mu.Lock()
	notes, outs, rel := q.notes, q.outs, q.rel
	q.notes, q.outs, q.rel = nil, nil, false
	q.mu.Unlock()
	for _, n := range notes {
		if q.cfg.Note != nil {
			q.cfg.Note(n)
		}
	}
	for _, o := range outs {
		if q.cfg.Outcome != nil {
			q.cfg.Outcome(o.round, o.word, o.at)
		}
	}
	if rel && q.cfg.Released != nil {
		q.cfg.Released()
	}
}

// dayLocked starts a new count when the local date moved.
func (q *Queue) dayLocked(now time.Time) {
	day := now.In(q.locLocked()).Format(time.DateOnly)
	if q.st.Day != day {
		q.st.Day, q.st.Turns = day, 0
	}
}

func (q *Queue) checkLocked() {
	if q.stop != nil {
		q.stop()
		q.stop = nil
	}
	if !q.on {
		return
	}
	now := q.cfg.Now()
	lim := sane(q.cfg.Limits())
	loc := q.locLocked()
	day := q.st.Day
	q.dayLocked(now)
	changed := day != q.st.Day
	var next time.Time
	quiet, quietEnd := Quiet(now, loc, lim)
	for len(q.st.Waiting) > 0 {
		if q.st.Turns >= lim.DailyTurns {
			if q.st.Noted != q.st.Day {
				q.st.Noted = q.st.Day
				changed = true
				q.notes = append(q.notes, fmt.Sprintf("Capcom looked at today's %d event batches; the rest wait for tomorrow", lim.DailyTurns))
			}
			next = NextMidnight(now, loc)
			break
		}
		var take []int
		if quiet {
			for i, w := range q.st.Waiting {
				if w.Urgent && len(take) < lim.BatchMax {
					take = append(take, i)
				}
			}
			if len(take) == 0 {
				next = quietEnd
				break
			}
		} else {
			due := q.st.Waiting[0].At.Add(lim.BatchWindow)
			urgent := slices.ContainsFunc(q.st.Waiting, func(e Event) bool { return e.Urgent })
			if len(q.st.Waiting) < lim.BatchMax && now.Before(due) && !urgent {
				next = due
				break
			}
			for i := range min(len(q.st.Waiting), lim.BatchMax) {
				take = append(take, i)
			}
		}
		q.releaseLocked(take, now)
		changed = true
	}
	if changed {
		if err := q.saveLocked(); err != nil {
			q.cfg.Logf("capcom events: save: %v", err)
		}
	}
	if !next.IsZero() {
		d := min(max(next.Sub(now), 0), maxTimer)
		q.stop = q.cfg.After(d, q.Check)
	}
}

// releaseLocked moves the waiting events at idx (ascending) into one new batch.
func (q *Queue) releaseLocked(idx []int, now time.Time) {
	n := max(uint64(now.UnixNano()), q.st.Last+1)
	q.st.Last = n
	b := Batch{ID: fmt.Sprintf("b%016x", n), At: now}
	var kept []Event
	j := 0
	for i, w := range q.st.Waiting {
		if j < len(idx) && idx[j] == i {
			j++
			b.Items = append(b.Items, w.Item)
			if w.Round != "" {
				b.Rounds = append(b.Rounds, w.Round)
			}
			b.Urgent = b.Urgent || w.Urgent
			continue
		}
		kept = append(kept, w)
	}
	q.st.Waiting = kept
	q.st.Released = append(q.st.Released, b)
	q.st.Turns++
	q.rel = true
}

func (q *Queue) saveLocked() error {
	q.st.Saved = q.cfg.Now().UTC()
	b, err := json.MarshalIndent(q.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.cfg.Path), 0o700); err != nil {
		return err
	}
	tmp := q.cfg.Path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, q.cfg.Path)
}

// sane puts each limit back in its range, as settings.set keeps them.
func sane(l Limits) Limits {
	d := DefaultLimits
	if l.DailyTurns < 1 {
		l.DailyTurns = d.DailyTurns
	}
	if l.BatchMax < 1 {
		l.BatchMax = d.BatchMax
	}
	if l.BatchWindow <= 0 {
		l.BatchWindow = d.BatchWindow
	}
	return l
}

// Quiet reports whether now is in quiet hours in loc, and when they end. Equal start and end
// mean no quiet hours.
func Quiet(now time.Time, loc *time.Location, l Limits) (bool, time.Time) {
	if l.QuietStart == l.QuietEnd {
		return false, time.Time{}
	}
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	at := func(dayOffset, minute int) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day()+dayOffset, minute/60, minute%60, 0, 0, loc)
	}
	s, e := l.QuietStart, l.QuietEnd
	if s < e {
		if m >= s && m < e {
			return true, at(0, e)
		}
		return false, time.Time{}
	}
	switch {
	case m >= s:
		return true, at(1, e)
	case m < e:
		return true, at(0, e)
	}
	return false, time.Time{}
}

// NextMidnight is the start of the next local day.
func NextMidnight(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
}

func cloneBatch(b Batch) Batch {
	b.Items = slices.Clone(b.Items)
	b.Rounds = slices.Clone(b.Rounds)
	return b
}
