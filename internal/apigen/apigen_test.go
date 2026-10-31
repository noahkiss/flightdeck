package apigen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const root = "../.."

// TestGeneratedFilesFresh fails when a generated file differs from what Generate produces.
// Run `make gen` to fix it.
func TestGeneratedFilesFresh(t *testing.T) {
	files, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("%s: %v (run make gen)", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s is stale: run make gen", path)
		}
	}
}

// TestCompatWithReleased compares the current schema with every released schema of the same
// revision. A breaking change needs a new revision and its translator (spec 3.3).
func TestCompatWithReleased(t *testing.T) {
	cur, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, ReleasedDir, "*.json"))
	if len(paths) == 0 {
		t.Fatal("no released schema found")
	}
	compared := 0
	for _, path := range paths {
		old := load(t, path)
		if old.Revision != cur.Revision {
			continue
		}
		compared++
		for _, b := range Compare(old, cur) {
			t.Errorf("%s: breaking change: %s", filepath.Base(path), b)
		}
	}
	t.Logf("compared against %d released schema(s) of revision %d", compared, cur.Revision)
}

func load(t *testing.T, path string) *Document {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// clone gives a deep copy of the current schema to mutate.
func clone(t *testing.T) *Document {
	t.Helper()
	d, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	var c Document
	if err := json.Unmarshal(d.JSON(), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func TestCompare(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *Document)
		breaks string // substring of the expected break; empty means compatible
	}{
		{"unchanged", func(d *Document) {}, ""},
		{"output field removed", func(d *Document) {
			delete(d.Components.Schemas["Welcome"].Properties, "epoch")
		}, "epoch: field removed"},
		{"input field removed", func(d *Document) {
			delete(d.Components.Schemas["HelloParams"].Properties, "label")
		}, "label: field removed"},
		{"output field added", func(d *Document) {
			d.Components.Schemas["Welcome"].Properties["motd"] = &Schema{Type: "string"}
			d.Components.Schemas["Welcome"].Required = append(d.Components.Schemas["Welcome"].Required, "motd")
		}, ""},
		{"optional input added", func(d *Document) {
			d.Components.Schemas["HelloParams"].Properties["theme"] = &Schema{Type: "string"}
		}, ""},
		{"required input added", func(d *Document) {
			s := d.Components.Schemas["HelloParams"]
			s.Properties["theme"] = &Schema{Type: "string"}
			s.Required = append(s.Required, "theme")
		}, "theme: new required input"},
		{"field type changed", func(d *Document) {
			d.Components.Schemas["Pane"].Properties["cols"] = &Schema{Type: "string"}
		}, "cols: type changed"},
		{"required output became optional", func(d *Document) {
			s := d.Components.Schemas["Welcome"]
			s.Required = without(s.Required, "epoch")
		}, "epoch: required output became optional"},
		{"optional input became required", func(d *Document) {
			s := d.Components.Schemas["HelloParams"]
			s.Required = append(s.Required, "label")
		}, "label: optional input became required"},
		{"method removed", func(d *Document) { d.Methods = d.Methods[1:] }, "hab.hello: removed"},
		{"method added", func(d *Document) {
			m := d.Methods[1]
			m.Name = "hab.new"
			d.Methods = append(d.Methods, m)
		}, ""},
		{"event removed", func(d *Document) { d.Events = d.Events[1:] }, "event project.updated: removed"},
		{"event added", func(d *Document) {
			d.Events = append(d.Events, EventDoc{Type: "later.thing", Data: &Schema{Ref: refPrefix + "Deleted"}})
		}, ""},
		{"output word added", func(d *Document) {
			s := d.Components.Schemas["ErrorKind"]
			s.KnownValues = append(s.KnownValues, "brand_new")
		}, `output gained word "brand_new"`},
		{"input word removed", func(d *Document) {
			s := d.Components.Schemas["ScopeKind"]
			s.KnownValues = without(s.KnownValues, "sessions")
		}, `input word "sessions" removed`},
		{"input word added", func(d *Document) {
			s := d.Components.Schemas["ScopeKind"]
			s.KnownValues = append(s.KnownValues, "hosts")
		}, ""},
		{"frame kind byte changed", func(d *Document) { d.Frames[0].Kind = 99 }, "kind byte changed"},
		{"nested field removed", func(d *Document) {
			delete(d.Components.Schemas["HostInfo"].Properties, "id")
		}, "id: field removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, cur := clone(t), clone(t)
			tc.mutate(cur)
			got := Compare(old, cur)
			if tc.breaks == "" {
				if len(got) != 0 {
					t.Fatalf("want compatible, got %v", got)
				}
				return
			}
			for _, b := range got {
				if strings.Contains(b, tc.breaks) {
					return
				}
			}
			t.Fatalf("want a break containing %q, got %v", tc.breaks, got)
		})
	}
}

func without(list []string, s string) []string {
	var out []string
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
