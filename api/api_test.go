package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		// Event.data and a setting's value and default are any JSON value, so only their own
		// null is allowed.
		s := string(b)
		for _, k := range []string{"data", "value", "default"} {
			s = strings.ReplaceAll(s, `"`+k+`":null`, "")
		}
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
// Golden files are frozen once written: they are what a hab of that revision sends, and an old
// client must keep reading them. `go test ./api -run TestGoldens -update` only adds files to the
// current revision's directory. It never rewrites or removes a file.
//
// A message can have several files, one per field set. <message>.json is the first field set;
// <message>@<n>.json is field set n, written when optional fields were added to the message
// within the revision (spec 3.3). Only the newest field set must match the current sample, and
// so fill every field. An older field set is what an older hab of the same revision sent: the
// current types must read it and give back exactly its fields, so a field it lacks must be
// optional. A newest file that no longer matches its sample fails the test; -update then adds
// the next field set.

var update = flag.Bool("update", false, "add golden files for new messages and new field sets to the current revision; existing files are never rewritten")

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

// fieldSetSep separates a message name from its field set number in a golden file name.
const fieldSetSep = "@"

// goldenName splits a golden file's base name, without .json, into its message and its field
// set number: result.structure.get is set 1, result.structure.get@2 is set 2.
func goldenName(base string) (message string, set int, ok bool) {
	message, n, found := strings.Cut(base, fieldSetSep)
	if !found {
		return message, 1, true
	}
	if _, err := fmt.Sscanf(n, "%d", &set); err != nil || set < 2 || fmt.Sprint(set) != n {
		return "", 0, false
	}
	return message, set, true
}

// goldenFile is the base name of a message's field set, without .json.
func goldenFile(message string, set int) string {
	if set == 1 {
		return message
	}
	return fmt.Sprintf("%s%s%d", message, fieldSetSep, set)
}

// goldenSets returns the field set numbers of each message in dir, ascending, and the files
// whose names do not parse.
func goldenSets(t *testing.T, dir string) (sets map[string][]int, bad []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sets = map[string][]int{}
	for _, p := range files { // Glob sorts, but @10 sorts before @2: sort numerically below
		msg, n, ok := goldenName(strings.TrimSuffix(filepath.Base(p), ".json"))
		if !ok {
			bad = append(bad, p)
			continue
		}
		sets[msg] = append(sets[msg], n)
	}
	for _, ns := range sets {
		slices.Sort(ns)
	}
	return sets, bad
}

// goldenType returns a pointer to a new zero value of the Go type a golden message name holds,
// and whether the file is an event (an Event whose data has the event type's shape).
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
var rawFields = map[string]bool{"input": true, "value": true, "default": true, "draft": true, "data": true} // PermissionRequest.Input, ChatTool.Input, Setting.Value and Default, LogbookRecord.Draft, RoundEvent.Data

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
		sets, _ := goldenSets(t, dir)
		for _, s := range samples {
			b := goldenBytes(t, s.value)
			next := 1
			if ns := sets[s.name]; len(ns) > 0 {
				newest := ns[len(ns)-1]
				got, err := os.ReadFile(filepath.Join(dir, goldenFile(s.name, newest)+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(got, b) {
					continue
				}
				next = newest + 1 // a new field set; the old files stay as they are
			}
			writeNew(t, filepath.Join(dir, goldenFile(s.name, next)+".json"), b)
		}
	}

	sets, bad := goldenSets(t, dir)
	for _, p := range bad {
		t.Errorf("%s: not a golden name (<message>.json or <message>%s<n>.json, n >= 2)", p, fieldSetSep)
	}
	want := map[string]bool{}
	for _, s := range samples {
		want[s.name] = true
		ns := sets[s.name]
		if len(ns) == 0 {
			t.Errorf("%s/%s.json is missing (a new message: run go test ./api -run TestGoldens -update)", dir, s.name)
			continue
		}
		for i, n := range ns {
			if n != i+1 {
				t.Errorf("%s: field set %d is missing; sets are numbered from 1 without gaps", s.name, i+1)
				break
			}
		}
		newest := goldenFile(s.name, ns[len(ns)-1])
		got, err := os.ReadFile(filepath.Join(dir, newest+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(goldenBytes(t, s.value)) {
			t.Errorf("%s/%s.json no longer matches its message. Added optional fields: run go test ./api -run TestGoldens -update to add the next field set. Any other change needs a new revision (spec 3.3)", dir, newest)
		}
	}
	for msg := range sets {
		if !want[msg] {
			t.Errorf("%s/%s has no sample: a message was removed, which needs a new revision (spec 3.3)", dir, msg)
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

// writeNew creates a golden file that must not exist yet.
func writeNew(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// checkGolden decodes one golden file into the current Go type. With roundTrip, re-encoding the
// value must give the file's content less its extra fields. For an older field set this is the
// compatibility promise: the fields the file lacks are optional, so they stay absent.
func checkGolden(t *testing.T, path string, roundTrip bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name, _, ok := goldenName(strings.TrimSuffix(filepath.Base(path), ".json"))
	if !ok {
		t.Errorf("%s: not a golden name", path)
		return
	}
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
// shows every field to the readers. The samples are the newest field set of each message; an
// older field set holds only the fields that existed when it was written.
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

// TestSwiftGoldensReadEveryResult: the Swift golden test keeps a hand-written list of result
// types. Every result golden must be on it, or `swift test` fails on the Mac; this catches the
// gap on any machine.
func TestSwiftGoldensReadEveryResult(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "swift", "HabAPI", "Tests", "HabAPITests", "GoldenTests.swift"))
	if err != nil {
		t.Skip("no Swift golden test:", err)
	}
	files, err := filepath.Glob(filepath.Join("testdata", "rev*", "result.*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no result goldens: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "result."), ".json")
		name, _, _ = strings.Cut(name, "@")
		var b strings.Builder
		for i, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '_' }) {
			if i > 0 && part != "" {
				part = strings.ToUpper(part[:1]) + part[1:]
			}
			b.WriteString(part)
		}
		if want := "result(HabMethods." + b.String() + ")"; !strings.Contains(string(src), want) {
			t.Errorf("%s: GoldenTests.swift has no %s", f, want)
		}
	}
}
