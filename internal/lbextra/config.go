// Package lbextra runs Logbook's always-on extras (logbook.md 13): the nightly backup, the git
// export and vector search, and writes the Markdown tree that the git export and
// `gff lb export --format markdown` share.
package lbextra

import (
	"errors"
	"fmt"
	"net/url"
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
		BaseURL    string   `toml:"base_url"`
		Model      string   `toml:"model"`
		KeyCommand []string `toml:"key_command"` // nil: no Authorization header
		Index      string   `toml:"index"`
		// URL is the key before base_url; LoadConfig refuses it so an old config is noticed.
		URL string `toml:"url"`
	} `toml:"vectors"`
}

// VectorsOn reports whether the config turns vector search on.
func (c Config) VectorsOn() bool { return c.Vectors.BaseURL != "" && c.Vectors.Model != "" }

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
	if err := checkVectors(&c); err != nil {
		return Config{}, fmt.Errorf("%s: [logbook.vectors] %v", path, err)
	}
	return c, nil
}

// checkVectors checks base_url, model and key_command with the rules of [summaries]
// (app.md 16.5): neither is off, one without the other is an error.
func checkVectors(c *Config) error {
	v := &c.Vectors
	if v.URL != "" {
		return errors.New("url is replaced by base_url (without /embeddings) and model")
	}
	v.BaseURL, v.Model = strings.TrimSpace(v.BaseURL), strings.TrimSpace(v.Model)
	switch {
	case v.BaseURL == "" && v.Model == "":
		return nil
	case v.BaseURL == "":
		return errors.New("model is set but base_url is not")
	case v.Model == "":
		return errors.New("base_url is set but model is not")
	}
	u, err := url.Parse(v.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("base_url %q is not an http or https URL", v.BaseURL)
	}
	if u.User != nil {
		return errors.New("base_url must not hold a user or password; use key_command")
	}
	if v.KeyCommand != nil && (len(v.KeyCommand) == 0 || strings.TrimSpace(v.KeyCommand[0]) == "") {
		return errors.New("key_command names no program")
	}
	return nil
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
