package capcom

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestLoadConfig(t *testing.T) {
	d := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(d, "hab.toml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, file, want string
		problems         int
		err              bool
	}{
		{"no file", "", Claude, 0, false},
		{"no section", "[catalog]\nalways_on = true\n", Claude, 0, false},
		{"codex", "[capcom]\nharness = \"codex\"\n", Codex, 0, false},
		{"opencode", "[capcom]\nharness = \"opencode\"\n", Opencode, 0, false},
		{"unknown harness", "[capcom]\nharness = \"pi\"\n", Claude, 1, false},
		{"broken file", "[capcom\n", Claude, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(d, "missing.toml")
			if tc.file != "" {
				p = write(tc.file)
			}
			c, problems, err := LoadConfig(p)
			if c.Harness != tc.want || len(problems) != tc.problems || (err != nil) != tc.err {
				t.Fatalf("got %+v, %q, %v", c, problems, err)
			}
		})
	}
	c, problems, err := LoadConfig(filepath.Join("..", "..", "docs", "examples", "hab.toml"))
	if err != nil || len(problems) != 0 || c.Harness != Claude {
		t.Fatalf("the example config: %+v, %q, %v", c, problems, err)
	}
}

func TestBeats(t *testing.T) {
	for _, tc := range []struct {
		a, b Belief
		want bool
	}{
		{Belief{2, "b"}, Belief{1, "a"}, true},
		{Belief{1, "a"}, Belief{2, "b"}, false},
		{Belief{3, "a"}, Belief{3, "b"}, true}, // a tie: the lower device id
		{Belief{3, "b"}, Belief{3, "a"}, false},
		{Belief{3, "a"}, Belief{3, "a"}, false},
		{Belief{1, "a"}, Belief{}, true},
		{Belief{}, Belief{}, false},
	} {
		if got := tc.a.Beats(tc.b); got != tc.want {
			t.Errorf("%+v beats %+v: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

const exe = "/opt/gff dir/gff-hab"

func TestFilesPerHarness(t *testing.T) {
	files := func(h string, scope ReadScope) map[string]string {
		t.Helper()
		fs, err := Files(h, exe, scope)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range fs {
			out[f.Path] = string(f.Data)
		}
		return out
	}

	c := files(Claude, ReadScope{})
	if c["CLAUDE.md"] != Manual {
		t.Fatal("Claude Code manual")
	}
	var mcp struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(c[".mcp.json"]), &mcp); err != nil {
		t.Fatal(err)
	}
	if s := mcp.MCPServers[MCPServer]; s.Command != exe || !slices.Equal(s.Args, []string{"mcp"}) || s.Type != "stdio" {
		t.Fatalf(".mcp.json %s", c[".mcp.json"])
	}
	var set struct {
		Enabled []string `json:"enabledMcpjsonServers"`
		Perms   struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(c[filepath.Join(".claude", "settings.local.json")]), &set); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(set.Enabled, []string{MCPServer}) || !slices.Equal(set.Perms.Allow, []string{"mcp__" + MCPServer}) || set.Perms.Deny != nil {
		t.Fatalf("settings %+v", set)
	}
	// The read scope's seam: deny rules appear once the scope carries them.
	scoped := files(Claude, ReadScope{Deny: []string{"/home/user/.ssh/**", "**/.env"}})
	set.Perms.Deny = nil
	json.Unmarshal([]byte(scoped[filepath.Join(".claude", "settings.local.json")]), &set)
	if !slices.Equal(set.Perms.Deny, []string{"Read(//home/user/.ssh/**)", "Read(**/.env)"}) {
		t.Fatalf("deny rules %q", set.Perms.Deny)
	}

	x := files(Codex, ReadScope{})
	if x["AGENTS.md"] != Manual || len(x) != 2 {
		t.Fatalf("Codex files %v", x)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(x[filepath.Join(".codex", "config.toml")], &cfg); err != nil {
		t.Fatal(err)
	}
	if s := cfg.MCPServers[MCPServer]; s.Command != exe || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Fatalf("codex config %s", x[filepath.Join(".codex", "config.toml")])
	}

	o := files(Opencode, ReadScope{})
	var oc struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(o["opencode.json"]), &oc); err != nil {
		t.Fatal(err)
	}
	if s := oc.MCP[MCPServer]; o["AGENTS.md"] != Manual || s.Type != "local" || !slices.Equal(s.Command, []string{exe, "mcp"}) || !s.Enabled {
		t.Fatalf("opencode config %s", o["opencode.json"])
	}

	if _, err := Files("pi", exe, ReadScope{}); err == nil {
		t.Fatal("an unknown harness")
	}
	if _, err := Files(Claude, "gff-hab", ReadScope{}); err == nil {
		t.Fatal("a relative gff-hab path")
	}
}

// TestWriteFolderRewrites: a second write puts back the manual and settings a user edited or
// replaced with a link, and leaves other files alone.
func TestWriteFolderRewrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c1")
	if err := WriteFolder(dir, Claude, exe, ReadScope{}); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, ".claude", "settings.local.json")
	want, _ := os.ReadFile(settings)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o600)
	elsewhere := filepath.Join(t.TempDir(), "target")
	os.WriteFile(elsewhere, []byte("outside"), 0o600)
	os.Remove(settings)
	if err := os.Symlink(elsewhere, settings); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "photo.png"), []byte("attachment"), 0o600)
	if err := WriteFolder(dir, Claude, exe, ReadScope{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != Manual {
		t.Fatalf("manual %q", b)
	}
	if fi, err := os.Lstat(settings); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("settings still a link: %v", err)
	}
	if b, _ := os.ReadFile(settings); string(b) != string(want) {
		t.Fatalf("settings %s", b)
	}
	if b, _ := os.ReadFile(elsewhere); string(b) != "outside" {
		t.Fatalf("written through the link: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "photo.png")); string(b) != "attachment" {
		t.Fatalf("attachment %q", b)
	}
}

func TestStartAndResume(t *testing.T) {
	for _, tc := range []struct {
		harness, id     string
		start, resume   string
		startH, resumeH string
	}{
		{Claude, "u-1", "claude --session-id u-1", "claude --resume u-1", "", ""},
		{Codex, "t-9", "codex", "codex resume t-9", Codex, Codex},
		{Codex, "", "codex", "codex resume --last", Codex, Codex},
		{Opencode, "ses_1", "opencode", "opencode --session ses_1", Opencode, Opencode},
		{Opencode, "", "opencode", "opencode --continue", Opencode, Opencode},
	} {
		st, err := StartFor(tc.harness, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		rs, err := ResumeFor(tc.harness, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(st.Argv, " ") != tc.start || st.Harness != tc.startH ||
			strings.Join(rs.Argv, " ") != tc.resume || rs.Harness != tc.resumeH {
			t.Errorf("%s %q: start %+v, resume %+v", tc.harness, tc.id, st, rs)
		}
		if ResumeCommand(tc.harness, tc.id) != tc.resume {
			t.Errorf("resume command %q", ResumeCommand(tc.harness, tc.id))
		}
	}
	if st, _ := StartFor(Claude, "u-1"); !slices.Contains(st.Env, "AGENT_KIND=claude") {
		t.Fatalf("Claude Code env %v", st.Env)
	}
	if _, err := StartFor(Claude, ""); err == nil {
		t.Fatal("Claude Code with no session id")
	}
}

func TestState(t *testing.T) {
	d := t.TempDir()
	st, err := LoadState(d)
	if err != nil || st.Assignment != 0 || st.Sessions != nil {
		t.Fatalf("first start: %+v, %v", st, err)
	}
	want := State{Assignment: 4, Home: "dev-a", Standing: "s-1", Sessions: []Session{{ID: "s-1", Dir: "/x", Harness: Claude, Resume: "u"}}}
	if err := SaveState(d, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(d)
	if err != nil || got.Belief() != (Belief{4, "dev-a"}) || got.Standing != "s-1" || len(got.Sessions) != 1 || got.Sessions[0].Resume != "u" {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(d, StateFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	os.WriteFile(filepath.Join(d, StateFile), []byte("{"), 0o600)
	if st, err := LoadState(d); err == nil || st.Assignment != 0 {
		t.Fatalf("broken file: %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(d, StateFile+".bad")); err != nil {
		t.Fatal("the broken file was not kept aside")
	}
}
