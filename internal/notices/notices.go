// Package notices builds the third-party notice files Flightdeck ships: NOTICE for the release
// archives and helper bundles, and the app's Acknowledgements.txt. Inputs are pinned and in the
// repository (third_party/notices.toml, the licence texts under third_party/licenses/, go.mod,
// apple/FlightdeckKit/Package.resolved), plus the Go module cache for Go module licence texts.
// Generation fails when a Go module has no licence file, a pinned Swift package has no entry, or
// an entry names a package that is no longer pinned, so a new dependency cannot ship without its
// notice.
package notices

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The two output sets.
const (
	SetRelease = "release"
	SetApp     = "app"
)

// Paths from the repository root.
const (
	ManifestPath        = "third_party/notices.toml"
	SwiftLicensesDir    = "third_party/licenses/swift"
	PackageResolvedPath = "apple/FlightdeckKit/Package.resolved"
	ReleaseOutput       = "NOTICE"
	AppOutput           = "apple/App/Acknowledgements.txt"
)

// Placement says where a component sits in one set.
type Placement struct {
	In      string `toml:"in"`
	Version string `toml:"version"`
}

// Component is a [[component]] entry.
type Component struct {
	Name    string     `toml:"name"`
	URL     string     `toml:"url"`
	License string     `toml:"license"`
	Files   []string   `toml:"files"`
	Release *Placement `toml:"release"`
	App     *Placement `toml:"app"`
}

// SwiftEntry is a [[swift]] entry.
type SwiftEntry struct {
	Identity string `toml:"identity"`
	License  string `toml:"license"`
}

// Manifest is third_party/notices.toml.
type Manifest struct {
	Components []Component  `toml:"component"`
	Swift      []SwiftEntry `toml:"swift"`
}

// GoModule is one Go module linked into a release binary.
type GoModule struct {
	Path     string
	Version  string
	Dir      string   // the module's folder in the module cache
	Binaries []string // the binaries that link it, sorted
}

// Inputs are what Generate reads beyond the repository files.
type Inputs struct {
	Root string // the repository root
	// GoModules lists the Go modules the release binaries link. Used for the release set only.
	GoModules func() ([]GoModule, error)
	// GoRoot is the Go toolchain's GOROOT, for the standard library's licence. Release set only.
	GoRoot string
	// GoBinaries names the binaries the standard library is linked into.
	GoBinaries []string
}

// entry is one rendered section.
type entry struct {
	name, version, url, license, in string
	files                           []string // absolute paths
	labels                          []string // how each file is named in the output
}

// Generate returns the notice file for set.
func Generate(set string, in Inputs) ([]byte, error) {
	if set != SetRelease && set != SetApp {
		return nil, fmt.Errorf("unknown set %q", set)
	}
	m, err := LoadManifest(filepath.Join(in.Root, ManifestPath))
	if err != nil {
		return nil, err
	}
	var entries []entry
	if set == SetRelease {
		if in.GoRoot != "" {
			goVer, err := goToolchain(filepath.Join(in.Root, "go.mod"))
			if err != nil {
				return nil, err
			}
			e := entry{name: "Go standard library and runtime", version: goVer, url: "https://go.dev",
				license: "BSD-3-Clause", in: strings.Join(in.GoBinaries, ", ")}
			for _, f := range []string{"LICENSE", "PATENTS"} {
				p := filepath.Join(in.GoRoot, f)
				if _, err := os.Stat(p); err == nil {
					e.files = append(e.files, p)
					e.labels = append(e.labels, "Go "+f)
				}
			}
			if len(e.files) == 0 {
				return nil, fmt.Errorf("no LICENSE in GOROOT %s", in.GoRoot)
			}
			entries = append(entries, e)
		}
		if in.GoModules != nil {
			mods, err := in.GoModules()
			if err != nil {
				return nil, err
			}
			sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
			for _, mod := range mods {
				files, err := moduleLicenseFiles(mod.Dir)
				if err != nil {
					return nil, err
				}
				if len(files) == 0 {
					return nil, fmt.Errorf("Go module %s %s has no licence file (LICENSE, COPYING, NOTICE or PATENTS); add its notice before linking it", mod.Path, mod.Version)
				}
				e := entry{name: mod.Path, version: mod.Version, url: "https://pkg.go.dev/" + mod.Path,
					license: "see the text below", in: strings.Join(mod.Binaries, ", ")}
				for _, f := range files {
					e.files = append(e.files, filepath.Join(mod.Dir, f))
					e.labels = append(e.labels, mod.Path+"/"+f)
				}
				entries = append(entries, e)
			}
		}
	}
	for _, c := range m.Components {
		p := c.Release
		if set == SetApp {
			p = c.App
		}
		if p == nil {
			continue
		}
		if len(c.Files) == 0 {
			return nil, fmt.Errorf("component %q names no licence file", c.Name)
		}
		ver, err := resolveVersion(in.Root, p.Version)
		if err != nil {
			return nil, fmt.Errorf("component %q: %w", c.Name, err)
		}
		e := entry{name: c.Name, version: ver, url: c.URL, license: c.License, in: p.In}
		for _, f := range c.Files {
			e.files = append(e.files, filepath.Join(in.Root, filepath.FromSlash(f)))
			e.labels = append(e.labels, f)
		}
		entries = append(entries, e)
	}
	if set == SetApp {
		swift, err := swiftEntries(in.Root, m.Swift)
		if err != nil {
			return nil, err
		}
		entries = append(entries, swift...)
	} else if _, err := swiftEntries(in.Root, m.Swift); err != nil {
		// The release file does not list Swift packages, but the check runs for both so a
		// stale manifest fails whichever file is generated.
		return nil, err
	}
	return render(set, entries)
}

// LoadManifest reads and checks third_party/notices.toml.
func LoadManifest(path string) (*Manifest, error) {
	var m Manifest
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown key %s", path, und[0])
	}
	seen := map[string]bool{}
	for _, c := range m.Components {
		if c.Name == "" || c.URL == "" || c.License == "" {
			return nil, fmt.Errorf("%s: component %q needs name, url and license", path, c.Name)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("%s: component %q twice", path, c.Name)
		}
		seen[c.Name] = true
		for _, p := range []*Placement{c.Release, c.App} {
			if p != nil && (p.In == "" || p.Version == "") {
				return nil, fmt.Errorf("%s: component %q: each placement needs in and version", path, c.Name)
			}
		}
		if c.Release == nil && c.App == nil {
			return nil, fmt.Errorf("%s: component %q is in no set", path, c.Name)
		}
	}
	for _, s := range m.Swift {
		if s.Identity == "" || s.License == "" {
			return nil, fmt.Errorf("%s: swift entry %q needs identity and license", path, s.Identity)
		}
	}
	return &m, nil
}

var licenseName = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([._-].*)?$`)

// moduleLicenseFiles lists the licence files at the top of a module folder, sorted.
func moduleLicenseFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && licenseName.MatchString(e.Name()) && !strings.HasSuffix(e.Name(), ".go") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

type resolvedPin struct {
	Identity string `json:"identity"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	State    struct {
		Revision string `json:"revision"`
		Version  string `json:"version"`
	} `json:"state"`
}

// swiftEntries checks the manifest against Package.resolved and returns one entry per pin.
func swiftEntries(root string, manifest []SwiftEntry) ([]entry, error) {
	raw, err := os.ReadFile(filepath.Join(root, PackageResolvedPath))
	if err != nil {
		return nil, err
	}
	var res struct {
		Pins []resolvedPin `json:"pins"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("%s: %w", PackageResolvedPath, err)
	}
	byID := map[string]SwiftEntry{}
	for _, s := range manifest {
		if _, dup := byID[s.Identity]; dup {
			return nil, fmt.Errorf("%s: swift entry %q twice", ManifestPath, s.Identity)
		}
		byID[s.Identity] = s
	}
	pinned := map[string]bool{}
	var out []entry
	sort.Slice(res.Pins, func(i, j int) bool { return res.Pins[i].Identity < res.Pins[j].Identity })
	for _, p := range res.Pins {
		pinned[p.Identity] = true
		s, ok := byID[p.Identity]
		if !ok {
			return nil, fmt.Errorf("Swift package %s is pinned in %s but has no [[swift]] entry in %s; add its licence texts under %s/%s and the entry", p.Identity, PackageResolvedPath, ManifestPath, SwiftLicensesDir, p.Identity)
		}
		dir := filepath.Join(root, SwiftLicensesDir, p.Identity)
		var files, labels []string
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".") {
				rel, _ := filepath.Rel(root, path)
				files = append(files, path)
				labels = append(labels, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil || len(files) == 0 {
			return nil, fmt.Errorf("Swift package %s: no licence texts under %s/%s", p.Identity, SwiftLicensesDir, p.Identity)
		}
		ver := p.State.Version
		if ver == "" {
			ver = "revision " + p.State.Revision
		}
		out = append(out, entry{name: p.Identity, version: ver, url: strings.TrimSuffix(p.Location, ".git"),
			license: s.License, in: "Flightdeck (Swift package)", files: files, labels: labels})
	}
	for _, s := range manifest {
		if !pinned[s.Identity] {
			return nil, fmt.Errorf("%s: swift entry %q is not pinned in %s; remove it and its licence texts", ManifestPath, s.Identity, PackageResolvedPath)
		}
	}
	return out, nil
}

// resolveVersion turns "make:VAR" and "mise:tool" into the pinned value; anything else is literal.
func resolveVersion(root, v string) (string, error) {
	switch {
	case strings.HasPrefix(v, "make:"):
		return assignment(filepath.Join(root, "Makefile"), strings.TrimPrefix(v, "make:"),
			`^%s\s*:?=\s*(\S+)\s*$`)
	case strings.HasPrefix(v, "mise:"):
		return assignment(filepath.Join(root, "mise.toml"), strings.TrimPrefix(v, "mise:"),
			`^%s\s*=\s*"([^"]+)"\s*$`)
	}
	return v, nil
}

func assignment(path, name, pattern string) (string, error) {
	re := regexp.MustCompile(fmt.Sprintf(pattern, regexp.QuoteMeta(name)))
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			return m[1], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s: no value for %s", filepath.Base(path), name)
}

// goToolchain returns the toolchain go.mod pins, else its go line.
func goToolchain(path string) (string, error) {
	if v, err := assignment(path, "toolchain", `^%s\s+(\S+)\s*$`); err == nil {
		return v, nil
	}
	v, err := assignment(path, "go", `^%s\s+(\S+)\s*$`)
	if err != nil {
		return "", err
	}
	return "go" + v, nil
}

const rule = "=============================================================================="

var headers = map[string]string{
	SetRelease: `Flightdeck
Copyright (c) 2026 Noah Kiss. Licensed under the MIT License (see LICENSE).

Third-party notices for the release archives (gff, gff-hab, gff-hab-holder) and the macOS helper
bundles (GFF Hab.app with gff-hab and, when built with one, the gff-hab-speech helper;
GFF Holder.app with gff-hab-holder). Each entry names the component, its version, where it is,
its licence, then the licence text.

The speech model is not part of any binary: gff-hab downloads it on first use. It is Parakeet
TDT 0.6B v2 (NVIDIA, https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2), licensed under
Creative Commons Attribution 4.0 (CC-BY-4.0), converted to ONNX by the sherpa-onnx project.
`,
	SetApp: `Flightdeck
Copyright (c) 2026 Noah Kiss. Licensed under the MIT License.

Third-party notices for the Flightdeck app (iPhone, iPad and Mac). Each entry names the
component, its version, where it is, its licence, then the licence text.

On iPhone and iPad, on-device speech downloads its model on first use; the model is not part of
the app. It is Parakeet TDT 0.6B v2 (NVIDIA), licensed under Creative Commons Attribution 4.0
(CC-BY-4.0).
`,
}

const generatedLine = "Generated by internal/notices from third_party/notices.toml (`make notices`). Do not edit.\n"

// render writes the header, a contents list, then each entry with its licence texts. A text
// identical to one already printed (ignoring whitespace) is named instead of repeated.
func render(set string, entries []entry) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(headers[set])
	b.WriteString("\n")
	b.WriteString(generatedLine)
	b.WriteString("\nContents\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s\n", e.name)
	}
	printed := map[string]string{} // normalized text -> label of the first copy
	for _, e := range entries {
		fmt.Fprintf(&b, "\n%s\n%s\n", rule, e.name)
		fmt.Fprintf(&b, "Version: %s\nSource: %s\nLicence: %s\nIn: %s\n", e.version, e.url, e.license, e.in)
		for i, f := range e.files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.name, err)
			}
			text := cleanText(raw)
			key := strings.Join(strings.Fields(text), " ")
			fmt.Fprintf(&b, "\n--- %s\n", e.labels[i])
			if first, ok := printed[key]; ok {
				fmt.Fprintf(&b, "(The same text as %s above.)\n", first)
				continue
			}
			printed[key] = e.labels[i]
			b.WriteString("\n")
			b.WriteString(text)
		}
	}
	return b.Bytes(), nil
}

// cleanText drops a byte order mark and carriage returns, trims trailing spaces on each line and
// blank lines at either end, and ends the text with one newline.
func cleanText(raw []byte) string {
	s := strings.TrimPrefix(string(raw), "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n") + "\n"
}
