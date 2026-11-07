package mcpreg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// rot13 maps lower-case letters a-m to n-z and back, as `tr a-z n-za-m` does. The fake
// provider prints rot13 of its reference, so the invented secret value appears in no file:
// not the config, not the provider script and not this test's source.
func rot13(s string) string {
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

// The reference whose rot13 is the invented secret value.
const inventedRef = "fhcre-frperg-gbxra-7781"

func inventedSecret() string { return rot13(inventedRef) }

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLauncherRender(t *testing.T) {
	cfg, err := loadConfig(t, `
[[mcp.server]]
name = "docs"
transport = "stdio"
command = "docs-mcp"
args = ["--port", "0"]
cwd = "/srv/docs"
timeout = 20
env = { LOG_LEVEL = "info", HOME_DIR = { env = "HOME" }, DOCS_TOKEN = { secret = "vault:docs/token" } }

[[mcp.server]]
name = "web"
transport = "http"
url = "https://web.example/mcp"
headers = { X-Team = "core", X-Org = { env = "WEB_ORG" }, Authorization = { command = ["print-token", "web"] } }

[[mcp.provider]]
name = "vault"
command = ["vault-cli", "read"]
env = ["VAULT_ADDR", "VAULT_TOKEN"]
`)
	if err != nil {
		t.Fatal(err)
	}
	r := &Renderer{Config: cfg, Launcher: Launcher{Program: "/opt/gff dir/gff-hab", Config: "/etc/flightdeck's.toml"}}
	docs, _ := cfg.Server("docs")
	web, _ := cfg.Server("web")
	launchArgs := `["mcp","launch","-config","/etc/flightdeck's.toml","docs"]`
	helper := `'/opt/gff dir/gff-hab' mcp header -config '/etc/flightdeck'\''s.toml' web`
	cases := []struct {
		h    Harness
		s    Server
		want string
	}{
		{instructions.Claude, docs, `{"args":` + launchArgs + `,"command":"/opt/gff dir/gff-hab","type":"stdio"}`},
		{instructions.Codex, docs, `{"args":` + launchArgs + `,"command":"/opt/gff dir/gff-hab","env_vars":["HOME","VAULT_ADDR","VAULT_TOKEN"],"startup_timeout_sec":20}`},
		{instructions.Opencode, docs, `{"command":["/opt/gff dir/gff-hab","mcp","launch","-config","/etc/flightdeck's.toml","docs"],"timeout":20000,"type":"local"}`},
		{instructions.Claude, web, `{"headers":{"X-Org":"${WEB_ORG}","X-Team":"core"},"headersHelper":` + jsonString(helper) + `,"type":"http","url":"https://web.example/mcp"}`},
		{instructions.Codex, web, `{"env_http_headers":{"X-Org":"WEB_ORG"},"http_headers":{"X-Team":"core"},"http_headers_helper":` + jsonString(helper) + `,"url":"https://web.example/mcp"}`},
	}
	for _, c := range cases {
		e, err := r.render(c.h, c.s)
		if err != nil {
			t.Errorf("%s %s: %v", c.h, c.s.Name, err)
			continue
		}
		if got := canonical(e.value); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.h, c.s.Name, got, c.want)
		}
	}
	var nr notRendered
	if _, err := r.render(instructions.Opencode, web); !errors.As(err, &nr) || !strings.Contains(nr.reason, "no header helper") {
		t.Errorf("opencode http with a secret header: %v", err)
	}
	for _, l := range []Launcher{{}, {Config: "relative.toml"}} {
		r := &Renderer{Config: cfg, Launcher: l}
		if _, err := r.render(instructions.Claude, docs); !errors.As(err, &nr) || !strings.Contains(nr.reason, "absolute config path") {
			t.Errorf("launcher %+v: %v", l, err)
		}
	}
	r.Launcher.Config = "/x/${HOME}.toml"
	if _, err := r.render(instructions.Claude, docs); !errors.As(err, &nr) || !strings.Contains(nr.reason, "${") {
		t.Errorf("claude ${ in the launcher: %v", err)
	}
	r.Launcher.Config = "/x/{env:HOME}.toml"
	if _, err := r.render(instructions.Opencode, docs); !errors.As(err, &nr) || !strings.Contains(nr.reason, "{env:") {
		t.Errorf("opencode {env: in the launcher: %v", err)
	}
	if got := (Launcher{Config: "/c.toml"}).argv("launch", "a")[0]; got != "gff-hab" {
		t.Errorf("default program %q", got)
	}
}

func jsonString(s string) string { return canonical(s) }

func TestShellJoin(t *testing.T) {
	for in, want := range map[string]string{
		"plain":   "plain",
		"a b":     "'a b'",
		"it's":    `'it'\''s'`,
		"":        "''",
		"$HOME":   "'$HOME'",
		"/a/b.c":  "/a/b.c",
		"x;rm -r": "'x;rm -r'",
	} {
		if got := shellJoin([]string{in}); got != want {
			t.Errorf("shellJoin(%q) = %s, want %s", in, got, want)
		}
	}
}

func resolverFixture(t *testing.T) (*Resolver, string) {
	t.Helper()
	dir := t.TempDir()
	prov := writeScript(t, dir, "provider", `case "$1" in fail*) printf '%s\n' "${1#fail}" | tr a-z n-za-m >&2; exit 3;; esac
if [ "$1" = slow ]; then exec sleep 5; fi
if [ "$1" = empty ]; then exit 0; fi
if [ "$1" = lines ]; then printf 'a\nb\n'; exit 0; fi
printf '%s\n' "$1" | tr a-z n-za-m
`)
	cfg, err := loadConfig(t, `
[[mcp.server]]
name = "docs"
transport = "stdio"
command = "docs-mcp"
args = ["serve"]
cwd = "`+dir+`"
env = { LOG_LEVEL = "info", ORG = { env = "DOCS_ORG", prefix = "org-" }, DOCS_TOKEN = { secret = "vault:`+inventedRef+`" }, PLAIN = { command = ["`+prov+`", "cynva"] } }

[[mcp.server]]
name = "web"
transport = "http"
url = "https://web.example/mcp"
headers = { X-Team = "core", Authorization = { secret = "vault:`+inventedRef+`" } }

[[mcp.provider]]
name = "vault"
command = ["`+prov+`"]
timeout = 2
`)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	writeScript(t, bin, "docs-mcp", "exit 0\n")
	env := map[string]string{"DOCS_ORG": "core", "PATH": bin}
	r := &Resolver{Config: cfg, Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	return r, prov
}

func TestResolverLaunch(t *testing.T) {
	r, _ := resolverFixture(t)
	docs, _ := r.Config.Server("docs")
	base := []string{"PATH=" + r.mustLookup("PATH"), "DOCS_TOKEN=old", "KEEP=1"}
	l, err := r.Launch(context.Background(), "docs", base)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != filepath.Join(r.mustLookup("PATH"), "docs-mcp") || !reflect.DeepEqual(l.Argv, []string{"docs-mcp", "serve"}) || l.Dir != docs.Cwd {
		t.Fatalf("launch: %+v", l)
	}
	want := []string{"PATH=" + r.mustLookup("PATH"), "KEEP=1", "DOCS_TOKEN=" + inventedSecret(), "LOG_LEVEL=info", "ORG=org-core", "PLAIN=plain"}
	if !reflect.DeepEqual(l.Env, want) {
		t.Fatalf("env:\n got %q\nwant %q", l.Env, want)
	}
	if _, err := r.Launch(context.Background(), "web", base); err == nil || !strings.Contains(err.Error(), "mcp header") {
		t.Errorf("launch of an http server: %v", err)
	}
	if _, err := r.Launch(context.Background(), "nope", base); err == nil || !strings.Contains(err.Error(), "no server") {
		t.Errorf("launch of an unknown server: %v", err)
	}
	h, err := r.Headers(context.Background(), "web")
	if err != nil || !reflect.DeepEqual(h, map[string]string{"Authorization": inventedSecret()}) {
		t.Fatalf("headers: %v %v", h, err)
	}
	if _, err := r.Headers(context.Background(), "docs"); err == nil || !strings.Contains(err.Error(), "mcp launch") {
		t.Errorf("headers of a stdio server: %v", err)
	}
	r.Lookup = func(string) (string, bool) { return "", false }
	if _, err := r.Launch(context.Background(), "docs", nil); err == nil || !strings.Contains(err.Error(), "env ORG: variable DOCS_ORG is not set") {
		t.Errorf("unset variable: %v", err)
	}
}

func (r *Resolver) mustLookup(k string) string { v, _ := r.lookup(k); return v }

func TestResolverRefusals(t *testing.T) {
	r, prov := resolverFixture(t)
	ctx := context.Background()
	cases := map[string]struct {
		kind string
		v    Value
		want string
	}{
		"failing provider": {"env", Value{Secret: "vault:fail"}, "provider vault " + prov + ": exit status 3"},
		"slow provider":    {"env", Value{Secret: "vault:slow"}, "ran past 2s"},
		"empty output":     {"env", Value{Secret: "vault:empty"}, "printed nothing"},
		"two lines header": {"header", Value{Secret: "vault:lines"}, "more than one line"},
		"missing program":  {"env", Value{Command: []string{filepath.Join(t.TempDir(), "none")}}, "command"},
	}
	for name, c := range cases {
		start := time.Now()
		_, err := r.resolve(ctx, "docs", c.kind, "K", c.v)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "mcp server docs: "+c.kind+" K") {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if time.Since(start) > 4*time.Second {
			t.Errorf("%s took %v", name, time.Since(start))
		}
	}
	// A provider that echoes the secret on stderr as it fails: the error never quotes stderr.
	_, err := r.resolve(ctx, "docs", "env", "K", Value{Secret: "vault:fail " + inventedRef})
	if err == nil || strings.Contains(err.Error(), inventedSecret()) {
		t.Errorf("stderr echo: %v", err)
	}
	if v, err := r.resolve(ctx, "docs", "env", "K", Value{Secret: "vault:lines"}); err != nil || v != "a\nb" {
		t.Errorf("an env value may span lines: %q %v", v, err)
	}
}

func TestLaunchEnvNames(t *testing.T) {
	c := Config{Providers: []Provider{{Name: "p", Command: []string{"x"}, Env: []string{"P_ADDR"}}, {Name: "q", Command: []string{"y"}, Env: []string{"Q_ONLY"}}}}
	s := Server{Env: map[string]Value{"A": {Env: "B"}, "C": {Secret: "p:r"}, "D": {Command: []string{"z"}}, "E": {Literal: "e", isLit: true}}}
	if got := c.launchEnvNames(s); !reflect.DeepEqual(got, []string{"B", "P_ADDR"}) {
		t.Errorf("names %v", got)
	}
}
