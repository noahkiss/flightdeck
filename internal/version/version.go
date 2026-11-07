// Package version answers the version request that gff, gff-hab and gff-hab-holder share.
//
// Each command holds its own `var version = "dev"`, which a release build sets at link time with
// -ldflags "-X main.version=<version>" (docs/specs/ci-release.md 7.3). The version comes from the
// release tag without its leading v: 1.2.3, 1.2.3-rc.1 or 1.2.3-dry.1.
package version

import (
	"fmt"
	"io"
	"strings"
)

// Compare orders two release versions: x.y.z, or x.y.z-<word>.<n> for a pre-release (rc, dry),
// with an optional leading v. It returns -1, 0 or 1 as a is older than, the same as or newer
// than b. A pre-release is older than its x.y.z; two pre-releases of one x.y.z compare by word,
// then by n. ok is false when either is not a release version, such as dev: such a pair has no
// order.
func Compare(a, b string) (c int, ok bool) {
	ra, oka := parseRelease(a)
	rb, okb := parseRelease(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range 3 {
		if ra.num[i] != rb.num[i] {
			return sign(ra.num[i] - rb.num[i]), true
		}
	}
	switch {
	case ra.pre == "" && rb.pre == "":
		return 0, true
	case ra.pre == "":
		return 1, true
	case rb.pre == "":
		return -1, true
	case ra.pre != rb.pre:
		return strings.Compare(ra.pre, rb.pre), true
	}
	return sign(ra.preN - rb.preN), true
}

// IsRelease reports whether s is a release version Compare can order.
func IsRelease(s string) bool {
	_, ok := parseRelease(s)
	return ok
}

type release struct {
	num  [3]int64
	pre  string // the pre-release word; empty for a final release
	preN int64
}

// parseRelease reads x.y.z or x.y.z-<word>.<n>, with an optional leading v: decimal numbers
// with no sign and no leading zero, a word of lowercase letters.
func parseRelease(s string) (release, bool) {
	var r release
	s = strings.TrimPrefix(s, "v")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return r, false
	}
	for i, p := range parts {
		n, ok := decimal(p)
		if !ok {
			return r, false
		}
		r.num[i] = n
	}
	if !hasPre {
		return r, true
	}
	word, n, found := strings.Cut(pre, ".")
	if !found || word == "" || strings.Trim(word, "abcdefghijklmnopqrstuvwxyz") != "" {
		return r, false
	}
	pn, ok := decimal(n)
	if !ok {
		return r, false
	}
	r.pre, r.preN = word, pn
	return r, true
}

// decimal reads a non-negative decimal number of at most 18 digits with no leading zero.
func decimal(s string) (int64, bool) {
	if s == "" || len(s) > 18 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

func sign(n int64) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Requested reports whether a command's arguments (os.Args[1:]) ask for its version:
// `version`, `-version` or `--version` as the first argument.
func Requested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "-version", "--version":
		return true
	}
	return false
}

// Print writes "<name> <version>" and a newline to w.
func Print(w io.Writer, name, version string) {
	fmt.Fprintf(w, "%s %s\n", name, version)
}
