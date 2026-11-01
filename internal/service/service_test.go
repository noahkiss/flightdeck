package service

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// testConfig is an install whose every path is under root, with names no machine has.
func testConfig(in Init, root string) Config {
	c := Config{
		Init:      in,
		Prefix:    "svctest",
		StateDir:  filepath.Join(root, "state & <dir>"),
		HolderDir: filepath.Join(root, "holders"),
		Socket:    filepath.Join(root, "run", "hab.sock"),
		UnitDir:   filepath.Join(root, "units"),
		Path:      "/opt/tools/bin:/usr/bin:/bin",
		Locale:    []string{"LANG=xx_YY.UTF-8", "LC_TIME=zz_ZZ.UTF-8"},
		UID:       4242,
	}
	if in == Launchd {
		c.Build = "0123456789abcdef"
		c.HabBundleID = "test.example.hab"
		c.HolderBundleID = "test.example.hab-holder"
	}
	return c
}

// TestRenderNoLeaks: the job files carry only what Config gives them. No user name, host name or
// home directory of the machine that renders them leaks in, and every path is absolute.
func TestRenderNoLeaks(t *testing.T) {
	var leaks []string
	if h, err := os.Hostname(); err == nil && len(h) >= 3 {
		leaks = append(leaks, h)
	}
	if u, err := user.Current(); err == nil && len(u.Username) >= 3 {
		leaks = append(leaks, u.Username)
	}
	if h, err := os.UserHomeDir(); err == nil && len(h) > 1 {
		leaks = append(leaks, h)
	}
	for _, in := range []Init{Systemd, Launchd} {
		c := testConfig(in, "/srv/fdtest")
		files, err := Render(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if !filepath.IsAbs(f.Path) || !strings.HasPrefix(f.Path, c.UnitDir+"/") {
				t.Errorf("%s: job file outside the unit directory", f.Path)
			}
			for _, l := range leaks {
				if strings.Contains(string(f.Content), l) {
					t.Errorf("%s holds %q from this machine", f.Path, l)
				}
			}
			for _, want := range []string{"/srv/fdtest/holders", "/srv/fdtest/state"} {
				if !strings.Contains(string(f.Content), want) {
					t.Errorf("%s does not name %q:\n%s", f.Path, want, f.Content)
				}
			}
		}
	}
	// The holder plist the starter writes, too.
	c := testConfig(Launchd, "/srv/fdtest")
	hp := string(holderPlist(c, "0123abcd"))
	for _, l := range leaks {
		if strings.Contains(hp, l) {
			t.Errorf("holder plist holds %q from this machine", l)
		}
	}
}

// TestValidate refuses what cannot go into a unit file or a label.
func TestValidate(t *testing.T) {
	good := testConfig(Systemd, "/srv/fdtest")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"relative state":   func(c *Config) { c.StateDir = "state" },
		"relative units":   func(c *Config) { c.UnitDir = "./units" },
		"unclean holders":  func(c *Config) { c.HolderDir = "/srv/a/../b" },
		"newline socket":   func(c *Config) { c.Socket = "/srv/a\n/b" },
		"empty prefix":     func(c *Config) { c.Prefix = "" },
		"slash prefix":     func(c *Config) { c.Prefix = "a/b" },
		"space prefix":     func(c *Config) { c.Prefix = "a b" },
		"no init":          func(c *Config) { c.Init = None },
		"newline PATH":     func(c *Config) { c.Path = "/bin\n/usr/bin" },
		"dot-first prefix": func(c *Config) { c.Prefix = ".x" },
		"other variable":   func(c *Config) { c.Locale = []string{"HOME=/x"} },
		"newline locale":   func(c *Config) { c.Locale = []string{"LANG=a\nb"} },
		"odd grants":       func(c *Config) { c.Grants = "accessibility\nX=1" },
	} {
		c := good
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSystemdQuoting: specifiers, variables, quotes and backslashes in a path reach the program
// unchanged.
func TestSystemdQuoting(t *testing.T) {
	for in, want := range map[string]string{
		`/a b/c`:   `"/a b/c"`,
		`/50%/x`:   `"/50%%/x"`,
		`/$HOME/x`: `"/$$HOME/x"`,
		`/a"b\c`:   `"/a\"b\\c"`,
	} {
		if got := systemdQuote(in, true); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := systemdQuote("PATH=/$x%", false); got != `"PATH=/$x%%"` {
		t.Errorf("environment quoting: %s", got)
	}
	u := string(holderUnit(testConfig(Systemd, "/srv/fdtest")))
	if !strings.Contains(u, `"-instance" %i`) || !strings.Contains(u, "KillMode=process") ||
		!strings.Contains(u, "Restart=on-failure") {
		t.Errorf("holder unit:\n%s", u)
	}
	if l := string(logicUnit(testConfig(Systemd, "/srv/fdtest"))); !strings.Contains(l, "Restart=always") {
		t.Errorf("logic unit:\n%s", l)
	}
}

// TestJobsCarryLocale: both units and both plists carry the locale captured at install, so the
// programs in panes get it (a job's environment is bare otherwise).
func TestJobsCarryLocale(t *testing.T) {
	sd := testConfig(Systemd, "/srv/fdtest")
	ld := testConfig(Launchd, "/srv/fdtest")
	for name, doc := range map[string][]byte{
		"logic unit": logicUnit(sd), "holder unit": holderUnit(sd),
		"logic plist": logicPlist(ld), "holder plist": holderPlist(ld, "0123abcd"),
	} {
		for _, want := range []string{"LANG", "xx_YY.UTF-8", "LC_TIME", "zz_ZZ.UTF-8", "PATH"} {
			if !strings.Contains(string(doc), want) {
				t.Errorf("%s lacks %s:\n%s", name, want, doc)
			}
		}
	}
	if u := string(logicUnit(sd)); !strings.Contains(u, `Environment="LANG=xx_YY.UTF-8"`) {
		t.Errorf("logic unit:\n%s", u)
	}
	if p := string(logicPlist(ld)); !strings.Contains(p, "<key>LC_TIME</key>\n\t\t<string>zz_ZZ.UTF-8</string>") {
		t.Errorf("logic plist:\n%s", p)
	}
	// The opt-in grant list reaches every job when it is set (macos-identity.md 7.9).
	sd.Grants, ld.Grants = "accessibility,automation:com.apple.Terminal", "accessibility"
	if u := string(logicUnit(sd)); !strings.Contains(u, `Environment="GFF_MACOS_GRANTS=accessibility,automation:com.apple.Terminal"`) {
		t.Errorf("logic unit:\n%s", u)
	}
	if p := string(holderPlist(ld, "0123abcd")); !strings.Contains(p, "<key>GFF_MACOS_GRANTS</key>\n\t\t<string>accessibility</string>") {
		t.Errorf("holder plist:\n%s", p)
	}
	if err := sd.Validate(); err != nil {
		t.Error(err)
	}
	got := LocaleEnv([]string{"LC_ALL=a", "HOME=/x", "LANG=b", "LANGUAGE=c", "LC_=d", "XLC_CTYPE=e"})
	if !slices.Equal(got, []string{"LANG=b", "LC_ALL=a"}) {
		t.Errorf("LocaleEnv = %v", got)
	}
}

// TestPlistsAreXML: both plists parse, and a path with XML specials comes back unchanged.
func TestPlistsAreXML(t *testing.T) {
	c := testConfig(Launchd, "/srv/fdtest")
	for _, doc := range [][]byte{logicPlist(c), holderPlist(c, "0123abcd")} {
		d := xml.NewDecoder(strings.NewReader(string(doc)))
		var strs []string
		for {
			tok, err := d.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, doc)
			}
			if cd, ok := tok.(xml.CharData); ok {
				strs = append(strs, string(cd))
			}
		}
		if !slices.Contains(strs, c.HabPath()) && !slices.Contains(strs, c.HolderPath()) {
			t.Errorf("no binary path with its XML specials intact in:\n%s", doc)
		}
		for _, k := range []string{"<string>Aqua</string>", "StandardOutPath", "WorkingDirectory"} {
			if !strings.Contains(string(doc), k) {
				t.Errorf("plist lacks %s:\n%s", k, doc)
			}
		}
	}
	if h := string(holderPlist(c, "0123abcd")); !strings.Contains(h, "<key>SuccessfulExit</key>\n\t\t<false/>") ||
		!strings.Contains(h, "svctest.gff-hab-holder.0123abcd") {
		t.Errorf("holder plist:\n%s", h)
	}
}

// fakeInit stands in for systemctl, loginctl and launchctl. It keeps which jobs are loaded and
// running, and writes the enable link the way systemctl does.
type fakeInit struct {
	t       *testing.T
	c       Config
	mu      sync.Mutex
	loaded  map[string]bool // launchd labels, or systemd units that run
	idle    map[string]bool // launchd labels loaded with no process
	noGUI   bool
	cmds    []string
	stopped []string
	// disableFails makes systemctl disable fail and leaves the unit running.
	disableFails bool
	// badSig makes codesign --verify fail for every path that contains it.
	badSig string
	dittos int
}

func newFakeInit(t *testing.T, c Config) *fakeInit {
	return &fakeInit{t: t, c: c, loaded: map[string]bool{}, idle: map[string]bool{}}
}

func (f *fakeInit) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	fail := errors.New("exit status 1")
	switch name {
	case "ditto":
		f.dittos++
		return nil, copyTree(args[0], args[1])
	case "codesign":
		item := args[len(args)-1]
		if args[0] == "-d" {
			return []byte("Executable=" + item + "\ndesignated => identifier \"test.example\" and anchor apple generic\n"), nil
		}
		if f.badSig != "" && strings.Contains(item, f.badSig) {
			return []byte(item + ": a sealed resource is missing or invalid"), fail
		}
		if _, err := os.Stat(filepath.Join(item, "Contents", "Info.plist")); err != nil {
			return nil, fail
		}
		return nil, nil
	case "loginctl":
		return []byte("no\n"), nil
	case "systemctl":
		if args[0] != "--user" {
			f.t.Fatalf("systemctl without --user: %v", args)
		}
		switch args[1] {
		case "enable":
			wants := filepath.Join(f.c.UnitDir, "default.target.wants")
			os.MkdirAll(wants, 0o755)
			os.Remove(filepath.Join(wants, args[2])) // enable is idempotent
			return nil, os.Symlink(filepath.Join(f.c.UnitDir, args[2]), filepath.Join(wants, args[2]))
		case "restart", "start":
			f.loaded[args[2]] = true
		case "list-units":
			var out []string
			for u := range f.loaded {
				if strings.Contains(u, "-gff-hab-holder@") {
					out = append(out, u+" loaded active running holder")
				}
			}
			return []byte(strings.Join(out, "\n")), nil
		case "stop":
			for u := range f.loaded {
				if ok, _ := filepath.Match(args[2], u); ok {
					delete(f.loaded, u)
					f.stopped = append(f.stopped, u)
				}
			}
		case "is-active":
			if f.loaded[args[2]] {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), fail
		case "disable":
			if f.disableFails {
				return nil, fail
			}
			delete(f.loaded, args[len(args)-1])
			os.Remove(filepath.Join(f.c.UnitDir, "default.target.wants", args[len(args)-1]))
		}
		return nil, nil
	case "launchctl":
		switch args[0] {
		case "print":
			if args[1] == f.c.domain() {
				if f.noGUI {
					return nil, fail
				}
				return nil, nil
			}
			label := strings.TrimPrefix(args[1], f.c.domain()+"/")
			if f.idle[label] {
				return []byte("\tstate = not running\n"), nil
			}
			if !f.loaded[label] {
				return nil, fail
			}
			return []byte("\tstate = running\n\tpid = 99\n"), nil
		case "bootstrap":
			if _, err := os.Stat(args[2]); err != nil {
				return nil, err
			}
			f.loaded[strings.TrimSuffix(filepath.Base(args[2]), ".plist")] = true
		case "bootout":
			label := strings.TrimPrefix(args[1], f.c.domain()+"/")
			if !f.loaded[label] && !f.idle[label] {
				return nil, fail
			}
			delete(f.loaded, label)
			delete(f.idle, label)
			f.stopped = append(f.stopped, label)
		}
		return nil, nil
	}
	f.t.Fatalf("unexpected command %s %v", name, args)
	return nil, nil
}

// tree lists every file, link and directory under root.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// copyTree copies a directory tree, as ditto does for a bundle.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(out, fi.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, fi.Mode().Perm())
	})
}

// writeBundle writes a helper bundle as scripts/macos-bundle.sh lays it out: an Info.plist and
// the executable. tag goes into the executable, so two tags are two builds.
func writeBundle(t *testing.T, dir string, spec bundleSpec, id, tag string) string {
	t.Helper()
	b := filepath.Join(dir, spec.name)
	os.MkdirAll(filepath.Join(b, "Contents", "MacOS"), 0o755)
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>` + spec.exe + `</string>
	<key>CFBundleIdentifier</key>
	<string>` + id + `</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSLocalNetworkUsageDescription</key>
	<string>A program running in a Flightdeck pane wants to connect to devices on your local network.</string>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(b, "Contents", "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "Contents", "MacOS", spec.exe), []byte("#!/bin/sh\necho "+tag+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

// writeBundles writes both helper bundles of one build into dir.
func writeBundles(t *testing.T, dir, tag string) (hab, holder string) {
	t.Helper()
	return writeBundle(t, dir, habSpec, "test.example.hab", tag), writeBundle(t, dir, holderSpec, "test.example.hab-holder", tag)
}

// writeSources writes what install takes for init system in: bare binaries, or bundles.
func writeSources(t *testing.T, in Init, dir string) (hab, holder string) {
	t.Helper()
	if in == Launchd {
		return writeBundles(t, dir, "a")
	}
	return writeBinaries(t, dir)
}

func writeBinaries(t *testing.T, dir string) (hab, holder string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	hab, holder = filepath.Join(dir, "gff-hab"), filepath.Join(dir, "gff-hab-holder")
	os.WriteFile(hab, []byte("#!/bin/sh\necho hab\n"), 0o755)
	os.WriteFile(holder, []byte("#!/bin/sh\necho holder\n"), 0o755)
	return hab, holder
}

// TestUninstallRemovesWhatInstallWrote (core test 13 at unit level): Files lists exactly what
// install and the starter added to the disk, and uninstall removes exactly that, after asking
// once while holders run. A no changes nothing.
func TestUninstallRemovesWhatInstallWrote(t *testing.T) {
	for _, in := range []Init{Systemd, Launchd} {
		t.Run(string(in), func(t *testing.T) {
			root := t.TempDir()
			c := testConfig(in, root)
			hab, holder := writeSources(t, in, filepath.Join(root, "src"))
			if in == Launchd {
				if _, _, err := ResolveBundles(&c, hab, holder); err != nil {
					t.Fatal(err)
				}
			}
			// The unit directory is not there yet: install makes it, and uninstall removes it.
			os.MkdirAll(c.StateDir, 0o700)
			os.WriteFile(filepath.Join(c.StateDir, "state.json"), []byte("{}"), 0o600) // gff-hab's own
			before := tree(t, root)

			f := newFakeInit(t, c)
			if err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run}); err != nil {
				t.Fatal(err)
			}
			// A second install (an update) writes the same files.
			if err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run}); err != nil {
				t.Fatal(err)
			}
			ids := []string{"aaaa0001", "aaaa0002"}
			st, err := NewStarter(c, f.run, func() string { id := ids[0]; ids = ids[1:]; return id }, t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := st.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if in == Launchd { // launchd writes the logs
				for _, l := range []string{c.logicLog(), c.holderLog("aaaa0001"), c.holderLog("aaaa0002")} {
					os.WriteFile(l, []byte("log"), 0o600)
				}
			}
			after := tree(t, root)
			var added []string
			for _, p := range after {
				if !slices.Contains(before, p) {
					added = append(added, p)
				}
			}
			listed, err := Files(c)
			if err != nil {
				t.Fatal(err)
			}
			// A build directory is listed as one path: uninstall removes it with its bundles.
			underBuild := func(p string) bool {
				for _, l := range listed {
					if filepath.Dir(l) == c.BuildsDir() && strings.HasPrefix(p, l+"/") {
						return true
					}
				}
				return false
			}
			for _, p := range added {
				if !slices.Contains(listed, p) && !underBuild(p) {
					t.Errorf("install wrote %s, which Files does not list", p)
				}
			}
			for _, p := range listed {
				if !slices.Contains(added, p) {
					t.Errorf("Files lists %s, which install did not write", p)
				}
			}

			// Declined: nothing changes, no job stops.
			f.stopped = nil
			err = Uninstall(context.Background(), c, Options{Run: f.run, Confirm: func(string) bool { return false }})
			if !errors.Is(err, ErrDeclined) {
				t.Fatalf("declined uninstall: %v", err)
			}
			if !slices.Equal(tree(t, root), after) || len(f.stopped) != 0 {
				t.Fatalf("a declined uninstall changed something; stopped %v", f.stopped)
			}

			asked := 0
			if err := Uninstall(context.Background(), c, Options{Run: f.run, Confirm: func(string) bool { asked++; return true }}); err != nil {
				t.Fatal(err)
			}
			if asked != 1 {
				t.Errorf("asked %d times, want once", asked)
			}
			left := tree(t, root)
			for _, p := range left {
				if !slices.Contains(before, p) {
					t.Errorf("uninstall left %s", p)
				}
			}
			for _, p := range before {
				if !slices.Contains(left, p) {
					t.Errorf("uninstall removed %s, which install did not write", p)
				}
			}
			if len(f.loaded) != 0 {
				t.Errorf("jobs still loaded: %v", f.loaded)
			}
		})
	}
}

// TestLaunchdNoGUI: with no gui/<uid> domain, install and the starter refuse, write nothing and
// load nothing (spec 5.3, Q5).
func TestLaunchdNoGUI(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	hab, holder := writeBundles(t, filepath.Join(root, "src"), "a")
	before := tree(t, root)
	f := newFakeInit(t, c)
	f.noGUI = true
	err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run})
	if !errors.Is(err, ErrNoGUI) {
		t.Fatalf("install with no GUI login: %v", err)
	}
	st, _ := NewStarter(c, f.run, func() string { return "aaaa0001" }, t.Logf)
	if err := st.Start(context.Background()); !errors.Is(err, ErrNoGUI) {
		t.Fatalf("start with no GUI login: %v", err)
	}
	if !slices.Equal(tree(t, root), before) || len(f.loaded) != 0 {
		t.Fatal("a refused install changed something")
	}
}

// TestLaunchdRetire: a holder that said bye loses its job and plist once its process is gone.
func TestLaunchdRetire(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	os.MkdirAll(c.UnitDir, 0o755)
	f := newFakeInit(t, c)
	st, _ := NewStarter(c, f.run, func() string { return "aaaa0001" }, t.Logf)
	if err := st.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	f.mu.Lock()
	f.loaded[c.HolderJob("aaaa0001")] = true // loaded; the fake says it has a pid, so first stop it
	f.mu.Unlock()
	st.Retire("aaaa0001", cmd.Process.Pid)
	if _, err := os.Stat(c.holderPlist("aaaa0001")); err != nil {
		t.Fatalf("a job launchd still runs was retired: %v", err)
	}
	f.mu.Lock()
	delete(f.loaded, c.HolderJob("aaaa0001"))
	f.mu.Unlock()
	st.Retire("aaaa0001", cmd.Process.Pid)
	if _, err := os.Stat(c.holderPlist("aaaa0001")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist after retire: %v", err)
	}
}

// TestUninstallKeepsWhatInstallDidNotWrite: a state directory the user already had (here the
// "home" of a user who passed -state ~) keeps every file install did not write: a logs directory
// with the user's own files, and a gff-hab in <state>/bin that install was pointed at and so
// never wrote. The holder binary install did write goes, and so does each directory install
// made, but only once it is empty.
func TestUninstallKeepsWhatInstallDidNotWrite(t *testing.T) {
	for _, in := range []Init{Systemd, Launchd} {
		t.Run(string(in), func(t *testing.T) {
			root := t.TempDir()
			c := testConfig(in, root)
			c.StateDir = filepath.Join(root, "home")
			c.UnitDir = filepath.Join(root, "home", "units", "user")
			userBin := filepath.Join(c.StateDir, "bin")
			hab, _ := writeBinaries(t, userBin) // the user's own gff-hab, in what is <state>/bin
			_, holder := writeBinaries(t, filepath.Join(root, "src"))
			os.Remove(filepath.Join(userBin, HolderBinary))
			if in == Launchd { // launchd installs bundles and leaves <state>/bin alone
				hab, holder = writeBundles(t, filepath.Join(root, "src"), "a")
			}
			os.MkdirAll(c.LogDir(), 0o755)
			os.WriteFile(filepath.Join(c.LogDir(), "notes.txt"), []byte("mine"), 0o600)
			before := tree(t, root)

			f := newFakeInit(t, c)
			if err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run}); err != nil {
				t.Fatal(err)
			}
			if in == Launchd {
				os.WriteFile(c.logicLog(), []byte("log"), 0o600)
			}
			if err := Uninstall(context.Background(), c, Options{Run: f.run}); err != nil {
				t.Fatal(err)
			}
			left := tree(t, root)
			if !slices.Equal(left, before) {
				t.Errorf("before install:\n%s\nafter uninstall:\n%s", strings.Join(before, "\n"), strings.Join(left, "\n"))
			}
		})
	}
}

// TestUninstallNoRecord: an install with no record (written before records existed) loses its
// job files by name, and nothing else: <state>/bin and <state>/logs stay.
func TestUninstallNoRecord(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Systemd, root)
	hab, holder := writeBinaries(t, filepath.Join(root, "src"))
	f := newFakeInit(t, c)
	if err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run}); err != nil {
		t.Fatal(err)
	}
	os.Remove(c.manifestPath())
	var out strings.Builder
	if err := Uninstall(context.Background(), c, Options{Run: f.run, Out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{c.logicFile(), filepath.Join(c.UnitDir, c.holderTemplate()), c.enableLink()} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(c.HabPath()); err != nil {
		t.Errorf("a binary went with no record of writing it: %v", err)
	}
	if !strings.Contains(out.String(), "no install record") {
		t.Errorf("output: %q", out.String())
	}
}

// TestReinstallOtherPrefix: an install over a state directory that another prefix installed into
// is refused, and so is an uninstall with the wrong prefix; neither changes a file or a job.
func TestReinstallOtherPrefix(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Systemd, root)
	hab, holder := writeBinaries(t, filepath.Join(root, "src"))
	f := newFakeInit(t, c)
	if err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run}); err != nil {
		t.Fatal(err)
	}
	before := tree(t, root)
	f.cmds = nil
	other := c
	other.Prefix = "svcother"
	err := Install(context.Background(), other, Options{HabSource: hab, HolderSource: holder, Run: f.run})
	if !errors.Is(err, ErrOtherInstall) || !strings.Contains(err.Error(), "-prefix svctest") {
		t.Fatalf("install with another prefix: %v", err)
	}
	if err := Uninstall(context.Background(), other, Options{Run: f.run}); !errors.Is(err, ErrOtherInstall) {
		t.Fatalf("uninstall with another prefix: %v", err)
	}
	if !slices.Equal(tree(t, root), before) || len(f.cmds) != 0 {
		t.Fatalf("a refused install changed something; ran %v", f.cmds)
	}
}

// TestSystemdDisableFails: when disable --now fails and the logic unit still runs, uninstall
// stops there: it stops no holder, which the logic job would start again, and removes no file.
func TestSystemdDisableFails(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Systemd, root)
	hab, holder := writeBinaries(t, filepath.Join(root, "src"))
	f := newFakeInit(t, c)
	if err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run}); err != nil {
		t.Fatal(err)
	}
	before := tree(t, root)
	f.disableFails = true
	if err := Uninstall(context.Background(), c, Options{Run: f.run}); err == nil || !strings.Contains(err.Error(), "is active") {
		t.Fatalf("uninstall with a logic job that will not stop: %v", err)
	}
	if !slices.Equal(tree(t, root), before) || len(f.stopped) != 0 {
		t.Fatalf("uninstall went on; stopped %v", f.stopped)
	}
	// Not loaded at all: disable fails, and that is fine.
	f.mu.Lock()
	delete(f.loaded, c.LogicJob())
	f.mu.Unlock()
	if err := Uninstall(context.Background(), c, Options{Run: f.run}); err != nil {
		t.Fatal(err)
	}
}

// TestLaunchdRetireReusedPid: a holder whose pid now belongs to another process is retired at
// once, because launchd runs no process for its job; its plist and log go.
func TestLaunchdRetireReusedPid(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	os.MkdirAll(c.UnitDir, 0o755)
	f := newFakeInit(t, c)
	st, _ := NewStarter(c, f.run, func() string { return "aaaa0001" }, t.Logf)
	if err := st.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.holderLog("aaaa0001"), []byte("log"), 0o600)
	f.mu.Lock()
	delete(f.loaded, c.HolderJob("aaaa0001"))
	f.idle[c.HolderJob("aaaa0001")] = true // loaded, exited
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		st.Retire("aaaa0001", os.Getpid()) // a live pid: this test's own
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second): // Retire's own wait is a minute
		t.Fatal("Retire waits on a reused pid")
	}
	for _, p := range []string{c.holderPlist("aaaa0001"), c.holderLog("aaaa0001")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after retire: %v", p, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loaded[c.HolderJob("aaaa0001")] || f.idle[c.HolderJob("aaaa0001")] {
		t.Error("the job is still loaded")
	}
}

// TestLaunchdSweep: at start gff-hab removes the plist and log of each holder that is gone, and
// keeps every holder that runs: one with a live registry entry, and one launchd runs.
func TestLaunchdSweep(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	os.MkdirAll(c.UnitDir, 0o755)
	os.MkdirAll(c.LogDir(), 0o700)
	os.MkdirAll(c.HolderDir, 0o700)
	f := newFakeInit(t, c)
	insts := []string{"aaaa0001", "aaaa0002", "aaaa0003", "aaaa0004"}
	next := slices.Clone(insts)
	st, _ := NewStarter(c, f.run, func() string { id := next[0]; next = next[1:]; return id }, t.Logf)
	for range insts {
		if err := st.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range insts {
		os.WriteFile(c.holderLog(i), []byte("log"), 0o600)
	}
	os.WriteFile(c.holderLog("aaaa0009"), []byte("log"), 0o600) // a log left with no plist
	f.mu.Lock()
	// 0001: a live registry entry, launchd reports no process: kept. 0002: loaded, exited
	// cleanly, its registry entry stale: removed. 0003: not loaded: removed. 0004: launchd runs
	// it: kept.
	delete(f.loaded, c.HolderJob("aaaa0001"))
	f.idle[c.HolderJob("aaaa0001")] = true
	delete(f.loaded, c.HolderJob("aaaa0002"))
	f.idle[c.HolderJob("aaaa0002")] = true
	delete(f.loaded, c.HolderJob("aaaa0003"))
	f.mu.Unlock()
	writeEntry(t, c.HolderDir, "aaaa0001", os.Getpid())
	writeEntry(t, c.HolderDir, "aaaa0002", 0x7ffffff0) // a pid that does not run

	st.Sweep(context.Background())
	for i, keep := range map[string]bool{"aaaa0001": true, "aaaa0002": false, "aaaa0003": false, "aaaa0004": true} {
		for _, p := range []string{c.holderPlist(i), c.holderLog(i)} {
			_, err := os.Stat(p)
			if keep && err != nil {
				t.Errorf("%s: %v", p, err)
			}
			if !keep && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s survived the sweep: %v", p, err)
			}
		}
	}
	if _, err := os.Stat(c.holderLog("aaaa0009")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an orphan log survived the sweep: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idle[c.HolderJob("aaaa0002")] || !f.loaded[c.HolderJob("aaaa0004")] {
		t.Errorf("jobs after the sweep: loaded %v, idle %v", f.loaded, f.idle)
	}
}

// writeEntry writes a registry file for instance with pid.
func writeEntry(t *testing.T, dir, instance string, pid int) {
	t.Helper()
	e := holderreg.Entry{Instance: instance, Build: "b", Pid: pid, Socket: holderreg.SocketPath(dir, instance), Started: time.Now()}
	if err := holderreg.Write(dir, e); err != nil {
		t.Fatal(err)
	}
}
