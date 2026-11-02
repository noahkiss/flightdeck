package modules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// evaluator is a small JSON Schema (draft 2020-12) evaluator for the keywords the generated
// schemas use. It is independent of ValidateTree, so the schema files and the Go checks are
// tested against the same fixtures from two sides. Values come from decodeJSON (numbers as
// json.Number).
type evaluator struct {
	root map[string]any
	errs []string
}

func newEvaluator(schema []byte) (*evaluator, error) {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, err
	}
	return &evaluator{root: root}, nil
}

// valid reports whether v satisfies the root schema, with the reasons when it does not.
func (e *evaluator) valid(v any) (bool, []string) {
	e.errs = nil
	ok := e.eval(e.root, v, "")
	return ok, e.errs
}

func (e *evaluator) fail(path, format string, args ...any) bool {
	e.errs = append(e.errs, path+": "+fmt.Sprintf(format, args...))
	return false
}

var known = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true, "x-vocabulary": true, "$defs": true,
	"$ref": true, "type": true, "enum": true, "const": true, "required": true, "properties": true,
	"additionalProperties": true, "allOf": true, "anyOf": true, "oneOf": true, "not": true, "if": true,
	"then": true, "else": true, "items": true, "minItems": true, "maxItems": true, "uniqueItems": true,
	"minLength": true, "maxLength": true, "pattern": true, "minimum": true, "maximum": true, "format": true,
}

func (e *evaluator) eval(s map[string]any, v any, path string) bool {
	for k := range s {
		if !known[k] {
			panic("evaluator: keyword " + k + " is not supported; teach it to the evaluator")
		}
	}
	ok := true
	if r, has := s["$ref"].(string); has {
		name := strings.TrimPrefix(r, "#/$defs/")
		def, found := e.root["$defs"].(map[string]any)[name].(map[string]any)
		if !found {
			panic("evaluator: no definition " + r)
		}
		ok = e.eval(def, v, path) && ok
	}
	if t, has := s["type"].(string); has && !hasType(v, t) {
		return e.fail(path, "not of type %s", t)
	}
	if list, has := s["enum"].([]any); has {
		match := false
		for _, w := range list {
			if equal(w, v) {
				match = true
			}
		}
		if !match {
			ok = e.fail(path, "not in enum")
		}
	}
	if c, has := s["const"]; has && !equal(c, v) {
		ok = e.fail(path, "not the const %v", c)
	}
	if m, isObj := v.(map[string]any); isObj {
		for _, r := range asList(s["required"]) {
			if _, has := m[r.(string)]; !has {
				ok = e.fail(path, "missing %s", r)
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, pv := range m {
			ps, has := props[k].(map[string]any)
			if has {
				ok = e.eval(ps, pv, path+"/"+k) && ok
			} else if ap, set := s["additionalProperties"].(bool); set && !ap {
				ok = e.fail(path+"/"+k, "unknown property")
			}
		}
	}
	if list, isArr := v.([]any); isArr {
		if it, has := s["items"].(map[string]any); has {
			for i, x := range list {
				ok = e.eval(it, x, fmt.Sprintf("%s/%d", path, i)) && ok
			}
		}
		if n, has := intKeyword(s, "minItems"); has && len(list) < n {
			ok = e.fail(path, "fewer than %d items", n)
		}
		if n, has := intKeyword(s, "maxItems"); has && len(list) > n {
			ok = e.fail(path, "more than %d items", n)
		}
		if u, _ := s["uniqueItems"].(bool); u {
			for i := range list {
				for j := i + 1; j < len(list); j++ {
					if equal(list[i], list[j]) {
						ok = e.fail(path, "items %d and %d are equal", i, j)
					}
				}
			}
		}
	}
	if str, isStr := v.(string); isStr {
		n := utf8.RuneCountInString(str)
		if m, has := intKeyword(s, "minLength"); has && n < m {
			ok = e.fail(path, "shorter than %d", m)
		}
		if m, has := intKeyword(s, "maxLength"); has && n > m {
			ok = e.fail(path, "longer than %d", m)
		}
		if p, has := s["pattern"].(string); has && !regexp.MustCompile(p).MatchString(str) {
			ok = e.fail(path, "does not match %s", p)
		}
		if f, _ := s["format"].(string); f == "date-time" {
			if _, err := time.Parse(time.RFC3339, str); err != nil {
				ok = e.fail(path, "not a date-time")
			}
		}
	}
	if num, isNum := v.(json.Number); isNum {
		x, _ := num.Float64()
		if m, has := s["minimum"].(float64); has && x < m {
			ok = e.fail(path, "below %v", m)
		}
		if m, has := s["maximum"].(float64); has && x > m {
			ok = e.fail(path, "above %v", m)
		}
	}
	for _, sub := range asList(s["allOf"]) {
		ok = e.eval(sub.(map[string]any), v, path) && ok
	}
	if list := asList(s["anyOf"]); list != nil {
		if e.count(list, v, path) == 0 {
			ok = e.fail(path, "matches none of anyOf")
		}
	}
	if list := asList(s["oneOf"]); list != nil {
		if n := e.count(list, v, path); n != 1 {
			ok = e.fail(path, "matches %d of oneOf, not exactly one", n)
		}
	}
	if not, has := s["not"].(map[string]any); has && e.quiet(not, v, path) {
		ok = e.fail(path, "matches not")
	}
	if cond, has := s["if"].(map[string]any); has {
		if e.quiet(cond, v, path) {
			if then, has := s["then"].(map[string]any); has {
				ok = e.eval(then, v, path) && ok
			}
		} else if els, has := s["else"].(map[string]any); has {
			ok = e.eval(els, v, path) && ok
		}
	}
	return ok
}

// quiet evaluates without keeping the reasons.
func (e *evaluator) quiet(s map[string]any, v any, path string) bool {
	saved := e.errs
	ok := e.eval(s, v, path)
	e.errs = saved
	return ok
}

func (e *evaluator) count(list []any, v any, path string) int {
	n := 0
	for _, sub := range list {
		if e.quiet(sub.(map[string]any), v, path) {
			n++
		}
	}
	return n
}

func asList(v any) []any { l, _ := v.([]any); return l }

func intKeyword(s map[string]any, k string) (int, bool) {
	f, ok := s[k].(float64)
	return int(f), ok
}

func hasType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	}
	panic("evaluator: type " + t)
}

// equal compares a schema value (numbers as float64) with an instance value (json.Number).
func equal(a, b any) bool {
	norm := func(x any) any {
		raw, _ := json.Marshal(x)
		var out any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// decodeJSON decodes an instance with numbers kept as json.Number.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}
