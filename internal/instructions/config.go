package instructions

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Harness names a supported harness.
type Harness string

const (
	Claude   Harness = "claude"
	Codex    Harness = "codex"
	Opencode Harness = "opencode"
	Pi       Harness = "pi"
)

// Harnesses lists every harness with a global target, in report order.
var Harnesses = []Harness{Claude, Codex, Opencode, Pi}

// Scopes a source can have (spec 6.1).
const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
)

// Source is one instruction file the user names. There are no fixed layer names: a single file,
// a root file with imports, or a file per host are all sources.
type Source struct {
	Path      string   `toml:"path"`
	Scope     string   `toml:"scope"`
	Project   string   `toml:"project"`   // project scope: the project's repository
	Hosts     []string `toml:"hosts"`     // allow list of host ids; empty means every host
	Harnesses []string `toml:"harnesses"` // allow list; empty means every harness
	Order     int      `toml:"order"`     // position among sources of the same scope
}

// Config is the renderer's part of Flightdeck's config: the [instructions] table.
type Config struct {
	Sources []Source `toml:"source"`
	// Write lists the targets the user opted in to writing, by harness name. A target not
	// listed is shown in the dry run and never written.
	Write []string `toml:"write"`
}

type configFile struct {
	Instructions Config `toml:"instructions"`
}

// LoadConfig reads the [instructions] table of a TOML config file. Source paths are resolved:
// ~/ against home, a relative path against the config file's directory. Unknown keys inside
// [instructions] are an error, so a typo never silently drops a source.
func LoadConfig(path, home string) (Config, error) {
	var f configFile
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return Config{}, fmt.Errorf("instructions config: %w", err)
	}
	for _, k := range md.Undecoded() {
		if len(k) > 0 && k[0] == "instructions" {
			return Config{}, fmt.Errorf("instructions config %s: unknown key %s", path, k)
		}
	}
	c := f.Instructions
	dir := filepath.Dir(path)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for i := range c.Sources {
		if strings.TrimSpace(c.Sources[i].Path) == "" {
			return Config{}, fmt.Errorf("instructions source %d: path is empty", i+1)
		}
		c.Sources[i].Path = resolvePath(c.Sources[i].Path, dir, home)
	}
	return c, c.Validate()
}

// Validate checks the scopes and harness names.
func (c Config) Validate() error {
	for i, s := range c.Sources {
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("instructions source %d: path is empty", i+1)
		}
		switch s.Scope {
		case ScopeGlobal:
		case ScopeProject:
			if s.Project == "" {
				return fmt.Errorf("instructions source %s: project scope needs project", s.Path)
			}
		default:
			return fmt.Errorf("instructions source %s: scope %q is not global or project", s.Path, s.Scope)
		}
		for _, h := range s.Harnesses {
			if !known(h) {
				return fmt.Errorf("instructions source %s: unknown harness %q", s.Path, h)
			}
		}
	}
	for _, h := range c.Write {
		if !known(h) {
			return fmt.Errorf("instructions write: unknown harness %q", h)
		}
	}
	return nil
}

func known(h string) bool { return slices.Contains(Harnesses, Harness(h)) }

// globalSources returns the global sources that apply to a harness on a host, by order. Ties
// keep the config's order.
func (c Config) globalSources(h Harness, host string) []Source {
	var out []Source
	for _, s := range c.Sources {
		if s.Scope != ScopeGlobal {
			continue
		}
		if len(s.Hosts) > 0 && !slices.Contains(s.Hosts, host) {
			continue
		}
		if len(s.Harnesses) > 0 && !slices.Contains(s.Harnesses, string(h)) {
			continue
		}
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b Source) int { return a.Order - b.Order })
	return out
}

func (c Config) writeEnabled(h Harness) bool { return slices.Contains(c.Write, string(h)) }
