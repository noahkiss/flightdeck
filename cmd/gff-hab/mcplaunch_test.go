package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rot13Lower maps a-m to n-z and back, as `tr a-z n-za-m` does. The fake provider prints rot13
// of its reference, so the invented secret value is in no file: not the config, not a script,
// not this source.
func rot13Lower(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'm':
			return r + 13
		case r >= 'n' && r <= 'z':
			return r - 13
		}
		return r
	}, s)
}

// TestMCPLaunchCLI: a server with a secret renders as the launcher into Codex's and opencode's
// configs, the launcher run as the harness would (no gff-hab serving) resolves the secret
// through the provider and execs the server with it in its environment only, the header
// helper prints the secret header, and the invented value is nowhere on disk or in any output
// a harness would log.
func TestMCPLaunchCLI(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "ynhapu-frperg-4471" // rot13 of the invented value
	value := rot13Lower(ref)
	home := filepath.Join(dir, "home")
	state := filepath.Join(dir, "state")
	work := filepath.Join(dir, "work")
	for _, d := range []string{home, work} {
		os.MkdirAll(d, 0o755)
	}
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	prov := script("provider", `case "$1" in fail*) printf '%s\n' "${1#fail}" | tr a-z n-za-m >&2; exit 2;; esac
printf '%s\n' "$1" | tr a-z n-za-m
`)
	// The fake server proves it got the value by printing it back through rot13, and prints its
	// folder and a plain value; it writes nothing.
	server := script("docs-server", `printf '%s\n' "$DOCS_TOKEN" | tr a-z n-za-m
pwd
echo "level=$LOG_LEVEL arg=$1"
`)
	cfg := filepath.Join(dir, "flightdeck.toml")
	body := `[mcp]
write = ["codex", "opencode"]

[[mcp.server]]
name = "docs"
transport = "stdio"
command = "` + server + `"
args = ["serve"]
cwd = "` + work + `"
env = { LOG_LEVEL = "info", DOCS_TOKEN = { secret = "store:` + ref + `" } }

[[mcp.server]]
name = "broken"
transport = "stdio"
command = "` + server + `"
env = { DOCS_TOKEN = { secret = "store:fail` + ref + `" } }

[[mcp.server]]
name = "web"
transport = "http"
url = "https://web.example/mcp"
headers = { X-Team = "core", Authorization = { secret = "store:` + ref + `" } }

[[mcp.provider]]
name = "store"
command = ["` + prov + `"]
timeout = 5
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"CODEX_HOME="+filepath.Join(home, ".codex"),
		"GFF_HAB_STATE="+state,
		"GFF_HAB_HOST_ID=test-host",
		"GFF_HAB_SOCKET="+filepath.Join(dir, "no-hab.sock"),
	)
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}
	var logs strings.Builder
	for _, h := range []string{"codex", "opencode"} {
		out, stderr, err := run("mcp", "render", "-config", cfg, "-launcher", bin, "-write", h)
		logs.WriteString(out + stderr)
		if err != nil {
			t.Fatalf("render %s: %v\n%s%s", h, err, out, stderr)
		}
		if !strings.Contains(out, "server docs: add") || !strings.Contains(out, "secrets resolved at launch") {
			t.Errorf("render %s report:\n%s", h, out)
		}
	}
	codex, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[mcp_servers.docs]\ncommand = " + jsonQuote(bin) + "\nargs = [\"mcp\", \"launch\", \"-config\", " + jsonQuote(cfg) + ", \"docs\"]\n",
		"http_headers_helper = " + jsonQuote(bin+" mcp header -config "+cfg+" web"),
		`http_headers = { X-Team = "core" }`,
	} {
		if !strings.Contains(string(codex), want) {
			t.Errorf("codex config lacks %q:\n%s", want, codex)
		}
	}
	opencode, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(opencode), `"launch"`) || strings.Contains(string(opencode), `"web"`) {
		t.Errorf("opencode config:\n%s", opencode)
	}

	// The launcher as Codex would run it, from the rendered argv.
	out, stderr, err := run("mcp", "launch", "-config", cfg, "docs")
	logs.WriteString(out + stderr)
	if err != nil || stderr != "" {
		t.Fatalf("launch: %v\n%s%s", err, out, stderr)
	}
	if want := ref + "\n" + work + "\nlevel=info arg=serve\n"; out != want {
		t.Fatalf("server output %q, want %q", out, want)
	}
	// The header helper as Claude or Codex would run it: one JSON object on stdout.
	out, stderr, err = run("mcp", "header", "-config", cfg, "web")
	if err != nil || stderr != "" {
		t.Fatalf("header: %v\n%s", err, stderr)
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(out), &headers); err != nil || len(headers) != 1 || headers["Authorization"] != value {
		t.Fatalf("header output: %v (%d headers)", err, len(headers))
	}
	// A provider that fails and echoes the secret on stderr: the launcher fails, saying which
	// value and provider, and never quotes it.
	out, stderr, err = run("mcp", "launch", "-config", cfg, "broken")
	logs.WriteString(out + stderr)
	if err == nil || !strings.Contains(stderr, "mcp server broken: env DOCS_TOKEN: provider store") || !strings.Contains(stderr, "exit status 2") {
		t.Fatalf("broken launch: %v\n%s%s", err, out, stderr)
	}
	for _, args := range [][]string{{"mcp", "launch", "-config", cfg, "web"}, {"mcp", "header", "-config", cfg, "docs"}, {"mcp", "launch", "-config", cfg}, {"mcp", "launch", "docs"}} {
		out, stderr, err := run(args...)
		logs.WriteString(out + stderr)
		if err == nil {
			t.Errorf("%v: no error", args)
		}
	}

	if strings.Contains(logs.String(), value) {
		t.Fatal("the secret value reached an output a harness logs")
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(value)) {
			t.Errorf("%s holds the secret value", p)
		}
		return nil
	})
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
