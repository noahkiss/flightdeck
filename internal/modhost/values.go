package modhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"

	"github.com/dop251/goja"
)

// Bounds on a value that crosses the bridge (modules.md 7.3). The walk recurses at most
// maxValueDepth deep, so a module cannot make the host recurse without end.
const (
	maxValueDepth = 64
	maxValueNodes = 20000
)

// valueError is a JS value the bridge cannot carry.
type valueError struct{ msg string }

func (e *valueError) Error() string { return e.msg }

// exporter turns a JS value into JSON, bounded in depth, node count and bytes. It reads only own
// enumerable string keys and array indexes, calls a toJSON method as JSON.stringify does, and
// refuses functions, symbols and BigInts. Getters and proxies run module code, under the
// current call's time limit.
type exporter struct {
	r        *goja.Runtime
	maxBytes int
	nodes    int
	bytes    int
}

func exportJSON(r *goja.Runtime, v goja.Value, maxBytes int) ([]byte, error) {
	e := &exporter{r: r, maxBytes: maxBytes}
	out, err := e.value(v, 0, "")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, &valueError{err.Error()}
	}
	b := bytes.TrimRight(buf.Bytes(), "\n")
	if len(b) > maxBytes {
		return nil, &valueError{fmt.Sprintf("value is over %d bytes as JSON", maxBytes)}
	}
	return b, nil
}

func (e *exporter) count(n int) error {
	e.nodes++
	e.bytes += n
	if e.nodes > maxValueNodes {
		return &valueError{fmt.Sprintf("value has more than %d parts", maxValueNodes)}
	}
	if e.bytes > e.maxBytes {
		return &valueError{fmt.Sprintf("value is over %d bytes as JSON", e.maxBytes)}
	}
	return nil
}

func (e *exporter) value(v goja.Value, depth int, key string) (any, error) {
	if depth > maxValueDepth {
		return nil, &valueError{fmt.Sprintf("value is nested deeper than %d", maxValueDepth)}
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, e.count(4)
	}
	if o, ok := v.(*goja.Object); ok {
		if fn, ok := goja.AssertFunction(o.Get("toJSON")); ok {
			r, err := fn(o, e.r.ToValue(key))
			if err != nil {
				return nil, err
			}
			if ro, ok := r.(*goja.Object); ok && ro == o {
				return nil, &valueError{"toJSON returned its own object"}
			}
			if _, isObj := r.(*goja.Object); !isObj {
				return e.value(r, depth+1, key)
			}
			v, o = r, r.(*goja.Object)
		}
		if _, ok := goja.AssertFunction(o); ok {
			return nil, &valueError{"a function cannot cross the bridge"}
		}
		if o.ClassName() == "Array" {
			n := o.Get("length").ToInteger()
			if n > maxValueNodes {
				return nil, &valueError{fmt.Sprintf("array has more than %d items", maxValueNodes)}
			}
			if err := e.count(2); err != nil {
				return nil, err
			}
			list := make([]any, 0, n)
			for i := int64(0); i < n; i++ {
				s := strconv.FormatInt(i, 10)
				x, err := e.value(o.Get(s), depth+1, s)
				if err != nil {
					return nil, err
				}
				list = append(list, x)
			}
			return list, nil
		}
		// Keys lists every index of a String object, typed array or arguments object in Go,
		// before any bound applies; only plain objects reach it.
		if !plainObject(o) {
			return nil, &valueError{fmt.Sprintf("a %s cannot cross the bridge; use a plain object, an array or a string", kindOf(o))}
		}
		if err := e.count(2); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for _, k := range o.Keys() {
			pv := o.Get(k)
			if pv == nil || goja.IsUndefined(pv) {
				continue // JSON.stringify drops undefined members
			}
			if pf, ok := pv.(*goja.Object); ok {
				if _, isFn := goja.AssertFunction(pf); isFn {
					continue // and function members
				}
			}
			if err := e.count(len(k) + 3); err != nil {
				return nil, err
			}
			x, err := e.value(pv, depth+1, k)
			if err != nil {
				return nil, err
			}
			m[k] = x
		}
		return m, nil
	}
	switch x := v.Export().(type) {
	case bool:
		return x, e.count(5)
	case int64:
		return x, e.count(20)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, e.count(4)
		}
		return x, e.count(24)
	case string:
		return x, e.count(len(x) + 2)
	}
	return nil, &valueError{"a symbol or BigInt cannot cross the bridge"}
}

// plainObject reports whether o is an ordinary object: one made by an object literal, a class or
// Object.create. String, Number and Boolean objects, typed arrays, ArrayBuffers, Maps, Sets,
// arguments, errors and proxies are not: a proxy forwards the key listing to its target, which
// may be a typed array. goja names typed arrays, Maps and Sets "Object", so the export type
// tells them apart; reading it runs no module code.
func plainObject(o *goja.Object) bool {
	return o.ClassName() == "Object" && o.ExportType() == mapType
}

var (
	mapType   = reflect.TypeFor[map[string]any]()
	proxyType = reflect.TypeFor[goja.Proxy]()
)

// kindOf names an object that is not plain, for an error message.
func kindOf(o *goja.Object) string {
	if c := o.ClassName(); c != "Object" {
		return c
	}
	switch t := o.ExportType(); {
	case t == proxyType:
		return "Proxy"
	case t != nil && t.Kind() == reflect.Slice && t != reflect.TypeFor[[]any]() && t != reflect.TypeFor[[][2]any]():
		return "typed array"
	case t == reflect.TypeFor[goja.ArrayBuffer]():
		return "ArrayBuffer"
	}
	return "built-in object"
}

// jsString returns a JS string primitive as a Go string. It never exports an object, which would
// read its properties.
func jsString(v goja.Value) (string, bool) {
	if v == nil {
		return "", false
	}
	if _, isObj := v.(*goja.Object); isObj {
		return "", false
	}
	s, ok := v.Export().(string)
	return s, ok
}
