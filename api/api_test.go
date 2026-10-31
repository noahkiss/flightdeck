package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSchemaEmbedded(t *testing.T) {
	var doc struct {
		Revision int `json:"x-revision"`
	}
	if err := json.Unmarshal(SchemaJSON, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Revision != Revision {
		t.Fatalf("embedded schema revision %d, want %d", doc.Revision, Revision)
	}
}

func TestEventDecodeData(t *testing.T) {
	e := Event{Seq: 1, Type: EventPaneOwner, ID: "p1",
		Data: RawJSON(`{"reason":"typed","cols":80,"rows":24}`)}
	v, err := e.DecodeData()
	if err != nil {
		t.Fatal(err)
	}
	o, ok := v.(PaneOwner)
	if !ok || o.Reason != OwnerReasonTyped || o.Cols != 80 {
		t.Fatalf("got %#v", v)
	}
	e.Type = "later.thing"
	if v, _ := e.DecodeData(); string(v.(RawJSON)) != string(e.Data) {
		t.Fatalf("unknown type should return raw data")
	}
}

func TestResultsCarrySeq(t *testing.T) {
	for _, m := range Methods {
		if m.Name == "rpc.discover" {
			continue
		}
		b, _ := json.Marshal(m.Result)
		var fields map[string]any
		_ = json.Unmarshal(b, &fields)
		if _, ok := fields["seq"]; !ok {
			t.Errorf("%s: result has no seq (spec 3.5)", m.Name)
		}
	}
}

// TestNoNullArrays marshals every zero-value message through NonNil and fails on a null, which a
// Swift client would refuse for a required array.
func TestNoNullArrays(t *testing.T) {
	var values []any
	for _, m := range Methods {
		values = append(values, m.Result)
	}
	for _, n := range Notifications {
		values = append(values, n.Params)
	}
	for _, e := range Events {
		values = append(values, e.Data)
	}
	for _, f := range Frames {
		if f.Payload != nil {
			values = append(values, f.Payload)
		}
	}
	// A nested list: a structure with one project whose session list is nil.
	values = append(values, StructureResult{Structure: Structure{Projects: []Project{{ID: "p"}}}})
	for _, v := range values {
		if _, raw := v.(RawJSON); raw {
			continue
		}
		b, err := json.Marshal(NonNil(v))
		if err != nil {
			t.Fatal(err)
		}
		// Event.data is any JSON value, so only its own null is allowed.
		s := strings.ReplaceAll(string(b), `"data":null`, "")
		if strings.Contains(s, "null") {
			t.Errorf("%T marshals a null: %s", v, b)
		}
	}
}

// --- Cross-version goldens (docs/testing.md, core test 5) ---
//
// api/testdata/rev<N>/ holds the messages a revision N hab sends, as Go emits them: one file per
// method result, notification, event type and JSON data frame payload, plus error.data. Every
// field has a non-zero value, and every object carries extra fields named x_unknown_*, which a
// reader must skip. The Swift tests in swift/HabAPI decode every file, and the apigen tests check
// each one against the schemas, in both directions.
//
// `go test ./api -run TestGoldens -update` rewrites the directory of the current revision only.
// The directories of older revisions are frozen: they are what an old hab sends.

var update = flag.Bool("update", false, "rewrite the golden messages of the current revision")

// goldenDir is the directory of revision r's golden messages, relative to this package.
func goldenDir(r int) string { return filepath.Join("testdata", fmt.Sprintf("rev%d", r)) }

// unknownPrefix starts the name of every extra field a reader must skip.
const unknownPrefix = "x_unknown_"

// golden is one sample message: its file name and the Go value it holds.
type golden struct {
	name  string
	value any
}

// goldenSamples builds a filled sample of every message this revision's server or client sends.
// Each sample numbers its fields from the same start, so adding a message leaves the other
// files alone.
func goldenSamples() []golden {
	var f *filler
	reset := func() *filler { f = &filler{n: 2}; return f }
	var out []golden
	for _, m := range Methods {
		if _, raw := m.Result.(RawJSON); raw {
			continue // rpc.discover returns the schema document
		}
		out = append(out, golden{"result." + m.Name, reset().fill(m.Result)})
	}
	for _, n := range Notifications {
		v := n.Params
		reset()
		if _, ok := v.(Event); ok {
			v = f.event(Events[0])
		} else {
			v = f.fill(v)
		}
		out = append(out, golden{"notification." + n.Name, v})
	}
	for _, e := range Events {
		out = append(out, golden{"event." + string(e.Type), reset().event(e)})
	}
	for _, fr := range Frames {
		if fr.Payload != nil {
			out = append(out, golden{"frame." + fr.Name, reset().fill(fr.Payload)})
		}
	}
	out = append(out, golden{"error.data", reset().fill(ErrorData{})})
	return out
}

// goldenType returns a pointer to a new zero value of the Go type a golden file name holds, and
// whether the file is an event (an Event whose data has the event type's shape).
func goldenType(name string) (ptr any, event *EventDef, ok bool) {
	kind, rest, _ := strings.Cut(name, ".")
	newOf := func(v any) any { return reflect.New(reflect.TypeOf(v)).Interface() }
	switch kind {
	case "result":
		for _, m := range Methods {
			if m.Name == rest {
				return newOf(m.Result), nil, true
			}
		}
	case "notification":
		for _, n := range Notifications {
			if n.Name == rest {
				return newOf(n.Params), nil, true
			}
		}
	case "event":
		for i, e := range Events {
			if string(e.Type) == rest {
				return new(Event), &Events[i], true
			}
		}
	case "frame":
		for _, fr := range Frames {
			if fr.Name == rest && fr.Payload != nil {
				return newOf(fr.Payload), nil, true
			}
		}
	case "error":
		if rest == "data" {
			return new(ErrorData), nil, true
		}
	}
	return nil, nil, false
}

// filler gives every field a non-zero value. Numbers count up from n, so no two fields share
// one; a word set gets its last known word.
type filler struct{ n int }

func (f *filler) next() int { f.n++; return f.n }

func (f *filler) fill(zero any) any {
	v := reflect.New(reflect.TypeOf(zero)).Elem()
	f.value(v, "v")
	return v.Interface()
}

func (f *filler) value(v reflect.Value, name string) {
	t := v.Type()
	if t == reflect.TypeFor[RawJSON]() {
		v.SetBytes([]byte(fmt.Sprintf(`{"%s":%d}`, name, f.next())))
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(t.Elem()))
		f.value(v.Elem(), name)
	case reflect.String:
		if words, ok := Enums[t.Name()]; ok && t.PkgPath() != "" {
			v.SetString(words[len(words)-1])
		} else {
			v.SetString(fmt.Sprintf("%s-%d", name, f.next()))
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.next()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(f.next()))
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte(fmt.Sprintf("%s-%d\x00\xff", name, f.next())))
			return
		}
		s := reflect.MakeSlice(t, 1, 1)
		f.value(s.Index(0), name)
		v.Set(s)
	case reflect.Struct:
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			fname, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			if fname == "" {
				fname = strings.ToLower(sf.Name)
			}
			f.value(v.Field(i), fname)
		}
	default:
		panic(fmt.Sprintf("filler: unsupported type %s", t))
	}
}

// event builds an Event of type e with filled data. Its revision is set, as on a stored object.
func (f *filler) event(e EventDef) Event {
	data, err := json.Marshal(NonNil(f.fill(e.Data)))
	if err != nil {
		panic(err)
	}
	rev := uint64(f.next())
	return Event{Seq: uint64(f.next()), Type: e.Type, ID: fmt.Sprintf("id-%d", f.next()), Revision: &rev, Data: data}
}

// decodeJSON parses b keeping numbers exact.
func decodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// addUnknown adds extra fields to every object in v, recursively. The top level gets a nested
// object as well, to show a reader skips a whole unknown subtree.
func addUnknown(v any, top bool) {
	switch v := v.(type) {
	case map[string]any:
		for _, x := range v {
			addUnknown(x, false)
		}
		v[unknownPrefix+"field"] = "later"
		if top {
			v[unknownPrefix+"object"] = map[string]any{"list": []any{json.Number("1"), "two", nil}, "flag": true}
		}
	case []any:
		for _, x := range v {
			addUnknown(x, false)
		}
	}
}

// rawFields names nested fields of type RawJSON. Like an event's data, they keep extra fields on
// a round trip.
var rawFields = map[string]bool{"input": true} // PermissionRequest.Input

// stripUnknown removes the x_unknown_* fields from every object in v, recursively. Below a key
// in keep (such as an event's data), or a key in rawFields, it leaves v as it is.
func stripUnknown(v any, keep string) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			switch {
			case strings.HasPrefix(k, unknownPrefix):
			case k == keep || rawFields[k]:
				out[k] = x
			default:
				out[k] = stripUnknown(x, "")
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = stripUnknown(x, "")
		}
		return out
	}
	return v
}

// goldenBytes renders one sample as its golden file content.
func goldenBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(NonNil(v))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := decodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	addUnknown(tree, true)
	out, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// TestGoldens keeps the current revision's golden messages fresh, and checks that the current Go
// types read every golden of every revision: decoding succeeds with the extra fields present,
// and for the current revision every known value survives a round trip.
func TestGoldens(t *testing.T) {
	dir := goldenDir(Revision)
	samples := goldenSamples()
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		old, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, p := range old {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range samples {
			if err := os.WriteFile(filepath.Join(dir, s.name+".json"), goldenBytes(t, s.value), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	want := map[string]bool{}
	for _, s := range samples {
		want[s.name+".json"] = true
		got, err := os.ReadFile(filepath.Join(dir, s.name+".json"))
		if err != nil {
			t.Errorf("%v (run go test ./api -run TestGoldens -update)", err)
			continue
		}
		if string(got) != string(goldenBytes(t, s.value)) {
			t.Errorf("%s/%s.json is stale (run go test ./api -run TestGoldens -update)", dir, s.name)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, p := range files {
		if !want[filepath.Base(p)] {
			t.Errorf("%s has no sample (run go test ./api -run TestGoldens -update)", p)
		}
	}

	for r := 1; r <= Revision; r++ {
		files, _ := filepath.Glob(filepath.Join(goldenDir(r), "*.json"))
		if len(files) == 0 && r >= MinRevision {
			t.Errorf("no golden messages for revision %d", r)
		}
		for _, p := range files {
			checkGolden(t, p, r == Revision)
		}
	}
}

// checkGolden decodes one golden file into the current Go type. With roundTrip, re-encoding the
// value must give the file's content less its extra fields.
func checkGolden(t *testing.T, path string, roundTrip bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(filepath.Base(path), ".json")
	ptr, ev, ok := goldenType(name)
	if !ok {
		t.Errorf("%s: no current type reads it", path)
		return
	}
	if err := json.Unmarshal(b, ptr); err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	tree, err := decodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	isEvent := ev != nil || name == "notification.event"
	if ev != nil {
		e := ptr.(*Event)
		if e.Type != ev.Type {
			t.Errorf("%s: event type %q", path, e.Type)
			return
		}
		data, err := e.DecodeData()
		if err != nil {
			t.Errorf("%s: data: %v", path, err)
			return
		}
		if _, raw := data.(RawJSON); raw {
			t.Errorf("%s: data did not decode to its type", path)
			return
		}
		if roundTrip {
			sameJSON(t, path+" data", data, stripUnknown(tree.(map[string]any)["data"], ""))
		}
	}
	if !roundTrip {
		return
	}
	keep := ""
	if isEvent {
		keep = "data" // Event.Data is raw JSON: it keeps the extra fields
	}
	sameJSON(t, path, reflect.ValueOf(ptr).Elem().Interface(), stripUnknown(tree, keep))
}

// sameJSON fails unless v marshals to the same JSON value as want.
func sameJSON(t *testing.T, what string, v, want any) {
	t.Helper()
	b, err := json.Marshal(NonNil(v))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("%s: round trip lost a value\n got %s\nwant %s", what, g, w)
	}
}

// TestGoldensFillEveryField checks the samples leave no field at its zero value, so a golden
// shows every field to the readers.
func TestGoldensFillEveryField(t *testing.T) {
	for _, s := range goldenSamples() {
		b, _ := json.Marshal(NonNil(s.value))
		tree, _ := decodeJSON(b)
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch v := v.(type) {
			case map[string]any:
				for k, x := range v {
					walk(path+"."+k, x)
				}
			case []any:
				if len(v) == 0 {
					t.Errorf("%s%s is empty", s.name, path)
				}
				for _, x := range v {
					walk(path+"[]", x)
				}
			case json.Number:
				if v.String() == "0" {
					t.Errorf("%s%s is zero", s.name, path)
				}
			case string:
				if v == "" {
					t.Errorf("%s%s is empty", s.name, path)
				}
			case bool:
				if !v {
					t.Errorf("%s%s is false", s.name, path)
				}
			case nil:
				t.Errorf("%s%s is null", s.name, path)
			}
		}
		walk("", tree)
	}
}
