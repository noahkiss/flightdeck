package content

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/noahkiss/flightdeck/api"
)

// Shown is one stored reference from content.show.
type Shown struct {
	Path  string          `json:"path"`
	Kind  api.ContentKind `json:"kind"`
	Title string          `json:"title,omitempty"`
	At    string          `json:"at"`
}

// Store keeps each session's shown references, and the last folder its transcript recorded, in
// one file in the state directory. An empty path keeps them in memory only. It is safe for
// concurrent use.
type Store struct {
	path    string
	mu      sync.Mutex
	m       map[string][]Shown // by session id
	folders map[string]string  // by session id
}

// file is the store's file shape.
type file struct {
	Shown   map[string][]Shown `json:"shown"`
	Folders map[string]string  `json:"folders,omitempty"`
}

// OpenStore loads the store at path. A missing file is an empty store; an unreadable one is
// an error.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, m: map[string][]Shown{}, folders: map[string]string{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Shown != nil {
		s.m = f.Shown
	}
	if f.Folders != nil {
		s.folders = f.Folders
	}
	return s, nil
}

// Add stores a reference for session. A reference to the same path is replaced and moves to
// the end; past MaxShown the oldest go.
func (s *Store) Add(session string, r Shown) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.m[session]
	out := make([]Shown, 0, len(list)+1)
	for _, x := range list {
		if x.Path != r.Path {
			out = append(out, x)
		}
	}
	out = append(out, r)
	if len(out) > MaxShown {
		out = out[len(out)-MaxShown:]
	}
	s.m[session] = out
	return s.save()
}

// SetFolder records the folder a session's transcript last recorded. It writes only on a change.
func (s *Store) SetFolder(session, folder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if folder == "" || s.folders[session] == folder {
		return nil
	}
	s.folders[session] = folder
	return s.save()
}

// Folder returns the folder recorded for session, or "".
func (s *Store) Folder(session string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.folders[session]
}

// List returns session's references, oldest first.
func (s *Store) List(session string) []Shown {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Shown(nil), s.m[session]...)
}

// Prune drops the references of every session keep says is gone.
func (s *Store) Prune(keep func(session string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id := range s.m {
		if !keep(id) {
			delete(s.m, id)
			changed = true
		}
	}
	for id := range s.folders {
		if !keep(id) {
			delete(s.folders, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

// save writes the file atomically. Called with mu held.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(file{Shown: s.m, Folders: s.folders})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
