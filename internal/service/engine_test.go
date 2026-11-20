package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeEngine writes an engine folder as a release's capcom/ holds it; tag goes into the engine
// file, so two tags are two engines. With modules it also holds node_modules, as the Homebrew
// formula or a developer's npm ci leaves it, with a link as npm writes in .bin.
func writeEngine(t *testing.T, dir, tag string, modules bool) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, EngineFile), []byte("// engine "+tag+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"engine"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(`{"lockfileVersion":3}`), 0o644)
	if modules {
		sdk := filepath.Join(dir, "node_modules", "sdk")
		os.MkdirAll(sdk, 0o755)
		os.WriteFile(filepath.Join(sdk, "cli"), []byte("#!/bin/sh\n"), 0o755)
		os.MkdirAll(filepath.Join(dir, "node_modules", ".bin"), 0o755)
		if err := os.Symlink("../sdk/cli", filepath.Join(dir, "node_modules", ".bin", "cli")); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInstallEngine (capcom-service.md 3.1): install copies the engine folder into a directory
// named by its hash, with its SDKs (copied when the source has node_modules, else from npm ci),
// points current at it, keeps the previous engine for an engine still running, removes older
// ones, survives an engine that fails to install, and uninstall leaves none of it.
func TestInstallEngine(t *testing.T) {
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
			before := tree(t, root)
			f := newFakeInit(t, c)
			var npm []string
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "/opt/node/bin/npm" {
					npm = append(npm, strings.Join(args, " "))
					// npm ci --prefix <dir>: the SDKs land in <dir>/node_modules.
					return nil, os.MkdirAll(filepath.Join(args[2], "node_modules", "sdk"), 0o755)
				}
				return f.run(ctx, name, args...)
			}
			src := filepath.Join(root, "capcom")
			install := func(out *bytes.Buffer) {
				t.Helper()
				err := Install(context.Background(), c, Options{HabSource: hab, HolderSource: holder, Run: run, Out: out,
					Engine: &EngineOptions{Source: src, NPM: "/opt/node/bin/npm"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			current := func() string {
				t.Helper()
				p := InstalledEngine(c.StateDir)
				if p == "" {
					t.Fatal("no installed engine")
				}
				return filepath.Dir(p)
			}

			// From a source with node_modules: copied, link and all, and no npm run.
			writeEngine(t, filepath.Join(root, "capcom"), "a", true)
			install(&bytes.Buffer{})
			a := current()
			if l, err := os.Readlink(filepath.Join(a, "node_modules", ".bin", "cli")); err != nil || l != "../sdk/cli" {
				t.Errorf("node_modules link: %q, %v", l, err)
			}
			if fi, err := os.Stat(filepath.Join(a, "node_modules", "sdk", "cli")); err != nil || fi.Mode().Perm() != 0o755 {
				t.Errorf("node_modules file: %v, %v", fi, err)
			}
			if len(npm) != 0 {
				t.Errorf("ran npm with node_modules in the source: %v", npm)
			}
			engines, _ := filepath.EvalSymlinks(EnginesDir(c.StateDir)) // a temp dir may sit behind a link
			if got := InstalledEngine(c.StateDir); !strings.HasPrefix(got, engines+"/") || !engineRE.MatchString(filepath.Base(filepath.Dir(got))) {
				t.Errorf("InstalledEngine = %s", got)
			}

			// The same engine again: the directory is kept as it is.
			os.WriteFile(filepath.Join(a, "marker"), nil, 0o600)
			install(&bytes.Buffer{})
			if current() != a {
				t.Fatalf("the same engine moved to %s", current())
			}
			if _, err := os.Stat(filepath.Join(a, "marker")); err != nil {
				t.Error("the same engine's directory was written again")
			}
			os.Remove(filepath.Join(a, "marker"))

			// A new engine from a source with no node_modules: npm ci in the new directory's
			// temporary folder. The previous engine stays.
			os.RemoveAll(src)
			writeEngine(t, src, "b", false)
			install(&bytes.Buffer{})
			b := current()
			if b == a {
				t.Fatal("a changed engine kept its directory")
			}
			if len(npm) != 1 || !strings.HasPrefix(npm[0], "ci --prefix "+EnginesDir(c.StateDir)+"/.tmp-") ||
				!strings.Contains(npm[0], "--omit=dev") || !strings.Contains(npm[0], "--ignore-scripts") {
				t.Errorf("npm runs: %v", npm)
			}
			if _, err := os.Stat(filepath.Join(b, "node_modules", "sdk")); err != nil {
				t.Errorf("npm ci's node_modules: %v", err)
			}
			if _, err := os.Stat(a); err != nil {
				t.Errorf("the previous engine is gone: %v", err)
			}

			// A third: the oldest goes.
			writeEngine(t, src, "c", false)
			install(&bytes.Buffer{})
			if _, err := os.Stat(a); err == nil {
				t.Error("the engine before the previous one is still there")
			}
			if _, err := os.Stat(b); err != nil {
				t.Errorf("the previous engine is gone: %v", err)
			}

			// An engine that does not install is a line, not a failed install.
			os.Remove(filepath.Join(src, "package-lock.json"))
			var out bytes.Buffer
			install(&out)
			if !strings.Contains(out.String(), "capcom: the engine was not installed") {
				t.Errorf("no notice of the failed engine:\n%s", out.String())
			}

			if err := Uninstall(context.Background(), c, Options{Run: run}); err != nil {
				t.Fatal(err)
			}
			for _, p := range tree(t, root) {
				if !slices.Contains(before, p) && !strings.HasPrefix(p, src) {
					t.Errorf("uninstall left %s", p)
				}
			}
		})
	}
}

// TestEngineFolder: an engine folder holds the engine and both npm files.
func TestEngineFolder(t *testing.T) {
	d := t.TempDir()
	if EngineFolder(d) {
		t.Error("an empty folder is an engine folder")
	}
	writeEngine(t, d, "a", false)
	if !EngineFolder(d) {
		t.Error("a whole engine folder is not one")
	}
	os.Remove(filepath.Join(d, "package.json"))
	if EngineFolder(d) {
		t.Error("an engine folder with no package.json is one")
	}
	if InstalledEngine(d) != "" {
		t.Error("InstalledEngine found an engine where none is installed")
	}
}
