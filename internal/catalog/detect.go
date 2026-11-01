package catalog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Facts are the detected facts of the host (mesh.md 5.1).
type Facts struct {
	System       api.CatalogSystem
	Toolchains   []api.CatalogTool
	Harnesses    []api.CatalogTool
	Capabilities []string // detected capability words
}

// probe is one program whose version a catalog lists.
type probe struct {
	name string
	bin  string
	args []string
	goos string // "" for every OS
}

// toolchains are the tools a catalog lists when they are on PATH (mesh.md 5.1).
var toolchains = []probe{
	{name: "git", bin: "git", args: []string{"--version"}},
	{name: "go", bin: "go", args: []string{"version"}},
	{name: "node", bin: "node", args: []string{"--version"}},
	{name: "python", bin: "python3", args: []string{"--version"}},
	{name: "rust", bin: "rustc", args: []string{"--version"}},
	{name: "docker", bin: "docker", args: []string{"--version"}},
	{name: "podman", bin: "podman", args: []string{"--version"}},
	{name: "xcode", bin: "xcodebuild", args: []string{"-version"}, goos: "darwin"},
}

// harnesses are the coding-agent harnesses a catalog lists, by their detection manifest ids.
var harnesses = []probe{
	{name: "claude", bin: "claude", args: []string{"--version"}},
	{name: "codex", bin: "codex", args: []string{"--version"}},
	{name: "opencode", bin: "opencode", args: []string{"--version"}},
}

// probeTimeout bounds one version run.
const probeTimeout = 10 * time.Second

var versionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// Detect reads the host's facts. Each program runs with a time limit, all at once; one that
// is missing or fails is left out.
func Detect(ctx context.Context) Facts {
	f := Facts{System: system(ctx)}
	f.Toolchains = run(ctx, toolchains)
	f.Harnesses = run(ctx, harnesses)
	f.Capabilities = detectedWords(runtime.GOOS, f.Toolchains, has)
	return f
}

func has(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// detectedWords are the capability words that follow from the OS and the tools found.
func detectedWords(goos string, tools []api.CatalogTool, onPath func(string) bool) []string {
	var w []string
	switch goos {
	case "darwin":
		w = append(w, "macos")
	default:
		w = append(w, goos)
	}
	for _, t := range tools {
		switch t.Name {
		case "xcode":
			w = append(w, "xcode")
		case "docker", "podman":
			w = append(w, "docker")
		}
	}
	if onPath("nvidia-smi") {
		w = append(w, "gpu")
	}
	return w
}

// run finds each probe on PATH and reads its version, in the probes' order.
func run(ctx context.Context, ps []probe) []api.CatalogTool {
	out := make([]*api.CatalogTool, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		if p.goos != "" && p.goos != runtime.GOOS {
			continue
		}
		path, err := exec.LookPath(p.bin)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := output(ctx, path, p.args...)
			if err != nil {
				return
			}
			out[i] = &api.CatalogTool{Name: p.name, Version: versionRE.FindString(v)}
		}()
	}
	wg.Wait()
	var tools []api.CatalogTool
	for _, t := range out {
		if t != nil {
			tools = append(tools, *t)
		}
	}
	return tools
}

// output runs a program with a time limit and returns what it printed.
func output(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	cmd.WaitDelay = 200 * time.Millisecond
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

// system reads OS, version, arch, cores and memory.
func system(ctx context.Context) api.CatalogSystem {
	s := api.CatalogSystem{OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU()}
	switch runtime.GOOS {
	case "linux":
		s.OSVersion = osRelease("/etc/os-release")
		s.MemoryBytes = memTotal("/proc/meminfo")
	case "darwin":
		if v, err := output(ctx, "/usr/bin/sw_vers", "-productVersion"); err == nil {
			s.OSVersion = strings.TrimSpace(v)
		}
		if v, err := output(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize"); err == nil {
			s.MemoryBytes, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return s
}

// osRelease is PRETTY_NAME from an os-release file.
func osRelease(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// DetectBasic reads only what the Go runtime knows: OS, arch and cores. It runs no program and
// reads no file. gff-hab uses it when $GFF_HAB_DETECT is "off", as its tests do.
func DetectBasic(context.Context) Facts {
	return Facts{
		System:       api.CatalogSystem{OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU()},
		Capabilities: detectedWords(runtime.GOOS, nil, func(string) bool { return false }),
	}
}

// DetectFor is the detect function $GFF_HAB_DETECT names: "off" is DetectBasic, and empty or
// "on" is Detect, which runs the programs first on PATH. Any other value is an error.
func DetectFor(v string) (func(context.Context) Facts, error) {
	switch v {
	case "", "on":
		return Detect, nil
	case "off":
		return DetectBasic, nil
	}
	return nil, fmt.Errorf("GFF_HAB_DETECT is %q; use on or off", v)
}

// memTotal is MemTotal from /proc/meminfo, in bytes.
func memTotal(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			f := strings.Fields(v)
			if len(f) >= 1 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}
