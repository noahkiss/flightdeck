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
	"strings"
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
		if keep[n] || (strings.HasPrefix(n, ".tmp-") && young(d)) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// young reports whether d changed within tmpAge: an install may be writing into it now.
func young(d string) bool {
	fi, err := os.Stat(d)
	return err != nil || time.Since(fi.ModTime()) < tmpAge
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
		if err := copyTree(filepath.Join(o.Source, "node_modules"), filepath.Join(tmp, "node_modules")); err != nil {
			return err
		}
	} else {
		npm := o.NPM
		if npm == "" {
			npm = "npm"
		}
		fmt.Fprintf(out, "installing the Capcom engine's SDKs with npm ci (a download of several hundred MB)\n")
		if _, err := run(ctx, npm, "ci", "--prefix", tmp, "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
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

// copyTree copies the tree src to dst: directories and regular files with their mode bits, and
// links as links.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
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
