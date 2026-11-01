package content

import (
	"encoding/json"
	"errors"
	"fmt"
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
//
// Each change bumps a generation. A write marshals the state of the moment, writes it under its
// own lock, and skips a generation older than the one on disk, so writes never land out of
// order. Add, SetFolder and Prune write before they return. Forget leaves the write to a
// background writer, since it runs inside the structure store's event callback (app.md 4.7).
// Close writes what the writer has not.
type Store struct {
	path    string
	mu      sync.Mutex
	m       map[string][]Shown // by session id
	folders map[string]string  // by session id
	gen     uint64             // bumped on each change; under mu
	closed  bool               // under mu: after Close, Forget writes in line
	kick    chan struct{}      // wakes the writer; nil until the first Forget
	done    chan struct{}      // closed when the writer ends
	logf    func(string, ...any)

	wmu     sync.Mutex // serializes file writes
	written uint64     // the generation on disk; under wmu
}

// file is the store's file shape.
type file struct {
	Shown   map[string][]Shown `json:"shown"`
	Folders map[string]string  `json:"folders,omitempty"`
}

// CorruptError says OpenStore set an unreadable store file aside and started empty.
type CorruptError struct {
	Path, Aside string
	Err         error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("content store %s is unreadable (%v); moved it to %s and started empty", e.Path, e.Err, e.Aside)
}

// OpenStore loads the store at path. A missing file is an empty store. A file that does not
// parse (a torn write, a hand edit) is renamed to <path>.bad: OpenStore returns an empty store
// and a *CorruptError, so the hab still starts. A file it cannot read at all is an error.
// Call Close when done: it ends the background writer the first Forget starts and saves what
// that writer has not.
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
		aside := path + ".bad"
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("content store %s is unreadable (%v) and could not be moved aside: %w", path, err, rerr)
		}
		return s, &CorruptError{Path: path, Aside: aside, Err: err}
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
	s.gen++
	s.mu.Unlock()
	return s.persist()
}

// SetFolder records the folder a session's transcript last recorded. It writes only on a change.
func (s *Store) SetFolder(session, folder string) error {
	s.mu.Lock()
	if folder == "" || s.folders[session] == folder {
		s.mu.Unlock()
		return nil
	}
	s.folders[session] = folder
	s.gen++
	s.mu.Unlock()
	return s.persist()
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

// Forget drops a session's references and folder, when its record goes. It does not wait for
// the file: a background writer saves it, and Close saves what is left.
func (s *Store) Forget(session string) {
	s.mu.Lock()
	if !s.drop(func(id string) bool { return id != session }) || s.path == "" {
		s.mu.Unlock()
		return
	}
	if s.closed {
		s.mu.Unlock()
		if err := s.persist(); err != nil {
			s.log("content: %v", err)
		}
		return
	}
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
		s.done = make(chan struct{})
		go s.writer(s.kick, s.done)
	}
	select {
	case s.kick <- struct{}{}:
	default: // a write is already due; it takes this change too
	}
	s.mu.Unlock()
}

// writer saves the file each time Forget kicks it, until Close.
func (s *Store) writer(kick <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for range kick {
		if err := s.persist(); err != nil {
			s.log("content: %v", err)
		}
	}
}

// Close stops the background writer and saves any change it has not. Later changes write in
// line.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	kick, done := s.kick, s.done
	s.mu.Unlock()
	if kick != nil {
		close(kick)
		<-done
	}
	return s.persist()
}

// SetLogf sets where the background writer reports a failed write.
func (s *Store) SetLogf(f func(string, ...any)) {
	s.mu.Lock()
	s.logf = f
	s.mu.Unlock()
}

func (s *Store) log(format string, args ...any) {
	s.mu.Lock()
	f := s.logf
	s.mu.Unlock()
	if f != nil {
		f(format, args...)
	}
}

// Prune drops the references of every session keep says is gone.
func (s *Store) Prune(keep func(session string) bool) error {
	s.mu.Lock()
	changed := s.drop(keep)
	s.mu.Unlock()
	if !changed {
		return nil
	}
	return s.persist()
}

// drop removes the sessions keep says are gone, and bumps the generation when one went. Called
// with mu held.
func (s *Store) drop(keep func(session string) bool) bool {
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
	if changed {
		s.gen++
	}
	return changed
}

// persist writes the state of the moment, unless that generation or a newer one is on disk.
func (s *Store) persist() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	gen := s.gen
	b, err := json.Marshal(file{Shown: s.m, Folders: s.folders})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if gen <= s.written {
		return nil
	}
	if err := s.save(b); err != nil {
		return err
	}
	s.written = gen
	return nil
}

// save writes b to the file atomically. Called with wmu held.
func (s *Store) save(b []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// Synced before the rename, so a power loss leaves the old file or the new one, not a torn one.
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
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
	return os.Rename(tmp, s.path)
}
