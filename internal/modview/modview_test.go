package modview

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

var update = flag.Bool("update", false, "rewrite the golden renderings in testdata/golden")

// goldenNow is the clock of every golden rendering: half an hour before the fixtures' ends_at.
var goldenNow = time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)

const fixtures = "../../api/modules/testdata/blocks"

// golden is a rendering as a golden file holds it: each line between bars, so trailing spaces and
// the width show, then the targets with their keys and actions.
func golden(v *View) string {
	var b strings.Builder
	for _, l := range v.Lines {
		fmt.Fprintf(&b, "|%s|\n", l.Text())
	}
	if len(v.Targets) > 0 {
		b.WriteString("targets:\n")
	}
	kinds := []string{"button", "toggle", "field", "row", "section"}
	for _, t := range v.Targets {
		key := "-"
		if t.Key != 0 {
			key = string(t.Key)
		}
		a, _ := json.Marshal(t.Action)
		if t.Kind == KindSection {
			a = []byte("fold")
		}
		extra := ""
		if t.Confirm != "" {
			extra = " confirm " + t.Confirm
		}
		fmt.Fprintf(&b, "  %s %s %s %s%s\n", kinds[t.Kind], t.Ref, key, a, extra)
	}
	return b.String()
}

// TestGoldens draws every valid fixture, and the newer one, at 80 and 40 columns, and compares
// the text with testdata/golden. -update rewrites the files; read the diff before committing.
func TestGoldens(t *testing.T) {
	var files []string
	for _, dir := range []string{"valid", "newer"} {
		m, err := filepath.Glob(filepath.Join(fixtures, dir, "*.json"))
		if err != nil || len(m) == 0 {
			t.Fatalf("no fixtures in %s: %v", dir, err)
		}
		files = append(files, m...)
	}
	for _, f := range files {
		tree, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []int{80, 40} {
			name := fmt.Sprintf("%s-%s-%d.txt", filepath.Base(filepath.Dir(f)), strings.TrimSuffix(filepath.Base(f), ".json"), w)
			t.Run(name, func(t *testing.T) {
				v := Render(tree, Options{Width: w, Now: goldenNow})
				if v.Err != "" {
					t.Fatalf("valid fixture drew an error: %s", v.Err)
				}
				for i, l := range v.Lines {
					if l.Width() > w {
						t.Errorf("line %d is %d columns, over %d: %q", i, l.Width(), w, l.Text())
					}
				}
				got := golden(v)
				path := filepath.Join("testdata", "golden", name)
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run with -update to write it)", err)
				}
				if got != string(want) {
					t.Errorf("%s differs from its golden file:\n--- got\n%s--- want\n%s", name, got, want)
				}
			})
		}
	}
}

// TestInvalidTrees draws every invalid and beyond-schema fixture at several widths: none panics,
// every line fits, and a tree that is not a block tree draws as exactly one error line.
func TestInvalidTrees(t *testing.T) {
	var files []string
	for _, dir := range []string{"invalid", "beyond-schema"} {
		m, _ := filepath.Glob(filepath.Join(fixtures, dir, "*.json"))
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	broken := map[string]bool{
		"root-not-object.json": true, "type-not-string.json": true, "children-not-array.json": true,
		"toggle-value-string.json": true, "log-line-not-string.json": true, "gap-not-whole.json": true,
		"action-args-not-object.json": true, "no-type.json": true, "too-deep.json": true,
		"duplicate-id.json": true,
	}
	for _, f := range files {
		tree, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []int{80, 40, 10, 1} {
			v := Render(tree, Options{Width: w, Now: goldenNow})
			for i, l := range v.Lines {
				if l.Width() > w {
					t.Errorf("%s at %d: line %d is %d columns", f, w, i, l.Width())
				}
			}
			if broken[filepath.Base(f)] {
				if v.Err == "" || len(v.Lines) != 1 || !strings.HasPrefix(v.Lines[0].Text(), "module error") && w > 14 {
					t.Errorf("%s at %d: want one error line, got %q (err %q)", f, w, golden(v), v.Err)
				}
			} else if v.Err != "" {
				t.Errorf("%s at %d: a lenient client draws it; got error %q", f, w, v.Err)
			}
		}
	}
	for _, tree := range []string{``, `{`, `null`, `[]`, `"text"`, `{"type":"stack","children":[null]}`} {
		v := Render([]byte(tree), Options{Width: 40})
		if v.Err == "" || len(v.Lines) != 1 {
			t.Errorf("%q: want one error line, got %q", tree, golden(v))
		}
	}
	huge := append([]byte(`{"type":"text","text":"`), bytes.Repeat([]byte("x"), modules.MaxTreeBytes)...)
	if v := Render(append(huge, `"}`...), Options{Width: 40}); v.Err == "" {
		t.Error("a tree over the size limit drew")
	}
	var many strings.Builder
	many.WriteString(`{"type":"list","children":[`)
	for i := range modules.MaxBlocks + 1 {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"type":"text","text":"x"}`)
	}
	many.WriteString(`]}`)
	if v := Render([]byte(many.String()), Options{Width: 40}); v.Err == "" || !strings.Contains(v.Err, "blocks") {
		t.Errorf("a tree over the block limit: err %q", v.Err)
	}
}

func render(t *testing.T, tree string, opt Options) *View {
	t.Helper()
	if opt.Width == 0 {
		opt.Width = 40
	}
	if opt.Now.IsZero() {
		opt.Now = goldenNow
	}
	v := Render([]byte(tree), opt)
	if v.Err != "" {
		t.Fatalf("Render: %s", v.Err)
	}
	return v
}

func texts(v *View) []string {
	var out []string
	for _, l := range v.Lines {
		out = append(out, l.Text())
	}
	return out
}

func TestCutNeverWraps(t *testing.T) {
	tree := `{"type":"stack","children":[
		{"type":"text","text":"a long line of text that will not fit in the width"},
		{"type":"row","title":"title that is long","body":"and a body","trailing":"12m"},
		{"type":"text","style":"mono","text":"one\ntwo  "}]}`
	v := render(t, tree, Options{Width: 20})
	want := []string{"a long line of text…", "title that is l… 12m", "one", "two  "}
	if got := texts(v); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q, want %q", got, want)
	}
	// A width narrower than the trailing value keeps the row on one line.
	v = render(t, `{"type":"row","title":"ab","trailing":"a long trailing"}`, Options{Width: 6})
	if len(v.Lines) != 1 || v.Lines[0].Text() != "ab a …" {
		t.Errorf("narrow row: %q", texts(v))
	}
}

func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	v := render(t, `{"type":"stack","children":[
		{"type":"text","text":"\u001b[2Jgone\ttab"},
		{"type":"badge","text":"\u009b31m"},
		{"type":"log","lines":["\u0007bell\u007f"]}]}`, Options{})
	for _, l := range v.Lines {
		for _, c := range l.Text() {
			if c < 0x20 || c == 0x7f || c >= 0x80 && c < 0xa0 {
				t.Errorf("control character %U in %q", c, l.Text())
			}
		}
	}
	if got := v.Lines[0].Text(); got != "�[2Jgone tab" {
		t.Errorf("text: %q", got)
	}
}

func TestDrawnWords(t *testing.T) {
	// Unknown words draw as the field's default, through the generated accessors.
	v := render(t, `{"type":"stack","axis":"diagonal","children":[
		{"type":"text","text":"x","style":"shout"},
		{"type":"badge","text":"b","tint":"ultraviolet"},
		{"type":"button","title":"Go","role":"explosive","action":{"name":"go"}}]}`, Options{})
	if got := texts(v); len(got) != 3 {
		t.Fatalf("an unknown axis must draw vertical: %q", got)
	}
	if st := v.Lines[0].Spans[0].Style; st != (Style{}) {
		t.Errorf("unknown style drew %+v, want body", st)
	}
	if st := v.Lines[1].Spans[0].Style; st.Tint != "" {
		t.Errorf("unknown tint drew %q", st.Tint)
	}
	if v.Targets[0].Destructive {
		t.Error("an unknown role drew destructive")
	}
}

func TestStyles(t *testing.T) {
	v := render(t, `{"type":"stack","children":[
		{"type":"text","text":"t","style":"title"},
		{"type":"text","text":"c","style":"caption"},
		{"type":"badge","text":"b","tint":"#3A7BD5"},
		{"type":"metric","label":"L","value":"9","tint":"warning"},
		{"type":"row","symbol":"circle.fill","title":"r","tint":"blocked"},
		{"type":"row","title":"r2","tint":"done"},
		{"type":"button","title":"Clear","role":"destructive","action":{"name":"clear"}}]}`, Options{})
	check := func(line, span int, want Style) {
		t.Helper()
		if got := v.Lines[line].Spans[span].Style; got != want {
			t.Errorf("line %d span %d: %+v, want %+v", line, span, got, want)
		}
	}
	check(0, 0, Style{Bold: true})
	check(1, 0, Style{Dim: true})
	check(2, 0, Style{Tint: "#3A7BD5"})
	check(3, 1, Style{Bold: true, Tint: "warning"})
	check(4, 0, Style{Tint: "blocked"})
	check(5, 0, Style{Tint: "done"})
	check(6, 0, Style{Key: true, Bold: true})
	check(6, 1, Style{Tint: "error"})
}

func TestKeys(t *testing.T) {
	tree := `{"type":"stack","axis":"horizontal","children":[
		{"type":"button","id":"a","title":"Run","action":{"name":"a"}},
		{"type":"button","id":"b","title":"Rerun","action":{"name":"b"}},
		{"type":"button","id":"c","title":"Kill","action":{"name":"c"}},
		{"type":"button","id":"d","title":"!!","action":{"name":"d"}}]}`
	v := render(t, tree, Options{Reserved: "jkR"})
	keys := ""
	for _, tg := range v.Targets {
		keys += string(tg.Key)
	}
	// Hints are lowercase, and a reserved key blocks only its own case: R is reserved, r is free. "Kill" skips the reserved k; "!!" has
	// no letter and takes the first digit.
	if keys != "rei1" {
		t.Errorf("keys %q, want rei1", keys)
	}
	if got := v.Lines[0].Text(); got != "r Run  e Rerun  i Kill  1 !!" {
		t.Errorf("hints: %q", got)
	}
	if i := v.TargetByKey('e'); i != 1 {
		t.Errorf("TargetByKey(e) = %d", i)
	}
	// The same tree draws the same keys every time.
	if again := render(t, tree, Options{Reserved: "jkR"}); golden(again) != golden(v) {
		t.Error("keys changed between renders")
	}
}

func TestTargetsSend(t *testing.T) {
	v := render(t, `{"type":"stack","children":[
		{"type":"toggle","id":"w","title":"Watch","value":true,"action":{"name":"set_watch","args":{"k":1}}},
		{"type":"field","id":"n","action":{"name":"notes.add"}},
		{"type":"row","title":"r","action":{"name":"client.open_session","args":{"id":"s-1"}}}]}`, Options{})
	a, _ := v.Targets[0].Send("")
	if a.Name != "set_watch" || a.Args["value"] != false || a.Args["k"] != float64(1) {
		t.Errorf("toggle sends %+v", a)
	}
	if _, ok := v.Targets[0].Action.Args["value"]; ok {
		t.Error("Send changed the target's own args")
	}
	a, _ = v.Targets[1].Send("hello")
	if a.Name != "notes.add" || a.Args["text"] != "hello" {
		t.Errorf("field sends %+v", a)
	}
	a, _ = v.Targets[2].Send("")
	if !IsClient(a) || v.Targets[2].Ref != "#3:r:0" {
		t.Errorf("row sends %+v ref %q", a, v.Targets[2].Ref)
	}
}

func TestFocusAndFold(t *testing.T) {
	tree := `{"type":"section","id":"s","title":"Group","children":[
		{"type":"button","id":"go","title":"Go","action":{"name":"go"}}]}`
	v := render(t, tree, Options{Focus: "go"})
	if len(v.Lines) != 2 || !v.Lines[1].Spans[len(v.Lines[1].Spans)-1].Style.Focus {
		t.Fatalf("focused button not marked: %q", golden(v))
	}
	if v.Lines[0].Spans[0].Style.Focus {
		t.Error("the section head is marked focused")
	}
	v = render(t, tree, Options{Folded: map[string]bool{"s": true}})
	if got := texts(v); len(got) != 1 || got[0] != "▸ Group (1)" {
		t.Errorf("folded: %q", got)
	}
	if len(v.Targets) != 1 || v.TargetByKey('g') >= 0 {
		t.Errorf("a folded section's button takes a key: %q", golden(v))
	}
}

func TestStale(t *testing.T) {
	tree := `{"type":"stack","children":[{"type":"badge","text":"old","stale_after":60},{"type":"badge","text":"fresh"}]}`
	v := render(t, tree, Options{Age: 61 * time.Second})
	if !v.Lines[0].Spans[0].Style.Dim || v.Lines[1].Spans[0].Style.Dim {
		t.Errorf("stale_after: %+v", v.Lines)
	}
	if v := render(t, tree, Options{Age: 59 * time.Second}); v.Lines[0].Spans[0].Style.Dim {
		t.Error("dimmed before stale_after")
	}
}

func TestLog(t *testing.T) {
	var lines []string
	for i := range 12 {
		lines = append(lines, fmt.Sprintf("%q", fmt.Sprint("line ", i)))
	}
	list := strings.Join(lines, ",")
	v := render(t, `{"type":"log","follow":true,"lines":[`+list+`]}`, Options{LogLines: 4})
	if got := strings.Join(texts(v), "|"); got != "… 9 earlier lines|line 9|line 10|line 11" {
		t.Errorf("follow: %q", got)
	}
	v = render(t, `{"type":"log","lines":[`+list+`]}`, Options{LogLines: 4})
	if got := strings.Join(texts(v), "|"); got != "line 0|line 1|line 2|… 9 more lines" {
		t.Errorf("head: %q", got)
	}
}

func TestCountdown(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "ended", 0: "ended", 1500 * time.Millisecond: "2s left", 90 * time.Second: "1m left",
		2*time.Hour + 5*time.Minute: "2h 5m left", 3 * time.Hour: "3h left", 72 * time.Hour: "3d left",
	} {
		if got := Countdown(d); got != want {
			t.Errorf("Countdown(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestOneColumnPerRune: wide, zero-width, combining and format runes become U+FFFD, so a drawn
// row never takes more columns than Width counts and never shifts the cells after it.
func TestOneColumnPerRune(t *testing.T) {
	v := render(t, `{"type":"stack","children":[
		{"type":"row","title":"漢字テスト","body":"e\u0301 a\u200db","trailing":"9m"},
		{"type":"text","text":"🙂👍🏽❤\ufe0f🇺🇸x"},
		{"type":"text","text":"\u202eevil\u202c \u2066iso\u2069 \u200e\u200f\u061c\u2028\u2029\ufeff\u200b\u200c\u00ad\ue000"},
		{"type":"badge","text":"ｆｕｌｌ"}]}`, Options{Width: 20})
	// A heart keeps its one column; its emoji selector does not. The third line is cut at 20.
	want := []string{"����� · e� a�b    9m", "���❤���x", "�evil� �iso� ������…", "[����]"}
	got := texts(v)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	for _, l := range v.Lines {
		for _, c := range l.Text() {
			if !oneColumn(c) && c != '…' {
				t.Errorf("rune %U is not one column in %q", c, l.Text())
			}
		}
		if l.Width() > 20 {
			t.Errorf("line %q is %d columns", l.Text(), l.Width())
		}
	}
	// Plain text from many scripts that draws one column per rune stays as it is.
	for _, s := range []string{"café naïve", "Ελληνικά", "кириллица", "→ ✓ ● …", "a\u00a0b"} {
		if clean(s) != s {
			t.Errorf("clean(%q) = %q", s, clean(s))
		}
	}
}

// TestTargetTextIsClean: a target's title and confirm, which the client shows in prompts, are
// cleaned as drawn text is.
func TestTargetTextIsClean(t *testing.T) {
	v := render(t, `{"type":"stack","children":[
		{"type":"button","title":"Go\u001b]0;x\u0007","confirm":"Sure\u009b2J\u202e?","action":{"name":"go"}},
		{"type":"field","placeholder":"\u001b[31mname","action":{"name":"set"}}]}`, Options{})
	if got := v.Targets[0].Title; got != "Go�]0;x�" {
		t.Errorf("title %q", got)
	}
	if got := v.Targets[0].Confirm; got != "Sure�2J�?" {
		t.Errorf("confirm %q", got)
	}
	if got := v.Targets[1].Title; got != "�[31mname" {
		t.Errorf("placeholder %q", got)
	}
}

// TestRefsByTitle: a target with no id keeps its ref when a target of another title is added
// above it; two of one title are told apart by their order.
func TestRefsByTitle(t *testing.T) {
	btn := func(title string) string {
		return `{"type":"button","title":"` + title + `","action":{"name":"` + title + `"}}`
	}
	a := render(t, `{"type":"stack","children":[`+btn("Stop")+`,`+btn("Stop")+`]}`, Options{})
	b := render(t, `{"type":"stack","children":[`+btn("New")+`,`+btn("Stop")+`,`+btn("Stop")+`]}`, Options{})
	if a.Targets[1].Ref != b.Targets[2].Ref || a.Targets[0].Ref == a.Targets[1].Ref {
		t.Errorf("refs %q and %q", a.Targets[1].Ref, b.Targets[2].Ref)
	}
	if i := b.TargetByRef(a.Targets[0].Ref); i != 1 {
		t.Errorf("first Stop is target %d", i)
	}
}

func TestNoHints(t *testing.T) {
	tree := `{"type":"stack","axis":"horizontal","children":[
		{"type":"button","title":"Run","action":{"name":"a"}},
		{"type":"button","title":"Kill","action":{"name":"b"}}]}`
	v := render(t, tree, Options{NoHints: true})
	if got := v.Lines[0].Text(); got != "› Run  › Kill" {
		t.Errorf("line %q", got)
	}
	if v.TargetByKey('r') != 0 || v.TargetByKey('k') != 1 {
		t.Error("NoHints dropped the keys")
	}
}

func TestDuplicateIDs(t *testing.T) {
	for _, tree := range []string{
		`{"type":"stack","children":[{"type":"button","id":"x","title":"A","action":{"name":"a"}},{"type":"button","id":"x","title":"B","action":{"name":"b"}}]}`,
		`{"type":"stack","id":"x","children":[{"type":"text","id":"x","text":"t"}]}`,
		`{"type":"stack","children":[{"type":"button","id":"#0:A:0","title":"A","action":{"name":"a"}}]}`,
	} {
		v := Render([]byte(tree), Options{Width: 60})
		if v.Err != "two blocks have the same id" || len(v.Lines) != 1 {
			t.Errorf("%s: err %q lines %q", tree, v.Err, texts(v))
		}
	}
}
