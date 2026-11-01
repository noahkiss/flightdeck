// Package instructions renders a user's own instruction files into each harness's native file:
// imports flattened, a digest header on top, a dry run with a diff before any write.
// Spec: docs/specs/instructions-mcp-flight-rules.md 5 and 6.
package instructions

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Marker says what the expansion did with one file it visited. The digest covers it, so a
// missing file that appears changes the digest.
type Marker string

const (
	Read      Marker = "read"
	Missing   Marker = "missing"
	Cycle     Marker = "cycle"
	TooDeep   Marker = "depth"
	Duplicate Marker = "duplicate"
	TooLarge  Marker = "too-large"
)

// MaxFileBytes is the size above which a file is skipped whole, as Claude Code does (spec 5.8).
const MaxFileBytes = 4 << 20

// importHops is the deepest import, counted in hops from the root source (depth 0), per
// harness. Every harness without imports uses the Claude Code rule, so one edit here follows a
// change in Claude Code's docs (spec 5.4, D1).
var importHops = map[Harness]int{Claude: 4, Codex: 4, Opencode: 4, Pi: 4}

// Visit is one file the expansion reached, in the order it reached them.
type Visit struct {
	Path   string // real path when it resolves, else the cleaned absolute path
	Marker Marker
	Depth  int
	From   string // the importing file; empty for a root source
	data   []byte
}

// expander flattens one render's sources. Its seen set spans every source of the render, so a
// file reached twice, from one source or two, is included once (spec 5.6).
type expander struct {
	home     string
	maxDepth int
	maxBytes int64
	seen     map[string]bool
	visits   []Visit
}

func newExpander(home string, maxDepth int) *expander {
	return &expander{home: home, maxDepth: maxDepth, maxBytes: MaxFileBytes, seen: map[string]bool{}}
}

// expand returns the flattened text of one root source. With importsOnly it returns only the
// text of the root's own imports, each already holding its deeper imports (spec 5.9).
func (e *expander) expand(root string, importsOnly bool) string {
	text, chunks := e.file(root, 0, "", map[string]bool{})
	if importsOnly {
		return strings.Join(chunks, "")
	}
	return text
}

// file expands one file at a depth. It returns the expanded text and, separately, the text of
// each import made directly from this file.
func (e *expander) file(path string, depth int, from string, branch map[string]bool) (string, []string) {
	real := realPath(path)
	visit := Visit{Path: real, Depth: depth, From: from}
	data, marker := e.load(real, depth, branch)
	visit.Marker = marker
	if marker != Read {
		e.visits = append(e.visits, visit)
		return "", nil
	}
	visit.data = data
	e.visits = append(e.visits, visit)
	e.seen[real] = true
	branch[real] = true
	defer delete(branch, real)

	var out strings.Builder
	var chunks []string
	var fence fenceState
	dir := filepath.Dir(real)
	for _, line := range splitKeep(string(data)) {
		if fence.step(line) {
			out.WriteString(line)
			continue
		}
		target, ok := importPath(line)
		if !ok {
			out.WriteString(line)
			continue
		}
		sub, _ := e.file(e.resolve(target, dir), depth+1, real, branch)
		if sub != "" && !strings.HasSuffix(sub, "\n") {
			sub += "\n"
		}
		out.WriteString(sub)
		if sub != "" {
			chunks = append(chunks, sub)
		}
	}
	return out.String(), chunks
}

func (e *expander) load(real string, depth int, branch map[string]bool) ([]byte, Marker) {
	switch {
	case depth > e.maxDepth:
		return nil, TooDeep
	case branch[real]:
		return nil, Cycle
	case e.seen[real]:
		return nil, Duplicate
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return nil, Missing
	}
	if info.Size() > e.maxBytes {
		return nil, TooLarge
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return nil, Missing
	}
	if int64(len(data)) > e.maxBytes {
		return nil, TooLarge
	}
	return data, Read
}

// resolve turns an import path into an absolute one: ~/ is the home directory, an absolute
// path stays, a relative one resolves against the importing file's directory (spec 5.3).
func (e *expander) resolve(p, dir string) string {
	return resolvePath(p, dir, e.home)
}

func resolvePath(p, dir, home string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// realPath resolves symbolic links, so a cycle or a diamond through a link is still seen. A
// path that does not resolve keeps its cleaned absolute form.
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	} else if !errors.Is(err, fs.ErrNotExist) {
		return p
	}
	return filepath.Clean(p)
}

// importPath reports whether a line is an import, and its path. An import is a line whose only
// content, trimmed, is @ and one path. A backslash before a space keeps the space; any other
// unescaped space, a quoted path or a backtick (an inline code span) makes the line text.
func importPath(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if len(s) < 2 || s[0] != '@' {
		return "", false
	}
	s = s[1:]
	if s[0] == '"' || s[0] == '\'' {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == ' ':
			b.WriteByte(' ')
			i++
		case c == ' ' || c == '\t' || c == '`':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// fenceState tracks fenced code blocks (backtick and tilde fences). Lines inside a fence, and
// the fence lines themselves, are never imports (spec 5.2).
type fenceState struct {
	char byte
	n    int
}

// step reports whether the line belongs to a fenced block.
func (f *fenceState) step(line string) bool {
	s := strings.TrimLeft(strings.TrimRight(line, "\r\n"), " \t")
	if f.n == 0 {
		if c, n := fenceRun(s); n >= 3 && !(c == '`' && strings.ContainsRune(s[n:], '`')) {
			f.char, f.n = c, n
			return true
		}
		return false
	}
	if c, n := fenceRun(s); c == f.char && n >= f.n && strings.TrimSpace(s[n:]) == "" {
		f.n = 0
	}
	return true
}

func fenceRun(s string) (byte, int) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	return s[0], n
}

// splitKeep splits text into lines, each keeping its line ending.
func splitKeep(s string) []string {
	var out []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}
