// Package lbextra runs Logbook's always-on extras (logbook.md 13): the nightly backup, the git
// export and vector search, and writes the Markdown tree that the git export and
// `gff lb export --format markdown` share.
package lbextra

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultKeep is how many backups a hab keeps when logbook.backup.keep is unset (13).
const DefaultKeep = 30

// BackupDir is the backup folder under the state directory when logbook.backup.path is unset.
const BackupDir = "logbook-backups"

// VectorIndex is the vector index file under the state directory when logbook.vectors.index is
// unset.
const VectorIndex = "logbook-vectors.gob"

// Config is the [logbook.backup], [logbook.export] and [logbook.vectors] sections of the hab's
// config file.
type Config struct {
	Backup struct {
		Path string `toml:"path"`
		Keep int    `toml:"keep"`
	} `toml:"backup"`
	Export struct {
		Git string `toml:"git"`
	} `toml:"export"`
	Vectors struct {
		URL   string `toml:"url"`
		Model string `toml:"model"`
		Index string `toml:"index"`
	} `toml:"vectors"`
}

// VectorsOn reports whether the config turns vector search on.
func (c Config) VectorsOn() bool { return c.Vectors.URL != "" }

// LoadConfig reads the sections from the hab's config file, with paths made absolute (a leading
// ~/ is the home folder) and the defaults filled in under state. A missing file is no config.
func LoadConfig(path, state string) (Config, error) {
	var f struct {
		Logbook Config `toml:"logbook"`
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		if err == nil {
			if _, err := toml.Decode(string(b), &f); err != nil {
				return Config{}, fmt.Errorf("%s: %v", path, err)
			}
		}
	}
	c := f.Logbook
	if c.Backup.Keep < 0 {
		return Config{}, fmt.Errorf("%s: [logbook.backup] keep is 1 or more", path)
	}
	if c.Backup.Keep == 0 {
		c.Backup.Keep = DefaultKeep
	}
	var err error
	if c.Backup.Path, err = absPath(c.Backup.Path, filepath.Join(state, BackupDir)); err != nil {
		return Config{}, fmt.Errorf("%s: [logbook.backup] path: %v", path, err)
	}
	if c.Export.Git, err = absPath(c.Export.Git, ""); err != nil {
		return Config{}, fmt.Errorf("%s: [logbook.export] git: %v", path, err)
	}
	if c.Vectors.Index, err = absPath(c.Vectors.Index, filepath.Join(state, VectorIndex)); err != nil {
		return Config{}, fmt.Errorf("%s: [logbook.vectors] index: %v", path, err)
	}
	if c.VectorsOn() && c.Vectors.Model == "" {
		return Config{}, fmt.Errorf("%s: [logbook.vectors] needs a model with its url", path)
	}
	return c, nil
}

func absPath(p, def string) (string, error) {
	if p == "" {
		return def, nil
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	return filepath.Clean(p), nil
}
