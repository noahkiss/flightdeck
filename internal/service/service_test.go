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
)

// testConfig is an install whose every path is under root, with names no machine has.
func testConfig(in Init, root string) Config {
	return Config{
		Init:      in,
		Prefix:    "svctest",
		StateDir:  filepath.Join(root, "state & <dir>"),
		HolderDir: filepath.Join(root, "holders"),
		Socket:    filepath.Join(root, "run", "hab.sock"),
		UnitDir:   filepath.Join(root, "units"),
		Path:      "/opt/tools/bin:/usr/bin:/bin",
		UID:       4242,
	}
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
		if !slices.Contains(strs, filepath.Join(c.StateDir, "bin")+"/"+HabBinary) &&
			!slices.Contains(strs, filepath.Join(c.StateDir, "bin")+"/"+HolderBinary) {
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
	noGUI   bool
	cmds    []string
	stopped []string
}

func newFakeInit(t *testing.T, c Config) *fakeInit {
	return &fakeInit{t: t, c: c, loaded: map[string]bool{}}
}

func (f *fakeInit) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	fail := errors.New("exit status 1")
	switch name {
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
		case "disable":
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
			if !f.loaded[label] {
				return nil, fail
			}
			delete(f.loaded, label)
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
			hab, holder := writeBinaries(t, filepath.Join(root, "src"))
			os.MkdirAll(c.StateDir, 0o700)
			os.MkdirAll(c.UnitDir, 0o755)
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
			if in == Launchd {
				os.WriteFile(filepath.Join(c.LogDir(), "gff-hab.log"), []byte("log"), 0o600) // launchd writes it
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
			for _, p := range added {
				covered := slices.Contains(listed, p) || (in == Launchd && strings.HasPrefix(p, c.LogDir()+"/"))
				if !covered && p != filepath.Join(c.UnitDir, "default.target.wants") {
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
				if !slices.Contains(before, p) && p != filepath.Join(c.UnitDir, "default.target.wants") {
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
	hab, holder := writeBinaries(t, filepath.Join(root, "src"))
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
