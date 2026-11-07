package mcpreg

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// Tests for the review fixes. Temporary folders only.

func writeRecordFile(t *testing.T, e Env, h Harness, target string, servers map[string]string) {
	t.Helper()
	data, err := json.Marshal(record{Harness: h, Target: target, Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.RecordPath(h), string(data))
}

// A TOML nan or inf must not switch the nothing-else-changed check off.
func TestCheckEditNaN(t *testing.T) {
	old, err := decodeTOML("x = nan\ny = inf\n[a]\nb = 1\n")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := decodeTOML("x = nan\ny = inf\n[a]\nb = 2\nc = 3\n")
	if err := checkEdit(old, got, nil, "mcp_servers"); err == nil {
		t.Fatal("an edit of a file with nan passed the check")
	}
	same, _ := decodeTOML("x = nan\ny = inf\n[a]\nb = 1\n")
	if err := checkEdit(old, same, nil, "mcp_servers"); err != nil {
		t.Fatalf("an unchanged file with nan: %v", err)
	}
	if canonical(make(chan int)) == canonical(make(chan int)) {
		t.Fatal("two values that cannot be encoded compare equal")
	}
	f := newFixture(t)
	target := filepath.Join(f.env.CodexHome, "config.toml")
	writeFile(t, target, "ratio = nan\n")
	if _, err := f.renderer(registry, "").Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	if text := readFile(t, target); !strings.HasPrefix(text, "ratio = nan\n\n[mcp_servers.docs]") {
		t.Fatalf("written:\n%s", text)
	}
}

// Ownership belongs to the file the record names.
func TestRecordTarget(t *testing.T) {
	f := newFixture(t)
	r := f.renderer(registry, "")
	if _, err := r.Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.dir, "other-codex")
	writeFile(t, filepath.Join(other, "config.toml"), "[mcp_servers.docs]\ncommand = \"users-own\"\n")
	r.Env.CodexHome = other
	r.Force = []string{"docs"}
	p := r.Plan(instructions.Codex)
	if a := actions(p)["docs"]; a != Blocked {
		t.Fatalf("the user's server in another file: %v", a)
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "the last render wrote") {
		t.Fatalf("warnings %v", p.Warnings)
	}
	if _, err := r.Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(other, "config.toml")), "users-own") {
		t.Fatal("-force replaced the user's server")
	}
}

// The record is written before the rename, so a write cut short keeps ownership.
func TestPendingRecord(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.env.CodexHome, "config.toml")
	if _, err := f.renderer(registry, "").Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, target)
	r2 := f.renderer(strings.ReplaceAll(registry, "docs-mcp", "docs-mcp-2"), "")
	if err := r2.writePending(r2.Plan(instructions.Codex)); err != nil {
		t.Fatal(err)
	}
	if a := actions(r2.Plan(instructions.Codex))["docs"]; a != Update {
		t.Fatalf("old entry after a cut-short write: %v", a)
	}
	writeFile(t, target, strings.Replace(before, "docs-mcp", "docs-mcp-2", 1))
	if p := r2.Plan(instructions.Codex); p.Status != Ready || actions(p)["docs"] != Keep {
		t.Fatalf("new entry after a cut-short write: %s %v", p.Status, actions(p))
	}
	if _, err := r2.Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	var rec record
	json.Unmarshal([]byte(readFile(t, f.env.RecordPath(instructions.Codex))), &rec)
	if len(rec.Pending) != 0 || rec.Target != target || rec.Servers["docs"] == "" {
		t.Fatalf("record after a write: %+v", rec)
	}
}

func TestEditEdges(t *testing.T) {
	// A multi-line nested array is no header.
	nested := "matrix = [\n  [\"a\", \"b\"],\n  [\"c\"]\n]\n\n[mcp_servers.x]\ncommand = \"x\"\n"
	d, err := parseTOMLDoc(nested)
	if err != nil || len(d.sections) != 1 {
		t.Fatalf("nested array: %v %+v", err, d)
	}
	// CRLF kept for written tables; a comment inside an owned table kept above it.
	crlf := "model = \"x\"\r\n\r\n[mcp_servers.x]\r\n# keep me\r\ncommand = \"old\"\r\n"
	d, _ = parseTOMLDoc(crlf)
	out := d.apply(map[string]string{"x": tomlSectionText("x", map[string]any{"command": "new"}), "y": tomlSectionText("y", map[string]any{"command": "y"})})
	want := "model = \"x\"\r\n\r\n# keep me\r\n[mcp_servers.x]\r\ncommand = \"new\"\r\n\r\n[mcp_servers.y]\r\ncommand = \"y\"\r\n"
	if out != want {
		t.Fatalf("crlf:\n%q\nwant\n%q", out, want)
	}
	// JSONC: a first member whose comma follows a comment; a comment-only file; a byte order mark.
	got, err := setServer(`{"mcp": {"servers": {"docs": {} /* c */, "mine": {}}}}`, "docs", nil)
	n, perr := parseJSONC(got)
	if err != nil || perr != nil || n.value.(map[string]any)["mcp"].(map[string]any)["servers"].(map[string]any)["mine"] == nil {
		t.Fatalf("delete before a comment: %v %v %q", err, perr, got)
	}
	for _, in := range []string{"// my settings", "\uFEFF{\"theme\": \"dark\"}"} {
		got, err := setServer(in, "x", map[string]any{"type": "local"})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if n, err := parseJSONC(got); err != nil || n.value.(map[string]any)["mcp"] == nil {
			t.Fatalf("%q: %v %q", in, err, got)
		}
	}
}

// The dry run's diff hides plain values under secret-looking keys.
func TestReportRedacts(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.env.CodexHome, "config.toml")
	writeFile(t, target, "[mcp_servers.mine]\ncommand = \"mine\"\nenv = { API_TOKEN = \"tok-123\" }\nbearer_token_env_var = \"MINE_TOKEN\"\n")
	var b strings.Builder
	f.renderer(registry, "").Plan(instructions.Codex).Report(&b)
	out := b.String()
	if strings.Contains(out, "tok-123") || !strings.Contains(out, `API_TOKEN = "<hidden>"`) || !strings.Contains(out, `"MINE_TOKEN"`) {
		t.Fatalf("report:\n%s", out)
	}
	if !strings.Contains(readFile(t, target), "tok-123") {
		t.Fatal("the file itself changed")
	}
}
