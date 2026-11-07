// Package mcpreg is the MCP registry: one neutral server list in Flightdeck's config, rendered
// into each harness's own MCP config (docs/specs/instructions-mcp-flight-rules.md 8).
package mcpreg

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// Harness names a harness; the registry shares the instruction renderer's names.
type Harness = instructions.Harness

// Rendered lists the harnesses with a renderer, in report order. pi is accepted in the config
// and has none yet.
var Rendered = []Harness{instructions.Claude, instructions.Codex, instructions.Opencode}

// Transports.
const (
	Stdio = "stdio"
	HTTP  = "http"
)

// Value is one env or header value: a plain value, a variable the harness reads by name, or a
// secret resolved at launch (spec 8.4). It never holds a secret value itself.
type Value struct {
	Literal string   // a plain, non-secret value
	Env     string   // a variable name the harness reads from its own environment
	Prefix  string   // text before the variable's value, such as "Bearer "
	Secret  string   // "<provider>:<reference>", resolved at launch
	Command []string // a command that prints the value, run at launch
	isLit   bool
}

// IsLiteral reports whether the value is a plain string.
func (v Value) IsLiteral() bool { return v.isLit }

// NeedsLaunch reports whether the value is resolved only at launch.
func (v Value) NeedsLaunch() bool { return v.Secret != "" || len(v.Command) > 0 }

// UnmarshalTOML reads a string or a table with env (and prefix), secret or command.
func (v *Value) UnmarshalTOML(data any) error {
	switch d := data.(type) {
	case string:
		*v = Value{Literal: d, isLit: true}
		return nil
	case map[string]any:
		*v = Value{}
		for k, x := range d {
			switch k {
			case "env", "prefix", "secret":
				s, ok := x.(string)
				if !ok {
					return fmt.Errorf("%s must be a string", k)
				}
				switch k {
				case "env":
					v.Env = s
				case "prefix":
					v.Prefix = s
				case "secret":
					v.Secret = s
				}
			case "command":
				arr, ok := x.([]any)
				if !ok {
					return fmt.Errorf("command must be an array of strings")
				}
				for _, a := range arr {
					s, ok := a.(string)
					if !ok {
						return fmt.Errorf("command must be an array of strings")
					}
					v.Command = append(v.Command, s)
				}
			default:
				return fmt.Errorf("unknown key %q (env, prefix, secret or command)", k)
			}
		}
		n := 0
		for _, set := range []bool{v.Env != "", v.Secret != "", len(v.Command) > 0} {
			if set {
				n++
			}
		}
		switch {
		case n != 1:
			return fmt.Errorf("needs exactly one of env, secret or command")
		case v.Prefix != "" && v.Env == "":
			return fmt.Errorf("prefix goes only with env")
		case v.Env != "" && !envName.MatchString(v.Env):
			return fmt.Errorf("env is not a variable name")
		case v.Secret != "" && !validSecretRef(v.Secret):
			return fmt.Errorf("secret is not <provider>:<reference>")
		case len(v.Command) > 0 && v.Command[0] == "":
			return fmt.Errorf("command has an empty program")
		}
		return nil
	}
	return fmt.Errorf("must be a string or a table")
}

// Server is one inventory entry (spec 8.1).
type Server struct {
	Name             string           `toml:"name"`
	Transport        string           `toml:"transport"`
	Command          string           `toml:"command"`
	Args             []string         `toml:"args"`
	Cwd              string           `toml:"cwd"`
	URL              string           `toml:"url"`
	Env              map[string]Value `toml:"env"`
	Headers          map[string]Value `toml:"headers"`
	Hosts            []string         `toml:"hosts"`
	Harnesses        []string         `toml:"harnesses"`
	Auth             string           `toml:"auth"`
	Timeout          int              `toml:"timeout"`
	InstructionsNote string           `toml:"instructions_note"`
}

// Provider is a secret provider (spec 8.5): a command that prints one secret. A reference
// "<provider>:<reference>" runs the provider's command with the reference as its last argument.
type Provider struct {
	Name    string   `toml:"name"`
	Command []string `toml:"command"`
	// Env names variables the command reads from the environment. Codex passes a stdio server
	// only a default set of variables, so a launcher entry lists these in env_vars.
	Env     []string `toml:"env"`
	Timeout int      `toml:"timeout"` // seconds; 0: DefaultSecretTimeout
}

// Config is the [mcp] table.
type Config struct {
	Servers   []Server   `toml:"server"`
	Providers []Provider `toml:"provider"`
	// Write lists the harnesses whose config the user opted in to writing.
	Write []string `toml:"write"`
}

type configFile struct {
	MCP Config `toml:"mcp"`
}

var (
	serverName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	// secretWords mark a key whose literal value is refused (spec 8.2).
	secretWords = []string{"token", "key", "secret", "password", "authorization"}
)

// LoadConfig reads the [mcp] table of a TOML config file. An unknown key in the table is an
// error, so a typo never drops a server silently.
func LoadConfig(path string) (Config, error) {
	var f configFile
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return Config{}, fmt.Errorf("mcp config: %w", err)
	}
	for _, k := range md.Undecoded() {
		if len(k) > 0 && k[0] == "mcp" {
			return Config{}, fmt.Errorf("mcp config %s: unknown key %s", path, k)
		}
	}
	return f.MCP, f.MCP.Validate()
}

func knownHarness(h string) bool { return slices.Contains(instructions.Harnesses, Harness(h)) }

// Validate checks every server and the write list.
func (c Config) Validate() error {
	seen := map[string]bool{}
	for i, s := range c.Servers {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("%d", i+1)
		}
		if err := s.validate(); err != nil {
			return fmt.Errorf("mcp server %s: %w", label, err)
		}
		if seen[s.Name] {
			return fmt.Errorf("mcp server %s: the name is used twice", s.Name)
		}
		seen[s.Name] = true
	}
	providers := map[string]bool{}
	for i, p := range c.Providers {
		label := p.Name
		if label == "" {
			label = fmt.Sprintf("%d", i+1)
		}
		if err := p.validate(); err != nil {
			return fmt.Errorf("mcp provider %s: %w", label, err)
		}
		if providers[p.Name] {
			return fmt.Errorf("mcp provider %s: the name is used twice", p.Name)
		}
		providers[p.Name] = true
	}
	for _, s := range c.Servers {
		for _, m := range []map[string]Value{s.Env, s.Headers} {
			for _, k := range sortedKeys(m) {
				if name := m[k].provider(); name != "" && !providers[name] {
					return fmt.Errorf("mcp server %s: %s names provider %q, which no [[mcp.provider]] defines", s.Name, k, name)
				}
			}
		}
	}
	for _, h := range c.Write {
		if !knownHarness(h) {
			return fmt.Errorf("mcp write: unknown harness %q", h)
		}
	}
	return nil
}

func (s Server) validate() error {
	if !serverName.MatchString(s.Name) {
		return fmt.Errorf("name %q: use letters, digits, _ and -", s.Name)
	}
	switch s.Transport {
	case Stdio:
		if s.Command == "" {
			return fmt.Errorf("stdio needs command")
		}
		if s.URL != "" || len(s.Headers) > 0 {
			return fmt.Errorf("stdio takes no url or headers")
		}
		if s.Auth == "oauth" {
			return fmt.Errorf("oauth goes only with http")
		}
	case HTTP:
		if s.Command != "" || len(s.Args) > 0 || s.Cwd != "" || len(s.Env) > 0 {
			return fmt.Errorf("http takes no command, args, cwd or env")
		}
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url %q is not an http or https URL", s.URL)
		}
		if _, set := u.User.Password(); set {
			return fmt.Errorf("url holds a password; reference it from a header instead")
		}
	default:
		return fmt.Errorf("transport %q is not stdio or http", s.Transport)
	}
	switch s.Auth {
	case "", "none", "oauth":
	default:
		return fmt.Errorf("auth %q is not none or oauth", s.Auth)
	}
	if s.Timeout < 0 {
		return fmt.Errorf("timeout is negative")
	}
	for _, h := range s.Harnesses {
		if !knownHarness(h) {
			return fmt.Errorf("unknown harness %q", h)
		}
	}
	for k, v := range s.Env {
		if !envName.MatchString(k) {
			return fmt.Errorf("env %q is not a variable name", k)
		}
		if v.IsLiteral() && secretLike(k) {
			return fmt.Errorf("env %s looks like a secret and holds a plain value; reference it with env, secret or command", k)
		}
	}
	for k, v := range s.Headers {
		if !headerName.MatchString(k) {
			return fmt.Errorf("header %q is not a header name", k)
		}
		if v.IsLiteral() && secretLike(k) {
			return fmt.Errorf("header %s looks like a secret and holds a plain value; reference it with env, secret or command", k)
		}
	}
	return nil
}

// validSecretRef reports whether a secret reference is "<provider>:<reference>" with both parts.
// Errors never quote it: a user may paste the secret itself there by mistake.
func validSecretRef(s string) bool {
	name, ref, ok := strings.Cut(s, ":")
	return ok && serverName.MatchString(name) && ref != ""
}

// MaxSecretTimeout bounds a provider's timeout, in seconds.
const MaxSecretTimeout = 300

func (p Provider) validate() error {
	switch {
	case !serverName.MatchString(p.Name):
		return fmt.Errorf("name %q: use letters, digits, _ and -", p.Name)
	case len(p.Command) == 0 || p.Command[0] == "":
		return fmt.Errorf("command needs a program")
	case p.Timeout < 0 || p.Timeout > MaxSecretTimeout:
		return fmt.Errorf("timeout must be 0 to %d seconds", MaxSecretTimeout)
	}
	for _, e := range p.Env {
		if !envName.MatchString(e) {
			return fmt.Errorf("env %q is not a variable name", e)
		}
	}
	return nil
}

// provider is the provider a secret reference names, or "".
func (v Value) provider() string {
	name, _, _ := strings.Cut(v.Secret, ":")
	return name
}

// provider finds a provider by name.
func (c Config) provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// Server finds a server by name.
func (c Config) Server(name string) (Server, bool) {
	for _, s := range c.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return Server{}, false
}

func secretLike(key string) bool {
	k := strings.ToLower(key)
	for _, w := range secretWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// applies reports whether a server goes to a harness on a host.
func (s Server) applies(h Harness, host string) bool {
	if len(s.Hosts) > 0 && !slices.Contains(s.Hosts, host) {
		return false
	}
	return len(s.Harnesses) == 0 || slices.Contains(s.Harnesses, string(h))
}

func (c Config) writeEnabled(h Harness) bool { return slices.Contains(c.Write, string(h)) }
