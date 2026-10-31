package detect

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// vendored holds herdr's manifests as data, with their licence and source commit
// (manifests/LICENSE, manifests/SOURCE).
//
//go:embed manifests/*.toml
var vendored embed.FS

// Where a manifest in a Set came from.
const (
	FromVendored = "vendored"
	FromData     = "data"
)

// Entry is the active manifest for one agent id.
type Entry struct {
	Manifest *Manifest
	From     string // FromVendored or FromData
	Path     string // file path for FromData; manifests/<file> for FromVendored
}

// Problem is a manifest the set could not use. gff-hab reports problems as drift.
type Problem struct {
	Path   string
	Reason string
}

func (p Problem) String() string { return p.Path + ": " + p.Reason }

// Set is the manifests gff-hab uses: the vendored ones, each replaced by a newer valid copy from
// the data directory when there is one.
type Set struct {
	entries  map[string]*Entry // by manifest id
	names    map[string]string // id or alias, lower case, to manifest id
	order    []string          // ids, sorted
	Problems []Problem
}

// Vendored returns the set built into the binary. A vendored manifest that fails to load is a
// Problem, never a panic, so one bad file cannot stop gff-hab.
func Vendored() *Set {
	s := &Set{entries: map[string]*Entry{}, names: map[string]string{}}
	files, _ := fs.Glob(vendored, "manifests/*.toml")
	for _, f := range files {
		if filepath.Base(f) == "index.toml" {
			continue
		}
		data, err := vendored.ReadFile(f)
		if err == nil {
			var m *Manifest
			if m, err = Parse(data); err == nil {
				s.put(&Entry{Manifest: m, From: FromVendored, Path: f})
				continue
			}
		}
		s.Problems = append(s.Problems, Problem{f, err.Error()})
	}
	s.reindex()
	return s
}

// Load returns the vendored set overlaid with the manifests in dir (the hab's data directory
// for manifests, written by Refresh). A copy in dir replaces the vendored one only when it is
// valid and its version is not older. A missing dir is not a problem.
func Load(dir string) *Set {
	s := Vendored()
	if dir == "" {
		return s
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		s.Problems = append(s.Problems, Problem{dir, err.Error()})
		return s
	}
	for _, f := range files {
		if filepath.Base(f) == "index.toml" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			s.Problems = append(s.Problems, Problem{f, err.Error()})
			continue
		}
		m, err := Parse(data)
		if err != nil {
			s.Problems = append(s.Problems, Problem{f, err.Error()})
			continue
		}
		if cur := s.entries[m.ID]; cur != nil && CompareVersions(m.Version, cur.Manifest.Version) < 0 {
			continue // the binary carries a newer copy, after an upgrade; not a problem
		}
		s.put(&Entry{Manifest: m, From: FromData, Path: f})
	}
	s.reindex()
	return s
}

func (s *Set) put(e *Entry) { s.entries[e.Manifest.ID] = e }

func (s *Set) reindex() {
	s.names = map[string]string{}
	s.order = s.order[:0]
	for id := range s.entries {
		s.order = append(s.order, id)
	}
	sort.Strings(s.order)
	// Ids first, so an alias never shadows another manifest's id.
	for _, id := range s.order {
		s.names[strings.ToLower(id)] = id
	}
	for _, id := range s.order {
		for _, a := range s.entries[id].Manifest.Aliases {
			if _, taken := s.names[strings.ToLower(a)]; !taken {
				s.names[strings.ToLower(a)] = id
			}
		}
	}
}

// Entries returns the active manifests, sorted by id.
func (s *Set) Entries() []*Entry {
	out := make([]*Entry, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.entries[id])
	}
	return out
}

// Lookup finds a manifest by id or alias, ignoring case.
func (s *Set) Lookup(name string) *Entry {
	if id, ok := s.names[strings.ToLower(name)]; ok {
		return s.entries[id]
	}
	return nil
}

// Screen runs the screen rules of the manifest for agent id over in. An unknown id gives state
// unknown.
func (s *Set) Screen(id string, in Input) ScreenResult {
	e := s.Lookup(id)
	if e == nil {
		return ScreenResult{State: ScreenUnknown}
	}
	return e.Manifest.Screen(in)
}

// AGENT_KIND values for the harnesses Flightdeck supports (spec, Environment contract). Any
// other detected agent's kind is its manifest id.
const (
	KindClaude   = "claude"
	KindCodex    = "codex"
	KindOpencode = "opencode"
)

// Kind maps a manifest id or alias to its AGENT_KIND value.
func (s *Set) Kind(name string) string {
	if e := s.Lookup(name); e != nil {
		return e.Manifest.ID
	}
	return ""
}

// Process is one process in a pane's foreground process group.
type Process struct {
	PID  int
	Name string   // the executable name the system reports (comm), may be a path
	Args []string // argv, when readable
}

// Foreground is a pane's foreground process group: its leader and the other members.
type Foreground struct {
	Leader  Process
	Members []Process
}

// Detection names the agent found in a pane's foreground.
type Detection struct {
	ID   string // manifest id; also the AGENT_KIND value
	PID  int    // the process that matched
	Name string // the name that matched
}

// ErrNoAgent means no foreground process matches a manifest.
var ErrNoAgent = errors.New("no known agent in the foreground")

// DetectProcess matches the leader first, then the members in the given order, against every
// manifest's id and aliases. The first process with a match wins. Each process offers several
// names: its executable name, the base of argv[0], and for a script interpreter (node, bun,
// deno, python) the base of its script argument, with a script extension removed.
func (s *Set) DetectProcess(fg Foreground) (Detection, error) {
	for _, p := range append([]Process{fg.Leader}, fg.Members...) {
		for _, n := range processNames(p) {
			if id, ok := s.names[n]; ok {
				return Detection{ID: id, PID: p.PID, Name: n}, nil
			}
		}
	}
	return Detection{}, ErrNoAgent
}

var interpreters = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true, "python": true, "python3": true}

func processNames(p Process) []string {
	var out []string
	add := func(path string) {
		n := strings.ToLower(filepath.Base(path))
		for _, ext := range []string{".exe", ".js", ".mjs", ".cjs", ".py"} {
			n = strings.TrimSuffix(n, ext)
		}
		if n != "" && n != "." && n != "/" {
			out = append(out, n)
		}
	}
	if p.Name != "" {
		add(p.Name)
	}
	if len(p.Args) > 0 {
		add(p.Args[0])
		if interpreters[strings.ToLower(filepath.Base(p.Args[0]))] || (p.Name != "" && interpreters[strings.ToLower(filepath.Base(p.Name))]) {
			for _, a := range p.Args[1:] {
				if a == "" || strings.HasPrefix(a, "-") {
					continue
				}
				add(a)
				break
			}
		}
	}
	return out
}

// String describes the set in one line per manifest, for logs and `gff-hab manifests list`.
func (s *Set) String() string {
	var b strings.Builder
	for _, e := range s.Entries() {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", e.Manifest.ID, e.Manifest.Version, e.From, e.Path)
	}
	return b.String()
}
