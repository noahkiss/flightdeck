package modules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The fixture folders and what each checker must say about every file in them:
//
//	valid          the schema and Go accept
//	invalid        the schema and Go refuse
//	beyond-schema  the schema accepts; Go refuses (a rule JSON Schema cannot state)
//	newer          (trees) a block of a later vocabulary: vocabulary 1 refuses it, the typed
//	               decoders keep it as an unknown block
type expect struct{ schema, golang bool }

var folders = map[string]expect{
	"valid":         {true, true},
	"invalid":       {false, false},
	"beyond-schema": {true, false},
	"newer":         {false, false},
}

func fixtures(t *testing.T, kind, folder string) []string {
	t.Helper()
	ext := ".json"
	if kind == "manifest" {
		ext = ".toml"
	}
	files, err := filepath.Glob(filepath.Join("testdata", kind, folder, "*"+ext))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func blockSchemaFile(v int) string { return fmt.Sprintf("blocks-v%d.schema.json", v) }

func TestTreeFixturesGo(t *testing.T) {
	n := 0
	for folder, want := range folders {
		for _, f := range fixtures(t, "blocks", folder) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateTree(data, Version)
			if got := err == nil; got != want.golang {
				t.Errorf("%s: ValidateTree ok=%v, want %v (%v)", f, got, want.golang, err)
			}
			if err != nil {
				var ps Problems
				if !errors.As(err, &ps) || len(ps) == 0 {
					t.Errorf("%s: error is not Problems: %v", f, err)
				}
				t.Logf("%s: %v", f, err)
			}
			n++
		}
	}
	if n < 40 {
		t.Fatalf("only %d tree fixtures", n)
	}
}

func TestTreeFixturesSchema(t *testing.T) {
	schema, err := os.ReadFile(blockSchemaFile(Version))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := newEvaluator(schema)
	if err != nil {
		t.Fatal(err)
	}
	for folder, want := range folders {
		for _, f := range fixtures(t, "blocks", folder) {
			data, _ := os.ReadFile(f)
			v, err := decodeJSON(data)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			got, why := ev.valid(v)
			if got != want.schema {
				t.Errorf("%s: schema ok=%v, want %v %v", f, got, want.schema, why)
			}
		}
	}
}

func TestManifestFixturesGo(t *testing.T) {
	for folder, want := range folders {
		for _, f := range fixtures(t, "manifest", folder) {
			data, _ := os.ReadFile(f)
			m, err := ParseManifest(data)
			if got := err == nil; got != want.golang {
				t.Errorf("%s: ParseManifest ok=%v, want %v (%v)", f, got, want.golang, err)
			}
			if err != nil {
				t.Logf("%s: %v", f, err)
			} else if m == nil {
				t.Errorf("%s: no manifest and no error", f)
			}
		}
	}
	m, err := ParseManifest(mustRead(t, "testdata/manifest/valid/refresh-raised.toml"))
	if err != nil || m.Refresh != MinRefresh {
		t.Errorf("refresh-raised: got %+v, %v; want refresh %d", m, err, MinRefresh)
	}
	m, err = ParseManifest(mustRead(t, "testdata/manifest/valid/full.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "agents-waiting" || m.Blocks != 1 || len(m.Powers) != 3 || len(m.Settings) != 4 || m.Settings[1].Default != int64(5) {
		t.Errorf("full.toml read as %+v", m)
	}
}

func TestManifestFixturesSchema(t *testing.T) {
	ev, err := newEvaluator(mustRead(t, "manifest.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	for folder, want := range folders {
		for _, f := range fixtures(t, "manifest", folder) {
			var raw map[string]any
			if _, err := toml.Decode(string(mustRead(t, f)), &raw); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			js, _ := json.Marshal(raw)
			v, _ := decodeJSON(js)
			got, why := ev.valid(v)
			if got != want.schema {
				t.Errorf("%s: schema ok=%v, want %v %v", f, got, want.schema, why)
			}
		}
	}
}

// TestStockTool runs check-jsonschema, a stock JSON Schema tool, over the same fixtures:
// $GFF_CHECK_JSONSCHEMA (a command line, such as "uvx check-jsonschema"), else check-jsonschema
// on PATH. Skipped when neither is there.
func TestStockTool(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	argv := strings.Fields(os.Getenv("GFF_CHECK_JSONSCHEMA"))
	if len(argv) == 0 {
		p, err := exec.LookPath("check-jsonschema")
		if err != nil {
			t.Skip("no check-jsonschema on PATH and GFF_CHECK_JSONSCHEMA unset")
		}
		argv = []string{p}
	}
	run := func(args ...string) bool {
		cmd := exec.Command(argv[0], append(append(argv[1:], "-q"), args...)...)
		cmd.Env = append(os.Environ(), "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("%v: %v %s", argv, err, out)
		}
		return err == nil
	}
	if !run("--check-metaschema", blockSchemaFile(Version), "manifest.schema.json") {
		t.Fatal("a schema file fails the draft 2020-12 meta-schema")
	}
	for _, kind := range []string{"blocks", "manifest"} {
		schema := blockSchemaFile(Version)
		if kind == "manifest" {
			schema = "manifest.schema.json"
		}
		for folder, want := range folders {
			for _, f := range fixtures(t, kind, folder) {
				if got := run("--schemafile", schema, f); got != want.schema {
					t.Errorf("%s: check-jsonschema ok=%v, want %v", f, got, want.schema)
				}
			}
		}
	}
}

func TestTreeLimits(t *testing.T) {
	texts := func(n int) []byte {
		var b bytes.Buffer
		b.WriteString(`{"type":"stack","children":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"type":"list","children":[{"type":"text","text":"x"}]}`)
		}
		b.WriteString(`]}`)
		return b.Bytes()
	}
	// 1 + 2*99 blocks: under the limit. 1 + 2*250: over it (stack holds at most 100, so nest).
	if err := ValidateTree(texts(99), Version); err != nil {
		t.Errorf("199 blocks refused: %v", err)
	}
	var many bytes.Buffer
	many.WriteString(`{"type":"list","children":[`)
	for i := range 3 {
		if i > 0 {
			many.WriteByte(',')
		}
		many.Write(texts(90))
	}
	many.WriteString(`]}`)
	err := ValidateTree(many.Bytes(), Version)
	if err == nil || !strings.Contains(err.Error(), "blocks; the limit is 500") {
		t.Errorf("544 blocks: %v", err)
	}
	long := `{"type":"log","lines":[` + strings.Repeat(`"`+strings.Repeat("x", 900)+`",`, 80) + `"end"]}`
	err = ValidateTree([]byte(long), Version)
	if err == nil || !strings.Contains(err.Error(), "bytes; the limit is") {
		t.Errorf("a %d byte tree: %v", len(long), err)
	}
	// Over the limit, the tree is refused before it is parsed: broken JSON past the limit
	// reports the size alone.
	huge := append([]byte(`{"type":"text","text":"`), bytes.Repeat([]byte("x"), MaxTreeBytes)...)
	var ps Problems
	if err := ValidateTree(huge, Version); !errors.As(err, &ps) || len(ps) != 1 || !strings.Contains(ps[0].Message, "bytes; the limit is") {
		t.Errorf("an unparsed tree over the limit: %v", err)
	}
	for _, v := range []int{0, Version + 1} {
		if err := ValidateTree([]byte(`{"type":"text","text":"x"}`), v); err == nil {
			t.Errorf("vocabulary %d accepted", v)
		}
	}
	for _, bad := range []string{``, `{"type":"text","text":"x"} {}`, `{"type":"text","text":"x"`} {
		if err := ValidateTree([]byte(bad), Version); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestTypedRoundTrip decodes every fixture the typed Go form must read (valid, beyond-schema,
// newer) and encodes it again: the JSON value must not change.
func TestTypedRoundTrip(t *testing.T) {
	for _, folder := range []string{"valid", "beyond-schema", "newer"} {
		for _, f := range fixtures(t, "blocks", folder) {
			data := mustRead(t, f)
			var b Block
			if err := json.Unmarshal(data, &b); err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			out, err := json.Marshal(b)
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			var want, got any
			_ = json.Unmarshal(data, &want)
			_ = json.Unmarshal(out, &got)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s: round trip changed it:\n%s", f, out)
			}
		}
	}
	var b Block
	if err := json.Unmarshal(mustRead(t, "testdata/blocks/newer/terminal.json"), &b); err != nil {
		t.Fatal(err)
	}
	term := b.Stack.Children[1]
	if term.Type != "terminal" || term.Raw == nil || term.Fallback != "Open the pane to see it" {
		t.Errorf("unknown block decoded as %+v", term)
	}
}

// TestFixturesCoverVocabulary fails when a block type, a field, a word or a client action
// appears in no valid fixture, so a new entry in the table needs a fixture.
func TestFixturesCoverVocabulary(t *testing.T) {
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if typ, ok := x["type"].(string); ok {
				seen["type "+typ] = true
				for k, fv := range x {
					seen[typ+"."+k] = true
					if s, ok := fv.(string); ok {
						seen[typ+"."+k+"="+s] = true
					}
				}
			}
			if name, ok := x["name"].(string); ok {
				seen["action "+name] = true
			}
			for _, fv := range x {
				walk(fv)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, f := range fixtures(t, "blocks", "valid") {
		v, _ := decodeJSON(mustRead(t, f))
		walk(v)
	}
	var missing []string
	need := func(k string) {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	for _, b := range Blocks {
		need("type " + b.Type)
		for _, f := range b.Fields {
			need(b.Type + "." + f.Name)
			for _, w := range f.Words {
				need(b.Type + "." + f.Name + "=" + w)
			}
		}
	}
	for _, f := range Common {
		found := false
		for _, b := range Blocks {
			found = found || seen[b.Type+"."+f.Name]
		}
		if !found {
			missing = append(missing, "common field "+f.Name)
		}
	}
	for _, ca := range ClientActions {
		need("action " + ca.Name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("no valid fixture uses: %s", strings.Join(missing, ", "))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
