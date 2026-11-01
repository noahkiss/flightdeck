package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLaunchdPlistsNameTheBundle: both plists run the executable inside the build's bundle, by its
// real path (never through current), name the bundle in AssociatedBundleIdentifiers, and the
// logic job passes -build so serve finds its own holder bundle.
func TestLaunchdPlistsNameTheBundle(t *testing.T) {
	c := testConfig(Launchd, "/srv/fdtest")
	build := "/srv/fdtest/state & <dir>/builds/0123456789abcdef/"
	for _, tc := range []struct {
		doc  []byte
		prog string
		id   string
	}{
		{logicPlist(c), build + "GFF Hab.app/Contents/MacOS/gff-hab", "test.example.hab"},
		{holderPlist(c, "0123abcd"), build + "GFF Holder.app/Contents/MacOS/gff-hab-holder", "test.example.hab-holder"},
	} {
		m, err := plistStrings(strings.NewReader(string(tc.doc)))
		if err != nil {
			t.Fatal(err)
		}
		if got := first(m["ProgramArguments"]); got != tc.prog {
			t.Errorf("program %q, want %q", got, tc.prog)
		}
		if got := m["AssociatedBundleIdentifiers"]; !slices.Equal(got, []string{tc.id}) {
			t.Errorf("AssociatedBundleIdentifiers %q, want [%s]", got, tc.id)
		}
		if strings.Contains(string(tc.doc), "/current/") {
			t.Errorf("a plist names current:\n%s", tc.doc)
		}
	}
	m, _ := plistStrings(strings.NewReader(string(logicPlist(c))))
	args := m["ProgramArguments"]
	if i := slices.Index(args, "-build"); i < 0 || i+1 >= len(args) || args[i+1] != c.Build {
		t.Errorf("logic job arguments %q lack -build %s", args, c.Build)
	}
	// Systemd keeps <state>/bin and no -build.
	sd := testConfig(Systemd, "/srv/fdtest")
	if sd.HabPath() != "/srv/fdtest/state & <dir>/bin/gff-hab" || slices.Contains(sd.ServeArgs(), "-build") {
		t.Errorf("systemd: %s %q", sd.HabPath(), sd.ServeArgs())
	}
}

// TestLaunchdNeedsBundle: launchd jobs are not rendered, or holders started, without a build and
// bundle ids, and install refuses bare binaries.
func TestLaunchdNeedsBundle(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	c.Build, c.HabBundleID = "", ""
	if _, err := Render(c); err == nil {
		t.Error("Render without a build")
	}
	if _, err := NewStarter(c, nil, nil, nil); err == nil {
		t.Error("NewStarter without a build")
	}
	c = testConfig(Launchd, root)
	hab, holder := writeBinaries(t, filepath.Join(root, "src"))
	f := newFakeInit(t, c)
	before := tree(t, root)
	err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run})
	if err == nil || !strings.Contains(err.Error(), "not in an app bundle") {
		t.Fatalf("install of bare binaries: %v", err)
	}
	if !slices.Equal(tree(t, root), before) {
		t.Error("a refused install wrote something")
	}
}

// TestResolveBundles: the sources may be the bundles or their executables; the build name is a
// hash of their content; a dev bundle and a release bundle, or two prefixes, do not pair.
func TestResolveBundles(t *testing.T) {
	root := t.TempDir()
	hab, holder := writeBundles(t, filepath.Join(root, "a"), "a")
	var c1, c2 Config
	if _, _, err := ResolveBundles(&c1, hab, holder); err != nil {
		t.Fatal(err)
	}
	h, o, err := ResolveBundles(&c2, filepath.Join(hab, "Contents", "MacOS", HabBinary), holder)
	if err != nil || h != hab || o != holder {
		t.Fatalf("from the executable: %s %s %v", h, o, err)
	}
	if !ValidBuild(c1.Build) || c1.Build != c2.Build || c1.HabBundleID != "test.example.hab" || c1.HolderBundleID != "test.example.hab-holder" {
		t.Fatalf("resolved %+v and %+v", c1, c2)
	}
	hb, ob := writeBundles(t, filepath.Join(root, "b"), "b")
	var c3 Config
	if _, _, err := ResolveBundles(&c3, hb, ob); err != nil || c3.Build == c1.Build {
		t.Fatalf("another build: %q, %v", c3.Build, err)
	}
	dev := writeBundle(t, filepath.Join(root, "dev"), holderSpec, "test.example.hab-holder.dev", "a")
	if _, _, err := ResolveBundles(&c3, hab, dev); err == nil {
		t.Error("a release gff-hab paired with a dev holder")
	}
	other := writeBundle(t, filepath.Join(root, "other"), holderSpec, "test.other.hab-holder", "a")
	if _, _, err := ResolveBundles(&c3, hab, other); err == nil {
		t.Error("two prefixes paired")
	}
	if _, _, err := ResolveBundles(&c3, holder, hab); err == nil {
		t.Error("the bundles swapped")
	}
}

// TestLaunchdPerBuildCopies: each build gets its own directory, written once by ditto into a
// temporary directory and a rename. A second build leaves the first one's files alone while a
// holder job names it; retiring that holder removes the first build. Reinstalling a build copies
// nothing.
func TestLaunchdPerBuildCopies(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	f := newFakeInit(t, c)
	habA, holderA := writeBundles(t, filepath.Join(root, "src-a"), "a")
	habB, holderB := writeBundles(t, filepath.Join(root, "src-b"), "b")
	ca, cb := c, c
	ResolveBundles(&ca, habA, holderA)
	ResolveBundles(&cb, habB, holderB)

	if err := Install(context.Background(), c, Options{HabSource: habA, HolderSource: holderA, Run: f.run}); err != nil {
		t.Fatal(err)
	}
	exeA := ca.HolderPath()
	fiA, err := os.Stat(exeA)
	if err != nil {
		t.Fatal(err)
	}
	if prog, _ := programOf(c.logicFile()); prog != ca.HabPath() {
		t.Errorf("logic job runs %s, want %s", prog, ca.HabPath())
	}
	if l, _ := os.Readlink(c.CurrentLink()); l != filepath.Join("builds", ca.Build) {
		t.Errorf("current -> %s", l)
	}
	// A holder of build A runs.
	st, err := NewStarter(ca, f.run, func() string { return "aaaa0001" }, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Reinstall A: nothing copied.
	f.dittos = 0
	if err := Install(context.Background(), c, Options{HabSource: habA, HolderSource: holderA, Run: f.run}); err != nil {
		t.Fatal(err)
	}
	if f.dittos != 0 {
		t.Errorf("reinstalling a build copied %d bundles", f.dittos)
	}

	// Install B: A stays, untouched, because the holder's job names it.
	if err := Install(context.Background(), c, Options{HabSource: habB, HolderSource: holderB, Run: f.run}); err != nil {
		t.Fatal(err)
	}
	fiA2, err := os.Stat(exeA)
	if err != nil {
		t.Fatalf("build A went while its holder's job names it: %v", err)
	}
	if !os.SameFile(fiA, fiA2) || fiA.ModTime() != fiA2.ModTime() {
		t.Error("build A's holder executable was written again")
	}
	if prog, _ := programOf(c.logicFile()); prog != cb.HabPath() {
		t.Errorf("logic job runs %s after the update, want %s", prog, cb.HabPath())
	}
	if prog, _ := programOf(c.holderPlist("aaaa0001")); prog != exeA {
		t.Errorf("holder job runs %s, want build A's %s", prog, exeA)
	}
	if l, _ := os.Readlink(c.CurrentLink()); l != filepath.Join("builds", cb.Build) {
		t.Errorf("current -> %s after the update", l)
	}
	if ents, _ := os.ReadDir(c.BuildsDir()); len(ents) != 2 {
		t.Errorf("builds: %v", ents)
	}

	// Retire A's holder (build B's gff-hab does it after drain): build A goes.
	stB, err := NewStarter(cb, f.run, func() string { return "bbbb0001" }, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.loaded, c.HolderJob("aaaa0001"))
	f.mu.Unlock()
	stB.Retire("aaaa0001", 0)
	if _, err := os.Stat(ca.BuildDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("build A after its last job went: %v", err)
	}
	if _, err := os.Stat(cb.HabPath()); err != nil {
		t.Errorf("build B: %v", err)
	}
}

// TestLaunchdCopyFailsVerify: a copy that fails codesign leaves no build directory and no
// temporary directory, and the jobs are not touched.
func TestLaunchdCopyFailsVerify(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	f := newFakeInit(t, c)
	hab, holder := writeBundles(t, filepath.Join(root, "src"), "a")
	f.badSig = filepath.Join(".tmp-")
	err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: f.run})
	if err == nil || !strings.Contains(err.Error(), "codesign --verify --strict") {
		t.Fatalf("install of a copy that fails codesign: %v", err)
	}
	if ents, _ := os.ReadDir(c.BuildsDir()); len(ents) != 0 {
		t.Errorf("left in builds: %v", ents)
	}
	if _, err := os.Stat(c.logicFile()); !errors.Is(err, os.ErrNotExist) || len(f.loaded) != 0 {
		t.Errorf("a failed install wrote or loaded a job: %v %v", err, f.loaded)
	}
}

// TestPruneKeepsWhatIsInUse: a prune keeps the build current names, the caller's own build, a
// build a holder plist names, and a fresh temporary directory; it removes the rest, and never a
// directory whose name is not a build's.
func TestPruneKeepsWhatIsInUse(t *testing.T) {
	root := t.TempDir()
	c := testConfig(Launchd, root)
	os.MkdirAll(c.UnitDir, 0o755)
	names := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc", "dddddddddddddddd",
		".tmp-eeeeeeeeeeeeeeee-1", "notabuild"}
	for _, n := range names {
		os.MkdirAll(filepath.Join(c.BuildsDir(), n), 0o755)
	}
	c.Build = "aaaaaaaaaaaaaaaa"
	os.Symlink(filepath.Join("builds", "bbbbbbbbbbbbbbbb"), c.CurrentLink())
	hc := c
	hc.Build = "cccccccccccccccc"
	os.WriteFile(c.holderPlist("aaaa0001"), holderPlist(hc, "aaaa0001"), 0o644)
	removed, err := pruneBuilds(c)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"dddddddddddddddd"}) {
		t.Errorf("removed %q, want only the unused build", removed)
	}
}

// TestPlistStrings reads string values and string arrays at the top level only.
func TestPlistStrings(t *testing.T) {
	doc := `<?xml version="1.0"?><plist version="1.0"><dict>
	<key>A</key><string>a &amp; b</string>
	<key>N</key><dict><key>Inner</key><string>x</string></dict>
	<key>L</key><array><string>1</string><string>2</string></array>
	<key>B</key><true/>
	<key>C</key><string>c</string>
	</dict></plist>`
	m, err := plistStrings(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if first(m["A"]) != "a & b" || !slices.Equal(m["L"], []string{"1", "2"}) || first(m["C"]) != "c" ||
		m["Inner"] != nil || m["B"] != nil || m["N"] != nil {
		t.Errorf("%q", m)
	}
	if _, err := plistStrings(strings.NewReader("<plist><dict><key>A</key>")); err == nil {
		t.Error("a cut property list parsed")
	}
}
