package notices

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mitText = "MIT License\n\nCopyright (c) 2026 Example Authors\n\nPermission is hereby granted.\n"

func writeFile(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

const resolved = `{
  "pins" : [
    {
      "identity" : "pkg-b",
      "kind" : "remoteSourceControl",
      "location" : "https://example.com/pkg-b.git",
      "state" : { "revision" : "bbbb", "version" : "2.0.0" }
    },
    {
      "identity" : "pkg-a",
      "kind" : "remoteSourceControl",
      "location" : "https://example.com/pkg-a.git",
      "state" : { "revision" : "aaaa" }
    }
  ],
  "version" : 3
}
`

const manifest = `
[[component]]
name = "libfoo"
url = "https://example.com/libfoo"
license = "MIT"
files = ["third_party/licenses/libfoo/LICENSE"]
release = { in = "gff", version = "make:FOO_VERSION" }
app = { in = "Flightdeck", version = "mise:zig" }

[[component]]
name = "appfont"
url = "https://example.com/font"
license = "OFL-1.1"
files = ["third_party/licenses/font/OFL.txt"]
app = { in = "Flightdeck (font)", version = "3" }

[[swift]]
identity = "pkg-a"
license = "Apache-2.0"

[[swift]]
identity = "pkg-b"
license = "Apache-2.0"
`

// fixture builds a repository with every input Generate reads.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, ManifestPath, manifest)
	writeFile(t, root, "Makefile", "FOO_VERSION := 1.2.3\nOTHER := x\n")
	writeFile(t, root, "mise.toml", "[tools]\nzig = \"0.17.0\"\n")
	writeFile(t, root, "go.mod", "module example.com/m\n\ngo 1.27\n\ntoolchain go1.27.3\n")
	writeFile(t, root, PackageResolvedPath, resolved)
	writeFile(t, root, "third_party/licenses/libfoo/LICENSE", "\ufeff"+mitText)
	writeFile(t, root, "third_party/licenses/font/OFL.txt", "SIL Open Font License 1.1\r\n")
	writeFile(t, root, SwiftLicensesDir+"/pkg-a/LICENSE.txt", "Apache License\nVersion 2.0\n")
	writeFile(t, root, SwiftLicensesDir+"/pkg-a/NOTICE.txt", "pkg-a notice\n")
	writeFile(t, root, SwiftLicensesDir+"/pkg-b/LICENSE.txt", "Apache License\n  Version 2.0  \n\n")
	writeFile(t, root, SwiftLicensesDir+"/pkg-b/ThirdPartyLicenses/x-LICENSE.md", "x licence\n")
	return root
}

func goModules(t *testing.T, root string) func() ([]GoModule, error) {
	dir := filepath.Join(root, "modcache", "example.com", "dep@v1.0.0")
	writeFile(t, root, "modcache/example.com/dep@v1.0.0/LICENSE", "dep licence\n")
	writeFile(t, root, "modcache/example.com/dep@v1.0.0/PATENTS", "dep patents\n")
	writeFile(t, root, "modcache/example.com/dep@v1.0.0/LICENSE_test.go", "package dep\n")
	return func() ([]GoModule, error) {
		return []GoModule{{Path: "example.com/dep", Version: "v1.0.0", Dir: dir, Binaries: []string{"gff", "gff-hab"}}}, nil
	}
}

func TestAppSet(t *testing.T) {
	root := fixture(t)
	out, err := Generate(SetApp, Inputs{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"libfoo\nVersion: 0.17.0\nSource: https://example.com/libfoo\nLicence: MIT\nIn: Flightdeck\n",
		"appfont\nVersion: 3\n",
		"pkg-a\nVersion: revision aaaa\nSource: https://example.com/pkg-a\nLicence: Apache-2.0\nIn: Flightdeck (Swift package)\n",
		"pkg-b\nVersion: 2.0.0\nSource: https://example.com/pkg-b\n",
		"--- " + SwiftLicensesDir + "/pkg-a/NOTICE.txt\n\npkg-a notice\n",
		"--- " + SwiftLicensesDir + "/pkg-b/LICENSE.txt\n(The same text as " + SwiftLicensesDir + "/pkg-a/LICENSE.txt above.)\n",
		"--- " + SwiftLicensesDir + "/pkg-b/ThirdPartyLicenses/x-LICENSE.md\n\nx licence\n",
		"SIL Open Font License 1.1\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(s, "\ufeff") || strings.Contains(s, "\r") {
		t.Error("byte order mark or carriage return kept")
	}
	if strings.Contains(s, root) {
		t.Error("output names the absolute root")
	}
	if strings.Index(s, "pkg-a\nVersion") > strings.Index(s, "pkg-b\nVersion") {
		t.Error("Swift packages not sorted by identity")
	}
}

func TestReleaseSet(t *testing.T) {
	root := fixture(t)
	goroot := filepath.Join(root, "goroot")
	writeFile(t, root, "goroot/LICENSE", "Copyright 2009 The Go Authors.\n")
	out, err := Generate(SetRelease, Inputs{Root: root, GoModules: goModules(t, root), GoRoot: goroot,
		GoBinaries: []string{"gff", "gff-hab"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"Go standard library and runtime\nVersion: go1.27.3\n",
		"example.com/dep\nVersion: v1.0.0\nSource: https://pkg.go.dev/example.com/dep\nLicence: see the text below\nIn: gff, gff-hab\n",
		"--- example.com/dep/LICENSE\n\ndep licence\n",
		"--- example.com/dep/PATENTS\n",
		"libfoo\nVersion: 1.2.3\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, bad := range []string{"appfont", "pkg-a", "LICENSE_test.go", root} {
		if strings.Contains(s, bad) {
			t.Errorf("release output holds %q", bad)
		}
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(t *testing.T, root string)
		set   string
		mods  bool
		error string
	}{
		{"swift pin without entry", func(t *testing.T, root string) {
			writeFile(t, root, PackageResolvedPath, strings.Replace(resolved, `"pkg-b"`, `"pkg-c"`, 1))
		}, SetApp, false, "pkg-c is pinned"},
		{"stale swift entry checked for release too", func(t *testing.T, root string) {
			writeFile(t, root, ManifestPath, manifest+"\n[[swift]]\nidentity = \"gone\"\nlicense = \"MIT\"\n")
		}, SetRelease, false, `"gone" is not pinned`},
		{"swift entry without texts", func(t *testing.T, root string) {
			os.RemoveAll(filepath.Join(root, SwiftLicensesDir, "pkg-b"))
		}, SetApp, false, "pkg-b: no licence texts"},
		{"go module without licence", func(t *testing.T, root string) {
			os.Remove(filepath.Join(root, "modcache/example.com/dep@v1.0.0/LICENSE"))
			os.Remove(filepath.Join(root, "modcache/example.com/dep@v1.0.0/PATENTS"))
		}, SetRelease, true, "example.com/dep v1.0.0 has no licence file"},
		{"missing licence file", func(t *testing.T, root string) {
			os.Remove(filepath.Join(root, "third_party/licenses/libfoo/LICENSE"))
		}, SetApp, false, "libfoo"},
		{"unknown make variable", func(t *testing.T, root string) {
			writeFile(t, root, "Makefile", "OTHER := x\n")
		}, SetRelease, false, "no value for FOO_VERSION"},
		{"unknown key", func(t *testing.T, root string) {
			writeFile(t, root, ManifestPath, manifest+"\n[[component]]\nname = \"x\"\nurl = \"u\"\nlicense = \"l\"\nfile = [\"a\"]\napp = { in = \"a\", version = \"1\" }\n")
		}, SetApp, false, "unknown key"},
		{"component in no set", func(t *testing.T, root string) {
			writeFile(t, root, ManifestPath, manifest+"\n[[component]]\nname = \"x\"\nurl = \"u\"\nlicense = \"l\"\nfiles = [\"a\"]\n")
		}, SetApp, false, "in no set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := fixture(t)
			in := Inputs{Root: root}
			if c.mods {
				in.GoModules = goModules(t, root)
			}
			c.edit(t, root)
			_, err := Generate(c.set, in)
			if err == nil || !strings.Contains(err.Error(), c.error) {
				t.Fatalf("err = %v, want one containing %q", err, c.error)
			}
		})
	}
}

// The committed app file matches what the repository's inputs produce. (NOTICE needs the module
// cache and the go command; make check-gen covers it.)
func TestCommittedAppFile(t *testing.T) {
	root := filepath.Join("..", "..")
	want, err := Generate(SetApp, Inputs{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, AppOutput))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: run make notices", AppOutput)
	}
}
