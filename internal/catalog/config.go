package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
)

// DefaultAlertAfter is how long a host marked always on stays offline before one notification
// goes out (mesh.md 8).
const DefaultAlertAfter = 30 * time.Minute

// Declared is the [catalog] section of the hab's own config file: the facts the user declares
// for this host (mesh.md 5.1, 8).
type Declared struct {
	Capabilities []string             // extra capability words
	Services     []api.CatalogService // name, URL, optional health URL
	Actions      []api.CatalogAction  // quick actions
	ProjectRoots []string             // absolute folders to scan for projects
	AlwaysOn     bool                 // the always-on mark
	AlertAfter   time.Duration        // how long a marked peer stays offline before the alert
}

type fileConfig struct {
	Catalog struct {
		Capabilities []string `toml:"capabilities"`
		Services     []struct {
			Name   string `toml:"name"`
			URL    string `toml:"url"`
			Health string `toml:"health"`
		} `toml:"services"`
		Actions []struct {
			Name    string `toml:"name"`
			Command string `toml:"command"`
		} `toml:"actions"`
		ProjectRoots  []string `toml:"project_roots"`
		AlwaysOn      bool     `toml:"always_on"`
		AlwaysOnAlert string   `toml:"always_on_alert"`
	} `toml:"catalog"`
}

// ConfigPath is the hab's own config file: $GFF_HAB_CONFIG, else gff/hab.toml beside the peer
// file, under the user config directory on macOS and under $XDG_CONFIG_HOME or ~/.config
// elsewhere.
func ConfigPath(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if p := getenv("GFF_HAB_CONFIG"); p != "" {
		return p, nil
	}
	if goos != "darwin" {
		if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "gff", "hab.toml"), nil
		}
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "gff", "hab.toml"), nil
	}
	return filepath.Join(h, ".config", "gff", "hab.toml"), nil
}

// DefaultConfigPath is ConfigPath for this process.
func DefaultConfigPath() (string, error) {
	return ConfigPath(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// LoadConfig reads the [catalog] section of the hab's config file. A missing file is an empty
// section. Entries that cannot be used are left out and reported as problems; a file that does
// not parse is an error.
func LoadConfig(path string, home func() (string, error)) (Declared, []string, error) {
	d := Declared{AlertAfter: DefaultAlertAfter}
	if path == "" {
		return d, nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil, nil
	}
	if err != nil {
		return d, nil, err
	}
	var fc fileConfig
	if _, err := toml.Decode(string(b), &fc); err != nil {
		return d, nil, fmt.Errorf("%s: %w", path, err)
	}
	c := fc.Catalog
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, path+": catalog: "+fmt.Sprintf(format, args...))
	}
	for _, w := range c.Capabilities {
		if w = strings.TrimSpace(w); w != "" {
			d.Capabilities = append(d.Capabilities, w)
		}
	}
	for _, s := range c.Services {
		if s.Name == "" || s.URL == "" {
			bad("a service needs a name and a url")
			continue
		}
		d.Services = append(d.Services, api.CatalogService{Name: s.Name, URL: s.URL, Health: s.Health})
	}
	for _, a := range c.Actions {
		if a.Name == "" || a.Command == "" {
			bad("an action needs a name and a command")
			continue
		}
		d.Actions = append(d.Actions, api.CatalogAction{Name: a.Name, Command: a.Command})
	}
	for _, r := range c.ProjectRoots {
		if r == "~" || strings.HasPrefix(r, "~/") {
			h, err := home()
			if err != nil {
				bad("project root %q: %v", r, err)
				continue
			}
			r = filepath.Join(h, strings.TrimPrefix(r, "~"))
		}
		if !filepath.IsAbs(r) {
			bad("project root %q is not an absolute path", r)
			continue
		}
		d.ProjectRoots = append(d.ProjectRoots, filepath.Clean(r))
	}
	d.AlwaysOn = c.AlwaysOn
	if c.AlwaysOnAlert != "" {
		t, err := time.ParseDuration(c.AlwaysOnAlert)
		if err != nil || t <= 0 {
			bad("always_on_alert %q is not a positive duration; using %v", c.AlwaysOnAlert, DefaultAlertAfter)
		} else {
			d.AlertAfter = t
		}
	}
	return d, problems, nil
}
