// Package service installs gff-hab and gff-hab-holder as init system jobs, and starts each
// holder as its own job (docs/specs/hab-holder.md 5). Linux uses systemd user units; macOS uses
// launch agents in the user's GUI domain, gui/<uid>.
//
// Every name and path in a unit or plist comes from Config: the running user's environment or
// flags at install time. Nothing names a user or a host.
//
// Install writes these files:
//
//   - systemd: <units>/<prefix>-gff-hab.service, <units>/<prefix>-gff-hab-holder@.service, the
//     enable link <units>/default.target.wants/<prefix>-gff-hab.service (written by systemctl),
//     and the binary copies in <state>/bin.
//   - launchd: <agents>/<prefix>.gff-hab.plist, the job logs in <state>/logs, one directory per
//     build in <state>/builds holding the two helper bundles (bundle.go), and the link
//     <state>/current. The starter adds one <agents>/<prefix>.gff-hab-holder.<instance>.plist per
//     holder, and launchd its log <state>/logs/gff-hab-holder.<instance>.log.
//
// Install records what it writes, and each directory it creates, in <state>/install.json before
// it writes it (manifest.go). Uninstall removes what that record lists, the holder plists and
// holder logs by their names, then each directory install created, when it is empty. It never
// removes a file by name alone that install did not write (Files). Build directories are found
// by their names too: only install and the starter write in <state>/builds.
//
// Commands run through an injected Runner, so tests need no init system. The package is pure Go.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// Init names an init system.
type Init string

// The init systems. None means gff-hab starts no holders: the holders are started by hand or by
// a test, which is how development and the Go tests run.
const (
	None    Init = "none"
	Systemd Init = "systemd"
	Launchd Init = "launchd"
)

// DefaultPrefix is the default of Config.Prefix.
const DefaultPrefix = "flightdeck"

// Binary names, in <state>/bin.
const (
	HabBinary    = "gff-hab"
	HolderBinary = "gff-hab-holder"
)

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec runs commands for real.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Config is one install. Every path is absolute.
type Config struct {
	Init Init
	// Prefix names the jobs: systemd units <prefix>-gff-hab.service and
	// <prefix>-gff-hab-holder@.service, launchd labels <prefix>.gff-hab and
	// <prefix>.gff-hab-holder.<instance>. Two installs with different prefixes do not touch each
	// other.
	Prefix string
	// StateDir is gff-hab's state directory. The binary copies go to <state>/bin and, on macOS,
	// the job logs to <state>/logs.
	StateDir string
	// HolderDir is the holder registry directory (spec 4.2).
	HolderDir string
	// Socket is gff-hab's control socket.
	Socket string
	// UnitDir is the systemd user unit directory, or the LaunchAgents directory.
	UnitDir string
	// Path is the PATH the jobs get. A job gets a bare environment otherwise (spec 5.2).
	Path string
	// Locale is the installing user's LANG and LC_* settings, as KEY=value, which the jobs get
	// too, so programs in panes keep the user's locale.
	Locale []string
	// UID is the user's id, for the launchd domain gui/<uid>.
	UID int
	// Build is the launchd build directory, <state>/builds/<build>, that the jobs run from.
	// Install sets it from the bundles it installs; serve gets it as -build.
	Build string
	// HabBundleID and HolderBundleID are the bundle ids of the build's helper bundles. Each
	// launchd plist names its own in AssociatedBundleIdentifiers (macos-identity.md 6.1).
	HabBundleID, HolderBundleID string
}

var prefixRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// localeRE matches one locale setting the jobs may carry.
var localeRE = regexp.MustCompile(`^(LANG|LC_[A-Z]+)=[^\x00\n\r]*$`)

// LocaleEnv picks the locale settings, LANG and LC_*, out of environ, sorted.
func LocaleEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if localeRE.MatchString(kv) {
			out = append(out, kv)
		}
	}
	slices.Sort(out)
	return out
}

// Validate checks c: a known init system, a prefix fit for unit names and labels, and absolute,
// clean paths with no control characters.
func (c Config) Validate() error {
	if c.Init != Systemd && c.Init != Launchd {
		return fmt.Errorf("service: init system %q: want systemd or launchd", c.Init)
	}
	if !prefixRE.MatchString(c.Prefix) {
		return fmt.Errorf("service: prefix %q: want letters, digits, '.', '_' or '-', at most 64", c.Prefix)
	}
	for _, p := range []struct{ name, v string }{
		{"state directory", c.StateDir}, {"holder directory", c.HolderDir},
		{"control socket", c.Socket}, {"unit directory", c.UnitDir},
	} {
		if err := checkPath(p.name, p.v); err != nil {
			return err
		}
	}
	if strings.ContainsAny(c.Path, "\x00\n\r") {
		return errors.New("service: PATH holds a control character")
	}
	for _, kv := range c.Locale {
		if !localeRE.MatchString(kv) {
			return fmt.Errorf("service: locale setting %q: want LANG or LC_* and a value with no control character", kv)
		}
	}
	if c.Build != "" && !ValidBuild(c.Build) {
		return fmt.Errorf("service: build %q: want 16 hex digits", c.Build)
	}
	for _, id := range []string{c.HabBundleID, c.HolderBundleID} {
		if id != "" && !bundleIDRE.MatchString(id) {
			return fmt.Errorf("service: bundle id %q is not reverse-DNS", id)
		}
	}
	return nil
}

// checkBundled fails unless a launchd config names its build and the bundle ids its plists need.
func (c Config) checkBundled() error {
	if c.Init != Launchd {
		return nil
	}
	if c.Build == "" || c.HabBundleID == "" || c.HolderBundleID == "" {
		return errors.New("service: launchd jobs run from a build's helper bundles, and no build or bundle id is given (gff-hab install sets them)")
	}
	return nil
}

func checkPath(name, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("service: no %s", name)
	case !filepath.IsAbs(p):
		return fmt.Errorf("service: %s %q is not absolute", name, p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("service: %s %q is not clean", name, p)
	case strings.ContainsAny(p, "\x00\n\r"):
		return fmt.Errorf("service: %s %q holds a control character", name, p)
	}
	return nil
}

// BinDir is where Install copies the binaries on systemd.
func (c Config) BinDir() string { return filepath.Join(c.StateDir, "bin") }

// LogDir is where launchd jobs write their output.
func (c Config) LogDir() string { return filepath.Join(c.StateDir, "logs") }

// HabPath is the installed gff-hab: in <state>/bin on systemd, inside the build's GFF Hab.app on
// launchd.
func (c Config) HabPath() string {
	if c.Init == Launchd {
		return bundleExe(c.BuildDir(), HabBundle, HabBinary)
	}
	return filepath.Join(c.BinDir(), HabBinary)
}

// HolderPath is the installed gff-hab-holder: in <state>/bin on systemd, inside the build's
// GFF Holder.app on launchd.
func (c Config) HolderPath() string {
	if c.Init == Launchd {
		return bundleExe(c.BuildDir(), HolderBundle, HolderBinary)
	}
	return filepath.Join(c.BinDir(), HolderBinary)
}

// LogicJob is the logic job's systemd unit or launchd label.
func (c Config) LogicJob() string {
	if c.Init == Launchd {
		return c.Prefix + ".gff-hab"
	}
	return c.Prefix + "-gff-hab.service"
}

// HolderJob is the systemd unit or launchd label of holder instance.
func (c Config) HolderJob(instance string) string {
	if c.Init == Launchd {
		return c.Prefix + ".gff-hab-holder." + instance
	}
	return c.Prefix + "-gff-hab-holder@" + instance + ".service"
}

// holderTemplate is the systemd holder template unit.
func (c Config) holderTemplate() string { return c.Prefix + "-gff-hab-holder@.service" }

// logicFile is the unit or plist file of the logic job.
func (c Config) logicFile() string {
	if c.Init == Launchd {
		return filepath.Join(c.UnitDir, c.LogicJob()+".plist")
	}
	return filepath.Join(c.UnitDir, c.LogicJob())
}

// holderPlist is the launchd plist of holder instance.
func (c Config) holderPlist(instance string) string {
	return filepath.Join(c.UnitDir, c.HolderJob(instance)+".plist")
}

// logicLog is the launchd log of the logic job.
func (c Config) logicLog() string { return filepath.Join(c.LogDir(), "gff-hab.log") }

// holderLog is the launchd log of holder instance.
func (c Config) holderLog(instance string) string {
	return filepath.Join(c.LogDir(), "gff-hab-holder."+instance+".log")
}

// enableLink is the link `systemctl --user enable` writes for the logic unit.
func (c Config) enableLink() string {
	return filepath.Join(c.UnitDir, "default.target.wants", c.LogicJob())
}

// ServeArgs are the arguments of `gff-hab serve` in the logic job.
func (c Config) ServeArgs() []string {
	args := []string{
		"serve",
		"-socket", c.Socket,
		"-state", c.StateDir,
		"-holders", c.HolderDir,
		"-init", string(c.Init),
		"-prefix", c.Prefix,
		"-unit-dir", c.UnitDir,
	}
	if c.Init == Launchd {
		args = append(args, "-build", c.Build)
	}
	return args
}

// HolderArgs are the arguments of a holder job for instance.
func (c Config) HolderArgs(instance string) []string {
	return []string{"-dir", c.HolderDir, "-instance", instance}
}

// File is one file Install writes.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// Render returns the job files Install writes for c, with their content. Binary copies, build
// directories, the systemd enable link and the per-instance holder plists are not among them.
func Render(c Config) ([]File, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.checkBundled(); err != nil {
		return nil, err
	}
	if c.Init == Launchd {
		return []File{{Path: c.logicFile(), Content: logicPlist(c), Mode: 0o644}}, nil
	}
	return []File{
		{Path: c.logicFile(), Content: logicUnit(c), Mode: 0o644},
		{Path: filepath.Join(c.UnitDir, c.holderTemplate()), Content: holderUnit(c), Mode: 0o644},
	}, nil
}

// Files lists what Uninstall removes, in order: each file the install record lists, each holder
// plist and holder log there is now, the record itself, then each directory install created
// (removed only when empty), deepest first. With no record it lists the job files by their names
// only: the units or the logic plist, the enable link and the holder plists.
func Files(c Config) ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	m, err := readManifest(c)
	if err != nil {
		return nil, err
	}
	return filesFor(c, m)
}

func filesFor(c Config, m *manifest) ([]string, error) {
	var out []string
	if m != nil {
		out = append(out, m.Files...)
	} else if c.Init == Launchd {
		out = append(out, c.logicFile())
	} else {
		rendered, err := Render(c)
		if err != nil {
			return nil, err
		}
		for _, f := range rendered {
			out = append(out, f.Path)
		}
		out = append(out, c.enableLink())
	}
	if c.Init == Launchd {
		plists, err := holderPlists(c)
		if err != nil {
			return nil, err
		}
		out = append(out, plists...)
		if m != nil {
			logs, err := holderLogs(c)
			if err != nil {
				return nil, err
			}
			out = append(out, logs...)
			// Every build directory goes, with its bundles; uninstall has stopped every job.
			builds, err := buildDirs(c)
			if err != nil {
				return nil, err
			}
			out = append(out, builds...)
		}
	}
	if m != nil {
		out = append(out, c.manifestPath())
		for i := len(m.Dirs) - 1; i >= 0; i-- {
			out = append(out, m.Dirs[i])
		}
	}
	return dedup(out), nil
}

// dedup drops repeats from paths, keeping the first of each.
func dedup(paths []string) []string {
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// holderPlists lists the per-instance holder plists in the LaunchAgents directory.
func holderPlists(c Config) ([]string, error) {
	return filepath.Glob(filepath.Join(globEscape(c.UnitDir), globEscape(c.Prefix)+".gff-hab-holder.*.plist"))
}

// holderLogs lists the holder logs in the log directory: gff-hab-holder.<instance>.log, for a
// valid instance id only.
func holderLogs(c Config) ([]string, error) {
	logs, err := filepath.Glob(filepath.Join(globEscape(c.LogDir()), "gff-hab-holder.*.log"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range logs {
		inst := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(l), "gff-hab-holder."), ".log")
		if holderreg.ValidInstance(inst) {
			out = append(out, l)
		}
	}
	return out, nil
}

// globEscape escapes the glob metacharacters in s.
func globEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(s)
}

// Options are the parts of one install or uninstall run that are not the install itself.
type Options struct {
	// HabSource and HolderSource are the binaries Install copies.
	HabSource, HolderSource string
	// Run runs commands. Nil means Exec.
	Run Runner
	// Out receives what the user should read: the linger hint, what was done.
	Out io.Writer
	// Confirm asks before uninstall ends programs; it returns true to go on. Nil means yes.
	Confirm func(question string) bool
}

func (o *Options) defaults() {
	if o.Run == nil {
		o.Run = Exec
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Confirm == nil {
		o.Confirm = func(string) bool { return true }
	}
}

// Install writes the job files and the binary copies, then loads and (re)starts the logic job.
// Running it again over an install replaces the binaries and the job files and restarts the
// logic job only: holders keep running, and a new gff-hab build drains them (spec 4.3).
//
// Each path goes into the install record before it is written, so an install that fails half
// way still leaves a record that names all it wrote. A state directory that holds the record of
// another install (another prefix, init system or unit directory) is refused: both logic jobs
// would run, and one would lose the state lock and restart forever.
//
// On launchd the sources are the two helper bundles (or the executables inside them). Each build
// gets its own directory, <state>/builds/<build>, so a drained holder keeps running from its own
// bundle copy, and the jobs name the executables inside the bundles (spec macos-identity.md 6.1).
func Install(ctx context.Context, c Config, o Options) error {
	o.defaults()
	var habBundle, holderBundle string
	if c.Init == Launchd {
		if err := c.Validate(); err != nil {
			return err
		}
		var err error
		if habBundle, holderBundle, err = ResolveBundles(&c, o.HabSource, o.HolderSource); err != nil {
			return err
		}
	}
	files, err := Render(c)
	if err != nil {
		return err
	}
	m, err := readManifest(c)
	if err != nil {
		return err
	}
	if m == nil {
		m = &manifest{Version: manifestVersion, Init: c.Init, Prefix: c.Prefix, UnitDir: c.UnitDir}
	} else if err := m.check(c); err != nil {
		return err
	}
	if c.Init == Launchd {
		if err := checkGUI(ctx, c, o.Run); err != nil {
			return err
		}
	}
	// The record lives in the state directory, so that one is made first and recorded at once.
	created := missingDirs(c.StateDir)
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return err
	}
	m.addDirs(created...)
	if err := m.save(c); err != nil {
		return err
	}
	if c.Init == Launchd {
		if err := installBuild(ctx, c, o.Run, m, habBundle, holderBundle); err != nil {
			return err
		}
		// current names the new build before any plist does, so a prune by a running gff-hab
		// of an older build never removes it.
		if err := m.willWrite(c, c.CurrentLink()); err != nil {
			return err
		}
		if err := setCurrent(c); err != nil {
			return err
		}
	} else if err := installBinaries(c, o, m); err != nil {
		return err
	}
	if c.Init == Launchd {
		// launchd discards a job's output unless it names files; the directory must exist first.
		if err := m.mkdirAll(c, c.LogDir(), 0o700); err != nil {
			return err
		}
		if err := m.willWrite(c, c.logicLog()); err != nil {
			return err
		}
	}
	if err := m.mkdirAll(c, c.UnitDir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if err := m.willWrite(c, f.Path); err != nil {
			return err
		}
		if err := writeAtomic(f.Path, f.Content, f.Mode); err != nil {
			return err
		}
	}
	if c.Init == Launchd {
		if err := installLaunchd(ctx, c, o); err != nil {
			return err
		}
		// Older builds no job names any more go now; a drained holder's build stays until
		// gff-hab retires it.
		removed, err := pruneBuilds(c)
		if len(removed) > 0 {
			fmt.Fprintf(o.Out, "removed %d unused build(s) from %s\n", len(removed), c.BuildsDir())
		}
		return err
	}
	// systemctl enable writes the link, and its directory when there is none.
	m.addDirs(missingDirs(filepath.Dir(c.enableLink()))...)
	if err := m.willWrite(c, c.enableLink()); err != nil {
		return err
	}
	return installSystemd(ctx, c, o)
}

// installBinaries copies both binaries into <state>/bin (systemd).
func installBinaries(c Config, o Options, m *manifest) error {
	if err := m.mkdirAll(c, c.BinDir(), 0o700); err != nil {
		return err
	}
	for _, b := range []struct{ src, dst string }{{o.HabSource, c.HabPath()}, {o.HolderSource, c.HolderPath()}} {
		if b.src == "" {
			return fmt.Errorf("service: no source for %s", filepath.Base(b.dst))
		}
		if !sameFile(b.src, b.dst) {
			// A source that is the installed copy itself is not written, so not recorded: it
			// may be a binary the user put there.
			if err := m.willWrite(c, b.dst); err != nil {
				return err
			}
		}
		if err := copyBinary(b.src, b.dst); err != nil {
			return err
		}
	}
	return nil
}

// Uninstall asks before it ends programs, stops the logic job, stops every holder job (each
// holder's shutdown ends its programs, spec 2.8), unloads every job and removes every file in
// Files. It returns ErrDeclined, and changes nothing, when the user says no. A state directory
// whose install record names another install is refused, and nothing changes.
func Uninstall(ctx context.Context, c Config, o Options) error {
	o.defaults()
	if err := c.Validate(); err != nil {
		return err
	}
	m, err := readManifest(c)
	if err != nil {
		return err
	}
	if m != nil {
		if err := m.check(c); err != nil {
			return err
		}
	}
	var running []string
	if c.Init == Launchd {
		running, err = runningLaunchdHolders(ctx, c, o.Run)
	} else {
		running, err = runningSystemdHolders(ctx, c, o.Run)
	}
	if err != nil {
		return err
	}
	if len(running) > 0 {
		q := fmt.Sprintf("%d holder job(s) are running. Uninstall shuts them down, which ends every program in their panes. Go on?", len(running))
		if !o.Confirm(q) {
			return ErrDeclined
		}
	}
	if c.Init == Launchd {
		err = uninstallLaunchd(ctx, c, o, m)
	} else {
		err = uninstallSystemd(ctx, c, o, m)
	}
	if err == nil && m == nil {
		fmt.Fprintf(o.Out, "no install record in %s: removed the job files only, and left %s and %s\n",
			c.StateDir, c.BinDir(), c.LogDir())
	}
	return err
}

// ErrDeclined means the user declined to end running programs.
var ErrDeclined = errors.New("service: uninstall declined; nothing was changed")

// removeFiles removes everything in Files for record m. A missing file is not an error, and a
// directory that is not empty stays.
func removeFiles(c Config, m *manifest) error {
	paths, err := filesFor(c, m)
	if err != nil {
		return err
	}
	dirs := map[string]bool{}
	if m != nil {
		for _, d := range m.Dirs {
			dirs[d] = true
		}
	}
	builds := map[string]bool{}
	if c.Init == Launchd && m != nil {
		bs, err := buildDirs(c)
		if err != nil {
			return err
		}
		for _, b := range bs {
			builds[b] = true
		}
	}
	var errs []error
	for _, p := range paths {
		if builds[p] {
			if err := os.RemoveAll(p); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		err := os.Remove(p)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if dirs[p] {
			if fi, serr := os.Lstat(p); serr == nil && fi.IsDir() {
				continue // not empty: it holds something install did not write
			}
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// sameFile reports whether a and b are one file.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// copyBinary copies src to dst through a temporary file and a rename, so a running copy of dst
// keeps its own file, and a reader never sees half a binary. A src that is dst is left alone.
func copyBinary(src, dst string) error {
	if src == "" {
		return fmt.Errorf("service: no source for %s", filepath.Base(dst))
	}
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if di, err := os.Stat(dst); err == nil && os.SameFile(si, di) {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// writeAtomic writes data to path through a temporary file and a rename.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Starter starts each holder as its own init system job (spec 5.4).
type Starter interface {
	// Start asks the init system for a new holder instance. It may return before the holder
	// listens.
	Start(ctx context.Context) error
	// Retire removes the job of a holder that exited cleanly (after drain), once its process
	// pid is gone. It never ends a running holder.
	Retire(instance string, pid int)
	// Sweep removes the jobs of holders that are gone and that Retire did not remove. gff-hab
	// calls it once at start, before any Start.
	Sweep(ctx context.Context)
}

// NewStarter returns the starter for c's init system.
func NewStarter(c Config, run Runner, newInstance func() string, logf func(string, ...any)) (Starter, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if run == nil {
		run = Exec
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if c.Init == Launchd {
		if err := c.checkBundled(); err != nil {
			return nil, err
		}
		return &launchdStarter{c: c, run: run, newInstance: newInstance, logf: logf}, nil
	}
	return &systemdStarter{c: c, run: run, newInstance: newInstance}, nil
}
