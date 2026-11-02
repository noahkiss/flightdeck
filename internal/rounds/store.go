package rounds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RoundState is the scheduler's view of one round, kept beside its definition in the rounds
// store (rounds.md 7.7). The scheduler alone writes it.
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
}

// Entry is one round in the store: its definition and its scheduler state.
type Entry struct {
	Round Round      `json:"round"`
	State RoundState `json:"state"`
}

// RunLog holds run records (rounds.md 7.7). The rounds store is the first implementation;
// a Logbook implementation can take over later with no change to its callers.
type RunLog interface {
	// Append adds a new run. The caller sets its id and number; a duplicate id is refused.
	Append(r Run) error
	// Update replaces a stored run, found by round id and run id.
	Update(r Run) error
	// Page returns up to limit runs of a round, newest first, with numbers below before
	// (before <= 0: from the newest). more says whether older runs remain.
	Page(roundID string, before, limit int) (runs []Run, more bool, err error)
	// Prune applies the retention rule (Keep) to a round's runs at now and returns the runs
	// it removed, so the caller can remove their exited sessions.
	Prune(roundID string, now time.Time) (removed []Run, err error)
}

// Retention: each round keeps its last KeepRuns runs or every run newer than KeepFor,
// whichever keeps more (rounds.md 7.6).
const (
	KeepRuns = 200
	KeepFor  = 90 * 24 * time.Hour
)

// Keep reports which of a round's runs the retention rule keeps at now. A run is kept while it
// is among the KeepRuns newest by number, is newer than KeepFor, or has not ended.
func Keep(runs []Run, now time.Time) []bool {
	order := make([]int, len(runs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return runs[order[a]].Number > runs[order[b]].Number })
	keep := make([]bool, len(runs))
	cutoff := now.Add(-KeepFor)
	for rank, i := range order {
		r := runs[i]
		keep[i] = rank < KeepRuns || r.Created.After(cutoff) || !r.State.Final()
	}
	return keep
}

// The state file: <state>/rounds/state.json holds every round and its runs. Every change is
// saved whole through a synced temporary file and one rename, so a crash leaves the old
// state or the new one. A file that does not parse is set aside as .bad.
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
	zone     string
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
)

// Options configure Open.
type Options struct {
	// Zone is the zone a new round takes when it names none. Default HostZone().
	Zone string
	// Logf receives one line when a corrupt file is set aside. Default: discard.
	Logf func(format string, a ...any)
}

// Open loads <stateDir>/rounds/state.json, creating the folder. A file that does not parse is
// renamed to state.json.bad and the store starts empty; setAside is then true, so the caller
// can raise one notice. A file that cannot be read, or that a newer build wrote, is an error:
// the hab refuses to start rather than lose rounds.
func Open(stateDir string, opt Options) (s *Store, setAside bool, err error) {
	if opt.Zone == "" {
		opt.Zone = HostZone()
	}
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
			if opt.Logf != nil {
				opt.Logf("rounds: %s: %v; set aside as .bad, starting empty", s.path, derr)
			}
			if err := os.Rename(s.path, s.path+".bad"); err != nil {
				return nil, false, err
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

type newerError struct{ version int }

func (e *newerError) Error() string {
	return fmt.Sprintf("state version %d is newer than this build's %d", e.version, stateVersion)
}

func decodeState(b []byte) (state, error) {
	st := newState()
	if err := json.Unmarshal(b, &st); err != nil {
		return newState(), err
	}
	if st.Version > stateVersion {
		return newState(), &newerError{st.Version}
	}
	if st.Version < 1 {
		return newState(), fmt.Errorf("state version %d", st.Version)
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
	if err := r.Normalize(now, s.zone, nil); err != nil {
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
// time stay. A changed trigger sets the next slot from now (rounds.md 3, Edits).
func (s *Store) Update(r Round, now time.Time) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.st.Rounds[r.ID]
	if e == nil {
		return Entry{}, ErrNotFound
	}
	prev := e.Round
	if err := r.Normalize(now, s.zone, &prev); err != nil {
		return Entry{}, err
	}
	if o := s.byNameLocked(r.Name); o != nil && o != e {
		return Entry{}, fmt.Errorf("%w: %s", ErrNameTaken, r.Name)
	}
	r.CreatedBy, r.CreatedAt = prev.CreatedBy, prev.CreatedAt
	r.UpdatedAt = now.UTC()
	e.Round = r
	if !prev.Trigger.Equal(r.Trigger) && !e.State.Completed {
		sched, err := r.Trigger.Schedule()
		if err != nil {
			return Entry{}, err
		}
		e.State.NextSlot = nextSlot(sched, now)
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
	if r.ID == "" || r.RoundID == "" {
		return errors.New("rounds: a run needs an id and a round id")
	}
	for _, o := range s.st.Runs[r.RoundID] {
		if o.ID == r.ID {
			return fmt.Errorf("%w: %s", ErrRunExists, r.ID)
		}
	}
	s.st.Runs[r.RoundID] = append(s.st.Runs[r.RoundID], r)
	return s.commitLocked()
}

func (l storeRunLog) Update(r Run) error {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.st.Runs[r.RoundID]
	for i := range runs {
		if runs[i].ID == r.ID {
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
