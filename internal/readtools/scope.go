// Package readtools serves the read tools of the agent kit (docs/specs/capcom.md 4, 5): the
// past and current sessions found in each harness's native transcripts, transcript search and
// read, notices and files, on this hab and on every peer over the link this hab dialed. Each
// hab checks its own read scope before it reads. Nothing here writes.
package readtools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// DefaultDeny is the deny list a hab has when its config file sets no [read] deny (capcom.md
// 5.1, 5.5): SSH and cloud credential folders, .env files and private key files. The .env
// templates projects commit (.env.example, .env.sample, .env.template) stay readable. A word
// with no slash is matched against every part of a path; one with a slash against consecutive
// parts; one that starts with / or ~/ against the path from its start. A word that starts with
// ! exempts a part it matches from the slash-free words: .env.example is readable, while
// .ssh/.env.example is still refused by .ssh.
var DefaultDeny = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gcloud", ".oci",
	".env", ".env.*", "!.env.example", "!.env.sample", "!.env.template",
	".netrc", ".git-credentials", ".pgpass", ".credentials.json",
	"*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
}

// Scope is one hab's read scope: the roots reads may happen under, and the patterns refused
// even there. Deny wins.
type Scope struct {
	Host  string   // this hab's host id, named in a denial
	Home  string   // the user's home folder, for patterns under ~/
	Allow []string // absolute, clean roots
	Deny  []string // patterns, as DefaultDeny describes
	// Broken refuses every read: the config file did not parse and no good [read] section was
	// read before, so the user's own deny entries are unknown.
	Broken bool

	resolved bool // Allow already holds each root as written and resolved
}

// Resolved returns the scope with each allowed root also in its resolved form, so a check
// resolves no root again. Build it once per read that checks many paths.
func (s Scope) Resolved() Scope {
	if s.resolved {
		return s
	}
	out := s
	out.Allow = nil
	seen := map[string]bool{}
	for _, root := range s.Allow {
		for _, r := range []string{root, resolve(root)} {
			if !seen[r] {
				seen[r] = true
				out.Allow = append(out.Allow, r)
			}
		}
	}
	out.resolved = true
	return out
}

// Check refuses a path the scope does not allow: one outside every allowed root, or one that
// matches a deny pattern, before or after its links are resolved. It returns the clean,
// resolved path to read. A denial names the host and the list that refused it, never the
// pattern (capcom.md 5.2).
func (s Scope) Check(path string) (string, error) {
	p, err := s.abs(path)
	if err != nil {
		return "", err
	}
	if s.Broken {
		return "", s.refuse()
	}
	if s.denied(p) {
		return "", s.refuse()
	}
	real := resolve(p)
	if !s.allowed(real) {
		return "", s.refuse()
	}
	if s.denied(real) {
		return "", s.refuse()
	}
	return real, nil
}

// Denied reports whether a path matches the deny list, as given or with its links resolved.
func (s Scope) Denied(path string) bool {
	p, err := s.abs(path)
	if err != nil || s.Broken {
		return true
	}
	return s.denied(p) || s.denied(resolve(p))
}

// refuse is the one denial for every reason: a path outside every root, a deny match, a
// session folder denied, or a config that does not parse. One text keeps a denial from telling
// whether a path lies under an allowed root.
func (s Scope) refuse() error {
	return control.Errorf(api.ErrorKindDenied, "%s: the read scope refused this read; the user can see its rules in the app", s.Host)
}

// abs makes a path absolute and clean; it takes ~/ for the home folder.
func (s Scope) abs(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if s.Home == "" {
			return "", control.Errorf(api.ErrorKindInvalid, "%s: no home folder for %q", s.Host, path)
		}
		path = filepath.Join(s.Home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		return "", control.Errorf(api.ErrorKindInvalid, "path %q is not absolute", path)
	}
	return filepath.Clean(path), nil
}

// resolve resolves every link in an absolute, clean path. A part that does not exist is kept
// as written after the resolved part that does.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, rest := filepath.Dir(p), filepath.Base(p)
	for dir != p {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		p = dir
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = filepath.Dir(dir)
	}
	return filepath.Join(p, rest)
}

func (s Scope) allowed(real string) bool {
	for _, root := range s.Allow {
		if within(real, root) {
			return true
		}
		if !s.resolved && within(real, resolve(root)) {
			return true
		}
	}
	return false
}

// within reports whether p is root or under it.
func within(p, root string) bool {
	if root == "" {
		return false
	}
	if p == root || root == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// fold maps each rune to the smallest member of its simple case fold orbit, so every spelling a
// case-insensitive file system treats as one name folds to one string: .ENV, .Env and .ſsh
// (U+017F, long s) fold like .env and .ssh. Lower-casing alone misses runes such as U+017F.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

// denied matches the deny list in any case: macOS and Windows file systems ignore case, so
// .ENV opens .env. On Linux the guard also refuses a .ENV that is its own file.
func (s Scope) denied(p string) bool {
	p = fold(p)
	parts := strings.Split(strings.TrimPrefix(p, string(filepath.Separator)), string(filepath.Separator))
	var exempt []string
	for _, pat := range s.Deny {
		if w, ok := strings.CutPrefix(pat, "!"); ok && w != "" && !strings.Contains(w, "/") {
			exempt = append(exempt, fold(w))
		}
	}
	for _, pat := range s.Deny {
		if pat == "" || strings.HasPrefix(pat, "!") {
			continue
		}
		if pat == "~" || strings.HasPrefix(pat, "~/") || filepath.IsAbs(pat) {
			full, err := s.abs(pat)
			if err != nil {
				continue
			}
			if anchored(p, fold(full)) {
				return true
			}
			continue
		}
		words := strings.Split(strings.Trim(fold(pat), "/"), "/")
		if len(words) == 1 {
			if single(parts, words[0], exempt) {
				return true
			}
			continue
		}
		if sequence(parts, words) {
			return true
		}
	}
	return false
}

// single reports whether a slash-free word matches a part of a path that no exemption matches.
func single(parts []string, w string, exempt []string) bool {
	for _, part := range parts {
		if m, _ := filepath.Match(w, part); !m {
			continue
		}
		if !slices.ContainsFunc(exempt, func(e string) bool { m, _ := filepath.Match(e, part); return m }) {
			return true
		}
	}
	return false
}

// anchored matches an absolute pattern against p and each of p's ancestors.
func anchored(p, pat string) bool {
	for q := p; ; q = filepath.Dir(q) {
		if ok, _ := filepath.Match(pat, q); ok {
			return true
		}
		if q == filepath.Dir(q) {
			return false
		}
	}
}

// sequence reports whether pat's parts match consecutive parts of a path.
func sequence(parts, pat []string) bool {
	for i := 0; i+len(pat) <= len(parts); i++ {
		ok := true
		for j, w := range pat {
			if m, _ := filepath.Match(w, parts[i+j]); !m {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ScopeConfig is the [read] section of the hab's config file: roots added to the default roots,
// and a deny list that replaces DefaultDeny when the section sets one.
type ScopeConfig struct {
	Allow []string
	Deny  []string
	// DenySet: the section sets deny, so Deny replaces DefaultDeny; deny = [] turns it off.
	DenySet bool
}

// LoadScopeConfig reads the [read] section of the hab's config file. A missing file or section
// is empty. Entries that cannot be used are left out and reported; a file that does not parse
// is an error.
func LoadScopeConfig(path, home string) (ScopeConfig, []string, error) {
	var c ScopeConfig
	if path == "" {
		return c, nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, err
	}
	var fc struct {
		Read struct {
			Allow []string `toml:"allow"`
			Deny  []string `toml:"deny"`
		} `toml:"read"`
	}
	md, err := toml.Decode(string(b), &fc)
	if err != nil {
		return c, nil, fmt.Errorf("%s: %w", path, err)
	}
	c.DenySet = md.IsDefined("read", "deny")
	var problems []string
	for _, r := range fc.Read.Allow {
		r = strings.TrimSpace(r)
		if r == "~" || strings.HasPrefix(r, "~/") {
			if home == "" {
				problems = append(problems, fmt.Sprintf("%s: read: allow %q: no home folder", path, r))
				continue
			}
			r = filepath.Join(home, strings.TrimPrefix(r, "~"))
		}
		if !filepath.IsAbs(r) {
			problems = append(problems, fmt.Sprintf("%s: read: allow %q is not an absolute path", path, r))
			continue
		}
		c.Allow = append(c.Allow, filepath.Clean(r))
	}
	for _, d := range fc.Read.Deny {
		if d = strings.TrimSpace(d); d != "" {
			c.Deny = append(c.Deny, d)
		}
	}
	return c, problems, nil
}

// ScopeSource builds the current scope: the defaults, the hab's own roots and the [read]
// section of its config file, read again when the file changes.
type ScopeSource struct {
	Host   string
	Home   string
	Config string               // the hab's config file; "" for none
	Roots  func() []string      // the default roots: project folders, transcript folders, Capcom's folder
	Logf   func(string, ...any) // problems with the [read] section

	mu     sync.Mutex
	key    string // size and modification time of the config file last read
	last   ScopeConfig
	good   bool // last came from a file that parsed
	broken bool // the file does not parse and no good section was read before
}

// Scope returns the scope now.
func (s *ScopeSource) Scope() Scope {
	c, broken := s.config()
	sc := Scope{Host: s.Host, Home: s.Home, Broken: broken}
	if s.Roots != nil {
		sc.Allow = append(sc.Allow, s.Roots()...)
	}
	sc.Allow = append(sc.Allow, c.Allow...)
	if c.DenySet {
		sc.Deny = append(sc.Deny, c.Deny...)
	} else {
		sc.Deny = append(sc.Deny, DefaultDeny...)
	}
	return sc.Resolved()
}

func (s *ScopeSource) config() (ScopeConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Config == "" {
		return s.last, false
	}
	key := "none"
	if fi, err := os.Stat(s.Config); err == nil {
		key = fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
	}
	if key == s.key {
		return s.last, s.broken
	}
	s.key = key
	c, problems, err := LoadScopeConfig(s.Config, s.Home)
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	for _, p := range problems {
		logf("%s", p)
	}
	if err != nil {
		if s.good {
			logf("read scope: %v; keeping the last good [read] section", err)
		} else {
			logf("read scope: %v; every read is refused until it parses", err)
			s.broken = true
		}
		return s.last, s.broken
	}
	s.last, s.good, s.broken = c, true, false
	return c, false
}
