package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/service"
)

// putEngine writes an engine folder at dir: the engine and, with npm, its npm manifest.
func putEngine(t *testing.T, dir string, npm bool) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, service.EngineFile)
	os.WriteFile(f, []byte("// engine\n"), 0o644)
	if npm {
		os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)
		os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o644)
	}
	return f
}

// TestEngineLookup (capcom-service.md 3.1): with no $GFF_HAB_CAPCOM_ENGINE, gff-hab finds the
// engine beside itself in each shipped layout, and an installed gff-hab, which runs from a copy
// in the state directory, finds the engine install put there.
func TestEngineLookup(t *testing.T) {
	t.Setenv("GFF_HAB_CAPCOM_ENGINE", "")
	cases := []struct {
		name   string
		exe    string // relative to the test folder
		engine string // the engine folder, relative to the test folder; "" for none beside
	}{
		{"linux archive", "gff_1.0.0_linux_amd64/gff-hab", "gff_1.0.0_linux_amd64/capcom"},
		{"homebrew linux", "prefix/bin/gff-hab", "prefix/libexec/capcom"},
		{"darwin archive and homebrew macos", "libexec/GFF Hab.app/Contents/MacOS/gff-hab", "libexec/capcom"},
		{"checkout", "repo/gff-hab", "repo/capcom-engine/dist"},
		{"installed linux", "state/bin/gff-hab", ""},
		{"installed macos", "state/builds/0123456789abcdef/GFF Hab.app/Contents/MacOS/gff-hab", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			exe := filepath.Join(root, tc.exe)
			state := filepath.Join(root, "state")
			if got := engineFile(exe, state); got != "" {
				t.Fatalf("found %s with no engine anywhere", got)
			}
			want := ""
			if tc.engine != "" {
				want = putEngine(t, filepath.Join(root, tc.engine), false)
				if filepath.Base(tc.engine) == "capcom" {
					// A release's capcom/ runs once npm ci put its SDKs there.
					if got := engineFile(exe, state); got != "" {
						t.Fatalf("found %s: a capcom/ with no node_modules", got)
					}
					os.MkdirAll(filepath.Join(root, tc.engine, "node_modules"), 0o755)
				}
			} else {
				// What gff-hab install leaves: an engine directory and the current link.
				putEngine(t, filepath.Join(service.EnginesDir(state), "0123456789abcdef"), true)
				os.Symlink("0123456789abcdef", service.EngineCurrent(state))
				want = service.InstalledEngine(state)
				if want == "" {
					t.Fatal("InstalledEngine found nothing")
				}
			}
			if got := engineFile(exe, state); got != want {
				t.Errorf("engineFile = %q, want %q", got, want)
			}
			// $GFF_HAB_CAPCOM_ENGINE wins.
			t.Setenv("GFF_HAB_CAPCOM_ENGINE", "/opt/engine.mjs")
			if got := engineFile(exe, state); got != "/opt/engine.mjs" {
				t.Errorf("with $GFF_HAB_CAPCOM_ENGINE: %q", got)
			}
		})
	}

	// An engine beside gff-hab that can run wins over the installed one: a hab run from a
	// release or a checkout runs the engine that came with it. One with no SDKs does not.
	root := t.TempDir()
	state := filepath.Join(root, "state")
	putEngine(t, filepath.Join(service.EnginesDir(state), "0123456789abcdef"), true)
	os.Symlink("0123456789abcdef", service.EngineCurrent(state))
	beside := putEngine(t, filepath.Join(root, "rel", "capcom"), true)
	if got := engineFile(filepath.Join(root, "rel", "gff-hab"), state); got != service.InstalledEngine(state) {
		t.Errorf("engineFile = %q, want the installed one over a capcom/ with no SDKs", got)
	}
	os.MkdirAll(filepath.Join(root, "rel", "capcom", "node_modules"), 0o755)
	if got := engineFile(filepath.Join(root, "rel", "gff-hab"), state); got != beside {
		t.Errorf("engineFile = %q, want the one beside gff-hab, %q", got, beside)
	}
}

// TestEngineVersionMismatch (capcom-service.md 3.1): a release gff-hab warns about an engine of
// another version; a dev gff-hab runs any engine quietly.
func TestEngineVersionMismatch(t *testing.T) {
	for _, tc := range []struct {
		hab, engine string
		warn        bool
	}{
		{"0.4.0", "0.4.0", false},
		{"0.4.0", "0.3.0", true},
		{"0.4.0", "dev", true},
		{"0.4.0", "", true},
		{"dev", "0.3.0", false},
		{"", "0.3.0", false},
	} {
		msg := engineVersionMismatch(tc.hab, tc.engine)
		if (msg != "") != tc.warn {
			t.Errorf("hab %q engine %q: %q", tc.hab, tc.engine, msg)
		}
		if tc.warn && !strings.Contains(msg, "run gff-hab install") {
			t.Errorf("hab %q engine %q: no fix in %q", tc.hab, tc.engine, msg)
		}
	}
}

// TestEngineSource: install copies the first engine folder beside gff-hab that holds the npm
// manifest too; a checkout's dist/, which holds the engine alone, is not one.
func TestEngineSource(t *testing.T) {
	t.Setenv("GFF_HAB_CAPCOM_ENGINE", "")
	root := t.TempDir()
	exe := filepath.Join(root, "GFF Hab.app", "Contents", "MacOS", "gff-hab")
	if got := engineSource(exe); got != "" {
		t.Fatalf("engineSource = %q with none", got)
	}
	putEngine(t, filepath.Join(root, "GFF Hab.app", "Contents", "MacOS", "capcom-engine", "dist"), false)
	if got := engineSource(exe); got != "" {
		t.Errorf("engineSource = %q: a folder with no npm manifest", got)
	}
	putEngine(t, filepath.Join(root, "capcom"), true)
	if got, want := engineSource(exe), filepath.Join(root, "capcom"); got != want {
		t.Errorf("engineSource = %q, want %q", got, want)
	}
	other := filepath.Join(root, "other")
	putEngine(t, other, true)
	t.Setenv("GFF_HAB_CAPCOM_ENGINE", filepath.Join(other, service.EngineFile))
	if got := engineSource(exe); got != other {
		t.Errorf("with $GFF_HAB_CAPCOM_ENGINE: %q, want %q", got, other)
	}
}
