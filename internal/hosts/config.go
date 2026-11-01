package hosts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/internal/habconn"
)

// EndpointKind says how an endpoint reaches a hab (mesh.md 3.1).
type EndpointKind int

const (
	EndpointSocket  EndpointKind = iota + 1 // the hab's local socket path
	EndpointSSH                             // an SSH destination: the system ssh runs gff-hab connect
	EndpointCommand                         // a command whose stdio carries the stream, as -via
)

// Endpoint is one way to reach a hab.
type Endpoint struct {
	Kind  EndpointKind
	Value string // the path, the destination, or the command
}

// ParseEndpoint reads an endpoint as gff's config and flags write it: an absolute path is a
// socket, "cmd:<command>" is a command run under /bin/sh -c, and anything else is an SSH
// destination, an alias from the user's SSH config included.
func ParseEndpoint(s string) (Endpoint, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Endpoint{}, errors.New("empty endpoint")
	case strings.HasPrefix(s, "/"):
		return Endpoint{EndpointSocket, s}, nil
	case strings.HasPrefix(s, "cmd:"):
		c := strings.TrimSpace(strings.TrimPrefix(s, "cmd:"))
		if c == "" {
			return Endpoint{}, errors.New("cmd: endpoint without a command")
		}
		return Endpoint{EndpointCommand, c}, nil
	case strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t"):
		return Endpoint{}, fmt.Errorf("endpoint %q: an SSH destination cannot start with - or hold a space", s)
	}
	return Endpoint{EndpointSSH, s}, nil
}

func (e Endpoint) String() string {
	if e.Kind == EndpointCommand {
		return "cmd:" + e.Value
	}
	return e.Value
}

// Transport makes the habconn transport for the endpoint.
func (e Endpoint) Transport() (habconn.Transport, error) {
	switch e.Kind {
	case EndpointSocket:
		return habconn.UnixTransport{Path: e.Value}, nil
	case EndpointSSH:
		return habconn.SSHTransport{Host: e.Value}, nil
	case EndpointCommand:
		return habconn.CommandTransport{Argv: []string{"/bin/sh", "-c", e.Value}}, nil
	}
	return nil, fmt.Errorf("endpoint %q: unknown kind", e.Value)
}

// fileConfig is gff's config file. Its hosts take the shape of mesh peer entries.
type fileConfig struct {
	Hosts []Entry `toml:"hosts"`
}

// Entry is one host as a config file writes it. gff's [[hosts]] and the hab's [[peers]]
// (mesh.md 3.1) share it.
type Entry struct {
	Label     string   `toml:"label"`
	Endpoints []string `toml:"endpoints"`
	DeviceID  string   `toml:"device_id"`
}

// ConfigPath is gff's config file: $GFF_CONFIG, else gff/config.toml under the user config
// directory on macOS, under $XDG_CONFIG_HOME or ~/.config elsewhere.
func ConfigPath(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if p := getenv("GFF_CONFIG"); p != "" {
		return p, nil
	}
	if goos != "darwin" {
		if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "gff", "config.toml"), nil
		}
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "gff", "config.toml"), nil
	}
	return filepath.Join(h, ".config", "gff", "config.toml"), nil
}

// DefaultConfigPath is ConfigPath for this process.
func DefaultConfigPath() (string, error) {
	return ConfigPath(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// LoadConfig reads the hosts in gff's config file. A missing file is no hosts and no error. An
// entry with no label, a label given twice or a bad endpoint is reported in problems, and the
// good entries still load (mesh.md 3.1, bad lines).
func LoadConfig(path string) (specs []Spec, problems []string, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var fc fileConfig
	md, err := toml.Decode(string(b), &fc)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range md.Undecoded() {
		problems = append(problems, fmt.Sprintf("%s: unknown key %s", path, k))
	}
	specs, more := ParseEntries(path, "host", fc.Hosts, HeaderLines(b, "hosts"))
	return specs, append(problems, more...), nil
}

// ParseEntries checks config entries and makes their specs. path names the file, what names
// an entry ("host", "peer"), and lines[i], when known, is the line of entry i's table header.
// An entry with no label, a label given twice, no endpoints or a bad endpoint is a problem, by
// line, and the good entries still load (mesh.md 3.1, bad lines).
func ParseEntries(path, what string, entries []Entry, lines []int) (specs []Spec, problems []string) {
	seen := map[string]bool{}
	for i, h := range entries {
		where := fmt.Sprintf("%s: %s %d", path, what, i+1)
		if len(lines) == len(entries) {
			where = fmt.Sprintf("%s:%d: %s", path, lines[i], what)
		}
		if h.Label != "" {
			where += fmt.Sprintf(" %q", h.Label)
		}
		switch {
		case h.Label == "":
			problems = append(problems, where+": no label")
			continue
		case seen[h.Label]:
			problems = append(problems, where+": label given twice")
			continue
		case len(h.Endpoints) == 0:
			problems = append(problems, where+": no endpoints")
			continue
		}
		sp := Spec{Label: h.Label, DeviceID: h.DeviceID}
		bad := false
		for _, e := range h.Endpoints {
			ep, err := ParseEndpoint(e)
			if err != nil {
				problems = append(problems, where+": "+err.Error())
				bad = true
				break
			}
			sp.Endpoints = append(sp.Endpoints, ep)
		}
		if bad {
			continue
		}
		seen[h.Label] = true
		specs = append(specs, sp)
	}
	return specs, problems
}

// HeaderLines returns the 1-based line of each [[table]] header in a TOML file, in order. It
// reads headers written on a line of their own, as people and gff-hab write them. ParseEntries
// uses the lines only when there is one per entry.
func HeaderLines(b []byte, table string) []int {
	var lines []int
	for i, l := range strings.Split(string(b), "\n") {
		if c := strings.IndexByte(l, '#'); c >= 0 {
			l = l[:c]
		}
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[[") && strings.HasSuffix(l, "]]") &&
			strings.TrimSpace(l[2:len(l)-2]) == table {
			lines = append(lines, i+1)
		}
	}
	return lines
}
