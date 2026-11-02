package apigen

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// ReleasedVocabularyDir holds a copy of each released block vocabulary's schema, taken when the
// app that draws it shipped (docs/specs/modules.md 9). make gen never writes it.
const ReleasedVocabularyDir = "api/modules/released"

// annotations are schema keys that describe and never decide; they may change freely.
var annotations = []string{"description", "title", "$comment"}

// VocabularyGains lists every change from a released block schema to the current one that would
// refuse a tree the released schema accepts. A released vocabulary may only gain: a new optional
// field (a new key under properties) or a new word (a new entry in an enum). Anything else, such
// as a removed field, a new required field or a changed limit, is listed. Both arguments are
// schema files as JSON.
func VocabularyGains(released, cur []byte) ([]string, error) {
	var old, now any
	if err := json.Unmarshal(released, &old); err != nil {
		return nil, fmt.Errorf("released schema: %w", err)
	}
	if err := json.Unmarshal(cur, &now); err != nil {
		return nil, fmt.Errorf("current schema: %w", err)
	}
	var breaks []string
	gainsOnly("", old, now, &breaks)
	sort.Strings(breaks)
	return breaks, nil
}

func gainsOnly(path string, old, cur any, breaks *[]string) {
	fail := func(format string, args ...any) {
		*breaks = append(*breaks, path+": "+fmt.Sprintf(format, args...))
	}
	switch o := old.(type) {
	case map[string]any:
		c, ok := cur.(map[string]any)
		if !ok {
			fail("was an object")
			return
		}
		for _, k := range sortedKeys(o) {
			if slices.Contains(annotations, k) {
				continue
			}
			cv, ok := c[k]
			if !ok {
				fail("%s removed", k)
				continue
			}
			switch k {
			case "properties", "$defs":
				// May gain entries; each entry it had must only gain.
				om, _ := o[k].(map[string]any)
				cm, isMap := cv.(map[string]any)
				if !isMap {
					fail("%s is no longer an object", k)
					continue
				}
				for _, name := range sortedKeys(om) {
					if _, ok := cm[name]; !ok {
						fail("%s/%s removed", k, name)
						continue
					}
					gainsOnly(path+"/"+k+"/"+name, om[name], cm[name], breaks)
				}
			case "enum":
				ol, _ := o[k].([]any)
				cl, _ := cv.([]any)
				for _, w := range ol {
					if !slices.ContainsFunc(cl, func(x any) bool { return reflect.DeepEqual(x, w) }) {
						fail("enum lost %v", w)
					}
				}
			default:
				gainsOnly(path+"/"+k, o[k], cv, breaks)
			}
		}
		for _, k := range sortedKeys(c) {
			if _, ok := o[k]; !ok && !slices.Contains(annotations, k) {
				fail("%s added", k)
			}
		}
	case []any:
		c, ok := cur.([]any)
		if !ok || len(c) != len(o) {
			fail("array changed")
			return
		}
		for i := range o {
			gainsOnly(fmt.Sprintf("%s/%d", path, i), o[i], c[i], breaks)
		}
	default:
		if !reflect.DeepEqual(old, cur) {
			fail("changed from %v to %v", old, cur)
		}
	}
}
