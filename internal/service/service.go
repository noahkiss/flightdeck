// Package service installs gff-hab and gff-hab-holder as init system jobs, and starts each
// holder as its own job (docs/specs/hab-holder.md 5). Linux uses systemd user units; macOS uses
// launch agents in the user's GUI domain, gui/<uid>.
//
// Every name and path in a unit or plist comes from Config: the running user's environment or
// flags at install time. Nothing names a user or a host.
//
// Install writes these files, and Uninstall removes exactly these (Files):
//
//   - systemd: <units>/<prefix>-gff-hab.service, <units>/<prefix>-gff-hab-holder@.service, the
//     enable link <units>/default.target.wants/<prefix>-gff-hab.service (written by systemctl),
//     and the binary copies in <state>/bin.
//   - launchd: <agents>/<prefix>.gff-hab.plist, one <agents>/<prefix>.gff-hab-holder.<instance>.plist
//     per holder (written by the starter), the job logs in <state>/logs, and the binary copies
//     in <state>/bin.
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
	"strings"
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
	// UID is the user's id, for the launchd domain gui/<uid>.
	UID int
}

var prefixRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

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

// BinDir is where Install copies the binaries.
func (c Config) BinDir() string { return filepath.Join(c.StateDir, "bin") }

// LogDir is where launchd jobs write their output.
func (c Config) LogDir() string { return filepath.Join(c.StateDir, "logs") }

// HabPath is the installed gff-hab.
func (c Config) HabPath() string { return filepath.Join(c.BinDir(), HabBinary) }

// HolderPath is the installed gff-hab-holder.
func (c Config) HolderPath() string { return filepath.Join(c.BinDir(), HolderBinary) }

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

// ServeArgs are the arguments of `gff-hab serve` in the logic job.
func (c Config) ServeArgs() []string {
	return []string{
		"serve",
		"-socket", c.Socket,
		"-state", c.StateDir,
		"-holders", c.HolderDir,
		"-init", string(c.Init),
		"-prefix", c.Prefix,
		"-unit-dir", c.UnitDir,
	}
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

// Render returns the job files Install writes for c, with their content. Binary copies, the
// systemd enable link and the per-instance holder plists are not among them.
func Render(c Config) ([]File, error) {
	if err := c.Validate(); err != nil {
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

// Files lists every file and directory Install writes, and every holder plist the starter wrote
// that exists now: what Uninstall removes. Directories come after the files in them.
func Files(c Config) ([]string, error) {
	rendered, err := Render(c)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range rendered {
		out = append(out, f.Path)
	}
	if c.Init == Systemd {
		out = append(out, filepath.Join(c.UnitDir, "default.target.wants", c.LogicJob()))
	} else {
		plists, err := holderPlists(c)
		if err != nil {
			return nil, err
		}
		out = append(out, plists...)
		out = append(out, c.LogDir())
	}
	out = append(out, c.HabPath(), c.HolderPath(), c.BinDir())
	return out, nil
}

// holderPlists lists the per-instance holder plists in the LaunchAgents directory.
func holderPlists(c Config) ([]string, error) {
	return filepath.Glob(filepath.Join(globEscape(c.UnitDir), globEscape(c.Prefix)+".gff-hab-holder.*.plist"))
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
func Install(ctx context.Context, c Config, o Options) error {
	o.defaults()
	files, err := Render(c)
	if err != nil {
		return err
	}
	if c.Init == Launchd {
		if err := checkGUI(ctx, c, o.Run); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(c.BinDir(), 0o700); err != nil {
		return err
	}
	if err := copyBinary(o.HabSource, c.HabPath()); err != nil {
		return err
	}
	if err := copyBinary(o.HolderSource, c.HolderPath()); err != nil {
		return err
	}
	if c.Init == Launchd {
		// launchd discards a job's output unless it names files; the directory must exist first.
		if err := os.MkdirAll(c.LogDir(), 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(c.UnitDir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if err := writeAtomic(f.Path, f.Content, f.Mode); err != nil {
			return err
		}
	}
	if c.Init == Launchd {
		return installLaunchd(ctx, c, o)
	}
	return installSystemd(ctx, c, o)
}

// Uninstall asks before it ends programs, stops the logic job, stops every holder job (each
// holder's shutdown ends its programs, spec 2.8), unloads every job and removes every file in
// Files. It returns ErrDeclined, and changes nothing, when the user says no.
func Uninstall(ctx context.Context, c Config, o Options) error {
	o.defaults()
	if err := c.Validate(); err != nil {
		return err
	}
	var running []string
	var err error
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
		return uninstallLaunchd(ctx, c, o)
	}
	return uninstallSystemd(ctx, c, o)
}

// ErrDeclined means the user declined to end running programs.
var ErrDeclined = errors.New("service: uninstall declined; nothing was changed")

// removeFiles removes everything in Files. A missing file is not an error.
func removeFiles(c Config) error {
	paths, err := Files(c)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range paths {
		var err error
		if p == c.LogDir() {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
		return &launchdStarter{c: c, run: run, newInstance: newInstance, logf: logf}, nil
	}
	return &systemdStarter{c: c, run: run, newInstance: newInstance}, nil
}
