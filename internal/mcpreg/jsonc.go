package mcpreg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A small JSONC reader that keeps byte offsets, so an edit changes only the member it targets
// and leaves comments, trailing commas and layout alone (spec 7.2, 8.4).

type jnode struct {
	start, end int // the value's bytes
	members    []jmember
	value      any // decoded: map[string]any, []any, string, float64, bool or nil
	object     bool
}

type jmember struct {
	key      string
	keyStart int
	val      *jnode
}

func (n *jnode) member(key string) (int, *jmember) {
	for i := range n.members {
		if n.members[i].key == key {
			return i, &n.members[i]
		}
	}
	return -1, nil
}

type jparser struct {
	s   string
	pos int
}

// parseJSONC parses a JSONC text. Empty text (only blanks and comments) gives a nil node.
func parseJSONC(text string) (*jnode, error) {
	p := &jparser{s: text}
	if err := p.ws(); err != nil {
		return nil, err
	}
	if p.pos == len(p.s) {
		return nil, nil
	}
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	if err := p.ws(); err != nil {
		return nil, err
	}
	if p.pos != len(p.s) {
		return nil, p.errf("text after the value")
	}
	return n, nil
}

func (p *jparser) errf(f string, a ...any) error {
	line := strings.Count(p.s[:p.pos], "\n") + 1
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(f, a...))
}

func (p *jparser) ws() error {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case strings.HasPrefix(p.s[p.pos:], "//"):
			i := strings.IndexByte(p.s[p.pos:], '\n')
			if i < 0 {
				p.pos = len(p.s)
			} else {
				p.pos += i + 1
			}
		case strings.HasPrefix(p.s[p.pos:], "/*"):
			i := strings.Index(p.s[p.pos+2:], "*/")
			if i < 0 {
				return p.errf("unterminated comment")
			}
			p.pos += i + 4
		default:
			return nil
		}
	}
	return nil
}

const maxJSONDepth = 200

func (p *jparser) value(depth int) (*jnode, error) {
	if depth > maxJSONDepth {
		return nil, p.errf("nested too deep")
	}
	if p.pos >= len(p.s) {
		return nil, p.errf("unexpected end")
	}
	start := p.pos
	switch c := p.s[p.pos]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		p.pos++
		var arr []any
		for {
			if err := p.ws(); err != nil {
				return nil, err
			}
			if p.pos < len(p.s) && p.s[p.pos] == ']' {
				p.pos++
				return &jnode{start: start, end: p.pos, value: arrOrEmpty(arr)}, nil
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v.value)
			if err := p.ws(); err != nil {
				return nil, err
			}
			if p.pos < len(p.s) && p.s[p.pos] == ',' {
				p.pos++
				continue
			}
			if p.pos < len(p.s) && p.s[p.pos] == ']' {
				continue
			}
			return nil, p.errf("expected , or ]")
		}
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &jnode{start: start, end: p.pos, value: s}, nil
	default:
		end := p.pos
		for end < len(p.s) && strings.IndexByte("+-.0123456789eEtrufalsn", p.s[end]) >= 0 {
			end++
		}
		lit := p.s[p.pos:end]
		var v any
		dec := json.NewDecoder(strings.NewReader(lit))
		dec.UseNumber()
		if lit == "" || dec.Decode(&v) != nil || dec.More() {
			return nil, p.errf("bad value")
		}
		if n, ok := v.(json.Number); ok {
			f, err := n.Float64()
			if err != nil {
				return nil, p.errf("bad number")
			}
			v = f
		}
		p.pos = end
		return &jnode{start: start, end: end, value: v}, nil
	}
}

func arrOrEmpty(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}

func (p *jparser) object(depth int) (*jnode, error) {
	n := &jnode{start: p.pos, object: true}
	m := map[string]any{}
	p.pos++
	for {
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos < len(p.s) && p.s[p.pos] == '}' {
			p.pos++
			n.end, n.value = p.pos, m
			return n, nil
		}
		if p.pos >= len(p.s) || p.s[p.pos] != '"' {
			return nil, p.errf("expected a key")
		}
		keyStart := p.pos
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, dup := m[key]; dup {
			return nil, p.errf("duplicate key %q", key)
		}
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.s) || p.s[p.pos] != ':' {
			return nil, p.errf("expected :")
		}
		p.pos++
		if err := p.ws(); err != nil {
			return nil, err
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		m[key] = v.value
		n.members = append(n.members, jmember{key: key, keyStart: keyStart, val: v})
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos < len(p.s) && p.s[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.pos < len(p.s) && p.s[p.pos] == '}' {
			continue
		}
		return nil, p.errf("expected , or }")
	}
}

func (p *jparser) str() (string, error) {
	end := p.pos + 1
	for end < len(p.s) && p.s[end] != '"' {
		if p.s[end] == '\\' {
			end++
		}
		if end < len(p.s) && p.s[end] == '\n' {
			return "", p.errf("newline in a string")
		}
		end++
	}
	if end >= len(p.s) {
		return "", p.errf("unterminated string")
	}
	var s string
	if err := json.Unmarshal([]byte(p.s[p.pos:end+1]), &s); err != nil {
		return "", p.errf("bad string")
	}
	p.pos = end + 1
	return s, nil
}

// lineIndent is the blank run that starts the line holding offset i.
func lineIndent(text string, i int) string {
	ls := strings.LastIndexByte(text[:i], '\n') + 1
	j := ls
	for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
		j++
	}
	return text[ls:j]
}

// alone reports whether only blanks sit between the start of offset i's line and i.
func alone(text string, i int) bool {
	ls := strings.LastIndexByte(text[:i], '\n') + 1
	return strings.TrimLeft(text[ls:i], " \t") == ""
}

func jsonText(v any, indent string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent(indent, "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// insertMember adds key: v as the last member of an object node.
func insertMember(text string, obj *jnode, key string, v any) string {
	outer := lineIndent(text, obj.start)
	ind := outer + "  "
	if len(obj.members) > 0 && alone(text, obj.members[0].keyStart) {
		ind = lineIndent(text, obj.members[0].keyStart)
	}
	member := jsonText(key, "") + ": " + jsonText(v, ind)
	if len(obj.members) == 0 {
		pos := obj.start + 1
		return text[:pos] + "\n" + ind + member + "\n" + outer + text[pos:]
	}
	pos := obj.members[len(obj.members)-1].val.end
	return text[:pos] + ",\n" + ind + member + text[pos:]
}

// deleteMember removes member i of an object node, with its comma, and its line when the
// member had the line to itself.
func deleteMember(text string, obj *jnode, i int) string {
	m := obj.members[i]
	start, end := m.keyStart, m.val.end
	p := &jparser{s: text, pos: end}
	if p.ws() == nil && p.pos < len(text) && text[p.pos] == ',' && onlyBlanks(text[end:p.pos]) {
		end = p.pos + 1
	} else if i > 0 {
		// The last member: the comma after the previous value goes instead.
		q := &jparser{s: text, pos: obj.members[i-1].val.end}
		if q.ws() == nil && q.pos < len(text) && text[q.pos] == ',' {
			c := q.pos
			from := start
			if alone(text, start) {
				if ls := strings.LastIndexByte(text[:start], '\n'); ls > c {
					from = ls
				}
			}
			return text[:c] + text[c+1:from] + text[end:]
		}
	}
	if alone(text, start) {
		k := end
		for k < len(text) && (text[k] == ' ' || text[k] == '\t' || text[k] == '\r') {
			k++
		}
		if k == len(text) || text[k] == '\n' {
			start = strings.LastIndexByte(text[:start], '\n') + 1
			end = k
			if end < len(text) {
				end++
			}
		}
	}
	return text[:start] + text[end:]
}

func onlyBlanks(s string) bool { return strings.Trim(s, " \t") == "" }

var errNotObject = errors.New("not an object")

// setServer sets or removes (v nil) mcp.servers.<name> in a JSONC text.
func setServer(text, name string, v map[string]any) (string, error) {
	root, err := parseJSONC(text)
	if err != nil {
		return "", err
	}
	if root == nil {
		if v == nil {
			return text, nil
		}
		return text + jsonText(map[string]any{"mcp": map[string]any{"servers": map[string]any{name: v}}}, "") + "\n", nil
	}
	if !root.object {
		return "", fmt.Errorf("the file is %w", errNotObject)
	}
	_, mcp := root.member("mcp")
	if mcp == nil {
		if v == nil {
			return text, nil
		}
		return insertMember(text, root, "mcp", map[string]any{"servers": map[string]any{name: v}}), nil
	}
	if !mcp.val.object {
		return "", fmt.Errorf("mcp is %w", errNotObject)
	}
	_, servers := mcp.val.member("servers")
	if servers == nil {
		if v == nil {
			return text, nil
		}
		return insertMember(text, mcp.val, "servers", map[string]any{name: v}), nil
	}
	if !servers.val.object {
		return "", fmt.Errorf("mcp.servers is %w", errNotObject)
	}
	i, m := servers.val.member(name)
	switch {
	case m == nil && v == nil:
		return text, nil
	case m == nil:
		return insertMember(text, servers.val, name, v), nil
	case v == nil:
		return deleteMember(text, servers.val, i), nil
	}
	return text[:m.val.start] + jsonText(v, lineIndent(text, m.keyStart)) + text[m.val.end:], nil
}
