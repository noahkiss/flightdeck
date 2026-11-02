package modules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Problem is one reason a tree or manifest is refused, at a JSON pointer-like path.
type Problem struct {
	Path    string
	Message string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Message
	}
	return p.Path + ": " + p.Message
}

// Problems is the error ValidateTree and ParseManifest return.
type Problems []Problem

func (ps Problems) Error() string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, "; ")
}

var (
	idRE     = regexp.MustCompile(IDPattern)
	tintRE   = regexp.MustCompile(TintPattern)
	symbolRE = regexp.MustCompile(SymbolPattern)
	actionRE = regexp.MustCompile(ActionPattern)
	urlRE    = regexp.MustCompile(URLPattern)
)

// ValidateTree checks a tree, as JSON, against vocabulary version v: everything the generated
// JSON Schema states, and the rules it cannot state (the tree limits, unique ids, a step count
// within its total). It returns nil or Problems.
func ValidateTree(data []byte, v int) error {
	if v < 1 || v > Version {
		return Problems{{"", fmt.Sprintf("vocabulary %d is not known (this build knows 1 to %d)", v, Version)}}
	}
	t := &treeCheck{version: v, ids: map[string]string{}}
	if len(data) > MaxTreeBytes {
		t.fail("", "tree is %d bytes; the limit is %d", len(data), MaxTreeBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return Problems{{"", "not JSON: " + err.Error()}}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.fail("", "trailing data after the tree")
	}
	t.block("", root, 1)
	if t.count > MaxBlocks {
		t.fail("", "tree has %d blocks; the limit is %d", t.count, MaxBlocks)
	}
	if len(t.problems) == 0 {
		return nil
	}
	return t.problems
}

type treeCheck struct {
	version  int
	count    int
	depthHit bool
	ids      map[string]string
	problems Problems
}

func (t *treeCheck) fail(path, format string, args ...any) {
	t.problems = append(t.problems, Problem{path, fmt.Sprintf(format, args...)})
}

func (t *treeCheck) block(path string, v any, depth int) {
	t.count++
	if depth > MaxDepth && !t.depthHit {
		t.depthHit = true
		t.fail(path, "tree is deeper than %d blocks", MaxDepth)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.fail(path, "a block is a JSON object")
		return
	}
	typ, ok := m["type"].(string)
	if !ok {
		t.fail(path, "type is missing or not a string")
		return
	}
	def, ok := Lookup(typ, t.version)
	if !ok {
		t.fail(path+"/type", "unknown block type %q in vocabulary %d", typ, t.version)
		return
	}
	fields := map[string]Field{}
	for _, f := range Common {
		fields[f.Name] = f
	}
	for _, f := range def.Fields {
		fields[f.Name] = f
	}
	for _, k := range sortedKeys(m) {
		if k == "type" {
			continue
		}
		f, ok := fields[k]
		if !ok {
			t.fail(path+"/"+k, "%s has no field %q", typ, k)
			continue
		}
		t.field(path+"/"+k, f, m[k], depth)
	}
	for _, f := range def.Fields {
		if _, ok := m[f.Name]; f.Required && !ok {
			t.fail(path, "%s needs %s", typ, f.Name)
		}
	}
	if len(def.ExactlyOne) > 0 {
		n := 0
		for _, name := range def.ExactlyOne {
			if _, ok := m[name]; ok {
				n++
			}
		}
		if n != 1 {
			t.fail(path, "%s sets exactly one of %s", typ, strings.Join(def.ExactlyOne, ", "))
		}
	}
	if id, ok := m["id"].(string); ok && id != "" {
		if prev, dup := t.ids[id]; dup {
			t.fail(path+"/id", "id %q is also used at %s", id, rootPath(prev))
		} else {
			t.ids[id] = path
		}
	}
}

func rootPath(p string) string {
	if p == "" {
		return "the root"
	}
	return p
}

func (t *treeCheck) field(path string, f Field, v any, depth int) {
	switch f.Kind {
	case KindString:
		s, ok := v.(string)
		if !ok {
			t.fail(path, "not a string")
			return
		}
		t.text(path, s, f.Max)
		if f.Name == "id" && !idRE.MatchString(s) {
			t.fail(path, "an id is letters, digits and _ . : -")
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			t.fail(path, "not true or false")
		}
	case KindInt:
		n, ok := wholeNumber(v)
		if !ok {
			t.fail(path, "not a whole number")
			return
		}
		if n < int64(f.Min) || n > int64(f.Max) {
			t.fail(path, "%d is outside %d to %d", n, f.Min, f.Max)
		}
	case KindFraction:
		x, ok := number(v)
		if !ok || x < 0 || x > 1 {
			t.fail(path, "not a number from 0 to 1")
		}
	case KindWords:
		s, ok := v.(string)
		if !ok || !contains(f.Words, s) {
			t.fail(path, "not one of %s", strings.Join(f.Words, ", "))
		}
	case KindTint:
		s, ok := v.(string)
		if !ok || !contains(Tints, s) && !tintRE.MatchString(s) {
			t.fail(path, "not a tint: one of %s, or #RRGGBB", strings.Join(Tints, ", "))
		}
	case KindSymbol:
		s, ok := v.(string)
		if !ok || !symbolRE.MatchString(s) {
			t.fail(path, "not a symbol name")
			return
		}
		t.text(path, s, SymbolMax)
	case KindTime:
		s, ok := v.(string)
		if !ok {
			t.fail(path, "not an RFC 3339 time")
			return
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.fail(path, "not an RFC 3339 time")
		}
	case KindAction:
		t.action(path, v)
	case KindSteps:
		m, ok := v.(map[string]any)
		if !ok {
			t.fail(path, "steps is an object {total, current}")
			return
		}
		for _, k := range sortedKeys(m) {
			if k != "total" && k != "current" {
				t.fail(path+"/"+k, "steps has no field %q", k)
			}
		}
		total, okT := wholeNumber(m["total"])
		current, okC := wholeNumber(m["current"])
		if !okT || total < 1 || total > 1_000_000 {
			t.fail(path+"/total", "total is a whole number from 1 to 1000000")
		}
		if !okC || current < 0 || current > 1_000_000 {
			t.fail(path+"/current", "current is a whole number from 0 to 1000000")
		}
		if okT && okC && current > total {
			t.fail(path, "current %d is above total %d", current, total)
		}
	case KindChildren:
		list, ok := v.([]any)
		if !ok {
			t.fail(path, "children is an array of blocks")
			return
		}
		if len(list) < f.Min || len(list) > f.Max {
			t.fail(path, "has %d children; %d to %d allowed", len(list), f.Min, f.Max)
		}
		for i, c := range list {
			cp := fmt.Sprintf("%s/%d", path, i)
			if f.Only != "" {
				if m, ok := c.(map[string]any); ok && m["type"] != f.Only {
					t.fail(cp+"/type", "only %s blocks go here", f.Only)
					t.count++
					continue
				}
			}
			t.block(cp, c, depth+1)
		}
	case KindLines:
		list, ok := v.([]any)
		if !ok {
			t.fail(path, "lines is an array of strings")
			return
		}
		if len(list) > f.Max {
			t.fail(path, "has %d lines; at most %d", len(list), f.Max)
		}
		for i, l := range list {
			s, ok := l.(string)
			if !ok {
				t.fail(fmt.Sprintf("%s/%d", path, i), "not a string")
				continue
			}
			t.text(fmt.Sprintf("%s/%d", path, i), s, f.LineMax)
		}
	}
}

func (t *treeCheck) text(path, s string, max int) {
	if n := utf8.RuneCountInString(s); max > 0 && n > max {
		t.fail(path, "%d characters; at most %d", n, max)
	}
}

func (t *treeCheck) action(path string, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		t.fail(path, "an action is an object {name, args}")
		return
	}
	for _, k := range sortedKeys(m) {
		if k != "name" && k != "args" {
			t.fail(path+"/"+k, "an action has no field %q", k)
		}
	}
	name, ok := m["name"].(string)
	if !ok {
		t.fail(path+"/name", "an action needs a name")
		return
	}
	if !actionRE.MatchString(name) || len(name) > ActionMax {
		t.fail(path+"/name", "an action name is dotted lowercase words, at most %d characters", ActionMax)
		return
	}
	var args map[string]any
	if a, ok := m["args"]; ok {
		if args, ok = a.(map[string]any); !ok {
			t.fail(path+"/args", "args is an object")
			return
		}
	}
	if !strings.HasPrefix(name, ClientPrefix) {
		return
	}
	for _, ca := range ClientActions {
		if ca.Name != name {
			continue
		}
		for _, a := range ca.Args {
			s, ok := args[a].(string)
			if !ok || s == "" {
				t.fail(path+"/args", "%s needs args.%s, a string", name, a)
				continue
			}
			if a == "url" && !urlRE.MatchString(s) {
				t.fail(path+"/args/url", "client.open_url takes an https URL")
			}
		}
		return
	}
	t.fail(path+"/name", "%q is not a client action", name)
}

func wholeNumber(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	// 3.0 is a whole number in JSON Schema.
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
