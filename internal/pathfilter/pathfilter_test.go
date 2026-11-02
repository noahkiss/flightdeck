package pathfilter

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestGlobs(t *testing.T) {
	for _, c := range []struct {
		glob string
		yes  []string
		no   []string
	}{
		{"**", []string{"a", "a/b/c.go", ".github/workflows/ci.yml"}, nil},
		{"*", []string{"Makefile", "go.mod"}, []string{"cmd/gff/main.go"}},
		{"**/*.md", []string{"README.md", "docs/specs/a.md"}, []string{"docs/a.mdx", "a.md/b"}},
		{"docs/**", []string{"docs/a.md", "docs/examples/hab.toml"}, []string{"docs", "xdocs/a"}},
		{"docs/**/*.md", []string{"docs/a.md", "docs/specs/a.md"}, []string{"docs/a.toml"}},
		{"*.go", []string{"main.go"}, []string{"cmd/main.go"}},
		{"LICENSE", []string{"LICENSE"}, []string{"LICENSE.txt", "a/LICENSE"}},
		{"*.jsx?", []string{"a.js", "a.jsx"}, []string{"a.jsxx", "a.j"}},
		{"ab+c", []string{"abc", "abbbc"}, []string{"ac"}},
		{"v[12].txt", []string{"v1.txt", "v2.txt"}, []string{"v3.txt"}},
		{`a\*b`, []string{"a*b"}, []string{"axb"}},
		{"a.b", []string{"a.b"}, []string{"axb"}},
		{"a/**/b", []string{"a/b", "a/x/b", "a/x/y/b"}, []string{"a/xb", "b", "a/b/c"}},
		{"docs/é.txt", []string{"docs/é.txt"}, []string{"docs/e.txt"}},
		{"*é*", []string{"café.md"}, []string{"cafe.md"}},
	} {
		f, err := Compile([]string{c.glob})
		if err != nil {
			t.Fatalf("%q: %v", c.glob, err)
		}
		for _, p := range c.yes {
			if !f.Match(p) {
				t.Errorf("%q should match %q", c.glob, p)
			}
		}
		for _, p := range c.no {
			if f.Match(p) {
				t.Errorf("%q should not match %q", c.glob, p)
			}
		}
	}
}

func TestLastMatchWins(t *testing.T) {
	f, err := Compile([]string{"**", "!docs/**", "docs/examples/**", "!docs/examples/*.md"})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"cmd/gff/main.go":           true,
		"docs/specs/ci-release.md":  false,
		"docs/examples/hab.toml":    true,
		"docs/examples/README.md":   false,
		"docs/examples/x/README.md": true,
	} {
		if got := f.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	none, _ := Compile([]string{"!**"})
	if none.Match("a") {
		t.Error("a path only a negation matches must not trigger")
	}
	if _, err := Compile([]string{"!"}); err == nil {
		t.Error("an empty pattern must be refused")
	}
}

// TestCIPaths reads ci.yml's own push filter, as release.yml does, and checks the files the
// spec names (ci-release.md 2, 3.1).
func TestCIPaths(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^    paths: \[(.*)\]$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("no one-line paths list under push in ci.yml")
	}
	var pats []string
	for _, p := range strings.Split(string(m[1]), ",") {
		pats = append(pats, strings.Trim(strings.TrimSpace(p), `'"`))
	}
	f, err := Compile(pats)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"cmd/gff-hab/main.go":           true,
		"Makefile":                      true,
		".github/workflows/release.yml": true,
		"docs/examples/hab.toml":        true,
		"swift/HabAPI/Package.swift":    true,
		"README.md":                     false,
		"internal/holder/README.md":     false,
		"docs/specs/ci-release.md":      false,
		"apple/App/iOS/Info.plist":      false,
		"design/icons/app.svg":          false,
		"LICENSE":                       false,
		// Generated notices: make check-notices compares them (ci-release.md 7.8).
		"NOTICE":                               true,
		"third_party/notices.toml":             true,
		"apple/App/Acknowledgements.txt":       true,
		"apple/FlightdeckKit/Package.resolved": true,
	} {
		if got := f.Match(p); got != want {
			t.Errorf("ci.yml filter on %q = %v, want %v", p, got, want)
		}
	}
}
