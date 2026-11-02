// Package capcom holds the pure parts of the Capcom session host (docs/specs/capcom.md 7): the
// [capcom] section of the hab's config file, the folder each Capcom session runs in with its
// manual and harness settings, how each harness starts and resumes there, the assignment rule
// that picks the home host, and the state file. gff-hab wires it to the structure, the holder
// link and the catalog. Pure Go.
package capcom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/BurntSushi/toml"
)

// Capability is the capability a hab that runs Capcom sends (hab-logic-api.md 3.4).
const Capability = "capcom"

// The harnesses that can run Capcom (capcom.md 7.3).
const (
	Claude   = "claude"
	Codex    = "codex"
	Opencode = "opencode"
)

// Harnesses lists them in a stable order.
var Harnesses = []string{Claude, Codex, Opencode}

// DefaultHarness runs Capcom when the config names none. One key changes it.
const DefaultHarness = Claude

// Config is the [capcom] section of the hab's config file.
type Config struct {
	Harness string // claude, codex or opencode
}

type fileConfig struct {
	Capcom struct {
		Harness string `toml:"harness"`
	} `toml:"capcom"`
}

// LoadConfig reads the [capcom] section. A missing file or section gives the defaults. A
// harness outside Harnesses is a problem: it is left out, and the default runs. A file that
// does not parse is an error, with the defaults.
func LoadConfig(path string) (Config, []string, error) {
	c := Config{Harness: DefaultHarness}
	if path == "" {
		return c, nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, err
	}
	var f fileConfig
	if _, err := toml.Decode(string(b), &f); err != nil {
		return c, nil, fmt.Errorf("%s: %v", path, err)
	}
	var problems []string
	switch h := f.Capcom.Harness; {
	case h == "":
	case slices.Contains(Harnesses, h):
		c.Harness = h
	default:
		problems = append(problems, fmt.Sprintf("%s: [capcom] harness %q: want one of %q; using %s", path, h, Harnesses, DefaultHarness))
	}
	return c, problems, nil
}

// Belief is one hab's view of the assignment: the highest counter it has seen, and the home
// host's device id at that counter (capcom.md 7.3).
type Belief struct {
	Assignment uint64
	Home       string
}

// Beats reports whether a replaces b: a higher counter, or the same counter with a lower home
// device id (two assignments made at once on two sides of a split).
func (a Belief) Beats(b Belief) bool {
	if a.Assignment != b.Assignment {
		return a.Assignment > b.Assignment
	}
	return a.Home != "" && (b.Home == "" || a.Home < b.Home)
}

// Session is one Capcom session this hab started.
type Session struct {
	ID      string    `json:"id"`      // the gff-hab session id
	Dir     string    `json:"dir"`     // its folder
	Harness string    `json:"harness"` // the harness it runs; a resume keeps it
	Resume  string    `json:"resume,omitempty"`
	Created time.Time `json:"created"`
}

// State is capcom.json: the assignment this hab believes, its standing conversation and its
// Capcom sessions.
type State struct {
	Assignment uint64    `json:"assignment"`
	Home       string    `json:"home,omitempty"`
	Standing   string    `json:"standing,omitempty"` // session id; empty: the next open starts one
	Sessions   []Session `json:"sessions"`
}

// StateFile is the name of the state file in the state directory.
const StateFile = "capcom.json"

// Belief returns the state's assignment.
func (s State) Belief() Belief { return Belief{s.Assignment, s.Home} }

// LoadState reads capcom.json from dir. A missing file is a first start. A file that does not
// parse is set aside as capcom.json.bad and the state starts empty; the problem is returned
// with it.
func LoadState(dir string) (State, error) {
	p := filepath.Join(dir, StateFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		os.Rename(p, p+".bad")
		return State{}, fmt.Errorf("%s: %v; kept as %s.bad, starting empty", p, err, StateFile)
	}
	return st, nil
}

// SaveState writes capcom.json through a temporary file and a rename.
func SaveState(dir string, st State) error {
	if st.Sessions == nil {
		st.Sessions = []Session{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, StateFile), append(b, '\n'), 0o600)
}

// writeFile writes p through a temporary file in its folder, synced, then renamed into place.
// A file or link left at the temporary name is removed first, and the new one is made with
// O_EXCL, so a planted link is never written through.
func writeFile(p string, b []byte, mode fs.FileMode) error {
	tmp := p + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
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
	if err == nil {
		err = os.Chmod(tmp, mode) // the umask may have narrowed it
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}
