package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// putPlatform writes the SDKs' platform packages of this system under dir, as npm ci does.
func putPlatform(t *testing.T, dir string) {
	t.Helper()
	for _, p := range PlatformPackages(runtime.GOOS, runtime.GOARCH) {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

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
		putPlatform(t, dir)
	}
}

// TestInstallEngine (capcom-service.md 3.1): install copies the engine folder into a directory
// named by its hash, with its SDKs (copied when the source has node_modules, else from npm ci),
// points current at it, keeps the previous engine for an engine still running, removes older
// ones, keeps the previous engine current when a new one fails to install, and uninstall
// leaves none of it.
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
			var npmFail error // the next npm ci fails with this and a page of output
			platform := true  // npm ci installs the platform packages
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "/opt/node/bin/npm" {
					npm = append(npm, strings.Join(args, " "))
					if npmFail != nil {
						var page []string
						for i := range 40 {
							page = append(page, "npm error line "+string(rune('a'+i%26)))
						}
						page = append(page, "npm error last line")
						return []byte(strings.Join(page, "\n")), npmFail
					}
					// npm ci --prefix <dir>: the SDKs land in <dir>/node_modules.
					if platform {
						putPlatform(t, args[2])
					}
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
				!strings.Contains(npm[0], "--omit=dev") || !strings.Contains(npm[0], "--ignore-scripts") ||
				!strings.Contains(npm[0], "--include=optional") || !strings.Contains(npm[0], "--fetch-retries=1") {
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

			// An engine that does not install is a line, not a failed install, and the previous
			// engine stays current, named in the line (3.1).
			cur := current()
			kept := "install keeps the previous engine " + filepath.Base(cur)[:8]
			failed := func(why string, wantInOut ...string) {
				t.Helper()
				var out bytes.Buffer
				install(&out)
				if !strings.Contains(out.String(), "capcom: the new engine was not installed; "+kept) {
					t.Errorf("%s: no notice naming the kept engine:\n%s", why, out.String())
				}
				for _, w := range wantInOut {
					if !strings.Contains(out.String(), w) {
						t.Errorf("%s: no %q in:\n%s", why, w, out.String())
					}
				}
				if got := current(); got != cur {
					t.Errorf("%s: current moved to %s, want the previous %s", why, got, cur)
				}
				if dirs, _ := engineDirs(c.StateDir); len(dirs) != 2 {
					t.Errorf("%s: engine directories %v, want the current and the previous", why, dirs)
				}
			}
			writeEngine(t, src, "d", false)
			npmFail = errors.New("exit status 1")
			failed("npm ci failed", "npm error last line")
			npmFail = nil
			platform = false
			failed("no platform package", "npm installed no claude-agent-sdk-")
			platform = true
			os.Remove(filepath.Join(src, "package-lock.json"))
			failed("not an engine folder")
			// The notice holds npm's last lines, not its whole output.
			var out bytes.Buffer
			writeEngine(t, src, "d", false)
			npmFail = errors.New("exit status 1")
			install(&out)
			npmFail = nil
			if strings.Contains(out.String(), "npm error line a") {
				t.Errorf("the notice holds npm's whole output:\n%s", out.String())
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

// TestEngineNPM: npm ci and the node_modules copy stop at engineTimeout with a line that names
// the limit, install says while npm ci runs that it still runs, and an engine directory another install wrote first counts as this
// one's (capcom-service.md 3.1).
func TestEngineNPM(t *testing.T) {
	defer func(d, p time.Duration) { engineTimeout, engineProgress = d, p }(engineTimeout, engineProgress)
	engineTimeout, engineProgress = 300*time.Millisecond, 50*time.Millisecond
	root := t.TempDir()
	src := filepath.Join(root, "capcom")
	writeEngine(t, src, "a", false)
	hang := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return []byte("npm http fetch GET attempt 1 failed"), errors.New("signal: killed")
	}
	state := filepath.Join(root, "state")
	os.MkdirAll(EnginesDir(state), 0o700)
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	var out bytes.Buffer
	err := buildEngine(ctx, hang, &out, EngineOptions{Source: src}, EnginesDir(state), "0123456789abcdef",
		filepath.Join(EnginesDir(state), "0123456789abcdef"))
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("a hung npm ci: %v", err)
	}
	if !strings.Contains(out.String(), "npm ci still running") {
		t.Errorf("no progress line:\n%s", out.String())
	}
	if dirs, _ := engineDirs(state); len(dirs) != 0 {
		t.Errorf("a hung npm ci left %v", dirs)
	}

	// Another install renamed the same engine into place while this one ran.
	dst := filepath.Join(EnginesDir(state), "0123456789abcdef")
	race := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		putPlatform(t, args[2])
		return nil, os.MkdirAll(filepath.Join(dst, "node_modules"), 0o755)
	}
	if err := buildEngine(context.Background(), race, &out, EngineOptions{Source: src}, EnginesDir(state), "0123456789abcdef", dst); err != nil {
		t.Errorf("an engine another install wrote first: %v", err)
	}

	// The copy of a node_modules the source holds stops with the context too.
	writeEngine(t, filepath.Join(root, "withmods"), "b", true)
	done, stop := context.WithCancel(context.Background())
	stop()
	if err := copyTree(done, filepath.Join(root, "withmods", "node_modules"), filepath.Join(root, "copy")); !errors.Is(err, context.Canceled) {
		t.Errorf("copyTree after the context ended: %v", err)
	}

	// The notices name the limit in words.
	engineTimeout = 10 * time.Minute
	if got := limit(); got != "10 minutes" {
		t.Errorf("limit() = %q, want 10 minutes", got)
	}

	if got := EngineKept(filepath.Join(root, "none")); got != "no engine is installed, so the Capcom service will not start" {
		t.Errorf("EngineKept with none: %q", got)
	}
}
