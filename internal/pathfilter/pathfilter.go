// Package pathfilter matches file paths against a GitHub Actions `paths` filter: the same globs,
// in order, with `!` negating and the last matching pattern winning. release.yml uses it to ask
// whether a commit range changed any file that ci.yml tests (docs/specs/ci-release.md 7.4). A git
// pathspec cannot answer that: it cannot take docs/examples/** back after excluding docs/**.
package pathfilter

import (
	"fmt"
	"regexp"
	"strings"
)

// Filter is a compiled `paths` list.
type Filter struct {
	rules []rule
}

type rule struct {
	neg bool
	re  *regexp.Regexp
}

// Compile compiles patterns in order. An empty pattern is an error.
func Compile(patterns []string) (*Filter, error) {
	f := &Filter{}
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		glob := strings.TrimPrefix(p, "!")
		if glob == "" {
			return nil, fmt.Errorf("empty pattern %q", p)
		}
		re, err := regexp.Compile(globRegexp(glob))
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		f.rules = append(f.rules, rule{neg: neg, re: re})
	}
	return f, nil
}

// Match reports whether path triggers the filter: the last pattern that matches it is not
// negated. A path no pattern matches does not trigger it.
func (f *Filter) Match(path string) bool {
	hit := false
	for _, r := range f.rules {
		if r.re.MatchString(path) {
			hit = !r.neg
		}
	}
	return hit
}

// globRegexp turns one glob into an anchored regular expression, following GitHub's filter
// pattern rules: `*` is any run of characters but `/`; `**` is any run of characters, and `**/`
// also matches no folder at all; `?` and `+` make the character before them optional or
// repeated; `[...]` is a character class; `\` escapes the next character.
func globRegexp(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			if i+2 < len(glob) && glob[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?' || c == '+':
			b.WriteByte(c)
		case c == '[':
			if j := strings.IndexByte(glob[i+1:], ']'); j >= 0 {
				b.WriteString(glob[i : i+j+2])
				i += j + 1
			} else {
				b.WriteString(`\[`)
			}
		case c == '\\' && i+1 < len(glob):
			i++
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}
