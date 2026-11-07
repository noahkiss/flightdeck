package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/grants"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/service"
)

// serviceFlags are the flags that place an install: install, uninstall and serve share them, so
// the jobs that install writes find what serve uses.
type serviceFlags struct {
	init, prefix, state, holders, socket, unitDir *string
	// build is serve's -build: the launchd build directory it runs from. Nil elsewhere.
	build *string
}

func addServiceFlags(fs *flag.FlagSet, initHelp string) *serviceFlags {
	return &serviceFlags{
		init:    fs.String("init", os.Getenv("GFF_HAB_INIT"), initHelp),
		prefix:  fs.String("prefix", "", "job name prefix (default: $GFF_SERVICE_PREFIX, else "+service.DefaultPrefix+")"),
		state:   fs.String("state", "", "state directory (default: see gff-hab -h)"),
		holders: fs.String("holders", "", "holder registry directory (default: $GFF_HOLDER_DIR, else the holder's default)"),
		socket:  socketFlag(fs),
		unitDir: fs.String("unit-dir", "", "systemd user unit or LaunchAgents directory (default: the user's)"),
	}
}

// initFor picks the init system: the flag or $GFF_HAB_INIT when set, else def.
func initFor(v string, def service.Init) (service.Init, error) {
	if v == "" {
		return def, nil
	}
	switch i := service.Init(v); i {
	case service.None, service.Systemd, service.Launchd:
		return i, nil
	}
	return "", fmt.Errorf("-init %q: want none, systemd or launchd", v)
}

// osInit is this system's init system for install and uninstall.
func osInit() (service.Init, error) {
	switch runtime.GOOS {
	case "linux":
		return service.Systemd, nil
	case "darwin":
		return service.Launchd, nil
	}
	return "", fmt.Errorf("no service install on %s", runtime.GOOS)
}

// config resolves the flags into an install for init system in. Every path is made absolute.
func (f *serviceFlags) config(in service.Init) (service.Config, error) {
	c := service.Config{
		Init: in, Prefix: *f.prefix, Path: os.Getenv("PATH"), Locale: service.LocaleEnv(os.Environ()), UID: os.Getuid(),
	}
	c.Grants, c.GrantsSet = os.LookupEnv("GFF_MACOS_GRANTS")
	if c.Prefix == "" {
		c.Prefix = os.Getenv("GFF_SERVICE_PREFIX")
	}
	if c.Prefix == "" {
		c.Prefix = service.DefaultPrefix
	}
	var err error
	if c.StateDir, err = stateDir(*f.state); err != nil {
		return c, err
	}
	c.HolderDir = *f.holders
	if c.HolderDir == "" {
		if c.HolderDir, err = holderreg.DefaultDir(runtime.GOOS, os.Getenv, os.UserConfigDir); err != nil {
			return c, err
		}
	}
	if c.Socket, err = socketPath(*f.socket); err != nil {
		return c, err
	}
	c.DefaultSocket = *f.socket == "" && os.Getenv("GFF_HAB_SOCKET") == ""
	c.UnitDir = *f.unitDir
	if c.UnitDir == "" {
		if c.UnitDir, err = defaultUnitDir(in, os.Getenv, os.UserHomeDir); err != nil {
			return c, err
		}
	}
	for _, p := range []*string{&c.StateDir, &c.HolderDir, &c.Socket, &c.UnitDir} {
		if *p, err = filepath.Abs(*p); err != nil {
			return c, err
		}
	}
	if f.build != nil {
		c.Build = *f.build
	}
	return c, nil
}

// defaultUnitDir is where the user's jobs live: $XDG_CONFIG_HOME/systemd/user (else
// ~/.config/systemd/user) for systemd, ~/Library/LaunchAgents for launchd.
func defaultUnitDir(in service.Init, getenv func(string) string, home func() (string, error)) (string, error) {
	if in == service.Systemd {
		if d := getenv("XDG_CONFIG_HOME"); d != "" {
			return filepath.Join(d, "systemd", "user"), nil
		}
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	if in == service.Launchd {
		return filepath.Join(h, "Library", "LaunchAgents"), nil
	}
	return filepath.Join(h, ".config", "systemd", "user"), nil
}

// serveStarter returns the holder starter serve uses, or nil with -init none (or unset): then
// gff-hab starts no holders. It first sweeps the jobs of holders that are gone (spec 5.4).
func serveStarter(f *serviceFlags, logf func(string, ...any)) (service.Starter, error) {
	in, err := initFor(*f.init, service.None)
	if err != nil || in == service.None {
		return nil, err
	}
	c, err := f.config(in)
	if err != nil {
		return nil, err
	}
	if in == service.Launchd {
		// The holder of this build runs from the GFF Holder.app beside this gff-hab's bundle.
		if c.Build == "" {
			return nil, errors.New("-init launchd needs -build: the build directory gff-hab install chose (the plist it writes passes it)")
		}
		if err := c.LoadBundleIDs(); err != nil {
			return nil, err
		}
	}
	st, err := service.NewStarter(c, nil, newInstanceID, logf)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st.Sweep(ctx)
	return st, nil
}

// install writes and loads the jobs.
func install(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	sf := addServiceFlags(fs, "init system: systemd or launchd (default: this system's)")
	habBin := fs.String("hab-bin", "", "gff-hab to install; on macOS its GFF Hab.app or the executable in it (default: this one)")
	holderBin := fs.String("holder-bin", "", "gff-hab-holder to install; on macOS its GFF Holder.app (default: beside gff-hab or its bundle, else on PATH)")
	gffBin := fs.String("gff-bin", "", "macOS: the gff client to copy into <state>/bin (default: gff beside GFF Hab.app, when there is one)")
	dry := fs.Bool("dry-run", false, "print the job files and change nothing")
	fs.Parse(args)
	c, err := installConfig(sf)
	if err != nil {
		return err
	}
	if *gffBin != "" && c.Init != service.Launchd {
		return errors.New("-gff-bin: only a macOS install copies gff")
	}
	hab, holderSrc, err := sources(*habBin, *holderBin, c.Init)
	if err != nil && !(*dry && c.Init == service.Systemd) {
		return err
	}
	if err := grantsNotice(&c, stdout); err != nil {
		return err
	}
	capcomInstallNotice(hab, stdout)
	if *dry {
		if c.Init == service.Launchd {
			// The plists name the build directory and bundle ids, which come from the bundles.
			if _, _, err := service.ResolveBundles(&c, hab, holderSrc); err != nil {
				return err
			}
		}
		files, err := service.Render(c)
		if err != nil {
			return err
		}
		for _, f := range files {
			fmt.Fprintf(stdout, "# %s\n%s\n", f.Path, f.Content)
		}
		return nil
	}
	var client string
	if c.Init == service.Launchd {
		if client, err = clientSource(*gffBin, hab); err != nil {
			return err
		}
	}
	return service.Install(context.Background(), c, service.Options{
		HabSource: hab, HolderSource: holderSrc, ClientSource: client, Out: stdout,
	})
}

// clientSource finds the gff to install on macOS: the flag, else gff beside the GFF Hab.app that
// hab is or is in (the layout scripts/macos-bundle.sh writes), else none.
func clientSource(flagValue, hab string) (string, error) {
	if flagValue != "" {
		fi, err := os.Stat(flagValue)
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("-gff-bin %s is not a file", flagValue)
		}
		return filepath.Abs(flagValue)
	}
	root, err := service.BundleRoot(hab)
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(root), service.ClientBinary)
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, nil
	}
	return "", nil
}

// grantsNotice resolves the opt-in grant list the jobs get: $GFF_MACOS_GRANTS when set, else the
// last install's. It says when it kept the old list, and names each entry gff-hab does not know.
func grantsNotice(c *service.Config, out io.Writer) error {
	kept, err := c.KeepGrants()
	if err != nil {
		return err
	}
	if kept {
		fmt.Fprintf(out, "GFF_MACOS_GRANTS is not set: keeping %q from the last install (set it, even empty, to change it)\n", c.Grants)
	}
	if bad := grants.UnknownOptIns(macosGrants(func(string) string { return c.Grants })); len(bad) > 0 {
		fmt.Fprintf(out, "warning: GFF_MACOS_GRANTS names unknown grants %q; gff setup --check fails until they are fixed\n", bad)
	}
	return nil
}

// uninstall stops and removes the jobs, after confirmation when holders run.
func uninstall(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	sf := addServiceFlags(fs, "init system: systemd or launchd (default: this system's)")
	yes := fs.Bool("yes", false, "end running programs without asking")
	fs.Parse(args)
	c, err := installConfig(sf)
	if err != nil {
		return err
	}
	confirm := func(q string) bool {
		if *yes {
			return true
		}
		if f, ok := stdin.(*os.File); !ok || !isTerminal(f) {
			fmt.Fprintf(stdout, "%s\nNot a terminal: pass -yes to go on.\n", q)
			return false
		}
		fmt.Fprintf(stdout, "%s [y/N] ", q)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes"
	}
	ctx := context.Background()
	if err := service.Uninstall(ctx, c, service.Options{Out: stdout, Confirm: confirm}); err != nil {
		return err
	}
	stopEngine(ctx, c.Init, c.Prefix, c.StateDir, stdout)
	return nil
}

func installConfig(sf *serviceFlags) (service.Config, error) {
	def, err := osInit()
	if err != nil {
		return service.Config{}, err
	}
	in, err := initFor(*sf.init, def)
	if err != nil {
		return service.Config{}, err
	}
	if in == service.None {
		return service.Config{}, errors.New("-init none installs nothing")
	}
	c, err := sf.config(in)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}

// sources finds the binaries to install: the flags, else this executable and the holder beside
// it, else the holder on PATH. On launchd the holder's default is the GFF Holder.app beside the
// GFF Hab.app that gff-hab is in.
func sources(hab, holderBin string, in service.Init) (string, string, error) {
	if hab == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", "", err
		}
		if hab, err = filepath.EvalSymlinks(exe); err != nil {
			return "", "", err
		}
	}
	if holderBin == "" && in == service.Launchd {
		root, err := service.BundleRoot(hab)
		if err != nil {
			return "", "", err
		}
		holderBin = filepath.Join(filepath.Dir(root), service.HolderBundle)
	}
	if holderBin == "" {
		beside := filepath.Join(filepath.Dir(hab), service.HolderBinary)
		if _, err := os.Stat(beside); err == nil {
			holderBin = beside
		} else if p, err := exec.LookPath(service.HolderBinary); err == nil {
			holderBin = p
		} else {
			return "", "", fmt.Errorf("no %s beside %s or on PATH: pass -holder-bin", service.HolderBinary, hab)
		}
	}
	var err error
	if hab, err = filepath.Abs(hab); err != nil {
		return "", "", err
	}
	if holderBin, err = filepath.Abs(holderBin); err != nil {
		return "", "", err
	}
	return hab, holderBin, nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// capcomInstallNotice says, in one line each, what keeps the Capcom service from running on the
// installed hab (capcom-service.md 3.1, 10.2): Node.js missing or too old, or no credential
// command, since install never copies CLAUDE_CODE_OAUTH_TOKEN into the job.
func capcomInstallNotice(hab string, out io.Writer) {
	cpath, err := habConfigPath("")
	if err != nil {
		return
	}
	cfg, _, err := capcom.LoadServiceConfig(cpath)
	if err != nil || cfg.Engine == capcom.EngineNone {
		return
	}
	if _, err := engineArgv(hab)(); err != nil {
		fmt.Fprintf(out, "capcom: the service will not start: %v\n", err)
	}
	switch {
	case cfg.Auth == capcom.AuthAPIKey && len(cfg.APIKeyCommand) == 0:
		fmt.Fprintln(out, "capcom: set [capcom] api_key_command: an installed hab reads the key from it, and install never copies ANTHROPIC_API_KEY into the job")
	case cfg.Auth != capcom.AuthAPIKey && len(cfg.OAuthCommand) == 0:
		fmt.Fprintln(out, "capcom: set [capcom] oauth_token_command: an installed hab reads the token from it, and install never copies CLAUDE_CODE_OAUTH_TOKEN into the job")
	}
}
