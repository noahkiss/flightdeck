package rounds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RoundState is the scheduler's view of one round, kept beside its definition in the rounds
// store (rounds.md 7.7). The scheduler alone writes it, but for Target.
type RoundState struct {
	// NextSlot is the next slot the round runs at; nil when it has none (manual, a spent once,
	// a completed round).
	NextSlot *time.Time `json:"next_slot,omitempty"`
	// LastSlot is the last slot served. A slot at or before it never runs again (8.5).
	LastSlot *time.Time `json:"last_slot,omitempty"`
	// LastOK is when the last successful run started: GFF_ROUND_LAST_OK (5.1).
	LastOK *time.Time `json:"last_ok,omitempty"`
	// LastRun is the number given to the newest run; the next run takes LastRun+1.
	LastRun int `json:"last_run,omitempty"`
	// Finished counts finished runs, for max_runs.
	Finished int `json:"finished,omitempty"`
	// Failures and Skips count failed and skipped runs in a row (10.1).
	Failures int `json:"failures,omitempty"`
	Skips    int `json:"skips,omitempty"`
	// PausedReason says why the round paused itself; empty for a pause by the user.
	PausedReason string `json:"paused_reason,omitempty"`
	// Completed is set once the round will never run again, with the reason.
	Completed       bool   `json:"completed,omitempty"`
	CompletedReason string `json:"completed_reason,omitempty"`
	// Target is what gff-hab last saw of a resume or fork round's target: its harness and the
	// harness's own session id. gff-hab writes it (not the scheduler), so a run can resume or
	// fork a target whose program exited, after a restart too.
	Target *TargetSeen `json:"target,omitempty"`
	// Pending is an event round's pending batch (rounds.md 4.5). The scheduler saves it at most
	// once per state_save_every, and in the write that makes a run.
	Pending *Pending `json:"pending,omitempty"`
	// LastEventStart is when the last event run started: the cooldown counts from it.
	LastEventStart *time.Time `json:"last_event_start,omitempty"`
	// Watch is a watch round's cursor and dedup keys (rounds.md 4.4), saved as Pending is.
	Watch *WatchState `json:"watch,omitempty"`
}

// TargetSeen is a target session's harness and native session id, as last seen live. Session
// is the Flightdeck session it was seen in: the memo holds only while that is the round's
// target.
type TargetSeen struct {
	Session string `json:"session,omitempty"`
	Harness string `json:"harness"`
	Native  string `json:"native,omitempty"`
}

// Entry is one round in the store: its definition and its scheduler state.
type Entry struct {
	Round Round      `json:"round"`
	State RoundState `json:"state"`
}

// RunLog holds run records (rounds.md 7.7). The rounds store is the first implementation;
// a Logbook implementation can take over later with no change to its callers.
type RunLog interface {
	// Append adds a new run. The caller sets its id and number; an id already in the log,
	// under any round, is refused.
	Append(r Run) error
	// Update replaces a stored run, found by round id and run id. A run that has ended is
	// refused a change of state, number or creation time (ErrRunEnded).
	Update(r Run) error
	// Page returns up to limit runs of a round, newest first, with numbers below before
	// (before <= 0: from the newest). more says whether older runs remain.
	Page(roundID string, before, limit int) (runs []Run, more bool, err error)
	// Prune applies the retention rule (Keep) to a round's runs at now and returns the runs
	// it removed, so the caller can remove their exited sessions.
	Prune(roundID string, now time.Time) (removed []Run, err error)
	// Live returns every run that has not ended, under any round, deleted rounds included.
	// The scheduler loads them at start.
	Live() ([]Run, error)
}

// Retention: each round keeps its last KeepRuns runs or every run newer than KeepFor,
// whichever keeps more, and never more than MaxKeepRuns ended runs (rounds.md 7.6). The cap
// bounds the state file, which is saved whole on every change: a 1-minute round would
// otherwise keep about 130,000 runs inside 90 days.
const (
	KeepRuns    = 200
	KeepFor     = 90 * 24 * time.Hour
	MaxKeepRuns = 2000
)

// Keep reports which of a round's runs the retention rule keeps at now. A run that has not
// ended is always kept. An ended run is kept while it is among the KeepRuns newest by number
// or newer than KeepFor, and among the MaxKeepRuns newest ended runs.
func Keep(runs []Run, now time.Time) []bool {
	order := make([]int, len(runs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return runs[order[a]].Number > runs[order[b]].Number })
	keep := make([]bool, len(runs))
	cutoff := now.Add(-KeepFor)
	ended := 0
	for rank, i := range order {
		r := runs[i]
		if !r.State.Final() {
			keep[i] = true
			continue
		}
		keep[i] = ended < MaxKeepRuns && (rank < KeepRuns || r.Created.After(cutoff))
		if keep[i] {
			ended++
		}
	}
	return keep
}

// The state file: <state>/rounds/state.json holds every round and its runs. Every change is
// saved whole through a synced temporary file and one rename, so a crash leaves the old
// state or the new one. A file that does not parse is set aside as state.json.bad-<unix
// seconds>, never over an earlier one.
const (
	dirName      = "rounds"
	stateFile    = "state.json"
	stateVersion = 1
)

type state struct {
	Version int               `json:"version"`
	Rounds  map[string]*Entry `json:"rounds"`
	// Runs holds each round's runs, oldest first. A deleted round's live runs stay until
	// they end and are pruned.
	Runs map[string][]Run `json:"runs"`
}

func newState() state {
	return state{Version: stateVersion, Rounds: map[string]*Entry{}, Runs: map[string][]Run{}}
}

// Store is the rounds store. It is safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	path     string
	zone     string // empty: HostZone() at each create and edit
	st       state
	lastGood []byte
	// write saves the state file; tests replace it. Default writeSynced.
	write func(path string, b []byte) error
}

// Errors the store returns.
var (
	ErrNotFound  = errors.New("no such round")
	ErrNameTaken = errors.New("name taken")
	ErrRunExists = errors.New("run exists")
	ErrNoRun     = errors.New("no such run")
	ErrRunEnded  = errors.New("run has ended")
)

// Options configure Open.
type Options struct {
	// Zone is the zone a new round takes when it names none. Default: HostZone() at the
	// moment the round is created or edited, so a host that changes zone while the hab runs
	// gives new rounds its new zone (rounds.md 4.1).
	Zone string
	// Logf receives one line when a corrupt file is set aside. Default: discard.
	Logf func(format string, a ...any)
}

// Open loads <stateDir>/rounds/state.json, creating the folder. A file that does not parse, or
// that names no version, is renamed to state.json.bad-<unix seconds> (with a counter when that
// name is taken) and the store starts empty; setAside is then true, so the caller can raise
// one notice. A file that cannot be read, or that a newer build wrote, is an error:
// the hab refuses to start rather than lose rounds.
func Open(stateDir string, opt Options) (s *Store, setAside bool, err error) {
	s = &Store{
		path:  filepath.Join(stateDir, dirName, stateFile),
		zone:  opt.Zone,
		st:    newState(),
		write: writeSynced,
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, false, err
	}
	os.Remove(s.path + ".tmp") // a temp file left by a crash before its rename
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, false, err
	default:
		st, derr := decodeState(b)
		var newer *newerError
		switch {
		case errors.As(derr, &newer):
			return nil, false, fmt.Errorf("rounds: %s: %w", s.path, derr)
		case derr != nil:
			bad, err := setAsideName(s.path, time.Now())
			if err != nil {
				return nil, false, err
			}
			if err := os.Rename(s.path, bad); err != nil {
				return nil, false, err
			}
			if opt.Logf != nil {
				opt.Logf("rounds: %s: %v; set aside as %s, starting empty", s.path, derr, filepath.Base(bad))
			}
			setAside = true
		default:
			s.st = st
		}
	}
	if s.lastGood, err = json.Marshal(s.st); err != nil {
		return nil, false, err
	}
	return s, setAside, nil
}

// setAsideName returns a name for a corrupt state file that no earlier set-aside file holds:
// <path>.bad-<unix seconds>, then -2, -3 and so on.
func setAsideName(path string, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.bad-%d", path, now.Unix())
	for n := 1; n < 1000; n++ {
		p := base
		if n > 1 {
			p = fmt.Sprintf("%s-%d", base, n)
		}
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("rounds: no free name to set %s aside", path)
}

func (s *Store) zoneNow() string {
	if s.zone != "" {
		return s.zone
	}
	return HostZone()
}

type newerError struct{ version int }

func (e *newerError) Error() string {
	return fmt.Sprintf("state version %d is newer than this build's %d", e.version, stateVersion)
}

// decodeState reads the version first, so a newer file whose fields this build cannot decode
// is refused rather than set aside (rounds.md 7.7). A file with no version, or that is null,
// is not a state file this build wrote, and is set aside like a corrupt one.
func decodeState(b []byte) (state, error) {
	var head struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return newState(), err
	}
	if head.Version == nil {
		return newState(), errors.New("no state version")
	}
	if *head.Version > stateVersion {
		return newState(), &newerError{*head.Version}
	}
	if *head.Version < 1 {
		return newState(), fmt.Errorf("state version %d", *head.Version)
	}
	st := state{}
	if err := json.Unmarshal(b, &st); err != nil {
		return newState(), err
	}
	if st.Rounds == nil {
		st.Rounds = map[string]*Entry{}
	}
	if st.Runs == nil {
		st.Runs = map[string][]Run{}
	}
	for id, e := range st.Rounds {
		if e == nil || e.Round.ID != id {
			return newState(), fmt.Errorf("round %q: bad entry", id)
		}
	}
	return st, nil
}

// TriggerErrors returns, by round id, the rounds whose stored trigger no longer compiles: a
// zone this host's zone database lacks, or a rule a later build tightened. Such a round loads
// and can be read, edited or deleted, but has no schedule. The scheduler reads this at start
// and after each load, leaves those rounds unscheduled, and reports each error on the round
// rather than skipping it silently. Create and Update never store such a trigger.
func (s *Store) TriggerErrors() map[string]error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]error{}
	for id, e := range s.st.Rounds {
		if _, err := e.Round.Trigger.Schedule(); err != nil {
			out[id] = err
		}
	}
	return out
}

// commitLocked saves the state. When the save fails, the state returns to the last saved one
// and the error is returned: no caller answers as if a change were on disk.
func (s *Store) commitLocked() error {
	b, err := json.Marshal(s.st)
	if err == nil {
		err = s.write(s.path, b)
	}
	if err != nil {
		if st, derr := decodeState(s.lastGood); derr == nil {
			s.st = st
		}
		return err
	}
	s.lastGood = b
	return nil
}

// writeSynced writes a file through a temporary file that is synced before the rename, then
// syncs the folder so the rename itself is on disk.
func writeSynced(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync() // some file systems refuse a folder sync; the rename is done either way
		d.Close()
	}
	return nil
}

func (s *Store) byNameLocked(name string) *Entry {
	for _, e := range s.st.Rounds {
		if e.Round.Name == name {
			return e
		}
	}
	return nil
}

// Create checks and stores a new round, gives it an id, and sets its first slot after now.
// createdBy is the session of the agent that created it, or empty for a user.
func (s *Store) Create(r Round, createdBy string, now time.Time) (Entry, error) {
	if err := r.Normalize(now, s.zoneNow(), nil); err != nil {
		return Entry{}, err
	}
	sched, err := r.Trigger.Schedule()
	if err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byNameLocked(r.Name) != nil {
		return Entry{}, fmt.Errorf("%w: %s", ErrNameTaken, r.Name)
	}
	r.ID = NewID("round")
	r.CreatedBy = createdBy
	r.CreatedAt = now.UTC()
	r.UpdatedAt = r.CreatedAt
	e := &Entry{Round: r}
	e.State.NextSlot = nextSlot(sched, now)
	s.st.Rounds[r.ID] = e
	if err := s.commitLocked(); err != nil {
		return Entry{}, err
	}
	return cloneEntry(e), nil
}

// Update replaces a round's definition with r, found by r.ID. The id, created_by and creation
// time stay. A changed trigger sets the next slot from now (rounds.md 3, Edits), and re-arms a
// completed round: Completed and its reason are cleared.
func (s *Store) Update(r Round, now time.Time) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.st.Rounds[r.ID]
	if e == nil {
		return Entry{}, ErrNotFound
	}
	prev := e.Round
	if err := r.Normalize(now, s.zoneNow(), &prev); err != nil {
		return Entry{}, err
	}
	if o := s.byNameLocked(r.Name); o != nil && o != e {
		return Entry{}, fmt.Errorf("%w: %s", ErrNameTaken, r.Name)
	}
	r.CreatedBy, r.CreatedAt = prev.CreatedBy, prev.CreatedAt
	r.UpdatedAt = now.UTC()
	e.Round = r
	if !prev.Trigger.Equal(r.Trigger) {
		sched, err := r.Trigger.Schedule()
		if err != nil {
			return Entry{}, err
		}
		e.State.NextSlot = nextSlot(sched, now)
		e.State.Completed, e.State.CompletedReason = false, ""
		if !r.Trigger.Kind.Batches() {
			e.State.Pending, e.State.LastEventStart = nil, nil
		}
		if r.Trigger.Kind != TriggerWatch {
			e.State.Watch = nil
		}
	}
	if err := s.commitLocked(); err != nil {
		return Entry{}, err
	}
	return cloneEntry(s.st.Rounds[r.ID]), nil
}

func nextSlot(sched *Schedule, now time.Time) *time.Time {
	if t, ok := sched.Next(now); ok {
		return &t
	}
	return nil
}

// SetState changes a round's scheduler state through fn and saves it.
func (s *Store) SetState(id string, fn func(*RoundState)) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.st.Rounds[id]
	if e == nil {
		return Entry{}, ErrNotFound
	}
	fn(&e.State)
	if err := s.commitLocked(); err != nil {
		return Entry{}, err
	}
	return cloneEntry(s.st.Rounds[id]), nil
}

// SetPending saves the pending batches of several rounds, and the watcher states of watch
// rounds, in one write. A round that is gone is left out; a nil or empty batch or state clears
// the round's.
func (s *Store) SetPending(p map[string]*Pending, w map[string]*WatchState) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var ids []string
	for id, b := range p {
		e := s.st.Rounds[id]
		if e == nil {
			continue
		}
		if b == nil || b.Len() == 0 {
			e.State.Pending = nil
		} else {
			c := clonePending(b)
			e.State.Pending = &c
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for id, ws := range w {
		e := s.st.Rounds[id]
		if e == nil {
			continue
		}
		if ws.empty() || e.Round.Trigger.Kind != TriggerWatch {
			e.State.Watch = nil
		} else {
			e.State.Watch = cloneWatchState(ws)
		}
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		if e := s.st.Rounds[id]; e != nil {
			out = append(out, cloneEntry(e))
		}
	}
	return out, nil
}

func clonePending(p *Pending) Pending {
	b, err := json.Marshal(p)
	if err != nil {
		panic("rounds: " + err.Error())
	}
	var c Pending
	if err := json.Unmarshal(b, &c); err != nil {
		panic("rounds: " + err.Error())
	}
	return c
}

// QueueRun adds a new run and changes its round's scheduler state through fn, in one save: a
// failed save leaves neither the run nor the state change, so a slot is never marked served
// without its run, nor a run stored for a slot still due. The run is checked as Append checks
// it.
func (s *Store) QueueRun(r Run, fn func(*RoundState)) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.st.Rounds[r.RoundID]
	if e == nil {
		return Entry{}, ErrNotFound
	}
	if err := s.appendLocked(r); err != nil {
		return Entry{}, err
	}
	fn(&s.st.Rounds[r.RoundID].State)
	if err := s.commitLocked(); err != nil {
		return Entry{}, err
	}
	return cloneEntry(s.st.Rounds[r.RoundID]), nil
}

// Delete removes a round and its ended runs. A live run stays in the log until it ends; the
// next Prune for the round then removes it (rounds.md 11: a deleted round's run goes on).
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Rounds[id] == nil {
		return ErrNotFound
	}
	delete(s.st.Rounds, id)
	s.dropEndedLocked(id)
	return s.commitLocked()
}

func (s *Store) dropEndedLocked(id string) []Run {
	var kept, removed []Run
	for _, r := range s.st.Runs[id] {
		if r.State.Final() {
			removed = append(removed, r)
		} else {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		delete(s.st.Runs, id)
	} else {
		s.st.Runs[id] = kept
	}
	return removed
}

// Run returns one run by its id, under any round.
func (s *Store) Run(id string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, runs := range s.st.Runs {
		for _, r := range runs {
			if r.ID == id {
				return r, true
			}
		}
	}
	return Run{}, false
}

// RunByMessage returns the run whose channel message has this id (rounds.md 5.2).
func (s *Store) RunByMessage(msg string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg == "" {
		return Run{}, false
	}
	for _, runs := range s.st.Runs {
		for _, r := range runs {
			if r.Message == msg {
				return r, true
			}
		}
	}
	return Run{}, false
}

// --- Event files (rounds.md 4.5) ---

// EventsPath is the file that holds a run's event batch: GFF_ROUND_EVENTS.
func (s *Store) EventsPath(roundID, runID string) string {
	return filepath.Join(filepath.Dir(s.path), "events", roundID, runID+".json")
}

// WriteEvents saves a run's batch to its file before the run is queued, so a run never exists
// without its events.
func (s *Store) WriteEvents(roundID, runID string, b Batch) error {
	p := s.EventsPath(roundID, runID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if b.Events == nil {
		b.Events = []Event{}
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return writeSynced(p, data)
}

// ReadEvents reads a run's batch from its file.
func (s *Store) ReadEvents(roundID, runID string) (Batch, error) {
	var b Batch
	data, err := os.ReadFile(s.EventsPath(roundID, runID))
	if err != nil {
		return b, err
	}
	err = json.Unmarshal(data, &b)
	return b, err
}

// RemoveEvents removes a run's event file.
func (s *Store) RemoveEvents(roundID, runID string) {
	os.Remove(s.EventsPath(roundID, runID))
}

// PruneEvents keeps the event files of a round's live runs and of its keep newest ended runs,
// and removes the rest; a round that is gone with no live run loses its folder.
func (s *Store) PruneEvents(roundID string, keep int) {
	dir := filepath.Dir(s.EventsPath(roundID, "x"))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	s.mu.Lock()
	runs := append([]Run(nil), s.st.Runs[roundID]...)
	_, alive := s.st.Rounds[roundID]
	s.mu.Unlock()
	sort.Slice(runs, func(i, j int) bool { return runs[i].Number > runs[j].Number })
	kept := map[string]bool{}
	ended := 0
	for _, r := range runs {
		if !r.State.Final() {
			kept[r.ID] = true
		} else if ended < keep {
			kept[r.ID] = true
			ended++
		}
	}
	left := 0
	for _, de := range ents {
		id, ok := strings.CutSuffix(de.Name(), ".json")
		if ok && kept[id] {
			left++
			continue
		}
		os.Remove(filepath.Join(dir, de.Name()))
	}
	if left == 0 && !alive {
		os.Remove(dir)
	}
}

// Get returns one round by id.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.st.Rounds[id]
	if e == nil {
		return Entry{}, false
	}
	return cloneEntry(e), true
}

// ByName returns one round by name.
func (s *Store) ByName(name string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byNameLocked(name)
	if e == nil {
		return Entry{}, false
	}
	return cloneEntry(e), true
}

// List returns every round, by name.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.st.Rounds))
	for _, e := range s.st.Rounds {
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Round.Name < out[j].Round.Name })
	return out
}

// cloneEntry copies an entry so a caller never shares the store's slices and pointers.
func cloneEntry(e *Entry) Entry {
	b, err := json.Marshal(e)
	if err != nil {
		panic("rounds: " + err.Error())
	}
	var c Entry
	if err := json.Unmarshal(b, &c); err != nil {
		panic("rounds: " + err.Error())
	}
	return c
}

// Runs returns the store's run log.
func (s *Store) Runs() RunLog { return storeRunLog{s} }

type storeRunLog struct{ s *Store }

func (l storeRunLog) Append(r Run) error {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(r); err != nil {
		return err
	}
	return s.commitLocked()
}

// appendLocked checks a new run and adds it to the state in memory; the caller commits.
func (s *Store) appendLocked(r Run) error {
	if r.ID == "" || r.RoundID == "" {
		return errors.New("rounds: a run needs an id and a round id")
	}
	for _, runs := range s.st.Runs {
		for _, o := range runs {
			if o.ID == r.ID {
				return fmt.Errorf("%w: %s", ErrRunExists, r.ID)
			}
		}
	}
	s.st.Runs[r.RoundID] = append(s.st.Runs[r.RoundID], r)
	return nil
}

func (l storeRunLog) Live() ([]Run, error) {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, runs := range s.st.Runs {
		for _, r := range runs {
			if !r.State.Final() {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (l storeRunLog) Update(r Run) error {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.st.Runs[r.RoundID]
	for i := range runs {
		if runs[i].ID == r.ID {
			if o := runs[i]; o.State.Final() &&
				(r.State != o.State || r.Number != o.Number || !r.Created.Equal(o.Created)) {
				return fmt.Errorf("%w: %s is %s", ErrRunEnded, r.ID, o.State)
			}
			runs[i] = r
			return s.commitLocked()
		}
	}
	return fmt.Errorf("%w: %s", ErrNoRun, r.ID)
}

func (l storeRunLog) Page(roundID string, before, limit int) ([]Run, bool, error) {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return nil, false, errors.New("rounds: page limit must be positive")
	}
	runs := append([]Run(nil), s.st.Runs[roundID]...)
	sort.Slice(runs, func(i, j int) bool { return runs[i].Number > runs[j].Number })
	var out []Run
	more := false
	for _, r := range runs {
		if before > 0 && r.Number >= before {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		out = append(out, r)
	}
	return out, more, nil
}

func (l storeRunLog) Prune(roundID string, now time.Time) ([]Run, error) {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []Run
	if s.st.Rounds[roundID] == nil {
		removed = s.dropEndedLocked(roundID)
	} else {
		runs := s.st.Runs[roundID]
		keep := Keep(runs, now)
		var kept []Run
		for i, r := range runs {
			if keep[i] {
				kept = append(kept, r)
			} else {
				removed = append(removed, r)
			}
		}
		s.st.Runs[roundID] = kept
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return removed, nil
}
