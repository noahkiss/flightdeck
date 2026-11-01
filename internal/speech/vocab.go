package speech

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The session vocabulary fix-up (voice.md, Recommendation 2). The engine writes identifiers as
// words: "fetch user" for fetchUser, "link test dot go" for link_test.go. The fix-up turns a run
// of words back into a term the hab knows when the words, joined and lowercased, spell the term
// with its separators and case taken out. It is deterministic: no scores, no model.

const (
	minKey    = 5    // a term's key must be at least this long, so short words stay as spoken
	maxSpan   = 6    // the most words one term may cover
	maxTerms  = 4000 // terms kept from one vocabulary
	maxPaths  = 20000
	maxTermsB = 256 // longest term, in bytes
)

// connectors are words a speaker says for a separator inside a term.
var connectors = map[string]bool{"dot": true, "underscore": true, "dash": true, "hyphen": true, "slash": true}

// Terms turns a folder's file paths (slash-separated, relative) into vocabulary terms: each
// path element and each file name without its extension. Terms are unique and in first-seen
// order; at most maxTerms.
func Terms(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t == "" || len(t) > maxTermsB || seen[t] || len(out) >= maxTerms {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for i, p := range paths {
		if i >= maxPaths || len(out) >= maxTerms {
			break
		}
		for _, el := range strings.Split(p, "/") {
			add(el)
		}
		base := path.Base(p)
		if ext := path.Ext(base); ext != "" && ext != base {
			add(strings.TrimSuffix(base, ext))
		}
	}
	return out
}

// entry is one usable term.
type entry struct {
	form      string // the term as written
	ambiguous bool   // two terms share the key: leave the words as spoken
}

// key returns a term's key: its letters and digits, lowercased. ok is false for a term too short
// to match safely.
func key(t string) (string, bool) {
	var b strings.Builder
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	k := b.String()
	return k, utf8.RuneCountInString(k) >= minKey
}

// marked reports whether a term looks like code on its own: it holds a separator, or a capital
// letter after its first. Only such a term replaces a single spoken word; a plain word such as
// "Server" never recases the word "server".
func marked(t string) bool {
	for i, r := range t {
		if strings.ContainsRune("._-/", r) || (i > 0 && unicode.IsUpper(r)) {
			return true
		}
	}
	return false
}

// Fixup applies terms to text and reports whether it changed anything. It scans the words left
// to right and tries the longest run first at each word.
func Fixup(text string, terms []string) (string, bool) {
	if len(terms) == 0 || text == "" {
		return text, false
	}
	index := map[string]*entry{}
	for _, t := range terms {
		k, ok := key(t)
		if !ok {
			continue
		}
		if e, ok := index[k]; ok {
			if e.form != t {
				e.ambiguous = true
			}
			continue
		}
		index[k] = &entry{form: t}
	}
	words := strings.Fields(text)
	var out []string
	changed := false
	for i := 0; i < len(words); {
		n, form := match(words[i:], index)
		if n == 0 {
			out = append(out, words[i])
			i++
			continue
		}
		lead, _, _ := split(words[i])
		_, _, trail := split(words[i+n-1])
		repl := lead + form + trail
		if repl != strings.Join(words[i:i+n], " ") {
			changed = true
		}
		out = append(out, repl)
		i += n
	}
	if !changed {
		return text, false
	}
	return strings.Join(out, " "), true
}

// match returns how many of words, from the first, a term covers, and the term.
func match(words []string, index map[string]*entry) (int, string) {
	for n := min(maxSpan, len(words)); n >= 1; n-- {
		var b strings.Builder
		ok := true
		for j, w := range words[:n] {
			lead, core, trail := split(w)
			if core == "" || (j > 0 && lead != "") || (j < n-1 && trail != "") {
				ok = false // punctuation between the words: they are not one term
				break
			}
			lc := strings.ToLower(core)
			if n > 1 && j > 0 && j < n-1 && connectors[lc] {
				continue
			}
			b.WriteString(lc)
		}
		if !ok {
			continue
		}
		k, _ := key(b.String())
		e := index[k]
		if e == nil || e.ambiguous {
			continue
		}
		if n == 1 {
			_, core, _ := split(words[0])
			if !marked(e.form) || core == e.form {
				continue
			}
		}
		return n, e.form
	}
	return 0, ""
}

// split cuts a word into its leading punctuation, its core and its trailing punctuation. Marks
// inside the core, such as the dot of link_test.go, stay in the core.
func split(w string) (lead, core, trail string) {
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	start := strings.IndexFunc(w, isWord)
	if start < 0 {
		return w, "", ""
	}
	end := strings.LastIndexFunc(w, isWord)
	_, size := utf8.DecodeRuneInString(w[end:])
	return w[:start], w[start : end+size], w[end+size:]
}
