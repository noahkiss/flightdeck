package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The Capcom engine an install copies (docs/specs/capcom-service.md 3.1). A release carries it as
// a folder, capcom/, that holds the bundled engine and the npm manifest of the SDKs it loads.
// Install copies that folder into a directory of its own, named by a hash of those files, and
// installs the SDKs there:
//
//	<state>/capcom-engine/<hash>/   gff-hab-capcom.mjs, package.json, package-lock.json, node_modules/
//	<state>/capcom-engine/current   -> <hash>
//
// An engine directory is written once, through a temporary directory and a rename. The engine
// that runs keeps its own directory until the next install: install keeps the current and the
// previous one, so an engine still finishing its turn on the old files never loses them.

// EngineFile is the bundled engine's file name.
const EngineFile = "gff-hab-capcom.mjs"

// engineFiles are the files of an engine folder that install copies and hashes.
var engineFiles = []string{EngineFile, "package.json", "package-lock.json"}

// engineTimeout bounds the engine step of an install: npm ci, or the copy of a node_modules
// the source holds. A var for tests. A
// temporary engine directory younger than twice this is another install's, and stays.
var engineTimeout = 10 * time.Minute

// engineProgress is how often install says npm ci is still running. A var for tests.
var engineProgress = 30 * time.Second

// npmArgs are npm ci's flags: production packages with the optional ones, which hold each
// SDK's native binary (a user's omit=optional must not drop them); no install scripts; a short
// fetch timeout and one retry, so an offline install fails in minutes, not after npm's defaults.
var npmArgs = []string{"--omit=dev", "--include=optional", "--ignore-scripts", "--no-audit", "--no-fund",
	"--fetch-retries=1", "--fetch-timeout=60000"}

// PlatformPackages are the folders, relative to an engine directory, that hold the SDKs'
// native binaries for goos and goarch; each is a glob prefix, so a musl build matches too.
func PlatformPackages(goos, goarch string) []string {
	arch := goarch
	if arch == "amd64" {
		arch = "x64"
	}
	return []string{
		filepath.Join("node_modules", "@anthropic-ai", "claude-agent-sdk-"+goos+"-"+arch),
		filepath.Join("node_modules", "@openai", "codex-"+goos+"-"+arch),
	}
}

// checkPlatform fails when dir lacks a platform package of this system.
func checkPlatform(dir string) error {
	for _, p := range PlatformPackages(runtime.GOOS, runtime.GOARCH) {
		m, _ := filepath.Glob(filepath.Join(dir, p) + "*")
		if len(m) == 0 {
			return fmt.Errorf("npm installed no %s: the SDK has no native binary for this system", filepath.Base(p))
		}
	}
	return nil
}

// EngineHash is the short form of an installed engine's directory name, for a notice.
func EngineHash(engine string) string {
	h := filepath.Base(filepath.Dir(engine))
	if len(h) > 8 {
		h = h[:8]
	}
	return h
}

// EngineKept says what runs when this install sets up no new engine (capcom-service.md 3.1):
// the engine already installed, which install keeps, or none.
func EngineKept(state string) string {
	if e := InstalledEngine(state); e != "" {
		return "install keeps the previous engine " + EngineHash(e)
	}
	return "no engine is installed, so the Capcom service will not start"
}

// tail is the last n lines of s, for a notice: npm's output can run to pages.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "...\n" + strings.Join(lines[len(lines)-n:], "\n")
}

// engineRE matches an engine directory name: 16 hex digits of the engine files' hash.
var engineRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// EnginesDir holds the installed engine directories of the install at state.
func EnginesDir(state string) string { return filepath.Join(state, "capcom-engine") }

// EngineCurrent is the link to the installed engine directory gff-hab runs.
func EngineCurrent(state string) string { return filepath.Join(EnginesDir(state), "current") }

// InstalledEngine is the installed engine file of the install at state, through its link
// resolved, so the path names the engine directory and changes when a new engine is installed.
// It returns "" when no engine is installed.
func InstalledEngine(state string) string {
	p, err := filepath.EvalSymlinks(filepath.Join(EngineCurrent(state), EngineFile))
	if err != nil {
		return ""
	}
	return p
}

// EngineFolder reports whether dir holds an engine folder: the bundled engine and its manifest.
func EngineFolder(dir string) bool {
	for _, f := range engineFiles {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// hashEngine names an engine directory: the first 16 hex digits of a SHA-256 over the engine
// files of src.
func hashEngine(src string) (string, error) {
	h := sha256.New()
	for _, f := range engineFiles {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", f, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// engineDirs lists the engine directories under the install at state, temporary ones included.
func engineDirs(state string) ([]string, error) {
	ents, err := os.ReadDir(EnginesDir(state))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() && (engineRE.MatchString(n) || strings.HasPrefix(n, ".tmp-")) {
			out = append(out, filepath.Join(EnginesDir(state), n))
		}
	}
	return out, nil
}

// EngineOptions is the Capcom engine an install copies (Options.Engine).
type EngineOptions struct {
	// Source is the engine folder to install (a release's capcom/).
	Source string
	// NPM is the npm command install runs when Source holds no node_modules.
	NPM string
}

// installEngine copies the engine folder into <state>/capcom-engine/<hash>, installs its SDKs
// there (copies Source's node_modules when it has one, else runs npm ci), points current at it,
// and removes engine directories other than the current and the previous one. Every path it
// writes goes into the install record m first, so uninstall removes the engine too. A directory
// that is there already holds the same engine (its name is the hash) and is kept as it is.
func installEngine(ctx context.Context, c Config, m *manifest, run Runner, out io.Writer, o EngineOptions) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	if !EngineFolder(o.Source) {
		return fmt.Errorf("%s is not a Capcom engine folder (want %s)", o.Source, strings.Join(engineFiles, ", "))
	}
	hash, err := hashEngine(o.Source)
	if err != nil {
		return err
	}
	root := EnginesDir(c.StateDir)
	if err := m.mkdirAll(c, root, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(root, hash)
	if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
		if err := buildEngine(ctx, run, out, o, root, hash, dst); err != nil {
			return err
		}
		fmt.Fprintf(out, "installed the Capcom engine into %s\n", dst)
	} else if err != nil {
		return err
	}
	prev, _ := os.Readlink(EngineCurrent(c.StateDir))
	if err := m.willWrite(c, EngineCurrent(c.StateDir)); err != nil {
		return err
	}
	tmp := filepath.Join(root, ".tmp-current-"+hash)
	os.Remove(tmp)
	if err := os.Symlink(hash, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, EngineCurrent(c.StateDir)); err != nil {
		os.Remove(tmp)
		return err
	}
	keep := map[string]bool{hash: true, filepath.Base(prev): prev != ""}
	dirs, err := engineDirs(c.StateDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range dirs {
		n := filepath.Base(d)
		if keep[n] || (strings.HasPrefix(n, ".tmp-") && young(d, 2*engineTimeout)) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// young reports whether d changed within age: an install may be writing into it now.
func young(d string, age time.Duration) bool {
	fi, err := os.Stat(d)
	return err != nil || time.Since(fi.ModTime()) < age
}

// buildEngine writes the engine directory dst through a temporary directory beside it.
func buildEngine(ctx context.Context, run Runner, out io.Writer, o EngineOptions, root, hash, dst string) error {
	tmp, err := os.MkdirTemp(root, ".tmp-"+hash+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // gone after the rename; cleans up a failure
	for _, f := range engineFiles {
		if err := copyFile(filepath.Join(o.Source, f), filepath.Join(tmp, f), 0o644); err != nil {
			return err
		}
	}
	if fi, err := os.Stat(filepath.Join(o.Source, "node_modules")); err == nil && fi.IsDir() {
		if err := copyTree(ctx, filepath.Join(o.Source, "node_modules"), filepath.Join(tmp, "node_modules")); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("copying node_modules did not finish within the %s limit", limit())
			}
			return err
		}
	} else {
		npm := o.NPM
		if npm == "" {
			npm = "npm"
		}
		fmt.Fprintf(out, "installing the Capcom engine's SDKs with npm ci (a download of several hundred MB, at most %s)\n", limit())
		if err := runNPM(ctx, run, out, npm, tmp); err != nil {
			return err
		}
	}
	if err := checkPlatform(tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Another install wrote the same engine first: its directory is this one.
		if fi, serr := os.Stat(dst); serr == nil && fi.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// runNPM runs npm ci into dir, says every engineProgress that it still runs, and cuts npm's
// output in an error to its last lines.
func runNPM(ctx context.Context, run Runner, out io.Writer, npm, dir string) error {
	done := make(chan struct{})
	var wg sync.WaitGroup
	// The progress lines end before runNPM returns: out is the caller's again after.
	defer func() { close(done); wg.Wait() }()
	start, every := time.Now(), engineProgress
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Fprintf(out, "  npm ci still running (%s)\n", time.Since(start).Round(time.Second))
			}
		}
	}()
	b, err := run(ctx, npm, append([]string{"ci", "--prefix", dir}, npmArgs...)...)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("npm ci did not finish within the %s limit", limit())
	}
	if len(b) > 0 {
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			return fmt.Errorf("npm ci failed (exit %d):\n%s", ee.ExitCode(), tail(string(b), 8))
		}
		return fmt.Errorf("npm ci failed:\n%s", tail(string(b), 8))
	}
	return fmt.Errorf("npm ci failed: %s", tail(err.Error(), 8))
}

// copyFile copies the regular file src to dst with mode.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// limit is engineTimeout in words, for a notice: "10 minutes".
func limit() string {
	if engineTimeout >= time.Minute && engineTimeout%time.Minute == 0 {
		if m := int(engineTimeout / time.Minute); m != 1 {
			return fmt.Sprintf("%d minutes", m)
		}
		return "1 minute"
	}
	return engineTimeout.String()
}

// copyTree copies the tree src to dst, until ctx ends: directories and regular files with their
// mode bits, and links as links.
func copyTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			return os.MkdirAll(to, fi.Mode().Perm())
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, to)
		case fi.Mode().IsRegular():
			return copyFile(p, to, fi.Mode().Perm())
		}
		return nil // a socket or device has no place in node_modules
	})
}
