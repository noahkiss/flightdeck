package capcomengine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StateFile is the engine's record in <state>/capcom/: the SDK sessions the thread used, the
// last item the model took, the pause mark and the crash times (3.2, 3.4).
const StateFile = "engine.json"

// SDKSession is one SDK session the thread used (3.4).
type SDKSession struct {
	ID    string    `json:"id"`
	Start time.Time `json:"start"`
	Why   string    `json:"why"`
	End   time.Time `json:"end,omitzero"`
	// Engine is the engine kind that ran it (ready's engine); "" for a session before A6, which
	// was the Claude engine's.
	Engine string `json:"engine,omitempty"`
}

// EngineOf is the engine kind of a session record: "" reads as claude.
func (s SDKSession) EngineOf() string {
	if s.Engine == "" {
		return "claude"
	}
	return s.Engine
}

// Record is engine.json.
type Record struct {
	Sessions []SDKSession `json:"sessions,omitempty"`
	// Taken is the newest item id the engine reported taken; at a new connection gff-hab sends
	// every item for the model after it (4.3).
	Taken   string      `json:"taken,omitempty"`
	Paused  bool        `json:"paused,omitempty"`
	Stopped string      `json:"stopped,omitempty"` // why the restarts stopped; "": they run
	Crashes []time.Time `json:"crashes,omitempty"`
	// Build names the engine build last started (3.2: an install drains the old build).
	Build string `json:"build,omitempty"`
	// Summary is the summary at hand (6.1), shown by a divider, and SummaryThrough the last
	// thread item it covers: the next update covers the items after it.
	Summary        string `json:"summary,omitempty"`
	SummaryThrough string `json:"summary_through,omitempty"`
	// Opening is the why (reseed or new) of a fresh session the engine announced with no id: a
	// Codex thread gets its id at its first turn (4.4). The old session ended then; the id's
	// session.started adds no second divider. An engine that starts before the id came opens
	// with a re-seed again.
	Opening string `json:"opening,omitempty"`
	// Sites are the hosts the user allowed with "Allow this site" (5.3), lowercase, sorted.
	Sites []string `json:"sites,omitempty"`
}

// Current is the SDK session to resume, or "".
func (r Record) Current() string {
	if n := len(r.Sessions); n > 0 && r.Sessions[n-1].End.IsZero() {
		return r.Sessions[n-1].ID
	}
	return ""
}

// Store keeps the record in memory and in its file, written through a temporary file and a
// rename, mode 0600.
type Store struct {
	path string
	mu   sync.Mutex
	rec  Record
}

// OpenStore loads the record in dir. A missing file is an empty record; a file that does not
// parse is set aside as .bad and an empty record runs, with the error.
func OpenStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, StateFile)}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if jerr := json.Unmarshal(b, &s.rec); jerr != nil {
		s.rec = Record{}
		os.Rename(s.path, s.path+".bad")
		return s, jerr
	}
	return s, nil
}

// Get returns a copy of the record.
func (s *Store) Get() Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rec
	r.Sessions = append([]SDKSession(nil), s.rec.Sessions...)
	r.Crashes = append([]time.Time(nil), s.rec.Crashes...)
	r.Sites = append([]string(nil), s.rec.Sites...)
	return r
}

// Update changes the record with fn and writes it.
func (s *Store) Update(fn func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.rec)
	b, err := json.MarshalIndent(s.rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tmp-engine-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
