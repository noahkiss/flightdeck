package mcpreg

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// tomlDoc is a TOML file cut into lines and table sections, for a format-preserving edit of
// the [mcp_servers.<name>] tables (spec 7.2, 8.4).
type tomlDoc struct {
	lines    []string // each with its newline, if it had one
	sections []tomlSection
	inside   []bool // per line: starts inside a value
}

type tomlSection struct {
	start, end int // line range [start, end): the header through the section's last content line
	path       []string
}

// parseTOMLDoc finds the table headers of a TOML text. A line that starts inside a multi-line
// string is never a header.
func parseTOMLDoc(text string) (*tomlDoc, error) {
	d := &tomlDoc{lines: splitLines(text)}
	inside := multilineStarts(text, len(d.lines))
	d.inside = inside
	var heads []int
	for i, l := range d.lines {
		if inside[i] {
			continue
		}
		t := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(t, "[") {
			path, err := headerPath(t)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			heads = append(heads, i)
			d.sections = append(d.sections, tomlSection{start: i, path: path})
		}
	}
	for j := range d.sections {
		end := len(d.lines)
		if j+1 < len(heads) {
			end = heads[j+1]
		}
		// Blank and comment lines just before the next header belong to that header.
		for end > d.sections[j].start+1 && !inside[end-1] && blankOrComment(d.lines[end-1]) {
			end--
		}
		d.sections[j].end = end
	}
	return d, nil
}

func blankOrComment(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || strings.HasPrefix(t, "#")
}

func splitLines(text string) []string {
	var out []string
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:i+1])
		text = text[i+1:]
	}
	return out
}

// multilineStarts reports, per line, whether the line starts inside a value: a multi-line
// string, or an array or inline table that spans lines. Such a line is never a header.
func multilineStarts(text string, n int) []bool {
	out := make([]bool, n+1)
	line := 0
	const (
		none = iota
		basic
		literal
		mlBasic
		mlLiteral
		comment
	)
	state, depth := none, 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\n' {
			if state == comment || state == basic || state == literal {
				state = none
			}
			line++
			if line < len(out) {
				out[line] = state == mlBasic || state == mlLiteral || depth > 0
			}
			continue
		}
		switch state {
		case none:
			switch {
			case c == '#':
				state = comment
			case strings.HasPrefix(text[i:], `"""`):
				state, i = mlBasic, i+2
			case strings.HasPrefix(text[i:], `'''`):
				state, i = mlLiteral, i+2
			case c == '"':
				state = basic
			case c == '\'':
				state = literal
			case c == '[' || c == '{':
				depth++
			case (c == ']' || c == '}') && depth > 0:
				depth--
			}
		case basic:
			if c == '\\' {
				i++
			} else if c == '"' {
				state = none
			}
		case literal:
			if c == '\'' {
				state = none
			}
		case mlBasic:
			if c == '\\' {
				if i+1 < len(text) && text[i+1] != '\n' {
					i++
				}
			} else if strings.HasPrefix(text[i:], `"""`) {
				// Up to two more quotes may close it ("""" or """"").
				j := i + 3
				for k := 0; k < 2 && j < len(text) && text[j] == '"'; k++ {
					j++
				}
				state, i = none, j-1
			}
		case mlLiteral:
			if strings.HasPrefix(text[i:], `'''`) {
				j := i + 3
				for k := 0; k < 2 && j < len(text) && text[j] == '\''; k++ {
					j++
				}
				state, i = none, j-1
			}
		}
	}
	return out[:n]
}

// headerPath reads the dotted key of a [table] or [[array]] header line.
func headerPath(t string) ([]string, error) {
	s := strings.TrimPrefix(t, "[")
	array := strings.HasPrefix(s, "[")
	if array {
		s = s[1:]
	}
	var path []string
	for {
		s = strings.TrimLeft(s, " \t")
		var part string
		switch {
		case strings.HasPrefix(s, `"`):
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(s) {
				return nil, fmt.Errorf("unterminated key in a header")
			}
			u, err := strconv.Unquote(s[:end+1])
			if err != nil {
				u = s[1:end]
			}
			part, s = u, s[end+1:]
		case strings.HasPrefix(s, "'"):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated key in a header")
			}
			part, s = s[1:end+1], s[end+2:]
		default:
			end := 0
			for end < len(s) && bareKeyByte(s[end]) {
				end++
			}
			if end == 0 {
				return nil, fmt.Errorf("bad header")
			}
			part, s = s[:end], s[end:]
		}
		path = append(path, part)
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, ".") {
			s = s[1:]
			continue
		}
		if !strings.HasPrefix(s, "]") || array && !strings.HasPrefix(s, "]]") {
			return nil, fmt.Errorf("bad header")
		}
		return path, nil
	}
}

func bareKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// serverSections returns the sections that belong to one server: its table and sub-tables.
func (d *tomlDoc) serverSections(name string) []int {
	var out []int
	for i, s := range d.sections {
		if len(s.path) >= 2 && s.path[0] == "mcp_servers" && s.path[1] == name {
			out = append(out, i)
		}
	}
	return out
}

// apply returns the text with each named server set to its new section text, or removed when
// the text is "". Updated servers stay in place; new ones go at the end in name order.
func (d *tomlDoc) apply(changes map[string]string) string {
	nl := "\n"
	if len(d.lines) > 0 && strings.HasSuffix(d.lines[0], "\r\n") {
		nl = "\r\n"
	}
	drop := make([]bool, len(d.lines))
	insert := map[int]string{} // line index -> text placed before it
	var tail []string
	for _, name := range sortedKeys(changes) {
		text := strings.ReplaceAll(changes[name], "\n", nl)
		secs := d.serverSections(name)
		var comments strings.Builder
		for k, si := range secs {
			end := d.sections[si].end
			if k+1 < len(secs) && secs[k+1] == si+1 {
				// The gap up to the server's next sub-table goes too.
				end = d.sections[si+1].start
			}
			for l := d.sections[si].start; l < end; l++ {
				drop[l] = true
				// A comment the user wrote inside the table stays, above the new table.
				if t := strings.TrimSpace(d.lines[l]); text != "" && !d.inside[l] && strings.HasPrefix(t, "#") {
					comments.WriteString(strings.TrimRight(d.lines[l], "\r\n") + nl)
				}
			}
		}
		if text == "" {
			continue
		}
		if len(secs) == 0 {
			tail = append(tail, text)
			continue
		}
		insert[d.sections[secs[0]].start] = comments.String() + text
	}
	var b strings.Builder
	for i, l := range d.lines {
		if t, ok := insert[i]; ok {
			b.WriteString(t)
		}
		if !drop[i] {
			b.WriteString(l)
		}
	}
	out := b.String()
	for _, t := range tail {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += nl
		}
		if out != "" && !strings.HasSuffix(out, nl+nl) {
			out += nl
		}
		out += t
	}
	return out
}

// codexKeyOrder puts the keys people look for first.
var codexKeyOrder = []string{"command", "args", "cwd", "url", "env", "env_vars", "http_headers", "env_http_headers", "bearer_token_env_var", "startup_timeout_sec"}

// tomlSectionText renders one server as a [mcp_servers.<name>] table, maps as inline tables.
func tomlSectionText(name string, v map[string]any) string {
	var b strings.Builder
	b.WriteString("[mcp_servers." + tomlKey(name) + "]\n")
	keys := sortedKeys(v)
	sort.SliceStable(keys, func(i, j int) bool { return rank(keys[i]) < rank(keys[j]) })
	for _, k := range keys {
		b.WriteString(tomlKey(k) + " = " + tomlValue(v[k]) + "\n")
	}
	return b.String()
}

func rank(k string) int {
	if i := slices.Index(codexKeyOrder, k); i >= 0 {
		return i
	}
	return len(codexKeyOrder)
}

func tomlKey(k string) string {
	if k != "" && strings.IndexFunc(k, func(r rune) bool { return r > 127 || !bareKeyByte(byte(r)) }) < 0 {
		return k
	}
	return tomlString(k)
}

func tomlValue(v any) string {
	switch x := v.(type) {
	case string:
		return tomlString(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, y := range x {
			parts[i] = tomlValue(y)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		if len(x) == 0 {
			return "{}"
		}
		var parts []string
		for _, k := range sortedKeys(x) {
			parts = append(parts, tomlKey(k)+" = "+tomlValue(x[k]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	panic(fmt.Sprintf("mcpreg: no TOML form for %T", v))
}

// tomlString is a TOML basic string. Control characters use \u escapes; TOML has no \x.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// decodeTOML decodes a TOML text into plain maps.
func decodeTOML(text string) (map[string]any, error) {
	m := map[string]any{}
	if _, err := toml.Decode(text, &m); err != nil {
		return nil, err
	}
	return m, nil
}
