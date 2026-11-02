package structure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// StateFile is the name of the state file inside the state directory.
const StateFile = "structure.json"

// SchemaVersion is the version of the state file this build writes and reads.
const SchemaVersion = 1

// stateFile is the on-disk form. It is internal: the API types may change without a migration
// here, and this file may change without an API revision.
type stateFile struct {
	Schema   int             `json:"schema"`
	Projects []projectRecord `json:"projects"`
	Sessions []sessionRecord `json:"sessions"`
	Panes    []paneRecord    `json:"panes"`
	Gone     []tombstone     `json:"gone,omitempty"`
}

// tombstone keeps a gone pane's final revision, so a pane that comes back after a holder dropped
// out keeps rising revisions.
type tombstone struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

// errNewerSchema marks a state file written by a newer build. It is not corrupt; the store must
// leave it alone.
var errNewerSchema = errors.New("state file is from a newer build")

type projectRecord struct {
	ID         string   `json:"id"`
	Revision   uint64   `json:"revision"`
	Name       string   `json:"name"`
	Remote     string   `json:"remote,omitempty"`
	RootCommit string   `json:"root_commit,omitempty"`
	Sessions   []string `json:"sessions"`
	// Dirs are the project's known working directories (repository top levels), most recently
	// used first. gff-hab reads the repository state from them. Not part of the API.
	Dirs []string `json:"dirs,omitempty"`
	// The last repository state read from Dirs (hab-logic-api.md 3.9). Kept so a restart shows
	// the last values until the next read.
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
	Ahead  *int   `json:"ahead,omitempty"`
	Behind *int   `json:"behind,omitempty"`
}

type sessionRecord struct {
	ID       string          `json:"id"`
	Revision uint64          `json:"revision"`
	Name     string          `json:"name"`
	Project  string          `json:"project,omitempty"`
	Pane     string          `json:"pane,omitempty"`
	State    string          `json:"state"`
	Exit     *api.ExitStatus `json:"exit,omitempty"`
	// Spawning marks a session filed for a pane about to be spawned (SessionSpec.NewPane) that
	// the holders have not reported yet. The first sight of the pane clears it. Only a marked
	// session can be dropped at start as never spawned. Not in the API.
	Spawning bool `json:"spawning,omitempty"`
	// Resuming goes with Spawning when an exited session was pointed at a new pane
	// (ResumeSession). Such a session is never dropped at start; it shows exited. Not in the API.
	Resuming bool `json:"resuming,omitempty"`
	// Kind and Dir mark a Capcom session (capcom.md 2.1, 7.4).
	Kind string `json:"kind,omitempty"`
	Dir  string `json:"dir,omitempty"`
	// Round marks a run session: the round and the run that started it (rounds.md 7.2).
	Round *api.SessionRound `json:"round,omitempty"`
}

// paneRecord keeps what gff-hab owns about a pane: its revision, plus the last size and build it
// saw, so a restart can tell whether the pane changed while gff-hab was down.
type paneRecord struct {
	ID          string `json:"id"`
	Revision    uint64 `json:"revision"`
	HolderBuild string `json:"holder_build"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

// loadState reads the state file. A missing file is an empty state and no problem. Any other
// failure returns an empty state and a problem; the caller keeps the bad file aside.
func loadState(path string) (stateFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return stateFile{Schema: SchemaVersion}, nil
	}
	if err != nil {
		return stateFile{Schema: SchemaVersion}, fmt.Errorf("read state file: %w", err)
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return stateFile{Schema: SchemaVersion}, fmt.Errorf("parse state file: %w", err)
	}
	if st.Schema > SchemaVersion {
		return stateFile{Schema: SchemaVersion}, fmt.Errorf("%w: schema %d, this build reads %d; running without saving", errNewerSchema, st.Schema, SchemaVersion)
	}
	if st.Schema != SchemaVersion {
		return stateFile{Schema: SchemaVersion}, fmt.Errorf("state file schema %d, this build reads %d", st.Schema, SchemaVersion)
	}
	if err := st.check(); err != nil {
		return stateFile{Schema: SchemaVersion}, fmt.Errorf("state file inconsistent: %w", err)
	}
	return st, nil
}

// check rejects a file that parses but cannot be a state this package wrote.
func (st *stateFile) check() error {
	ids := map[string]bool{}
	names := map[string]bool{}
	projects := map[string]*projectRecord{}
	for i := range st.Projects {
		p := &st.Projects[i]
		if p.ID == "" || ids[p.ID] {
			return fmt.Errorf("project id %q empty or repeated", p.ID)
		}
		ids[p.ID] = true
		projects[p.ID] = p
	}
	sessions := map[string]*sessionRecord{}
	for i := range st.Sessions {
		s := &st.Sessions[i]
		if s.ID == "" || ids[s.ID] {
			return fmt.Errorf("session id %q empty or repeated", s.ID)
		}
		if s.Name == "" || names[s.Name] {
			return fmt.Errorf("session name %q empty or repeated", s.Name)
		}
		if s.State != stateRunning && s.State != stateExited {
			return fmt.Errorf("session %s has state %q", s.ID, s.State)
		}
		if s.Project != "" && projects[s.Project] == nil {
			return fmt.Errorf("session %s names missing project %s", s.ID, s.Project)
		}
		ids[s.ID] = true
		names[s.Name] = true
		sessions[s.ID] = s
	}
	linked := map[string]bool{}
	for _, s := range st.Sessions {
		if s.Pane != "" && linked[s.Pane] {
			return fmt.Errorf("pane %s linked by two sessions", s.Pane)
		}
		linked[s.Pane] = true
	}
	listed := map[string]bool{}
	for _, p := range st.Projects {
		for _, sid := range p.Sessions {
			s := sessions[sid]
			if s == nil || s.Project != p.ID || listed[sid] {
				return fmt.Errorf("project %s lists session %s wrongly", p.ID, sid)
			}
			listed[sid] = true
		}
	}
	for _, s := range st.Sessions {
		if s.Project != "" && !listed[s.ID] {
			return fmt.Errorf("session %s missing from its project's order", s.ID)
		}
	}
	panes := map[string]bool{}
	for _, p := range st.Panes {
		if p.ID == "" || panes[p.ID] {
			return fmt.Errorf("pane id %q empty or repeated", p.ID)
		}
		panes[p.ID] = true
	}
	return nil
}

// keepAside renames a bad state file so a later save does not overwrite it. It returns the new
// path.
func keepAside(path string, now time.Time) (string, error) {
	aside := fmt.Sprintf("%s.bad-%s", path, now.UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	return aside, syncDir(filepath.Dir(path))
}

// writeState writes the state atomically: a temp file in the same directory, fsync, rename over
// the old file, fsync the directory.
func writeState(path string, st *stateFile) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

// syncFile is f.Sync; tests replace it.
var syncFile = (*os.File).Sync

// syncDir fsyncs a directory. Some filesystems refuse fsync on a directory with EINVAL; the
// rename is as durable as they make it, so that counts as success.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := syncFile(d); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}
