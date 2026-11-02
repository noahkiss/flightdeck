package modhost

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api/modules"
)

// cutTree cuts each text field of each known block to its limit, ending it with an ellipsis
// (modules.md 6.1, 12). Ids are left alone: a cut id could clash with another. Everything else
// is left for ValidateTree to judge.
func cutTree(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	cutBlock(root)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

var blockDefs = func() map[string]modules.BlockDef {
	m := map[string]modules.BlockDef{}
	for _, b := range modules.Blocks {
		m[b.Type] = b
	}
	return m
}()

func cutBlock(v any) {
	b, ok := v.(map[string]any)
	if !ok {
		return
	}
	t, _ := b["type"].(string)
	def, ok := blockDefs[t]
	if !ok {
		return
	}
	for _, fields := range [][]modules.Field{modules.Common, def.Fields} {
		for _, f := range fields {
			fv, ok := b[f.Name]
			if !ok {
				continue
			}
			switch f.Kind {
			case modules.KindString:
				if s, ok := fv.(string); ok && f.Name != "id" {
					b[f.Name] = cutText(s, f.Max)
				}
			case modules.KindLines:
				if list, ok := fv.([]any); ok {
					for i, l := range list {
						if s, ok := l.(string); ok {
							list[i] = cutText(s, f.LineMax)
						}
					}
				}
			case modules.KindChildren:
				if list, ok := fv.([]any); ok {
					for _, c := range list {
						cutBlock(c)
					}
				}
			}
		}
	}
}

// cutText returns s, or its first max-1 characters and an ellipsis when it is longer than max.
func cutText(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
