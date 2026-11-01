package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// manifestVersion is the version of the install record format.
const manifestVersion = 1

// ErrOtherInstall means the state directory holds the record of another install.
var ErrOtherInstall = errors.New("service: the state directory holds another install")

// manifest is the install record, <state>/install.json: which install wrote it, every file it
// wrote and every directory it created. Uninstall removes only what it lists, plus the holder
// plists and holder logs, which it finds by their names.
type manifest struct {
	Version int    `json:"version"`
	Init    Init   `json:"init"`
	Prefix  string `json:"prefix"`
	UnitDir string `json:"unit_dir"`
	// Grants is the GFF_MACOS_GRANTS the jobs got. A later install without the variable keeps
	// it (macos-identity.md 7.9).
	Grants string `json:"grants,omitempty"`
	// Files are the files install wrote, or that systemctl or launchd write for it.
	Files []string `json:"files"`
	// Dirs are the directories install created, in the order made: parents first.
	Dirs []string `json:"dirs"`
}

// manifestPath is the install record of c.
func (c Config) manifestPath() string { return filepath.Join(c.StateDir, "install.json") }

// readManifest reads c's install record. No record is nil and no error.
func readManifest(c Config) (*manifest, error) {
	b, err := os.ReadFile(c.manifestPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("service: install record %s: %w", c.manifestPath(), err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("service: install record %s: version %d, want %d", c.manifestPath(), m.Version, manifestVersion)
	}
	for _, p := range append(slices.Clone(m.Files), m.Dirs...) {
		if err := checkPath("recorded path", p); err != nil {
			return nil, fmt.Errorf("service: install record %s: %w", c.manifestPath(), err)
		}
	}
	return &m, nil
}

// check refuses a record that another install wrote: another init system, prefix or unit
// directory.
func (m *manifest) check(c Config) error {
	if m.Init == c.Init && m.Prefix == c.Prefix && m.UnitDir == c.UnitDir {
		return nil
	}
	return fmt.Errorf("%w: %s was installed with -prefix %s -unit-dir %s (%s). "+
		"Uninstall that install first: gff-hab uninstall -prefix %s -unit-dir %s -state %s",
		ErrOtherInstall, c.StateDir, m.Prefix, m.UnitDir, m.Init, m.Prefix, m.UnitDir, c.StateDir)
}

// save writes the record through a temporary file and a rename.
func (m *manifest) save(c Config) error {
	b, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return err
	}
	return writeAtomic(c.manifestPath(), append(b, '\n'), 0o600)
}

// addDirs records directories that install creates.
func (m *manifest) addDirs(dirs ...string) {
	for _, d := range dirs {
		if !slices.Contains(m.Dirs, d) {
			m.Dirs = append(m.Dirs, d)
		}
	}
}

// willWrite records path and saves the record, before path is written.
func (m *manifest) willWrite(c Config, path string) error {
	if slices.Contains(m.Files, path) {
		return nil
	}
	m.Files = append(m.Files, path)
	return m.save(c)
}

// mkdirAll records each directory of dir that does not exist yet, saves the record, then makes
// them.
func (m *manifest) mkdirAll(c Config, dir string, mode os.FileMode) error {
	if missing := missingDirs(dir); len(missing) > 0 {
		m.addDirs(missing...)
		if err := m.save(c); err != nil {
			return err
		}
	}
	return os.MkdirAll(dir, mode)
}

// missingDirs lists dir and each parent of it that does not exist, parents first.
func missingDirs(dir string) []string {
	var out []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		out = append(out, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	slices.Reverse(out)
	return out
}
