package detect

import (
	"errors"
	"strings"
	"testing"
)

func TestVendoredSetLoadsEveryManifest(t *testing.T) {
	s := Vendored()
	if len(s.Problems) > 0 {
		t.Fatalf("vendored manifests failed to load: %v", s.Problems)
	}
	for _, id := range []string{KindClaude, KindCodex, KindOpencode} {
		if s.Lookup(id) == nil {
			t.Errorf("no vendored manifest for %s", id)
		}
	}
	if n := len(s.Entries()); n < 20 {
		t.Errorf("only %d vendored manifests", n)
	}
}

func TestDetectProcess(t *testing.T) {
	s := Vendored()
	tests := []struct {
		name string
		fg   Foreground
		want string // manifest id; "" means no agent
	}{
		{"claude by name", Foreground{Leader: Process{PID: 10, Name: "claude"}}, KindClaude},
		{"claude native install: versioned binary, argv0 is the symlink",
			Foreground{Leader: Process{PID: 11, Name: "2.1.312", Args: []string{"/opt/tools/bin/claude", "--resume", "x"}}}, KindClaude},
		{"claude by alias", Foreground{Leader: Process{PID: 12, Name: "claude-code"}}, KindClaude},
		{"codex by name", Foreground{Leader: Process{PID: 20, Name: "codex"}}, KindCodex},
		{"codex launched through node",
			Foreground{Leader: Process{PID: 21, Name: "node", Args: []string{"node", "--no-warnings", "/usr/lib/node_modules/@openai/codex/bin/codex.js"}}}, KindCodex},
		{"opencode by name", Foreground{Leader: Process{PID: 30, Name: "opencode"}}, KindOpencode},
		{"opencode by alias, any case", Foreground{Leader: Process{PID: 31, Name: "Open-Code"}}, KindOpencode},
		{"agent is a member, leader is a wrapper",
			Foreground{Leader: Process{PID: 40, Name: "bash"}, Members: []Process{{PID: 41, Name: "less"}, {PID: 42, Name: "codex"}}}, KindCodex},
		{"leader wins over a member",
			Foreground{Leader: Process{PID: 50, Name: "opencode"}, Members: []Process{{PID: 51, Name: "claude"}}}, KindOpencode},
		{"plain shell", Foreground{Leader: Process{PID: 60, Name: "zsh", Args: []string{"-zsh"}}}, ""},
		{"node running something else", Foreground{Leader: Process{PID: 61, Name: "node", Args: []string{"node", "server.js"}}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := s.DetectProcess(tt.fg)
			if tt.want == "" {
				if !errors.Is(err, ErrNoAgent) {
					t.Fatalf("got %+v, %v; want no agent", d, err)
				}
				return
			}
			if err != nil || d.ID != tt.want {
				t.Fatalf("got %+v, %v; want %s", d, err, tt.want)
			}
			if k := s.Kind(d.ID); k != tt.want {
				t.Errorf("Kind(%s) = %q", d.ID, k)
			}
		})
	}
}

// Screens below are written by hand in the shape each harness draws.
const claudePermission = `● I'll remove the build directory.

──────────────────────────────────────────────────
 Bash command

   rm -rf build
   Remove build output

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for rm commands in this folder
   3. No, and tell Claude what to do differently (esc)

 Esc to cancel · Tab to amend
`

const claudeIdle = `● Done. All tests pass.

──────────────────────────────────────────────────
❯
──────────────────────────────────────────────────
  ? for shortcuts
`

const codexTrust = `> You are in /work/project

  Do you trust the contents of this directory? Working with untrusted
  contents comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit

  Press enter to continue
`

const opencodePermission = `  Edit src/main.go

  △ Permission required
  Allow once   Allow always   Reject

  enter confirm  esc dismiss
`

const opencodeWorking = `  Reading files...
  ■■■■⬝⬝⬝⬝  esc to interrupt
`

func TestScreen(t *testing.T) {
	s := Vendored()
	tests := []struct {
		name  string
		agent string
		in    Input
		state ScreenState
		rule  string
	}{
		{"claude permission dialog", KindClaude, Input{Screen: claudePermission}, ScreenBlocked, "bash_permission_prompt"},
		{"claude spinner in the title", KindClaude, Input{OSCTitle: "⠂ Fixing the tests"}, ScreenWorking, "osc_title_working"},
		{"claude prompt box", KindClaude, Input{Screen: claudeIdle}, ScreenIdle, "live_prompt_box"},
		{"claude nothing known", KindClaude, Input{Screen: "hello\n"}, ScreenUnknown, ""},
		{"codex trust dialog", KindCodex, Input{Screen: codexTrust}, ScreenBlocked, "trust_directory"},
		{"codex spinner in the title", KindCodex, Input{OSCTitle: "⠋ project"}, ScreenWorking, "osc_title_working"},
		{"codex action required title", KindCodex, Input{OSCTitle: "Action Required"}, ScreenBlocked, "osc_title_blocked"},
		{"opencode permission", KindOpencode, Input{Screen: opencodePermission}, ScreenBlocked, "permission_required"},
		{"opencode working", KindOpencode, Input{Screen: opencodeWorking}, ScreenWorking, "interrupt_hint_working"},
		{"unknown agent", "nonesuch", Input{Screen: claudePermission}, ScreenUnknown, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.Screen(tt.agent, tt.in)
			if r.State != tt.state || r.Rule != tt.rule || r.Matched != (tt.rule != "") {
				t.Fatalf("got %+v; want state %s rule %q", r, tt.state, tt.rule)
			}
		})
	}
}

func TestPriorityAndTies(t *testing.T) {
	m, err := Parse([]byte(`
id = "t"
version = "1"
[[rules]]
id = "low"
state = "idle"
priority = 1
contains = ["x"]
[[rules]]
id = "first_high"
state = "working"
priority = 5
contains = ["X"]
[[rules]]
id = "second_high"
state = "blocked"
priority = 5
contains = ["x"]
[[rules]]
id = "highest_no_match"
state = "blocked"
priority = 9
contains = ["y"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if r := m.Screen(Input{Screen: "x"}); r.Rule != "first_high" {
		t.Fatalf("got %+v; want first_high (higher priority, first of a tie, case-insensitive contains)", r)
	}
}

func TestGates(t *testing.T) {
	m, err := Parse([]byte(`
id = "t"
version = "1"
[[rules]]
id = "r"
state = "blocked"
contains = ["proceed?"]
line_regex = ['^\s*1\. yes$']
any = [ { contains = ["bash"] }, { contains = ["edit"] } ]
all = [ { regex = ['(?m)^end$'] } ]
not = [ { contains = ["cancelled"] } ]
`))
	if err != nil {
		t.Fatal(err)
	}
	base := "Bash\nProceed?\n  1. yes\nend\n"
	for name, tc := range map[string]struct {
		screen string
		want   bool
	}{
		"all hold":         {base, true},
		"any fails":        {strings.Replace(base, "Bash", "Read", 1), false},
		"not fires":        {base + "cancelled\n", false},
		"line_regex fails": {strings.Replace(base, "1. yes", "1. yes please", 1), false},
		"all fails":        {strings.Replace(base, "end", "fin", 1), false},
	} {
		if got := m.Screen(Input{Screen: tc.screen}).Matched; got != tc.want {
			t.Errorf("%s: matched %v, want %v", name, got, tc.want)
		}
	}
}

func TestRegions(t *testing.T) {
	screen := "a\n\nb\n──────\nbody 1\nbody 2\n──── label\nfooter\n"
	tests := []struct{ spec, want string }{
		{"whole_recent", screen},
		{"bottom_lines(2)", "──── label\nfooter\n"},
		{"bottom_non_empty_lines(1)", "footer\n"},
		{"top_non_empty_lines(2)", "a\n\nb\n"},
		{"after_last_horizontal_rule", "footer\n"},
		{"prompt_box_body", "body 1\nbody 2\n"},
		{"above_prompt_box", "a\n\nb\n"},
		{"last_non_empty_above_prompt_box", "b"},
	}
	for _, tt := range tests {
		if got := region(Input{Screen: screen}, tt.spec); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.spec, got, tt.want)
		}
	}

	prompt := "• ran tests\nworking (3s)\n› fix it\n"
	ptests := []struct{ spec, want string }{
		{"after_last_prompt_marker", ""},
		{"before_current_prompt_marker", "• ran tests\nworking (3s)\n"},
		{"whole_recent_without_current_prompt_marker", ""},
		{"current_prompt_block_marker", "• ran tests"},
		{"after_current_prompt_block_marker", prompt},
	}
	for _, tt := range ptests {
		if got := region(Input{Screen: prompt}, tt.spec); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.spec, got, tt.want)
		}
	}
	// A block marker after the prompt means the prompt is not current.
	stale := prompt + "• answer\n"
	if got := region(Input{Screen: stale}, "whole_recent_without_current_prompt_marker"); got != stale {
		t.Errorf("stale prompt: got %q", got)
	}
}

func TestParseRefuses(t *testing.T) {
	head := "id = \"t\"\nversion = \"1\"\n"
	rule := "[[rules]]\nid = \"r\"\nstate = \"idle\"\n"
	tests := map[string]string{
		"unknown key":            head + "colour = \"red\"\n",
		"no version":             "id = \"t\"\n",
		"bad version":            "id = \"t\"\nversion = \"1.x\"\n",
		"unknown state":          head + "[[rules]]\nid = \"r\"\nstate = \"sleepy\"\ncontains = [\"a\"]\n",
		"unknown region":         head + rule + "region = \"middle\"\ncontains = [\"a\"]\n",
		"no matcher":             head + rule,
		"bad regex":              head + rule + "regex = ['(']\n",
		"look-ahead":             head + rule + "regex = ['a(?=b)']\n",
		"skip without unknown":   head + rule + "skip_state_update = true\ncontains = [\"a\"]\n",
		"top region on engine 2": "id = \"t\"\nversion = \"1\"\nmin_engine_version = 2\n" + rule + "region = \"top_non_empty_lines(3)\"\ncontains = [\"a\"]\n",
	}
	for name, src := range tests {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	_, err := Parse([]byte("id = \"t\"\nversion = \"1\"\nmin_engine_version = 99\n"))
	if !errors.Is(err, ErrEngine) {
		t.Errorf("engine 99: %v, want ErrEngine", err)
	}
}

func TestGoRegex(t *testing.T) {
	const u = `\` + `u` // spelled apart so no tool rewrites the escape
	tests := map[string]string{
		`^[` + u + `2800-` + u + `28FF]`: `^[\x{2800}-\x{28FF}]`,
		`[]a]` + u + `0041`:              `[]a]\x{0041}`,
		`^⚠[\u{fe0e}\u{fe0f}]?`:          `^⚠[\x{fe0e}\x{fe0f}]?`,
		`\p{Alphabetic}+ing`:             `[\p{L}\p{Nl}]+ing`,
		`(⬡|[\p{Alphabetic}\d])`:         `(⬡|[\p{L}\p{Nl}\d])`,
		`[\x{2800}-\x{28FF}] \\u0041`:    `[\x{2800}-\x{28FF}] \\u0041`,
	}
	for in, want := range tests {
		if got := goRegex(in); got != want {
			t.Errorf("goRegex(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"2026.10.11.1", "2026.10.11.1", 0},
		{"2026.10.11.2", "2026.10.11.1", 1},
		{"2026.9.30.1", "2026.10.1.1", -1},
		{"1.2", "1.2.0", 0},
		{"1.2.1", "1.2", 1},
	}
	for _, tt := range tests {
		if got := CompareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// Version strings below follow the shapes the harnesses print; they are fixtures, not real
// output.
func TestCheckVersion(t *testing.T) {
	tests := []struct {
		kind, version string
		reason        string // "" means no warning
	}{
		{KindClaude, "2.1.312", ""},
		{KindClaude, "2.1.999 (Claude Code)", ""},
		{KindClaude, "2.0.70", ""},
		{KindClaude, "2.2.0", VersionNewer},
		{KindClaude, "3.0.0", VersionNewer},
		{KindCodex, "codex-cli 0.166.2", ""},
		{KindCodex, "0.167.0", VersionNewer},
		{KindCodex, "1.0", VersionNewer},
		{KindCodex, "nightly", VersionUnreadable},
		{KindCodex, "", ""},
		{KindOpencode, "99.0.0", ""}, // no tested range recorded
	}
	for _, tt := range tests {
		w := CheckVersion(tt.kind, tt.version)
		switch {
		case tt.reason == "" && w != nil:
			t.Errorf("%s %q: unexpected warning %+v", tt.kind, tt.version, w)
		case tt.reason != "" && (w == nil || w.Reason != tt.reason):
			t.Errorf("%s %q: got %+v, want reason %s", tt.kind, tt.version, w, tt.reason)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	if d := UnifiedDiff("a", "b", "x\n", "x\n"); d != "" {
		t.Fatalf("equal texts gave %q", d)
	}
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	b := "1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n11\n"
	want := "--- a\n+++ b\n@@ -2,9 +2,10 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n 9\n 10\n+11\n"
	if d := UnifiedDiff("a", "b", a, b); d != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", d, want)
	}
	if d := UnifiedDiff("a", "b", "", "new\n"); d != "--- a\n+++ b\n@@ -0,0 +1,1 @@\n+new\n" {
		t.Fatalf("add-only diff:\n%s", d)
	}
}
