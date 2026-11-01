package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
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

// On macOS each build runs from its own copy of two helper bundles (docs/specs/macos-identity.md
// 3.2, 6.1):
//
//	<state>/builds/<build>/GFF Holder.app   bundle id <prefix>.hab-holder
//	<state>/builds/<build>/GFF Hab.app      bundle id <prefix>.hab
//	<state>/current -> builds/<build>       for a CLI symlink only; no job names it
//
// TCC keys bundled code on its bundle id and designated requirement, not its path, so holders of
// two builds share every privacy grant. <build> is a hash of both bundles' content, so the same
// bundles always land in the same directory and a changed byte gets a new one. A build directory
// is written once, through a temporary directory and a rename, and never written again: a write
// into a running signed bundle breaks its next launch (1.1).

// The bundle names, and the executables inside them.
const (
	HolderBundle = "GFF Holder.app"
	HabBundle    = "GFF Hab.app"
)

// buildRE matches a build directory name: 16 hex digits of the bundles' content hash.
var buildRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// bundleIDRE matches a bundle identifier: reverse-DNS, ASCII letters, digits, '-' and '.'.
var bundleIDRE = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)

// ValidBuild reports whether b is a build directory name.
func ValidBuild(b string) bool { return buildRE.MatchString(b) }

// BuildsDir holds one directory per installed build (launchd).
func (c Config) BuildsDir() string { return filepath.Join(c.StateDir, "builds") }

// BuildDir is the directory of build c.Build.
func (c Config) BuildDir() string { return filepath.Join(c.BuildsDir(), c.Build) }

// CurrentLink is <state>/current, the link to the newest build.
func (c Config) CurrentLink() string { return filepath.Join(c.StateDir, "current") }

// bundleExe is the executable of bundle in build directory dir.
func bundleExe(dir, bundle, exe string) string {
	return filepath.Join(dir, bundle, "Contents", "MacOS", exe)
}

// bundleSpec is one of the two helper bundles: its name, executable and bundle id suffix.
type bundleSpec struct{ name, exe, suffix string }

var (
	holderSpec = bundleSpec{HolderBundle, HolderBinary, ".hab-holder"}
	habSpec    = bundleSpec{HabBundle, HabBinary, ".hab"}
)

// BundleRoot returns the .app bundle that path is, or that path's executable is in
// (<x>.app/Contents/MacOS/<exe>). Anything else is an error.
func BundleRoot(path string) (string, error) {
	p := filepath.Clean(path)
	if strings.HasSuffix(p, ".app") {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("service: %s is not a bundle directory", path)
		}
		return p, nil
	}
	macos := filepath.Dir(p)
	contents := filepath.Dir(macos)
	root := filepath.Dir(contents)
	if filepath.Base(macos) == "MacOS" && filepath.Base(contents) == "Contents" && strings.HasSuffix(root, ".app") {
		return root, nil
	}
	return "", fmt.Errorf("service: %s is not in an app bundle. On macOS gff-hab and gff-hab-holder run "+
		"from their helper bundles, so that privacy grants survive updates; build them with "+
		"scripts/macos-bundle.sh (-dev for a local build)", path)
}

// plistStrings reads the top-level string values of an XML property list: for each key, its
// string, or the strings of its array. Other values are skipped. It reads only what this package
// and scripts/macos-bundle.sh write.
func plistStrings(r io.Reader) (map[string][]string, error) {
	d := xml.NewDecoder(r)
	out := map[string][]string{}
	depth := 0 // 1 inside <plist>, 2 inside the top dict, 3 inside a top-level value
	key, inKey := "", false
	value := "" // the element name of the current top-level value
	var text strings.Builder
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 {
				return nil, errors.New("service: property list ends early")
			}
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("service: property list: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			text.Reset()
			inKey = depth == 3 && t.Name.Local == "key"
			if depth == 3 {
				value = t.Name.Local
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch {
			case inKey:
				key, inKey = strings.TrimSpace(text.String()), false
			case t.Name.Local == "string" && key != "" && (depth == 3 || depth == 4 && value == "array"):
				out[key] = append(out[key], text.String())
			}
			if depth == 3 && t.Name.Local != "key" {
				key = "" // the value of key is over
			}
			depth--
		}
	}
}

// readPlist reads the top-level strings of the property list at path.
func readPlist(path string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := plistStrings(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// bundleID reads bundle's CFBundleIdentifier and checks that it is spec's bundle: the expected
// executable, and an id ending in spec's suffix (or that suffix plus .dev).
func bundleID(bundle string, spec bundleSpec) (string, error) {
	m, err := readPlist(filepath.Join(bundle, "Contents", "Info.plist"))
	if err != nil {
		return "", err
	}
	id, exe := first(m["CFBundleIdentifier"]), first(m["CFBundleExecutable"])
	if exe != spec.exe {
		return "", fmt.Errorf("service: %s: executable %q, want %q", bundle, exe, spec.exe)
	}
	if !bundleIDRE.MatchString(id) || !(strings.HasSuffix(id, spec.suffix) || strings.HasSuffix(id, spec.suffix+".dev")) {
		return "", fmt.Errorf("service: %s: bundle id %q does not end in %s (or %s.dev)", bundle, id, spec.suffix, spec.suffix)
	}
	return id, nil
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// idParts splits a helper bundle id into its prefix and whether it is a dev id.
func idParts(id string, spec bundleSpec) (prefix string, dev bool) {
	if p, ok := strings.CutSuffix(id, spec.suffix+".dev"); ok {
		return p, true
	}
	return strings.TrimSuffix(id, spec.suffix), false
}

// LoadBundleIDs fills c.HabBundleID and c.HolderBundleID from the bundles of build c.Build.
func (c *Config) LoadBundleIDs() error {
	if !ValidBuild(c.Build) {
		return fmt.Errorf("service: build %q: want the build directory name gff-hab install chose", c.Build)
	}
	var err error
	if c.HolderBundleID, err = bundleID(filepath.Join(c.BuildDir(), HolderBundle), holderSpec); err != nil {
		return err
	}
	c.HabBundleID, err = bundleID(filepath.Join(c.BuildDir(), HabBundle), habSpec)
	return err
}

// ResolveBundles resolves the install sources to the two bundles, checks that they belong
// together (one prefix, both release or both dev), and fills in c.Build and the bundle ids.
func ResolveBundles(c *Config, habSrc, holderSrc string) (hab, holder string, err error) {
	if hab, err = BundleRoot(habSrc); err != nil {
		return "", "", err
	}
	if holder, err = BundleRoot(holderSrc); err != nil {
		return "", "", err
	}
	if c.HabBundleID, err = bundleID(hab, habSpec); err != nil {
		return "", "", err
	}
	if c.HolderBundleID, err = bundleID(holder, holderSpec); err != nil {
		return "", "", err
	}
	hp, hd := idParts(c.HabBundleID, habSpec)
	op, od := idParts(c.HolderBundleID, holderSpec)
	if hp != op || hd != od {
		return "", "", fmt.Errorf("service: bundles %s (%s) and %s (%s) are not from one build",
			hab, c.HabBundleID, holder, c.HolderBundleID)
	}
	if c.Build, err = hashBundles(hab, holder); err != nil {
		return "", "", err
	}
	return hab, holder, nil
}

// hashBundles names a build: the first 16 hex digits of a SHA-256 over both bundles' trees,
// each entry's relative path, kind, mode and content (a link's target).
func hashBundles(bundles ...string) (string, error) {
	h := sha256.New()
	for _, b := range bundles {
		fmt.Fprintf(h, "bundle %s\n", filepath.Base(b))
		err := filepath.WalkDir(b, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(b, p)
			fi, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%q %v\n", rel, fi.Mode())
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				t, err := os.Readlink(p)
				if err != nil {
					return err
				}
				fmt.Fprintf(h, "-> %q\n", t)
			case fi.Mode().IsRegular():
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				fmt.Fprintf(h, "%d\n", fi.Size())
				if _, err := io.Copy(h, f); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// designated returns the designated requirement of item, as codesign prints it after
// "designated => ".
func designated(ctx context.Context, run Runner, item string) (string, error) {
	out, err := run(ctx, "codesign", "-d", "-r-", item)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, r, ok := strings.Cut(line, "designated => "); ok {
			return strings.TrimSpace(r), nil
		}
	}
	return "", fmt.Errorf("service: codesign printed no designated requirement for %s", item)
}

// verifyCopy checks a bundle copy against its source (spec 5.3, the rows install runs): the
// signature and seal hold under --strict, and the copy satisfies the source's designated
// requirement.
func verifyCopy(ctx context.Context, run Runner, src, cp string) error {
	if _, err := run(ctx, "codesign", "--verify", "--strict", "--verbose=2", cp); err != nil {
		return fmt.Errorf("service: %s fails codesign --verify --strict: %w", cp, err)
	}
	req, err := designated(ctx, run, src)
	if err != nil {
		return err
	}
	if _, err := run(ctx, "codesign", "--verify", "-R="+req, cp); err != nil {
		return fmt.Errorf("service: %s does not satisfy the designated requirement of %s: %w", cp, src, err)
	}
	return nil
}

// installBuild puts both bundles into <state>/builds/<build>: ditto into a temporary directory
// beside it, verify each copy, then rename the directory into place. A build directory that is
// there already holds the same bytes (its name is their hash); it is verified and kept, never
// written.
func installBuild(ctx context.Context, c Config, run Runner, m *manifest, hab, holder string) error {
	if err := m.mkdirAll(c, c.BuildsDir(), 0o700); err != nil {
		return err
	}
	dst := c.BuildDir()
	if fi, err := os.Lstat(dst); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("service: %s is not a directory", dst)
		}
		for _, b := range []struct{ src, name string }{{hab, HabBundle}, {holder, HolderBundle}} {
			if err := verifyCopy(ctx, run, b.src, filepath.Join(dst, b.name)); err != nil {
				return fmt.Errorf("%w; the installed copy of this build is damaged: uninstall, then install again", err)
			}
		}
		return nil
	}
	tmp, err := os.MkdirTemp(c.BuildsDir(), ".tmp-"+c.Build+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // gone after the rename; cleans up a failure
	for _, b := range []struct{ src, name string }{{hab, HabBundle}, {holder, HolderBundle}} {
		cp := filepath.Join(tmp, b.name)
		if _, err := run(ctx, "ditto", b.src, cp); err != nil {
			return err
		}
		if err := verifyCopy(ctx, run, b.src, cp); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// setCurrent points <state>/current at builds/<build>, through a temporary link and a rename.
func setCurrent(c Config) error {
	tmp := filepath.Join(c.StateDir, ".tmp-current-"+c.Build)
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("builds", c.Build), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.CurrentLink()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// buildDirs lists the build directories and leftover temporary directories in <state>/builds.
func buildDirs(c Config) ([]string, error) {
	ents, err := os.ReadDir(c.BuildsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() {
			continue
		}
		if ValidBuild(n) || (strings.HasPrefix(n, ".tmp-") && len(n) > 22 && ValidBuild(n[5:21])) {
			out = append(out, filepath.Join(c.BuildsDir(), n))
		}
	}
	return out, nil
}

// programOf reads the program path of a launchd plist: the first of its ProgramArguments.
func programOf(path string) (string, error) {
	m, err := readPlist(path)
	if err != nil {
		return "", err
	}
	if p := first(m["ProgramArguments"]); p != "" {
		return p, nil
	}
	return first(m["Program"]), nil
}

// buildsInUse lists the builds a job of this install names: the logic plist and each holder
// plist. A build no job names runs no process of a job (spec 4.3 step 7): a holder's plist
// stays until the holder has exited and gff-hab retired it.
func buildsInUse(c Config) (map[string]bool, error) {
	plists, err := holderPlists(c)
	if err != nil {
		return nil, err
	}
	plists = append(plists, c.logicFile())
	use := map[string]bool{}
	prefix := c.BuildsDir() + string(filepath.Separator)
	for _, p := range plists {
		prog, err := programOf(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err // unreadable: keep every build rather than guess
		}
		if rest, ok := strings.CutPrefix(prog, prefix); ok {
			b, _, _ := strings.Cut(rest, string(filepath.Separator))
			use[b] = true
		}
	}
	return use, nil
}

// tmpAge is how old a temporary build directory must be before a prune removes it.
const tmpAge = 10 * time.Minute

// LSRegister is the LaunchServices registration tool, part of every macOS release. No other
// command registers a bundle.
const LSRegister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// registerBuild registers both bundles of build directory dir with LaunchServices (spec 6.1).
// System Settings and tccutil find a bundle by its id only through a registered copy. With none,
// tccutil reset fails with -10814. A plain copy is not registered; a launch agent's first start
// registers its bundle, but install does not rely on that.
func registerBuild(ctx context.Context, run Runner, dir string) error {
	var errs []error
	for _, b := range []string{HabBundle, HolderBundle} {
		p := filepath.Join(dir, b)
		if _, err := run(ctx, LSRegister, "-f", p); err != nil {
			errs = append(errs, fmt.Errorf("service: register %s with LaunchServices: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// unregisterBuild removes both bundles of build directory dir from the LaunchServices database
// before the directory goes, so no record names a deleted copy. Errors are ignored: a stale
// record does no harm.
func unregisterBuild(ctx context.Context, run Runner, dir string) {
	if !ValidBuild(filepath.Base(dir)) {
		return // a temporary copy is never registered
	}
	for _, b := range []string{HabBundle, HolderBundle} {
		p := filepath.Join(dir, b)
		if _, err := os.Stat(p); err == nil {
			run(ctx, LSRegister, "-u", p)
		}
	}
}

// pruneBuilds removes each build directory no job names, except c.Build and the build current
// points at, and each temporary directory older than tmpAge. It unregisters a build's bundles
// before it removes them. It returns what it removed.
//
// A concurrent install is safe: it sets current right after it renames its build into place,
// before it writes the plist that names it.
func pruneBuilds(ctx context.Context, c Config, run Runner) ([]string, error) {
	use, err := buildsInUse(c)
	if err != nil {
		return nil, err
	}
	if c.Build != "" {
		use[c.Build] = true
	}
	if t, err := os.Readlink(c.CurrentLink()); err == nil {
		use[filepath.Base(t)] = true
	}
	dirs, err := buildDirs(c)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, d := range dirs {
		n := filepath.Base(d)
		if use[n] {
			continue
		}
		if strings.HasPrefix(n, ".tmp-") {
			// An install may be copying into it now; one older than tmpAge is from an install
			// that failed.
			if fi, err := os.Stat(d); err != nil || time.Since(fi.ModTime()) < tmpAge {
				continue
			}
		}
		unregisterBuild(ctx, run, d)
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, n)
	}
	return removed, errors.Join(errs...)
}
