package apigen

import (
	"encoding/json"
	"github.com/noahkiss/flightdeck/api"
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

// TestCompatWithReleased checks the schema against api/released (spec 3.3).
func TestCompatWithReleased(t *testing.T) {
	cur, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, ReleasedDir, "*.json"))
	var released []*Document
	for _, path := range paths {
		released = append(released, load(t, path))
	}
	hasTranslator := func(r int) bool { return api.Translators[r] != nil }
	for _, e := range CheckReleased(cur, released, hasTranslator) {
		t.Error(e)
	}
}

func TestCheckReleased(t *testing.T) {
	none := func(int) bool { return false }
	some := func(int) bool { return true }
	rev := func(r int) *Document { d := clone(t); d.Revision = r; return d }
	cases := []struct {
		name     string
		cur      *Document
		released []*Document
		trans    func(int) bool
		ok       bool
	}{
		{"same revision", rev(1), []*Document{rev(1)}, none, true},
		{"nothing released", rev(1), nil, none, false},
		{"bump without translator", rev(2), []*Document{rev(1)}, none, false},
		{"bump with translator", rev(2), []*Document{rev(1)}, some, true},
		{"skipped revision", rev(3), []*Document{rev(1)}, some, false},
		{"older than released", rev(1), []*Document{rev(1), rev(2)}, some, false},
		{"compares the highest released", rev(2), []*Document{rev(2), rev(1)}, some, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := CheckReleased(tc.cur, tc.released, tc.trans)
			if tc.ok != (len(errs) == 0) {
				t.Fatalf("ok = %v, errors %v", tc.ok, errs)
			}
		})
	}
	// A breaking change in the same revision fails.
	cur := clone(t)
	delete(cur.Components.Schemas["Welcome"].Properties, "epoch")
	if errs := CheckReleased(cur, []*Document{clone(t)}, none); len(errs) == 0 {
		t.Fatal("a removed field passed")
	}
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
		}, ""},
		{"output word set became a plain string", func(d *Document) {
			d.Components.Schemas["ErrorKind"].KnownValues = nil
		}, ""},
		{"input string became a closed word set", func(d *Document) {
			d.Components.Schemas["InputParams"].Properties["text"].KnownValues = []string{"a"}
		}, "input string became a closed word set"},
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
