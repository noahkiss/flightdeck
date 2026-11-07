package mcpreg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// Every test works in a temporary folder; no test reads or writes a real harness config.

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func loadConfig(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flightdeck.toml")
	writeFile(t, p, body)
	return LoadConfig(p)
}

func TestConfig(t *testing.T) {
	c, err := loadConfig(t, `
[instructions]
write = ["codex"]

[mcp]
write = ["codex", "opencode"]

[[mcp.server]]
name = "docs"
transport = "stdio"
command = "docs-mcp"
args = ["--port", "0"]
env = { LOG_LEVEL = "info", API_TOKEN = { env = "DOCS_TOKEN" } }
timeout = 30

[[mcp.server]]
name = "tracker"
transport = "http"
url = "https://tracker.example.com/mcp"
harnesses = ["claude", "codex"]
hosts = ["dev-box"]
[mcp.server.headers]
Authorization = { env = "TRACKER_TOKEN", prefix = "Bearer " }
X-Team = "core"

[[mcp.server]]
name = "vault-backed"
transport = "stdio"
command = "server"
env = { KEY = { secret = "provider:item/field" }, OTHER = { command = ["print-it", "x"] } }

[[mcp.provider]]
name = "provider"
command = ["vault-cli", "read"]
env = ["VAULT_ADDR"]
timeout = 5
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 3 || !reflect.DeepEqual(c.Write, []string{"codex", "opencode"}) {
		t.Fatalf("config: %+v", c)
	}
	tok := c.Servers[0].Env["API_TOKEN"]
	if tok.IsLiteral() || tok.Env != "DOCS_TOKEN" {
		t.Errorf("env reference: %+v", tok)
	}
	if v := c.Servers[0].Env["LOG_LEVEL"]; !v.IsLiteral() || v.Literal != "info" {
		t.Errorf("literal: %+v", v)
	}
	if h := c.Servers[1].Headers["Authorization"]; h.Prefix != "Bearer " || h.Env != "TRACKER_TOKEN" {
		t.Errorf("header: %+v", h)
	}
	if v := c.Servers[2].Env["OTHER"]; !reflect.DeepEqual(v.Command, []string{"print-it", "x"}) || !v.NeedsLaunch() {
		t.Errorf("command: %+v", v)
	}

	bad := map[string]string{
		"unknown key":         "[mcp]\nwrit = [\"codex\"]\n",
		"empty provider":      "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { secret = \":r\" } }\n",
		"empty reference":     "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { secret = \"p:\" } }\n",
		"unknown provider":    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { secret = \"nope:r\" } }\n",
		"provider no command": "[[mcp.provider]]\nname = \"p\"\ncommand = []\n",
		"provider bad name":   "[[mcp.provider]]\nname = \"p q\"\ncommand = [\"x\"]\n",
		"provider timeout":    "[[mcp.provider]]\nname = \"p\"\ncommand = [\"x\"]\ntimeout = 301\n",
		"provider env":        "[[mcp.provider]]\nname = \"p\"\ncommand = [\"x\"]\nenv = [\"A-B\"]\n",
		"provider twice":      "[[mcp.provider]]\nname = \"p\"\ncommand = [\"x\"]\n[[mcp.provider]]\nname = \"p\"\ncommand = [\"y\"]\n",
		"provider key":        "[[mcp.provider]]\nname = \"p\"\ncommand = [\"x\"]\nshell = true\n",
		"unknown server key":  "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\ncwdd = \"/\"\n",
		"secret literal":      "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { API_KEY = \"abc\" }\n",
		"secret header":       "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\nurl = \"https://x.example\"\nheaders = { Authorization = \"Bearer abc\" }\n",
		"bad name":            "[[mcp.server]]\nname = \"a b\"\ntransport = \"stdio\"\ncommand = \"x\"\n",
		"twice":               "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\n[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"y\"\n",
		"value key":           "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { evn = \"B\" } }\n",
		"prefix alone":        "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { secret = \"p:r\", prefix = \"x\" } }\n",
		"two refs":            "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { env = \"B\", secret = \"p:r\" } }\n",
		"http with command":   "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\nurl = \"https://x.example\"\ncommand = \"x\"\n",
		"stdio with url":      "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nurl = \"https://x.example\"\n",
		"password in url":     "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\nurl = \"https://u:p@x.example\"\n",
		"not a url":           "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\nurl = \"x.example\"\n",
		"no transport":        "[[mcp.server]]\nname = \"a\"\ncommand = \"x\"\n",
		"unknown harness":     "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nharnesses = [\"vim\"]\n",
		"unknown write":       "[mcp]\nwrite = [\"vim\"]\n",
		"oauth on stdio":      "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nauth = \"oauth\"\n",
		"bad env name":        "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { \"A-B\" = \"x\" }\n",
		"value not a string":  "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = 3 }\n",
		"secret no provider":  "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { secret = \"ref\" } }\n",
		"negative timeout":    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\ntimeout = -1\n",
		"empty command entry": "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { A = { command = [\"\"] } }\n",
	}
	for name, body := range bad {
		if _, err := loadConfig(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRenderShapes(t *testing.T) {
	stdio := Server{Name: "docs", Transport: Stdio, Command: "docs-mcp", Args: []string{"-v"},
		Env: map[string]Value{"LEVEL": {Literal: "info", isLit: true}, "DOCS_TOKEN": {Env: "DOCS_TOKEN"}}, Timeout: 20}
	web := Server{Name: "web", Transport: HTTP, URL: "https://web.example/mcp",
		Headers: map[string]Value{"Authorization": {Env: "WEB_TOKEN", Prefix: "Bearer "}, "X-Team": {Literal: "core", isLit: true}}}
	cases := []struct {
		h    Harness
		s    Server
		want string
	}{
		{instructions.Claude, stdio, `{"args":["-v"],"command":"docs-mcp","env":{"DOCS_TOKEN":"${DOCS_TOKEN}","LEVEL":"info"},"type":"stdio"}`},
		{instructions.Codex, stdio, `{"args":["-v"],"command":"docs-mcp","env":{"LEVEL":"info"},"env_vars":["DOCS_TOKEN"],"startup_timeout_sec":20}`},
		{instructions.Opencode, stdio, `{"command":["docs-mcp","-v"],"environment":{"DOCS_TOKEN":"{env:DOCS_TOKEN}","LEVEL":"info"},"timeout":20000,"type":"local"}`},
		{instructions.Claude, web, `{"headers":{"Authorization":"Bearer ${WEB_TOKEN}","X-Team":"core"},"type":"http","url":"https://web.example/mcp"}`},
		{instructions.Codex, web, `{"bearer_token_env_var":"WEB_TOKEN","http_headers":{"X-Team":"core"},"url":"https://web.example/mcp"}`},
		{instructions.Opencode, web, `{"headers":{"Authorization":"Bearer {env:WEB_TOKEN}","X-Team":"core"},"type":"remote","url":"https://web.example/mcp"}`},
	}
	for _, c := range cases {
		e, err := render(c.h, c.s)
		if err != nil {
			t.Errorf("%s %s: %v", c.h, c.s.Name, err)
			continue
		}
		if got := canonical(e.value); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.h, c.s.Name, got, c.want)
		}
	}
	if e, _ := render(instructions.Claude, stdio); len(e.notes) != 1 || !strings.Contains(e.notes[0], "timeout") {
		t.Errorf("claude timeout note: %v", e.notes)
	}

	refused := []struct {
		h    Harness
		s    Server
		want string
	}{
		{instructions.Claude, Server{Name: "a", Transport: Stdio, Command: "x", Cwd: "/srv"}, "working-folder"},
		{instructions.Claude, Server{Name: "a", Transport: Stdio, Command: "x", Args: []string{"${HOME}"}}, "${"},
		{instructions.Opencode, Server{Name: "a", Transport: Stdio, Command: "x", Args: []string{"{env:HOME}"}}, "{env:"},
		{instructions.Codex, Server{Name: "a", Transport: Stdio, Command: "x", Env: map[string]Value{"A": {Env: "B"}}}, "own name"},
		{instructions.Codex, Server{Name: "a", Transport: HTTP, URL: "https://x.example", Headers: map[string]Value{"X-Key": {Env: "B", Prefix: "Key "}}}, "bearer"},
		{instructions.Pi, Server{Name: "a", Transport: Stdio, Command: "x"}, "no renderer"},
	}
	for _, c := range refused {
		_, err := render(c.h, c.s)
		var nr notRendered
		if !errors.As(err, &nr) || !strings.Contains(nr.reason, c.want) {
			t.Errorf("%s %+v: %v, want %q", c.h, c.s, err, c.want)
		}
	}
}

const codexBefore = `# My Codex settings
model = "big"

[projects."/work/app"]
trust_level = "trusted"

[mcp_servers.mine]
command = "my-server"
note = """
[mcp_servers.ours]
not a header
"""

# Flightdeck's server
[mcp_servers.ours]
command = "old"

[mcp_servers.ours.env]
A = "1"

# Comment before the next table
[tui]
theme = "dark"
`

func TestCodexEdit(t *testing.T) {
	d, err := parseTOMLDoc(codexBefore)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.serverSections("ours"); len(got) != 2 {
		t.Fatalf("ours sections: %v (the multi-line string is no header)", got)
	}
	if got := d.serverSections("mine"); len(got) != 1 {
		t.Fatalf("mine sections: %v", got)
	}
	upd := tomlSectionText("ours", map[string]any{"command": "new", "args": []any{"a b", `q"`}, "env": map[string]any{"A-B": "x", "a.b": "y"}})
	added := tomlSectionText("extra", map[string]any{"url": "https://x.example/mcp", "startup_timeout_sec": 5})
	out := d.apply(map[string]string{"ours": upd, "extra": added})
	want := strings.Replace(codexBefore, "[mcp_servers.ours]\ncommand = \"old\"\n\n[mcp_servers.ours.env]\nA = \"1\"\n",
		"[mcp_servers.ours]\ncommand = \"new\"\nargs = [\"a b\", \"q\\\"\"]\nenv = { A-B = \"x\", \"a.b\" = \"y\" }\n", 1) +
		"\n[mcp_servers.extra]\nurl = \"https://x.example/mcp\"\nstartup_timeout_sec = 5\n"
	if out != want {
		t.Fatalf("edit:\n%s\nwant:\n%s", out, want)
	}
	got, err := decodeTOML(out)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := decodeTOML(codexBefore)
	changes := map[string]map[string]any{
		"ours":  {"command": "new", "args": []any{"a b", `q"`}, "env": map[string]any{"A-B": "x", "a.b": "y"}},
		"extra": {"url": "https://x.example/mcp", "startup_timeout_sec": 5},
	}
	if err := checkEdit(old, got, changes, "mcp_servers"); err != nil {
		t.Fatal(err)
	}
	// A change the edit did not make is caught.
	changes["ours"]["command"] = "other"
	if err := checkEdit(old, got, changes, "mcp_servers"); err == nil {
		t.Fatal("check passed a wrong edit")
	}

	removed := d.apply(map[string]string{"ours": ""})
	if strings.Contains(removed, "[mcp_servers.ours]\ncommand") || strings.Contains(removed, "ours.env") || !strings.Contains(removed, "# Comment before the next table\n[tui]") ||
		!strings.Contains(removed, "[mcp_servers.ours]\nnot a header") {
		t.Fatalf("remove:\n%s", removed)
	}
}

const opencodeBefore = `{
  // my settings
  "$schema": "https://opencode.ai/config.json",
  "theme": "dark", /* inline */
  "mcp": {
    "servers": {
      "mine": { "type": "local", "command": ["mine"] }, // keep me
      "ours": {
        "type": "local",
        "command": ["old"],
      },
    },
  },
}
`

func TestJSONCEdit(t *testing.T) {
	ours := map[string]any{"type": "local", "command": []any{"new", "-v"}}
	out, err := setServer(opencodeBefore, "ours", ours)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(opencodeBefore, `{
        "type": "local",
        "command": ["old"],
      }`, `{
        "command": [
          "new",
          "-v"
        ],
        "type": "local"
      }`, 1)
	if out != want {
		t.Fatalf("replace:\n%s", out)
	}
	out, err = setServer(out, "extra", map[string]any{"type": "remote", "url": "https://x.example/a?b&c"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "      },\n      \"extra\": {\n        \"type\": \"remote\",\n        \"url\": \"https://x.example/a?b&c\"\n      },\n    },") {
		t.Fatalf("insert:\n%s", out)
	}
	out, err = setServer(out, "ours", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err = setServer(out, "extra", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"mine": { "type": "local", "command": ["mine"] }, // keep me`) || strings.Contains(out, "ours") ||
		!strings.Contains(out, "// my settings") || !strings.Contains(out, "/* inline */") {
		t.Fatalf("delete:\n%s", out)
	}
	if n, err := parseJSONC(out); err != nil || n.value.(map[string]any)["theme"] != "dark" {
		t.Fatalf("result does not parse: %v\n%s", err, out)
	}

	// No mcp key yet; an empty file; the last member with no trailing comma.
	for _, c := range []struct{ in, want string }{
		{"{\n  \"theme\": \"dark\"\n}\n", "{\n  \"theme\": \"dark\",\n  \"mcp\": {\n    \"servers\": {\n      \"x\": {\n        \"type\": \"local\"\n      }\n    }\n  }\n}\n"},
		{"", "{\n  \"mcp\": {\n    \"servers\": {\n      \"x\": {\n        \"type\": \"local\"\n      }\n    }\n  }\n}\n"},
		{"{}", "{\n  \"mcp\": {\n    \"servers\": {\n      \"x\": {\n        \"type\": \"local\"\n      }\n    }\n  }\n}"},
	} {
		got, err := setServer(c.in, "x", map[string]any{"type": "local"})
		if err != nil || got != c.want {
			t.Errorf("set on %q: %v\n%s", c.in, err, got)
		}
		back, err := setServer(got, "x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := parseJSONC(back); err != nil || n == nil {
			t.Errorf("removed from %q does not parse: %v\n%s", c.in, err, back)
		}
	}
	two := "{\"mcp\": {\"servers\": {\n  \"a\": 1,\n  \"b\": 2\n}}}"
	if got, _ := setServer(two, "b", nil); got != "{\"mcp\": {\"servers\": {\n  \"a\": 1\n}}}" {
		t.Errorf("delete last: %q", got)
	}
	if got, _ := setServer(two, "a", nil); got != "{\"mcp\": {\"servers\": {\n  \"b\": 2\n}}}" {
		t.Errorf("delete first: %q", got)
	}

	for _, bad := range []string{`{"a": 1, "a": 2}`, `{"a": }`, `[1, 2]`, `{"mcp": 3}`, `{"a": 1} x`, `/* open`} {
		if _, err := setServer(bad, "x", map[string]any{"type": "local"}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fixture is a temporary home with each harness's config folder and a state folder.
type fixture struct {
	t     *testing.T
	dir   string
	env   Env
	calls [][]string
}

func newFixture(t *testing.T) *fixture {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir}
	f.env = Env{
		ClaudeJSON:  filepath.Join(dir, "home", ".claude.json"),
		CodexHome:   filepath.Join(dir, "home", ".codex"),
		OpencodeDir: filepath.Join(dir, "home", ".config", "opencode"),
		StateDir:    filepath.Join(dir, "state"),
	}
	return f
}

// fakeClaude stands in for Claude's CLI: it edits mcpServers of the fixture's .claude.json
// the way `claude mcp add-json` and `remove` do, and fails an add when the name exists.
func (f *fixture) fakeClaude(failOn string) ClaudeCLI {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		if len(args) < 5 || args[0] != "mcp" || args[2] != "--scope" || args[3] != "user" {
			return []byte("bad args"), errors.New("exit status 1")
		}
		name := args[4]
		if name == failOn {
			return []byte("boom\nmore"), errors.New("exit status 1")
		}
		root := map[string]any{}
		if data, err := os.ReadFile(f.env.ClaudeJSON); err == nil {
			json.Unmarshal(data, &root)
		}
		servers, _ := root["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		switch args[1] {
		case "add-json":
			if _, ok := servers[name]; ok {
				return []byte("MCP server " + name + " already exists in user config"), errors.New("exit status 1")
			}
			var v any
			if err := json.Unmarshal([]byte(args[5]), &v); err != nil {
				return []byte("bad json"), err
			}
			servers[name] = v
		case "remove":
			if _, ok := servers[name]; !ok {
				return []byte("not found"), errors.New("exit status 1")
			}
			delete(servers, name)
		}
		root["mcpServers"] = servers
		data, _ := json.MarshalIndent(root, "", "  ")
		return nil, os.WriteFile(f.env.ClaudeJSON, data, 0o600)
	}
}

func (f *fixture) renderer(cfg string, failOn string) *Renderer {
	f.t.Helper()
	c, err := loadConfig(f.t, cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return &Renderer{Config: c, Env: f.env, Host: "box-a", Claude: f.fakeClaude(failOn)}
}

func actions(p Plan) map[string]Action {
	out := map[string]Action{}
	for _, sp := range p.Servers {
		out[sp.Name] = sp.Action
	}
	return out
}

const registry = `
[mcp]
write = ["claude", "codex", "opencode"]

[[mcp.server]]
name = "docs"
transport = "stdio"
command = "docs-mcp"
env = { DOCS_TOKEN = { env = "DOCS_TOKEN" } }

[[mcp.server]]
name = "web"
transport = "http"
url = "https://web.example/mcp"
headers = { Authorization = { env = "WEB_TOKEN", prefix = "Bearer " } }

[[mcp.server]]
name = "elsewhere"
transport = "stdio"
command = "x"
hosts = ["box-b"]

[[mcp.server]]
name = "later"
transport = "stdio"
command = "x"
env = { KEY = { secret = "provider:ref" } }

[[mcp.provider]]
name = "provider"
command = ["fake-provider"]
`

func TestRendererCodex(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.env.CodexHome, "config.toml")
	writeFile(t, target, "model = \"big\"\n\n[mcp_servers.mine]\ncommand = \"mine\"\n")
	os.Chmod(target, 0o640)
	r := f.renderer(registry, "")

	p := r.Plan(instructions.Codex)
	if p.Status != Ready {
		t.Fatalf("plan: %s %s", p.Status, p.Reason)
	}
	want := map[string]Action{"docs": Add, "web": Add, "later": NotRendered}
	if got := actions(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("actions %v, want %v", got, want)
	}
	if readFile(t, target) != "model = \"big\"\n\n[mcp_servers.mine]\ncommand = \"mine\"\n" {
		t.Fatal("a plan wrote the file")
	}
	var report strings.Builder
	p.Report(&report)
	for _, s := range []string{"server later: not rendered: needs secret resolution", "+[mcp_servers.docs]", "+env_vars = [\"DOCS_TOKEN\"]", "+bearer_token_env_var = \"WEB_TOKEN\""} {
		if !strings.Contains(report.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, report.String())
		}
	}

	if _, err := r.Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, target)
	if !strings.HasPrefix(text, "model = \"big\"\n\n[mcp_servers.mine]\ncommand = \"mine\"\n\n[mcp_servers.docs]\n") {
		t.Fatalf("written:\n%s", text)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 kept", info.Mode().Perm())
	}
	if p := r.Plan(instructions.Codex); p.Status != UpToDate {
		t.Fatalf("second plan: %s %v", p.Status, actions(p))
	}

	// A hand edit to an owned entry needs -force; a reformat alone is no edit.
	writeFile(t, target, strings.Replace(text, "command = \"docs-mcp\"", "command   =   \"docs-mcp\" # spaced", 1))
	if p := r.Plan(instructions.Codex); p.Status != UpToDate {
		t.Fatalf("reformat counted as a change: %s %v", p.Status, actions(p))
	}
	writeFile(t, target, strings.Replace(text, "command = \"docs-mcp\"", "command = \"docs-mcp-v2\"", 1))
	p = r.Plan(instructions.Codex)
	if a := actions(p)["docs"]; a != Blocked {
		t.Fatalf("hand edit: %v", a)
	}
	r.Force = []string{"docs"}
	if a := actions(r.Plan(instructions.Codex))["docs"]; a != Update {
		t.Fatalf("forced: %v", a)
	}
	if _, err := r.Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	r.Force = nil
	if !strings.Contains(readFile(t, target), "command = \"docs-mcp\"") {
		t.Fatal("force did not restore the entry")
	}

	// The registry drops docs: the owned entry goes, mine stays.
	r2 := f.renderer(strings.Replace(registry, "name = \"docs\"", "name = \"docs2\"", 1), "")
	p = r2.Plan(instructions.Codex)
	if got := actions(p); got["docs"] != Remove || got["docs2"] != Add {
		t.Fatalf("rename: %v", got)
	}
	if _, err := r2.Write(context.Background(), instructions.Codex); err != nil {
		t.Fatal(err)
	}
	text = readFile(t, target)
	if strings.Contains(text, "[mcp_servers.docs]") || !strings.Contains(text, "[mcp_servers.docs2]") || !strings.Contains(text, "[mcp_servers.mine]") {
		t.Fatalf("after rename:\n%s", text)
	}

	// A registry name the user already holds is never taken over.
	r3 := f.renderer(strings.Replace(registry, "name = \"docs\"", "name = \"mine\"", 1), "")
	if a := actions(r3.Plan(instructions.Codex))["mine"]; a != Blocked {
		t.Fatalf("user's server: %v", a)
	}

	// A user entry written as dotted keys is refused for that server only.
	f2 := newFixture(t)
	writeFile(t, filepath.Join(f2.env.CodexHome, "config.toml"), "[mcp_servers]\ndocs.command = \"docs-mcp\"\n")
	writeRecordFile(t, f2.env, instructions.Codex, filepath.Join(f2.env.CodexHome, "config.toml"), map[string]string{"docs": "0"})
	p = f2.renderer(registry, "").Plan(instructions.Codex)
	if got := actions(p); got["docs"] != Blocked || got["web"] != Add {
		t.Fatalf("dotted: %v %s", got, p.Reason)
	}

	// Not enabled.
	r4 := f.renderer(strings.Replace(registry, `write = ["claude", "codex", "opencode"]`, `write = ["claude"]`, 1), "")
	if _, err := r4.Write(context.Background(), instructions.Codex); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("not enabled: %v", err)
	}
}

func TestRendererCodexChangedDuringWrite(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.env.CodexHome, "config.toml")
	writeFile(t, target, "model = \"big\"\n")
	p := Plan{Path: target, raw: "model = \"big\"\n", exists: true, mode: 0o600, New: "x"}
	writeFile(t, target, "model = \"bigger\"\n")
	err := writeKeepMode(p.Path, []byte(p.New), p.mode, p.exists, func() error {
		if readFile(t, target) != p.raw {
			return ErrChanged
		}
		return nil
	})
	if !errors.Is(err, ErrChanged) || readFile(t, target) != "model = \"bigger\"\n" {
		t.Fatalf("changed file: %v", err)
	}
	entries, _ := os.ReadDir(f.env.CodexHome)
	if len(entries) != 1 {
		t.Fatalf("temporary file left: %v", entries)
	}
}

func TestRendererOpencode(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.env.OpencodeDir, "opencode.jsonc")
	writeFile(t, target, opencodeBefore)
	writeRecordFile(t, f.env, instructions.Opencode, target, map[string]string{"ours": entryDigest(map[string]any{"type": "local", "command": []any{"old"}})})
	r := f.renderer(registry, "")
	p := r.Plan(instructions.Opencode)
	want := map[string]Action{"docs": Add, "web": Add, "later": NotRendered, "ours": Remove}
	if got := actions(p); !reflect.DeepEqual(got, want) || p.Status != Ready {
		t.Fatalf("actions %v %s %s", got, p.Status, p.Reason)
	}
	if _, err := r.Write(context.Background(), instructions.Opencode); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, target)
	for _, s := range []string{"// my settings", "/* inline */", `"mine": { "type": "local", "command": ["mine"] }, // keep me`, `"Authorization": "Bearer {env:WEB_TOKEN}"`, `"DOCS_TOKEN": "{env:DOCS_TOKEN}"`} {
		if !strings.Contains(text, s) {
			t.Errorf("written file lacks %q:\n%s", s, text)
		}
	}
	if strings.Contains(text, `"ours"`) {
		t.Errorf("owned entry not removed:\n%s", text)
	}
	if p := r.Plan(instructions.Opencode); p.Status != UpToDate {
		t.Fatalf("second plan %s %v", p.Status, actions(p))
	}

	// Two global files: refused.
	writeFile(t, filepath.Join(f.env.OpencodeDir, "opencode.json"), "{}")
	if p := r.Plan(instructions.Opencode); p.Status != Refused || !strings.Contains(p.Reason, "both") {
		t.Fatalf("two files: %s %s", p.Status, p.Reason)
	}

	// No file at all: opencode.jsonc is created, owner-only.
	f2 := newFixture(t)
	r2 := f2.renderer(registry, "")
	if _, err := r2.Write(context.Background(), instructions.Opencode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(f2.env.OpencodeDir, "opencode.jsonc"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("new file: %v %v", err, info)
	}
}

func TestRendererRefusals(t *testing.T) {
	f := newFixture(t)
	real := filepath.Join(f.dir, "dotfiles", "config.toml")
	writeFile(t, real, "")
	os.MkdirAll(f.env.CodexHome, 0o700)
	if err := os.Symlink(real, filepath.Join(f.env.CodexHome, "config.toml")); err != nil {
		t.Fatal(err)
	}
	r := f.renderer(registry, "")
	if p := r.Plan(instructions.Codex); p.Status != Refused || !strings.Contains(p.Reason, "link") {
		t.Fatalf("link: %s %s", p.Status, p.Reason)
	}
	writeFile(t, f.env.ClaudeJSON, "{not json")
	if p := r.Plan(instructions.Claude); p.Status != Refused {
		t.Fatalf("bad json: %s", p.Status)
	}
	r.Env.OpencodeDir = "relative/opencode"
	if p := r.Plan(instructions.Opencode); p.Status != Refused || !strings.Contains(p.Reason, "absolute") {
		t.Fatalf("relative: %s %s", p.Status, p.Reason)
	}
	if _, err := r.Write(context.Background(), instructions.Pi); err == nil || !strings.Contains(err.Error(), "no renderer") {
		t.Fatalf("pi: %v", err)
	}
	if p := newFixture(t).renderer("[mcp]\n", "").Plan(instructions.Claude); p.Status != Nothing {
		t.Fatalf("empty registry: %s", p.Status)
	}

	// A target inside a git work tree warns.
	g := newFixture(t)
	os.MkdirAll(filepath.Join(g.dir, "home", ".git"), 0o700)
	if p := g.renderer(registry, "").Plan(instructions.Codex); len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "git work tree") {
		t.Fatalf("git warning: %v", p.Warnings)
	}
}

func TestRendererClaude(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.env.ClaudeJSON, `{"numStartups": 3, "projects": {"/work": {"allowedTools": []}}, "mcpServers": {"mine": {"type": "stdio", "command": "mine"}}}`)
	r := f.renderer(registry, "")
	p := r.Plan(instructions.Claude)
	if got := actions(p); got["docs"] != Add || got["web"] != Add || got["later"] != NotRendered {
		t.Fatalf("actions %v", got)
	}
	var report strings.Builder
	p.Report(&report)
	if !strings.Contains(report.String(), ".claude.json mcpServers (rendered)") || strings.Contains(report.String(), "numStartups") {
		t.Fatalf("the diff must show mcpServers only:\n%s", report.String())
	}
	if len(f.calls) != 0 {
		t.Fatal("a plan ran the CLI")
	}
	if _, err := r.Write(context.Background(), instructions.Claude); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0][1] != "add-json" || f.calls[0][4] != "docs" {
		t.Fatalf("calls %v", f.calls)
	}
	var root map[string]any
	json.Unmarshal([]byte(readFile(t, f.env.ClaudeJSON)), &root)
	if root["numStartups"].(float64) != 3 {
		t.Fatal("other state lost")
	}
	if got := canonical(root["mcpServers"].(map[string]any)["docs"]); got != `{"args":[],"command":"docs-mcp","env":{"DOCS_TOKEN":"${DOCS_TOKEN}"},"type":"stdio"}` {
		t.Fatalf("docs entry %s", got)
	}
	if p := r.Plan(instructions.Claude); p.Status != UpToDate {
		t.Fatalf("second plan %s %v", p.Status, actions(p))
	}

	// An update removes then adds; a failure part way keeps the record of what was done.
	f.calls = nil
	r2 := f.renderer(strings.ReplaceAll(registry, "docs-mcp", "docs-mcp-2"), "")
	r2.Config.Servers = append(r2.Config.Servers, Server{Name: "zzz", Transport: Stdio, Command: "z"})
	r2.Claude = f.fakeClaude("zzz")
	_, err := r2.Write(context.Background(), instructions.Claude)
	if err == nil || !strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "more") {
		t.Fatalf("failure: %v", err)
	}
	if len(f.calls) < 3 || f.calls[0][1] != "remove" || f.calls[1][1] != "add-json" {
		t.Fatalf("update calls %v", f.calls)
	}
	p = r2.Plan(instructions.Claude)
	if got := actions(p); got["docs"] != Keep || got["web"] != Keep || got["zzz"] != Add {
		t.Fatalf("after a partial write %v", got)
	}

	// The user's own server is untouched; a server removed by hand is added back with a note.
	r3 := f.renderer(registry, "")
	writeFile(t, f.env.ClaudeJSON, `{"mcpServers": {"mine": {"type": "stdio", "command": "mine"}, "docs": {"type": "stdio", "command": "docs-mcp-2", "args": [], "env": {"DOCS_TOKEN": "${DOCS_TOKEN}"}}}}`)
	p = r3.Plan(instructions.Claude)
	if got := actions(p); got["docs"] != Update || got["web"] != Add {
		t.Fatalf("hand removal %v", got)
	}
	for _, sp := range p.Servers {
		if sp.Name == "web" && len(sp.Notes) == 0 {
			t.Error("no note on an entry added back")
		}
		if sp.Name == "mine" {
			t.Error("the user's server is listed")
		}
	}
}

func TestEnvFrom(t *testing.T) {
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CODEX_HOME": "/x", "XDG_CONFIG_HOME": "/cfg"}
	e := EnvFrom(func(k string) string { return env[k] }, "/hm", "/s")
	if e.ClaudeJSON != "/c/.claude.json" || e.CodexHome != "/x" || e.OpencodeDir != "/cfg/opencode" || e.StateDir != "/s" {
		t.Fatalf("%+v", e)
	}
	e = EnvFrom(func(string) string { return "" }, "/hm", "/s")
	if e.ClaudeJSON != "/hm/.claude.json" || e.CodexHome != "/hm/.codex" {
		t.Fatalf("%+v", e)
	}
}
