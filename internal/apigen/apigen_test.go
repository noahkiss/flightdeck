package apigen

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
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

// goldenRoot holds the golden messages of each revision, written by the api package tests.
const goldenRoot = "api/testdata"

// TestGoldensBothDirections is core test 5 in docs/testing.md, at the schema level:
//   - A new client reads an old hab: every golden of every released revision is valid under the
//     current schema.
//   - An old client reads a new hab: every golden the current build emits is valid under the
//     released schema of the same revision. (Across a revision step, the server's translator
//     carries an old client instead; its own tests cover that.)
//
// Valid means what a strict decoder such as Swift's needs: each required field is present and
// not null, and each value has its schema's type. Extra fields are allowed and skipped.
func TestGoldensBothDirections(t *testing.T) {
	cur, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, ReleasedDir, "*.json"))
	released := map[int]*Document{}
	for _, p := range paths {
		d := load(t, p)
		released[d.Revision] = d
	}
	if len(released) == 0 {
		t.Fatal("no released schema")
	}
	for r, old := range released {
		files := goldens(t, r)
		if len(files) == 0 {
			if r >= cur.MinRevision {
				t.Errorf("released revision %d has no golden messages in %s", r, goldenRoot)
			}
			continue
		}
		for _, p := range files {
			for _, e := range checkGolden(cur, p, true) {
				t.Errorf("new client, revision %d hab: %s", r, e)
			}
		}
		if r == cur.Revision {
			for _, p := range goldens(t, cur.Revision) {
				for _, e := range checkGolden(old, p, false) {
					t.Errorf("revision %d client, new hab: %s", r, e)
				}
			}
		}
	}
	if _, ok := released[cur.Revision]; !ok && len(goldens(t, cur.Revision)) == 0 {
		t.Errorf("revision %d has no golden messages: run go test ./api -run TestGoldens -update", cur.Revision)
	}
}

// TestValidateGolden checks the validator rejects what a strict decoder rejects.
func TestValidateGolden(t *testing.T) {
	d, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	pane := &Schema{Ref: refPrefix + "Pane"}
	ok := `{"id":"p","revision":1,"holder_build":"b","cols":80,"rows":24,"offset":5,"later":{"x":1}}`
	for _, tc := range []struct {
		json string
		bad  string
	}{
		{ok, ""},
		{`{"id":"p","revision":1,"holder_build":"b","cols":80,"rows":24}`, ".offset: required field missing"},
		{`{"id":"p","revision":1,"holder_build":"b","cols":"80","rows":24,"offset":5}`, ".cols: want integer"},
		{`{"id":"p","revision":-1,"holder_build":"b","cols":80,"rows":24,"offset":5}`, ".revision: want integer/uint64"},
		{`{"id":"p","revision":1,"holder_build":"b","cols":80,"rows":24,"offset":5,"owner":null}`, ""},
		{`{"id":null,"revision":1,"holder_build":"b","cols":80,"rows":24,"offset":5}`, ".id: required field is null"},
		{`[]`, ": want object"},
	} {
		v, err := decodeGolden([]byte(tc.json))
		if err != nil {
			t.Fatal(err)
		}
		var errs []string
		validate(d, pane, v, "", &errs)
		if tc.bad == "" && len(errs) != 0 || tc.bad != "" && (len(errs) == 0 || !strings.Contains(errs[0], tc.bad)) {
			t.Errorf("%s: got %v, want %q", tc.json, errs, tc.bad)
		}
	}
	var errs []string
	validate(d, &Schema{Type: "string", ContentEncoding: "base64"}, "not base64!", "", &errs)
	if len(errs) == 0 {
		t.Error("bad base64 passed")
	}
}

func goldens(t *testing.T, revision int) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, goldenRoot, fmt.Sprintf("rev%d", revision), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func decodeGolden(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// checkGolden validates one golden file under d. The file name says which message it is:
// result.<method>, notification.<name>, event.<type>, frame.<name> or error.data, with an
// optional @<n> for a later field set of the message (docs/testing.md). With
// mustKnow, a message d does not describe is an error; without it, the message is skipped, as
// an old client skips an event type or never calls a method it does not know.
func checkGolden(d *Document, path string, mustKnow bool) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{err.Error()}
	}
	v, err := decodeGolden(b)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", path, err)}
	}
	name, _, _ := strings.Cut(strings.TrimSuffix(filepath.Base(path), ".json"), "@")
	kind, rest, _ := strings.Cut(name, ".")
	var s, data *Schema
	switch kind {
	case "result":
		for _, m := range d.Methods {
			if m.Name == rest {
				s = m.Result.Schema
			}
		}
	case "notification":
		for _, n := range d.Notifications {
			if n.Name == rest {
				s = n.Params
			}
		}
	case "event":
		for _, e := range d.Events {
			if e.Type == rest {
				data = e.Data
			}
		}
		if data != nil {
			for _, n := range d.Notifications {
				if n.Name == "event" {
					s = n.Params
				}
			}
		}
	case "frame":
		for _, f := range d.Frames {
			if f.Name == rest {
				s = f.Payload
			}
		}
	case "error":
		if rest == "data" {
			s = d.ErrorData
		}
	}
	if s == nil {
		if mustKnow {
			return []string{fmt.Sprintf("%s: revision %d describes no such message", path, d.Revision)}
		}
		return nil
	}
	var errs []string
	validate(d, s, v, name, &errs)
	if data != nil {
		if m, ok := v.(map[string]any); ok {
			if m["type"] != rest {
				errs = append(errs, fmt.Sprintf("%s.type: %v, want %q", name, m["type"], rest))
			}
			validate(d, data, m["data"], name+".data", &errs)
		}
	}
	return errs
}

// validate checks v against s the way a strict decoder reads it. Unknown fields are skipped.
func validate(d *Document, s *Schema, v any, path string, errs *[]string) {
	s, name := d.Resolve(s)
	if s == nil {
		*errs = append(*errs, fmt.Sprintf("%s: dangling $ref %q", path, name))
		return
	}
	bad := func() { *errs = append(*errs, fmt.Sprintf("%s: want %s, got %T %v", path, kind(s), v, v)) }
	switch s.Type {
	case "":
		// Any JSON value.
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			bad()
			return
		}
		for _, r := range s.Required {
			x, present := m[r]
			if !present {
				*errs = append(*errs, fmt.Sprintf("%s.%s: required field missing", path, r))
			} else if x == nil {
				*errs = append(*errs, fmt.Sprintf("%s.%s: required field is null", path, r))
			}
		}
		for _, k := range sortedKeys(m) {
			if p, known := s.Properties[k]; known && m[k] != nil {
				validate(d, p, m[k], path+"."+k, errs)
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			bad()
			return
		}
		for i, x := range a {
			validate(d, s.Items, x, fmt.Sprintf("%s[%d]", path, i), errs)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			bad()
			return
		}
		if s.ContentEncoding == "base64" {
			if _, err := base64.StdEncoding.DecodeString(str); err != nil {
				bad()
			}
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			bad()
			return
		}
		var err error
		if s.Format == "uint64" {
			_, err = strconv.ParseUint(n.String(), 10, 64)
		} else {
			_, err = strconv.ParseInt(n.String(), 10, 64)
		}
		if err != nil {
			bad()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			bad()
		}
	default:
		*errs = append(*errs, fmt.Sprintf("%s: schema type %q unknown to the validator", path, s.Type))
	}
}

// TestCheckSwiftNames refuses a field that Swift cannot name.
func TestCheckSwiftNames(t *testing.T) {
	d := &Document{Components: Components{Schemas: map[string]*Schema{
		"Entry": {Properties: map[string]*Schema{"label": {}, "self": {}}},
	}}}
	if err := checkSwiftNames(d); err == nil || !strings.Contains(err.Error(), "Entry.self") {
		t.Fatalf("checkSwiftNames = %v, want a refusal naming Entry.self", err)
	}
	d.Components.Schemas["Entry"].Properties = map[string]*Schema{"own": {}, "default": {}}
	if err := checkSwiftNames(d); err != nil {
		t.Fatalf("checkSwiftNames(own, default) = %v", err)
	}
}
