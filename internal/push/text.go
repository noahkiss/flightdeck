package push

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ellipsis marks a cut (app.md 16.1).
const ellipsis = "…"

// Fit fits s to at most words words and chars characters (app.md 16.1, 16.2). It collapses
// white space, cuts on a word boundary and marks the cut with "…", which counts as a character
// but not as a word. A first word longer than chars is the only thing cut inside a word. A
// budget of 0 has no limit.
func Fit(s string, words, chars int) string {
	ws := strings.Fields(s)
	if fits(ws, words, chars) {
		return strings.Join(ws, " ")
	}
	var b strings.Builder
	for i, w := range ws {
		n := utf8.RuneCountInString(b.String())
		add := utf8.RuneCountInString(w)
		if i > 0 {
			add++
		}
		if (words > 0 && i >= words) || (chars > 0 && n+add+1 > chars) {
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 && len(ws) > 0 {
		r := []rune(ws[0])
		if chars > 1 && len(r) > chars-1 {
			r = r[:chars-1]
		}
		b.WriteString(string(r))
	}
	out := strings.TrimRight(b.String(), " ,;:·-")
	return out + ellipsis
}

func fits(ws []string, words, chars int) bool {
	if words > 0 && countWords(ws) > words {
		return false
	}
	return chars <= 0 || utf8.RuneCountInString(strings.Join(ws, " ")) <= chars
}

// countWords counts the words, leaving out separators such as "·".
func countWords(ws []string) int {
	n := 0
	for _, w := range ws {
		if w != "·" && w != "-" {
			n++
		}
	}
	return n
}

// Words counts the words in s as Fit does.
func Words(s string) int { return countWords(strings.Fields(s)) }

// Fits reports whether s needs no cut for the budget.
func Fits(s string, words, chars int) bool { return fits(strings.Fields(s), words, chars) }

var (
	leadingCd = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|\S+)\s*(&&|;)\s*`)
	envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// maxQuoted is the longest quoted string kept whole in a shortened command.
const maxQuoted = 24

// ShortCommand shortens a shell command for a banner (app.md 16.1). It keeps the program, its
// subcommand and the flags with their values, and drops, in order: leading changes of
// directory, environment assignments, the directory part of paths inside cwd, then the middle
// of long quoted strings. It collapses white space.
func ShortCommand(cmd, cwd string) string {
	for {
		loc := leadingCd.FindStringIndex(cmd)
		if loc == nil {
			break
		}
		cmd = cmd[loc[1]:]
	}
	toks := splitShell(cmd)
	for len(toks) > 1 && envAssign.MatchString(toks[0]) {
		toks = toks[1:]
	}
	cwd = strings.TrimRight(cwd, "/")
	for i, t := range toks {
		if cwd != "" {
			t = strings.ReplaceAll(t, cwd+"/", "")
		}
		if len(t) >= 2 && (t[0] == '"' || t[0] == '\'') && t[len(t)-1] == t[0] {
			if r := []rune(t); len(r) > maxQuoted {
				t = string(r[:12]) + ellipsis + string(r[len(r)-7:])
			}
		}
		toks[i] = t
	}
	return strings.Join(toks, " ")
}

// splitShell splits on white space outside quotes; quotes stay in the tokens.
func splitShell(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

var subcommand = regexp.MustCompile(`^[a-z][a-z0-9:-]*$`)

// verbObject builds a permission title's object from a shortened command: the program's base
// name and its subcommand, when the second word is one ("wrangler deploy").
func verbObject(short string) string {
	toks := splitShell(short)
	if len(toks) == 0 {
		return ""
	}
	obj := path.Base(toks[0])
	if len(toks) > 1 && subcommand.MatchString(toks[1]) {
		obj += " " + toks[1]
	}
	return obj
}
