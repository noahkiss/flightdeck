package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// StateFile is the file in the state directory that holds client writes and revisions.
const StateFile = "settings.json"

// DefaultPoll is how often the service checks the config file for a change.
const DefaultPoll = 2 * time.Second

// Config is what Open needs.
type Config struct {
	State      string // the state directory
	ConfigPath string // the hab's config file; its [settings] table locks settings. "" or missing: none
	// Overrides lock settings from flags or the environment, over the config file. Each value
	// must pass its setting's Check.
	Overrides map[string]json.RawMessage
	// Publish sends a settings.updated event. Called with the service's lock held, so events go
	// out in revision order; it must not call back into the service.
	Publish func(structure.Event)
	// Apply hands a setting's value to the code that uses it: once for each setting at Open, then
	// on every change. Called with the service's lock held; it must not call back.
	Apply func(id string, value json.RawMessage)
	Poll  time.Duration // default DefaultPoll
	Logf  func(format string, args ...any)
}

// Service keeps the hab-side settings. It is safe for concurrent use.
type Service struct {
	cfg  Config
	done chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	rows    map[string]*row
	locks   map[string]json.RawMessage // id -> locked value, from the config file and Overrides
	stamp   fileStamp
	pending fileStamp
}

// row is one setting as stored.
type row struct {
	Revision uint64          `json:"revision"`
	Value    json.RawMessage `json:"value,omitempty"` // the client's value; nil: no client wrote it
	Lock     json.RawMessage `json:"lock,omitempty"`  // the locked value when last saved, to see a lock change across a restart
}

type stateFile struct {
	Settings map[string]*row `json:"settings"`
}

type fileStamp struct {
	ok    bool
	size  int64
	mtime time.Time
}

func stampOf(path string) fileStamp {
	if path == "" {
		return fileStamp{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{ok: true, size: fi.Size(), mtime: fi.ModTime()}
}

// Open reads the state file and the config file, applies each setting once, and starts
// watching the config file. A config file that does not parse is an error: the hab cannot
// enforce what it cannot read. An entry of the [settings] table that names no setting or holds
// a value outside its type is logged and left out.
func Open(cfg Config) (*Service, error) {
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Publish == nil {
		cfg.Publish = func(structure.Event) {}
	}
	if cfg.Apply == nil {
		cfg.Apply = func(string, json.RawMessage) {}
	}
	for id, v := range cfg.Overrides {
		d, ok := Lookup(id)
		if !ok {
			return nil, fmt.Errorf("settings: override of unknown setting %s", id)
		}
		if _, err := d.Check(v); err != nil {
			return nil, fmt.Errorf("settings: override: %v", err)
		}
	}
	s := &Service{cfg: cfg, done: make(chan struct{}), rows: map[string]*row{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.stamp = stampOf(cfg.ConfigPath)
	locks, err := s.readLocks()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	changed := false
	for _, d := range Defs {
		r := s.rows[d.ID]
		if r == nil {
			r = &row{Revision: 1}
			s.rows[d.ID] = r
		}
		if !bytes.Equal(r.Lock, locks[d.ID]) {
			// A lock that appeared, changed or went while gff-hab was stopped.
			r.Revision++
			r.Lock = locks[d.ID]
			changed = true
		}
	}
	s.locks = locks
	if changed {
		if err := s.saveLocked(); err != nil {
			s.cfg.Logf("settings: save: %v", err)
		}
	}
	for _, d := range Defs {
		s.cfg.Apply(d.ID, s.settingLocked(d).Value)
	}
	s.mu.Unlock()
	s.wg.Add(1)
	go s.watch()
	return s, nil
}

// Close stops watching the config file.
func (s *Service) Close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.wg.Wait()
}

// load reads the state file. A missing file is no client writes; a file that does not parse is
// logged and set aside, and revisions start again (a new epoch orders nothing across it).
func (s *Service) load() error {
	p := filepath.Join(s.cfg.State, StateFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("settings: %v", err)
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		s.cfg.Logf("settings: %s does not parse (%v); starting from the defaults", p, err)
		return nil
	}
	for id, r := range f.Settings {
		d, ok := Lookup(id)
		if !ok || r == nil {
			continue // a setting this build does not know: dropped at the next save
		}
		if r.Value != nil {
			v, err := d.Check(r.Value)
			if err != nil {
				s.cfg.Logf("settings: %s: stored value dropped: %v", p, err)
				r.Value = nil
			} else {
				r.Value = v
			}
		}
		if r.Revision == 0 {
			r.Revision = 1
		}
		s.rows[id] = r
	}
	return nil
}

// readLocks reads the [settings] table of the config file and adds the overrides over it.
func (s *Service) readLocks() (map[string]json.RawMessage, error) {
	locks := map[string]json.RawMessage{}
	if p := s.cfg.ConfigPath; p != "" {
		b, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("settings: %v", err)
		default:
			var fc struct {
				Settings map[string]any `toml:"settings"`
			}
			if _, err := toml.Decode(string(b), &fc); err != nil {
				return nil, fmt.Errorf("settings: %s: %w", p, err)
			}
			ids := make([]string, 0, len(fc.Settings))
			for id := range fc.Settings {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				d, ok := Lookup(id)
				if !ok {
					s.cfg.Logf("settings: %s: [settings] %q is not a hab-side setting; left out", p, id)
					continue
				}
				v, err := d.CheckGo(fc.Settings[id])
				if err != nil {
					s.cfg.Logf("settings: %s: [settings] %v; left out", p, err)
					continue
				}
				locks[id] = v
			}
		}
	}
	for id, v := range s.cfg.Overrides {
		d, _ := Lookup(id)
		cv, _ := d.Check(v)
		locks[id] = cv
	}
	return locks, nil
}

// watch checks the config file every poll.
func (s *Service) watch() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.Poll)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.check()
	}
}

// check reloads the config file once its size and modification time changed and then held for
// one poll, so a file in the middle of a write is not read (mesh.md 4.4).
func (s *Service) check() {
	if s.cfg.ConfigPath == "" {
		return
	}
	st := stampOf(s.cfg.ConfigPath)
	s.mu.Lock()
	settled := st != s.stamp && st == s.pending
	if st == s.stamp {
		s.pending = fileStamp{}
	} else {
		s.pending = st
	}
	s.mu.Unlock()
	if settled {
		s.Reload()
	}
}

// Reload reads the config file again. A lock that appears, changes or goes raises the
// setting's revision and sends settings.updated. A file that does not parse keeps the locks
// from the last good read.
func (s *Service) Reload() {
	st := stampOf(s.cfg.ConfigPath)
	locks, err := s.readLocks()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamp, s.pending = st, fileStamp{}
	if err != nil {
		s.cfg.Logf("%v; keeping the settings it set before", err)
		return
	}
	var changed []Def
	for _, d := range Defs {
		if !bytes.Equal(s.locks[d.ID], locks[d.ID]) {
			r := s.rows[d.ID]
			r.Revision++
			r.Lock = locks[d.ID]
			changed = append(changed, d)
		}
	}
	s.locks = locks
	if len(changed) == 0 {
		return
	}
	if err := s.saveLocked(); err != nil {
		s.cfg.Logf("settings: save: %v", err)
	}
	for _, d := range changed {
		s.emitLocked(d)
	}
}

// settingLocked builds the API form of one setting.
func (s *Service) settingLocked(d Def) api.Setting {
	r := s.rows[d.ID]
	out := api.Setting{ID: d.ID, Default: d.Default, Revision: r.Revision}
	switch {
	case s.locks[d.ID] != nil:
		out.Value, out.Source = s.locks[d.ID], api.SettingSourceConfig
	case r.Value != nil:
		out.Value, out.Source = r.Value, api.SettingSourceClient
	default:
		out.Value, out.Source = d.Default, api.SettingSourceDefault
	}
	return out
}

func (s *Service) emitLocked(d Def) {
	st := s.settingLocked(d)
	s.cfg.Apply(d.ID, st.Value)
	s.cfg.Publish(structure.Event{Type: api.EventSettingsUpdated, ID: d.ID, Revision: st.Revision,
		Data: api.SettingUpdated{Value: st.Value, Source: st.Source}})
}

// List returns every hab-side setting, for the host snapshot.
func (s *Service) List() []api.Setting {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.Setting, 0, len(Defs))
	for _, d := range Defs {
		out = append(out, s.settingLocked(d))
	}
	return out
}

// Value returns the value the hab enforces now for one setting.
func (s *Service) Value(id string) json.RawMessage {
	d, ok := Lookup(id)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settingLocked(d).Value
}

// Get serves settings.get: every setting, or the named ones. An id this hab does not know is
// left out, so a newer client can ask for a setting an older hab lacks.
func (s *Service) Get(p api.SettingsGetParams) api.SettingsResult {
	if p.IDs == nil {
		return api.SettingsResult{Settings: s.List()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.Setting{}
	seen := map[string]bool{}
	for _, id := range p.IDs {
		if d, ok := Lookup(id); ok && !seen[id] {
			seen[id] = true
			out = append(out, s.settingLocked(d))
		}
	}
	return api.SettingsResult{Settings: out}
}

// refuse builds a settings.set error that names its row (app.md 20.5).
func refuse(kind api.ErrorKind, id, format string, args ...any) *control.Error {
	e := control.Errorf(kind, format, args...)
	e.Data.ID = id
	return e
}

// Set serves settings.set: every row or none. Rows are checked in order; the first refused row
// refuses the call, and the error's data.id names it. Each row is checked for an unknown id
// (not_found), a lock (locked), a stale revision (conflict) and its value (invalid), in that
// order. A written row's source becomes client, even when its value is unchanged.
func (s *Service) Set(p api.SettingsSetParams) (api.SettingsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type write struct {
		d Def
		v json.RawMessage
	}
	var ws []write
	seen := map[string]bool{}
	for _, w := range p.Values {
		d, ok := Lookup(w.ID)
		if !ok {
			return api.SettingsResult{}, refuse(api.ErrorKindNotFound, w.ID, "settings.set: %s is not a setting on this hab", w.ID)
		}
		if seen[w.ID] {
			return api.SettingsResult{}, refuse(api.ErrorKindInvalid, w.ID, "settings.set: %s appears twice", w.ID)
		}
		seen[w.ID] = true
		if s.locks[w.ID] != nil {
			return api.SettingsResult{}, refuse(api.ErrorKindLocked, w.ID, "settings.set: %s is set in the hab's config", w.ID)
		}
		if r := s.rows[w.ID]; w.Revision != r.Revision {
			return api.SettingsResult{}, refuse(api.ErrorKindConflict, w.ID, "settings.set: %s is at revision %d, not %d", w.ID, r.Revision, w.Revision)
		}
		v, err := d.Check(w.Value)
		if err != nil {
			return api.SettingsResult{}, refuse(api.ErrorKindInvalid, w.ID, "settings.set: %v", err)
		}
		ws = append(ws, write{d, v})
	}
	if len(ws) == 0 {
		return api.SettingsResult{Settings: []api.Setting{}}, nil
	}
	old := map[string]row{}
	for _, w := range ws {
		r := s.rows[w.d.ID]
		old[w.d.ID] = *r
		r.Value = w.v
		r.Revision++
	}
	if err := s.saveLocked(); err != nil {
		for id, r := range old {
			*s.rows[id] = r
		}
		return api.SettingsResult{}, refuse(api.ErrorKindUnavailable, ws[0].d.ID, "settings.set: saving the settings failed: %v", err)
	}
	out := make([]api.Setting, 0, len(ws))
	for _, w := range ws {
		s.emitLocked(w.d)
		out = append(out, s.settingLocked(w.d))
	}
	return api.SettingsResult{Settings: out}, nil
}

// saveLocked writes the state file through a temporary file synced before its rename.
func (s *Service) saveLocked() error {
	b, err := json.MarshalIndent(stateFile{Settings: s.rows}, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(s.cfg.State, StateFile), append(b, '\n'))
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
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
