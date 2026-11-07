package aliases

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var harnesses = []string{"claude", "codex", "opencode"}

func TestParse(t *testing.T) {
	src := `
[catalog]
roots = ["/x"]

[aliases]
gpt = { harness = "codex", model = "gpt-5.5" }
Opus = { harness = "claude", model = "opus" }
codex = { harness = "codex", model = "gpt-5.5-codex" }
plain = { harness = "opencode" }
bad-harness = { harness = "pi" }
flag = { harness = "claude", model = "--dangerously-skip-permissions" }
spaced = { harness = "claude", model = "a b" }
extra = { harness = "claude", effort = "high" }
"no good" = { harness = "claude" }
notable = "codex"
`
	tb, problems, err := Parse([]byte(src), harnesses)
	if err != nil {
		t.Fatal(err)
	}
	want := Table{
		"gpt":   {Harness: "codex", Model: "gpt-5.5"},
		"opus":  {Harness: "claude", Model: "opus"},
		"codex": {Harness: "codex", Model: "gpt-5.5-codex"},
		"plain": {Harness: "opencode"},
	}
	if len(tb) != len(want) {
		t.Fatalf("table %v", tb)
	}
	for k, v := range want {
		if tb[k] != v {
			t.Fatalf("%s: %+v, want %+v", k, tb[k], v)
		}
	}
	joined := strings.Join(problems, "\n")
	for _, w := range []string{"bad-harness: harness \"pi\"", "flag: model", "spaced: model", "extra: unknown key effort", "no good: an alias is", "notable: want"} {
		if !strings.Contains(joined, w) {
			t.Errorf("problems lack %q:\n%s", w, joined)
		}
	}
	if a, k, ok := tb.Lookup("  GPT "); !ok || k != "gpt" || a.Model != "gpt-5.5" {
		t.Fatalf("lookup: %+v %q %v", a, k, ok)
	}
	if got := strings.Join(tb.Names(), ","); got != "codex,gpt,opus,plain" {
		t.Fatalf("names %s", got)
	}
	if _, _, err := Parse([]byte("[aliases\n"), harnesses); err == nil {
		t.Fatal("a broken file parsed")
	}
	if tb, _, err := Parse([]byte("[catalog]\n"), harnesses); err != nil || len(tb) != 0 {
		t.Fatalf("no table: %v %v", tb, err)
	}
}

// TestExample: the example config parses, and its aliases, uncommented, are all good.
func TestExample(t *testing.T) {
	b, err := os.ReadFile("../../docs/examples/hab.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, problems, err := Parse(b, harnesses); err != nil || len(problems) != 0 {
		t.Fatalf("example: %v %v", problems, err)
	}
	_, after, _ := strings.Cut(string(b), "\n[aliases]\n")
	tb, problems, err := Parse([]byte("[aliases]\n"+strings.ReplaceAll(after, "# ", "")), harnesses)
	if err != nil || len(problems) != 0 || len(tb) != 3 {
		t.Fatalf("uncommented: %v %v %v", tb, problems, err)
	}
}

// TestFileReload: the table follows the file without a restart; a file that does not parse
// keeps the last good table and logs once; a removed file is an empty table.
func TestFileReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hab.toml")
	var logs []string
	f := &File{Path: path, Harnesses: harnesses, Logf: func(format string, a ...any) {
		logs = append(logs, format)
	}}
	if len(f.Get()) != 0 {
		t.Fatal("a missing file has aliases")
	}
	write := func(s string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	write("[aliases]\ngpt = { harness = \"codex\", model = \"gpt-5.5\" }\n", base)
	if a, _, ok := f.Get().Lookup("gpt"); !ok || a.Model != "gpt-5.5" {
		t.Fatalf("first read: %+v", f.Get())
	}
	write("[aliases]\ngpt = { harness = \"codex\", model = \"gpt-6\" }\n", base.Add(time.Second))
	if a, _, _ := f.Get().Lookup("gpt"); a.Model != "gpt-6" {
		t.Fatalf("after the edit: %+v", a)
	}
	write("[aliases\n", base.Add(2*time.Second))
	if a, _, _ := f.Get().Lookup("gpt"); a.Model != "gpt-6" {
		t.Fatalf("a broken file dropped the last good table: %+v", a)
	}
	f.Get()
	if n := len(logs); n != 1 {
		t.Fatalf("logs %v", logs)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if len(f.Get()) != 0 {
		t.Fatal("a removed file kept its aliases")
	}
	var nilFile *File
	if len(nilFile.Get()) != 0 {
		t.Fatal("nil file")
	}
}
