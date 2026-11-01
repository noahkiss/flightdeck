package detect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func manifestText(id, version string, minEngine int, extra string) string {
	return fmt.Sprintf("id = %q\nversion = %q\nmin_engine_version = %d\n\n[[rules]]\nid = \"r\"\nstate = \"working\"\ncontains = [\"busy %s\"]\n%s", id, version, minEngine, id, extra)
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// catalogueFixture writes a catalogue that exercises each refresh rule against the vendored set.
func catalogueFixture(t *testing.T) string {
	dir := t.TempDir()
	v := Vendored()
	writeFiles(t, dir, map[string]string{
		"index.toml": `schema_version = 1
[[agents]]
id = "claude"
path = "claude.toml"
[[agents]]
id = "codex"
path = "codex.toml"
[[agents]]
id = "opencode"
path = "opencode.toml"
[[agents]]
id = "future"
path = "future.toml"
[[agents]]
id = "newagent"
path = "sub/newagent.toml"
[[agents]]
id = "liar"
path = "liar.toml"
[[agents]]
id = "gone"
path = "gone.toml"
`,
		"claude.toml":   manifestText("claude", "2026.12.31.9", 3, ""),
		"codex.toml":    manifestText("codex", "2000.1.1.1", 3, ""),       // older: never downgrade
		"opencode.toml": string(v.Lookup("opencode").Manifest.Raw),        // same version
		"future.toml":   manifestText("future", "1", EngineVersion+1, ""), // needs a newer engine
		"liar.toml":     manifestText("someone-else", "1", 1, ""),         // id mismatch
	})
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	writeFiles(t, filepath.Join(dir, "sub"), map[string]string{"newagent.toml": manifestText("newagent", "1.0", 1, "")})
	return dir
}

func TestRefreshDryRunThenWrite(t *testing.T) {
	cat := catalogueFixture(t)
	data := filepath.Join(t.TempDir(), "manifests")
	ctx := context.Background()

	res, err := Refresh(ctx, RefreshOptions{From: cat, Dir: data, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("dry run touched the data directory: %v", err)
	}
	var ids []string
	for _, c := range res.Changes {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "claude,newagent" {
		t.Fatalf("changes %v, want claude,newagent", ids)
	}
	cl := res.Changes[0]
	if cl.OldFrom != FromVendored || cl.NewVersion != "2026.12.31.9" || !strings.Contains(cl.Diff, "+version = \"2026.12.31.9\"") || !strings.Contains(cl.Diff, "-version = ") {
		t.Fatalf("claude change: %+v", cl)
	}
	if len(res.Unchanged) != 1 || res.Unchanged[0] != "opencode" {
		t.Fatalf("unchanged %v", res.Unchanged)
	}
	skipped := map[string]string{}
	for _, p := range res.Skipped {
		skipped[p.Path] = p.Reason
	}
	for path, want := range map[string]string{
		"codex.toml":  "not downgrading",
		"future.toml": "newer detection engine",
		"liar.toml":   "does not match",
		"gone.toml":   "no such file",
	} {
		if !strings.Contains(skipped[path], want) {
			t.Errorf("skipped[%s] = %q, want it to say %q", path, skipped[path], want)
		}
	}

	if _, err := Refresh(ctx, RefreshOptions{From: cat, Dir: data}); err != nil {
		t.Fatal(err)
	}
	s := Load(data)
	if len(s.Problems) > 0 {
		t.Fatalf("problems after refresh: %v", s.Problems)
	}
	if e := s.Lookup("claude"); e.From != FromData || e.Manifest.Version != "2026.12.31.9" {
		t.Fatalf("claude after refresh: %+v", e)
	}
	if e := s.Lookup("codex"); e.From != FromVendored {
		t.Fatalf("codex after refresh came from %s", e.From)
	}
	if e := s.Lookup("newagent"); e == nil || e.From != FromData {
		t.Fatalf("newagent after refresh: %+v", e)
	}
	// The written manifest is in force: the new rule matches.
	if r := s.Screen("claude", Input{Screen: "busy claude"}); r.State != ScreenWorking {
		t.Fatalf("refreshed claude rule did not apply: %+v", r)
	}

	res, err = Refresh(ctx, RefreshOptions{From: filepath.Join(cat, "index.toml"), Dir: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("second refresh changed %v", res.Changes)
	}
}

func TestRefreshOverHTTP(t *testing.T) {
	cat := catalogueFixture(t)
	srv := httptest.NewTLSServer(http.StripPrefix("/agent-detection/", http.FileServer(http.Dir(cat))))
	defer srv.Close()
	data := t.TempDir()
	res, err := Refresh(context.Background(), RefreshOptions{From: srv.URL + "/agent-detection/index.toml", Dir: data, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("changes %+v", res.Changes)
	}
	if _, err := os.Stat(filepath.Join(data, "newagent.toml")); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRefusesBadCatalogue(t *testing.T) {
	for name, index := range map[string]string{
		"schema":    "schema_version = 2\n",
		"unsafe":    "schema_version = 1\n[[agents]]\nid = \"x\"\npath = \"../x.toml\"\n",
		"absolute":  "schema_version = 1\n[[agents]]\nid = \"x\"\npath = \"/etc/x.toml\"\n",
		"url":       "schema_version = 1\n[[agents]]\nid = \"x\"\npath = \"https://elsewhere/x.toml\"\n",
		"duplicate": "schema_version = 1\n[[agents]]\nid = \"x\"\npath = \"a.toml\"\n[[agents]]\nid = \"x\"\npath = \"b.toml\"\n",
		"malformed": "schema_version = \n",
	} {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"index.toml": index})
		if _, err := Refresh(context.Background(), RefreshOptions{From: dir, Dir: t.TempDir()}); err == nil {
			t.Errorf("%s: refresh accepted the catalogue", name)
		}
	}
}

func TestLoadKeepsVendoredOverBadOrOlderData(t *testing.T) {
	data := t.TempDir()
	writeFiles(t, data, map[string]string{
		"claude.toml":   manifestText("claude", "1.0", 1, ""),              // older than vendored: ignored
		"codex.toml":    "id = \"codex\"\nversion = \"9999\"\n[[rules]]\n", // broken: a problem
		"opencode.toml": manifestText("opencode", "2026.12.1.1", 1, ""),    // newer: used
	})
	s := Load(data)
	if e := s.Lookup("claude"); e.From != FromVendored {
		t.Errorf("claude from %s, want vendored", e.From)
	}
	if e := s.Lookup("codex"); e.From != FromVendored {
		t.Errorf("codex from %s, want vendored", e.From)
	}
	if e := s.Lookup("opencode"); e.From != FromData {
		t.Errorf("opencode from %s, want data", e.From)
	}
	if len(s.Problems) != 1 || !strings.HasSuffix(s.Problems[0].Path, "codex.toml") {
		t.Errorf("problems %v, want one for codex.toml", s.Problems)
	}
	// The alias of the data copy still resolves to opencode.
	if s.Lookup("open-code") != nil {
		t.Errorf("the replacing manifest has no aliases, but open-code still resolves")
	}
}

// TestCache: a Cache returns the same set while the data directory is unchanged, and loads
// again when a manifest is added, rewritten or removed. Sets from Vendored share the parsed
// manifests but never their maps or problems.
func TestCache(t *testing.T) {
	data := t.TempDir()
	c := NewCache(data)
	s1 := c.Set()
	if e := s1.Lookup("opencode"); e == nil || e.From != FromVendored {
		t.Fatalf("opencode %+v, want vendored", e)
	}
	if c.Set() != s1 {
		t.Errorf("an unchanged directory loaded again")
	}
	writeFiles(t, data, map[string]string{"opencode.toml": manifestText("opencode", "2026.12.1.1", 1, "")})
	s2 := c.Set()
	if e := s2.Lookup("opencode"); s2 == s1 || e.From != FromData || e.Manifest.Version != "2026.12.1.1" {
		t.Fatalf("after a write: opencode %+v", e)
	}
	writeFiles(t, data, map[string]string{"opencode.toml": manifestText("opencode", "2026.12.1.22", 1, "")})
	if e := c.Set().Lookup("opencode"); e.Manifest.Version != "2026.12.1.22" {
		t.Errorf("after a rewrite: version %s", e.Manifest.Version)
	}
	if err := os.Remove(filepath.Join(data, "opencode.toml")); err != nil {
		t.Fatal(err)
	}
	if e := c.Set().Lookup("opencode"); e.From != FromVendored {
		t.Errorf("after a remove: from %s", e.From)
	}

	a, b := Vendored(), Vendored()
	if a.Lookup("claude").Manifest != b.Lookup("claude").Manifest {
		t.Errorf("vendored manifests parsed again")
	}
	a.Problems = append(a.Problems, Problem{"x", "y"})
	a.put(&Entry{Manifest: &Manifest{ID: "claude"}, From: FromData})
	if b.Lookup("claude").From != FromVendored || len(b.Problems) != 0 || len(Vendored().Problems) != 0 {
		t.Errorf("a change to one vendored set reached another")
	}
}

// TestRefreshPathTraversal: an id that is not a plain file name never writes outside the data
// directory.
func TestRefreshPathTraversal(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(root, "cat")
	data := filepath.Join(root, "data")
	os.Mkdir(cat, 0o700)
	writeFiles(t, cat, map[string]string{
		"index.toml":   "schema_version = 1\n[[agents]]\nid = \"../escaped\"\npath = \"escaped.toml\"\n[[agents]]\nid = \"Upper\"\npath = \"upper.toml\"\n",
		"escaped.toml": manifestText("../escaped", "1", 1, ""),
		"upper.toml":   manifestText("Upper", "1", 1, ""),
	})
	res, err := Refresh(context.Background(), RefreshOptions{From: cat, Dir: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 || len(res.Skipped) != 2 {
		t.Fatalf("changes %+v skipped %+v", res.Changes, res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.toml")); !os.IsNotExist(err) {
		t.Fatalf("a manifest escaped the data directory: %v", err)
	}
}

// TestRefreshHTTPSOnly: plain http and a redirect from https to http are refused.
func TestRefreshHTTPSOnly(t *testing.T) {
	cat := catalogueFixture(t)
	plain := httptest.NewServer(http.FileServer(http.Dir(cat)))
	defer plain.Close()
	if _, err := Refresh(context.Background(), RefreshOptions{From: plain.URL + "/index.toml", Dir: t.TempDir()}); err == nil {
		t.Error("an http catalogue was accepted")
	}
	tls := httptest.NewTLSServer(http.RedirectHandler(plain.URL+"/index.toml", http.StatusFound))
	defer tls.Close()
	if _, err := Refresh(context.Background(), RefreshOptions{From: tls.URL + "/index.toml", Dir: t.TempDir(), Client: tls.Client()}); err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Errorf("a redirect to http was followed: %v", err)
	}
}

// TestImplausibleVersion: a version dated far ahead cannot pin itself over later updates.
func TestImplausibleVersion(t *testing.T) {
	cat := t.TempDir()
	writeFiles(t, cat, map[string]string{
		"index.toml":  "schema_version = 1\n[[agents]]\nid = \"claude\"\npath = \"claude.toml\"\n",
		"claude.toml": manifestText("claude", "9999.1.1.1", 3, ""),
	})
	data := t.TempDir()
	res, err := Refresh(context.Background(), RefreshOptions{From: cat, Dir: data})
	if err != nil || len(res.Changes) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("res %+v err %v", res, err)
	}
	writeFiles(t, data, map[string]string{"claude.toml": manifestText("claude", "9999.1.1.1", 3, "")})
	if s := Load(data); s.Lookup("claude").From != FromVendored || len(s.Problems) != 1 {
		t.Fatalf("a far-future data copy was used: %+v", s.Problems)
	}
}
