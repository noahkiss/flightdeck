package structure

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/noahkiss/flightdeck/api"
)

// maxProjectDirs bounds the working directories kept per project.
const maxProjectDirs = 8

// RepoState is a project's repository state: the optional Project fields branch, head, ahead
// and behind (hab-logic-api.md 3.9). The zero value means "not a repository".
type RepoState struct {
	Branch string
	Head   string
	Ahead  *int // nil with no upstream
	Behind *int // nil with no upstream
}

// Equal reports whether two states carry the same values.
func (r RepoState) Equal(o RepoState) bool {
	return r.Branch == o.Branch && r.Head == o.Head && eqInt(r.Ahead, o.Ahead) && eqInt(r.Behind, o.Behind)
}

func eqInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ProjectDir is a project and its known working directories, most recently used first.
type ProjectDir struct {
	ID   string
	Dirs []string
}

// AddProjectDir records dir, an absolute path, as the project's most recently used working
// directory. It changes nothing a client sees, so it emits no event; it does save the state.
func (s *Store) AddProjectDir(id, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.project(id)
	if p == nil {
		return fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: project directory %q is not absolute", ErrInvalid, dir)
	}
	dir = filepath.Clean(dir)
	if len(p.Dirs) > 0 && p.Dirs[0] == dir {
		return nil
	}
	p.Dirs = slices.DeleteFunc(p.Dirs, func(d string) bool { return d == dir })
	p.Dirs = slices.Insert(p.Dirs, 0, dir)
	if len(p.Dirs) > maxProjectDirs {
		p.Dirs = p.Dirs[:maxProjectDirs]
	}
	s.commit(nil)
	return nil
}

// ProjectDirs lists every project with its working directories, in project order.
func (s *Store) ProjectDirs() []ProjectDir {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProjectDir, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, ProjectDir{ID: p.ID, Dirs: slices.Clone(p.Dirs)})
	}
	return out
}

// ProjectOfPane returns the project of the session that links pane, or "" for none.
func (s *Store) ProjectOfPane(pane string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessionByPane(pane); ss != nil {
		return ss.Project
	}
	return ""
}

// SessionOfPane returns the session that links pane.
func (s *Store) SessionOfPane(pane string) (api.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessionByPane(pane); ss != nil {
		return sessionAPI(ss), true
	}
	return api.Session{}, false
}

// SetProjectRepo stores a project's repository state. It raises the revision and emits
// project.updated only when a value changed, and reports whether one did.
func (s *Store) SetProjectRepo(id string, r RepoState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.project(id)
	if p == nil {
		return false, fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	cur := RepoState{Branch: p.Branch, Head: p.Head, Ahead: p.Ahead, Behind: p.Behind}
	if cur.Equal(r) {
		return false, nil
	}
	p.Branch, p.Head, p.Ahead, p.Behind = r.Branch, r.Head, copyInt(r.Ahead), copyInt(r.Behind)
	p.Revision++
	s.commit([]Event{s.projectEvent(p)})
	return true, nil
}
