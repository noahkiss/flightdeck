package modhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/noahkiss/flightdeck/api/modules"
)

// DirStore keeps each module's state as <Dir>/<module id>.json, written to a temporary file,
// synced, renamed into place and the folder synced, so a crash leaves the old state or the new
// one. Two modules with the same id share one file: the host refuses a second module with an id
// it already has (modules.md 8.4).
type DirStore struct {
	Dir string
}

func (d DirStore) file(module string) (string, error) {
	if !validID(module) {
		return "", fmt.Errorf("not a module id: %q", module)
	}
	return filepath.Join(d.Dir, module+".json"), nil
}

// Load reads a module's state; a module with no file has an empty state.
func (d DirStore) Load(module string) (map[string]json.RawMessage, error) {
	p, err := d.file(module)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return st, nil
}

// Save writes a module's whole state.
func (d DirStore) Save(module string, state map[string]json.RawMessage) error {
	p, err := d.file(module)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(d.Dir, "."+module+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, p)
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	// The rename is durable once the folder's entry is on disk.
	dir, err := os.Open(d.Dir)
	if err != nil {
		return err
	}
	serr := dir.Sync()
	if cerr := dir.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// MemStore keeps state in memory, for tests and `gff-hab modules check`.
type MemStore struct {
	mu sync.Mutex
	m  map[string]map[string]json.RawMessage
}

func (s *MemStore) Load(module string) (map[string]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.m[module]), nil
}

func (s *MemStore) Save(module string, state map[string]json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]map[string]json.RawMessage{}
	}
	s.m[module] = maps.Clone(state)
	return nil
}

var moduleIDRE = regexp.MustCompile(modules.ModuleIDPattern)

func validID(id string) bool {
	return len(id) <= modules.ModuleIDMax && moduleIDRE.MatchString(id)
}
