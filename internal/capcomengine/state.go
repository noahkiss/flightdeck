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
	// Summary is the summary at hand (6.1, filled from A4), shown by a divider.
	Summary string `json:"summary,omitempty"`
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
