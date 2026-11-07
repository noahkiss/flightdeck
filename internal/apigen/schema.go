// Package apigen builds the API schema from the Go types in package api, generates the Go and
// Swift code from it, and checks a schema against a released one for breaking changes.
package apigen

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// Schema is the subset of JSON Schema the API uses. An empty Schema means any JSON value.
type Schema struct {
	Ref             string             `json:"$ref,omitempty"`
	Type            string             `json:"type,omitempty"`
	Format          string             `json:"format,omitempty"`
	ContentEncoding string             `json:"contentEncoding,omitempty"`
	Description     string             `json:"description,omitempty"`
	Properties      map[string]*Schema `json:"properties,omitempty"`
	Required        []string           `json:"required,omitempty"`
	Items           *Schema            `json:"items,omitempty"`
	KnownValues     []string           `json:"x-known-values,omitempty"`
}

type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type ContentDescriptor struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema"`
}

type MethodDoc struct {
	Name           string              `json:"name"`
	Summary        string              `json:"summary"`
	ParamStructure string              `json:"paramStructure"`
	Params         []ContentDescriptor `json:"params"`
	Result         ContentDescriptor   `json:"result"`
	ParamsType     *Schema             `json:"x-params"`
}

type NotificationDoc struct {
	Name    string  `json:"name"`
	Summary string  `json:"summary"`
	Params  *Schema `json:"params"`
}

type EventDoc struct {
	Type    string  `json:"type"`
	Summary string  `json:"summary"`
	Data    *Schema `json:"data"`
}

type FrameDoc struct {
	Name      string  `json:"name"`
	Kind      int     `json:"kind"`
	Direction string  `json:"direction"`
	Payload   *Schema `json:"payload,omitempty"`
	Layout    string  `json:"layout,omitempty"`
}

type Components struct {
	Schemas map[string]*Schema `json:"schemas"`
}

// Document is an OpenRPC document with extensions for what OpenRPC has no place for:
// server notifications, event types, data frames and capabilities.
type Document struct {
	OpenRPC       string            `json:"openrpc"`
	Info          Info              `json:"info"`
	Revision      int               `json:"x-revision"`
	MinRevision   int               `json:"x-min-revision"`
	ErrorCode     int               `json:"x-error-code"`
	ErrorData     *Schema           `json:"x-error-data"`
	Capabilities  []string          `json:"x-capabilities"`
	Methods       []MethodDoc       `json:"methods"`
	Notifications []NotificationDoc `json:"x-notifications"`
	Events        []EventDoc        `json:"x-events"`
	Frames        []FrameDoc        `json:"x-frames"`
	Components    Components        `json:"components"`
}

const refPrefix = "#/components/schemas/"

var rawJSONType = reflect.TypeFor[json.RawMessage]()

type builder struct {
	doc   *Document
	types map[string]reflect.Type
}

// Build reflects the api package into a Document.
func Build() (doc *Document, err error) {
	b := &builder{
		doc: &Document{
			OpenRPC: "1.3.2",
			Info: Info{
				Title:       "gff-hab API",
				Version:     fmt.Sprint(api.Revision),
				Description: "Generated from the Go types in package api. Do not edit. See docs/specs/hab-logic-api.md.",
			},
			Revision:     api.Revision,
			MinRevision:  api.MinRevision,
			ErrorCode:    api.ErrorCode,
			Capabilities: api.Capabilities,
			Components:   Components{Schemas: map[string]*Schema{}},
		},
		types: map[string]reflect.Type{},
	}
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, fmt.Errorf("apigen: %v", r)
		}
	}()
	b.build()
	return b.doc, nil
}

func (b *builder) build() {
	d := b.doc
	d.ErrorData = b.schemaFor(reflect.TypeFor[api.ErrorData]())
	seen := map[string]bool{}
	for _, m := range api.Methods {
		if seen[m.Name] {
			panic("duplicate method " + m.Name)
		}
		seen[m.Name] = true
		pt := reflect.TypeOf(m.Params)
		pref := b.schemaFor(pt)
		obj := b.doc.Components.Schemas[strings.TrimPrefix(pref.Ref, refPrefix)]
		md := MethodDoc{
			Name:           m.Name,
			Summary:        m.Summary,
			ParamStructure: "by-name",
			Params:         []ContentDescriptor{},
			Result:         ContentDescriptor{Name: "result", Schema: b.schemaFor(reflect.TypeOf(m.Result))},
			ParamsType:     pref,
		}
		for _, name := range sortedKeys(obj.Properties) {
			p := obj.Properties[name]
			cp := *p
			cp.Description = ""
			md.Params = append(md.Params, ContentDescriptor{
				Name:        name,
				Description: p.Description,
				Required:    contains(obj.Required, name),
				Schema:      &cp,
			})
		}
		d.Methods = append(d.Methods, md)
	}
	for _, n := range api.Notifications {
		d.Notifications = append(d.Notifications, NotificationDoc{n.Name, n.Summary, b.schemaFor(reflect.TypeOf(n.Params))})
	}
	seen = map[string]bool{}
	for _, e := range api.Events {
		if seen[string(e.Type)] {
			panic("duplicate event " + e.Type)
		}
		seen[string(e.Type)] = true
		d.Events = append(d.Events, EventDoc{string(e.Type), e.Summary, b.schemaFor(reflect.TypeOf(e.Data))})
	}
	for _, f := range api.Frames {
		fd := FrameDoc{Name: f.Name, Kind: int(f.Kind), Direction: f.Direction, Layout: f.Layout}
		if f.Payload != nil {
			fd.Payload = b.schemaFor(reflect.TypeOf(f.Payload))
		}
		d.Frames = append(d.Frames, fd)
	}
	for name := range api.Enums {
		if _, ok := b.doc.Components.Schemas[name]; !ok {
			panic("enum " + name + " is not used by any type")
		}
	}
}

func (b *builder) register(t reflect.Type, s func() *Schema) *Schema {
	name := t.Name()
	if prev, ok := b.types[name]; ok {
		if prev != t {
			panic(fmt.Sprintf("two types named %s", name))
		}
		return &Schema{Ref: refPrefix + name}
	}
	b.types[name] = t
	b.doc.Components.Schemas[name] = &Schema{} // placeholder for recursion
	*b.doc.Components.Schemas[name] = *s()
	return &Schema{Ref: refPrefix + name}
}

func (b *builder) schemaFor(t reflect.Type) *Schema {
	if t == rawJSONType {
		return &Schema{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return b.schemaFor(t.Elem())
	case reflect.String:
		if values, ok := api.Enums[t.Name()]; ok && t.PkgPath() != "" {
			return b.register(t, func() *Schema { return &Schema{Type: "string", KnownValues: values} })
		}
		return &Schema{Type: "string"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &Schema{Type: "integer", Format: "int64"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer", Format: "uint64"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", ContentEncoding: "base64"}
		}
		return &Schema{Type: "array", Items: b.schemaFor(t.Elem())}
	case reflect.Struct:
		return b.register(t, func() *Schema {
			s := &Schema{Type: "object", Properties: map[string]*Schema{}}
			b.fields(t, s)
			if len(s.Properties) == 0 {
				s.Properties = nil
			}
			return s
		})
	}
	panic(fmt.Sprintf("unsupported type %s", t))
}

func (b *builder) fields(t reflect.Type, s *Schema) {
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			b.fields(f.Type, s)
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			panic(fmt.Sprintf("%s.%s has no json name", t.Name(), f.Name))
		}
		if _, dup := s.Properties[name]; dup {
			panic(fmt.Sprintf("%s: duplicate field %s", t.Name(), name))
		}
		p := b.schemaFor(f.Type)
		cp := *p
		cp.Description = f.Tag.Get("doc")
		s.Properties[name] = &cp
		optional := f.Type.Kind() == reflect.Pointer || strings.Contains(","+opts+",", ",omitempty,")
		if !optional {
			s.Required = append(s.Required, name)
		}
	}
}

// Resolve follows a $ref inside doc.
func (d *Document) Resolve(s *Schema) (*Schema, string) {
	if s == nil || s.Ref == "" {
		return s, ""
	}
	name := strings.TrimPrefix(s.Ref, refPrefix)
	r, ok := d.Components.Schemas[name]
	if !ok {
		return nil, name
	}
	return r, name
}

// JSON renders the document with stable formatting.
func (d *Document) JSON() []byte {
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(out, '\n')
}
