package apigen

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api/modules"
)

// TestReleasedVocabularies checks every released block schema against the one make gen writes
// now: a released vocabulary may only gain (docs/specs/modules.md 9). Every version below the
// newest must be released; the newest may still be in work.
func TestReleasedVocabularies(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(root, ReleasedVocabularyDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^blocks-v([1-9][0-9]*)\.schema\.json$`)
	released := map[int]bool{}
	for _, path := range paths {
		m := name.FindStringSubmatch(filepath.Base(path))
		if m == nil {
			t.Errorf("%s: not a released block schema name", path)
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v > modules.Version {
			t.Errorf("%s: vocabulary %d is above the newest, %d", path, v, modules.Version)
			continue
		}
		released[v] = true
		old, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		breaks, err := VocabularyGains(old, BlockSchema(v))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range breaks {
			t.Errorf("released vocabulary %d: %s", v, b)
		}
	}
	for v := 1; v < modules.Version; v++ {
		if !released[v] {
			t.Errorf("vocabulary %d is not in %s; copy its schema there before vocabulary %d starts", v, ReleasedVocabularyDir, v+1)
		}
	}
	if !released[1] {
		t.Error("vocabulary 1 is released and has no copy")
	}
}

func TestVocabularyGains(t *testing.T) {
	base := `{"$defs":{"text":{"description":"A run.","type":"object","required":["type","text"],"additionalProperties":false,
		"properties":{"type":{"const":"text"},"text":{"type":"string","maxLength":10},"style":{"enum":["body","mono"]}}}}}`
	for _, tc := range []struct {
		name, from, to string
		breaks         []string
	}{
		{"same", "", "", nil},
		{"new optional field", `"style":`, `"later":{"type":"boolean"},"style":`, nil},
		{"new word", `["body","mono"]`, `["body","mono","title"]`, nil},
		{"description changed", `"A run."`, `"Some text."`, nil},
		{"field removed", `,"style":{"enum":["body","mono"]}`, ``, []string{"properties/style removed"}},
		{"word removed", `["body","mono"]`, `["body"]`, []string{"enum lost mono"}},
		{"new required field", `["type","text"]`, `["type","text","style"]`, []string{"array changed"}},
		{"limit changed", `"maxLength":10`, `"maxLength":20`, []string{"changed from 10 to 20"}},
		{"rule added", `"maxLength":10`, `"maxLength":10,"minLength":1`, []string{"minLength added"}},
		{"block removed", `"text":{"description"`, `"later":{"description"`, []string{"$defs/text removed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := base
			if tc.from != "" {
				if !strings.Contains(base, tc.from) {
					t.Fatalf("%q not in the base", tc.from)
				}
				cur = strings.Replace(base, tc.from, tc.to, 1)
			}
			got, err := VocabularyGains([]byte(base), []byte(cur))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.breaks) {
				t.Fatalf("breaks %q, want %d like %q", got, len(tc.breaks), tc.breaks)
			}
			for i, want := range tc.breaks {
				if !strings.Contains(got[i], want) {
					t.Errorf("break %q does not say %q", got[i], want)
				}
			}
		})
	}
	if _, err := VocabularyGains([]byte("{"), []byte(base)); err == nil {
		t.Error("a broken released schema accepted")
	}
}
