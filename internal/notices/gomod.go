package notices

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Binary is one Go binary we publish, with how it is built.
type Binary struct {
	Name string
	Pkg  string
	Tags string
	Cgo  bool
}

// ReleaseBinaries are the Go binaries in the release archives and the helper bundles.
var ReleaseBinaries = []Binary{
	{Name: "gff", Pkg: "./cmd/gff", Tags: "ghostty", Cgo: true},
	{Name: "gff-hab", Pkg: "./cmd/gff-hab", Tags: "ghostty", Cgo: true},
	{Name: "gff-hab-holder", Pkg: "./cmd/gff-hab-holder"},
	{Name: "gff-hab-speech", Pkg: "./cmd/gff-hab-speech", Tags: "speech", Cgo: true},
}

// ReleasePlatforms are the GOOS/GOARCH pairs we publish.
var ReleasePlatforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}}

// BinaryNames returns the names of bins.
func BinaryNames(bins []Binary) []string {
	var out []string
	for _, b := range bins {
		out = append(out, b.Name)
	}
	return out
}

// ListGoModules asks the go command which modules each binary links on each platform, and where
// each module sits in the module cache (downloading it when it is missing). go list only reads
// build constraints, so it needs no C compiler for another platform.
func ListGoModules(root, goCmd string, bins []Binary, platforms [][2]string) ([]GoModule, error) {
	mainMod, err := run(root, goCmd, nil, "list", "-m")
	if err != nil {
		return nil, err
	}
	mainPath := strings.TrimSpace(mainMod)
	type key struct{ path, version string }
	users := map[key]map[string]bool{}
	for _, p := range platforms {
		for _, b := range bins {
			cgo := "0"
			if b.Cgo {
				cgo = "1"
			}
			env := []string{"GOOS=" + p[0], "GOARCH=" + p[1], "CGO_ENABLED=" + cgo}
			args := []string{"list", "-deps", "-f", "{{with .Module}}{{.Path}} {{.Version}}{{end}}"}
			if b.Tags != "" {
				args = append(args, "-tags", b.Tags)
			}
			args = append(args, b.Pkg)
			out, err := run(root, goCmd, env, args...)
			if err != nil {
				return nil, err
			}
			sc := bufio.NewScanner(strings.NewReader(out))
			for sc.Scan() {
				f := strings.Fields(sc.Text())
				if len(f) != 2 || f[0] == mainPath {
					continue
				}
				k := key{f[0], f[1]}
				if users[k] == nil {
					users[k] = map[string]bool{}
				}
				users[k][b.Name] = true
			}
		}
	}
	var mods []GoModule
	for k, u := range users {
		out, err := run(root, goCmd, nil, "mod", "download", "-json", k.path+"@"+k.version)
		if err != nil {
			return nil, err
		}
		var info struct{ Dir, Error string }
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			return nil, fmt.Errorf("go mod download %s: %w", k.path, err)
		}
		if info.Error != "" || info.Dir == "" {
			return nil, fmt.Errorf("go mod download %s@%s: %s", k.path, k.version, info.Error)
		}
		m := GoModule{Path: k.path, Version: k.version, Dir: info.Dir}
		for _, b := range bins { // in release order, not alphabetical
			if u[b.Name] {
				m.Binaries = append(m.Binaries, b.Name)
			}
		}
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}

// GoRoot returns the GOROOT of the toolchain the go command picks in root.
func GoRoot(root, goCmd string) (string, error) {
	out, err := run(root, goCmd, nil, "env", "GOROOT")
	return strings.TrimSpace(out), err
}

func run(dir, name string, env []string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
