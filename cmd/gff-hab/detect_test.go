package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// detectScreen is a hand-written Claude Code screen at its prompt, with the OSC 7501 report a
// Claude turn starts with in front of it.
const detectScreen = "\033]7501;state=working:app=claude\033\\" + `● Done. All tests pass.

──────────────────────────────────────────────────
❯
──────────────────────────────────────────────────
  ? for shortcuts
`

func explain(t *testing.T, e *habEnv, p api.DetectExplainParams) (api.DetectExplain, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	return api.Client{Caller: e.control()}.DetectExplain(ctx, p)
}

// TestDetectExplain is the end-to-end test of `gff-hab detect explain`: a real holder, gff-hab
// and a pane whose foreground program is a copy of sleep named claude, under a screen drawn as
// Claude's prompt and an OSC 7501 working report. detect.explain names the source that won
// (osc), the process that matched, the manifest and why it was chosen, and the winning screen
// rule with the region it read; it finds the pane by pane id, session id and session name, and
// refuses bad names. The CLI prints the same as text and as JSON.
func TestDetectExplain(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	e := newHabEnv(t, 0)
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	writeFile(t, filepath.Join(e.work, "screen"), detectScreen)
	if err := os.WriteFile(filepath.Join(e.work, "claude"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if w := welcome(t, e.sock); !slices.Contains(w.Capabilities, "detect-explain") {
		t.Errorf("capabilities %v lack detect-explain", w.Capabilities)
	}
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var sc api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Name: "explained", Argv: []string{"/bin/sh", "-c", `cat "$DIR/screen"; exec "$DIR/claude" 60`}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}}, &sc); err != nil {
		t.Fatal(err)
	}

	var res api.DetectExplain
	waitUntil(t, "explain to see claude in the foreground and its prompt on screen", func() bool {
		r, err := explain(t, e, api.DetectExplainParams{Pane: sc.Pane})
		res = r
		return err == nil && r.Process.Match == "claude" && r.Agent != nil && r.Screen != nil && r.Screen.Rule != ""
	})
	if a := res.Agent; a.Harness != "claude" || a.State != api.AgentStatusWorking || a.Source != api.StateSourceOsc {
		t.Errorf("agent %+v, want claude working from osc", a)
	}
	if res.TurnSource != api.StateSourceOsc {
		t.Errorf("turn source %q, want osc", res.TurnSource)
	}
	pr := res.Process
	if pr.Pgid == 0 || pr.MatchPID != pr.Pgid || pr.MatchName != "claude" || len(pr.Processes) != 1 || !pr.Processes[0].Leader ||
		pr.Processes[0].Name != "claude" || pr.Processes[0].Match != "claude" || pr.Error != "" {
		t.Errorf("process %+v", pr)
	}
	if m := res.Manifest; m == nil || m.ID != "claude" || m.ChosenBy != "process" || m.From != "vendored" || m.Version == "" {
		t.Errorf("manifest %+v", res.Manifest)
	}
	s := res.Screen
	if s.State != "idle" || s.Rule != "live_prompt_box" || s.Cols != 80 || s.Rows != 24 || s.Lines != 6 {
		t.Errorf("screen %+v", *s)
	}
	var win *api.DetectRule
	for i := range s.Rules {
		if s.Rules[i].ID == s.Rule {
			win = &s.Rules[i]
		}
	}
	if win == nil || !win.Matched || win.Region != "prompt_box_body" || strings.TrimSpace(win.Text) != "❯" {
		t.Errorf("winning rule %+v", win)
	}

	// By session id and by name; refusals.
	for _, p := range []api.DetectExplainParams{{Session: sc.Session}, {Session: "explained"}} {
		if r, err := explain(t, e, p); err != nil || r.Pane != sc.Pane {
			t.Errorf("%+v: pane %q, %v", p, r.Pane, err)
		}
	}
	for _, tc := range []struct {
		p    api.DetectExplainParams
		kind api.ErrorKind
	}{
		{api.DetectExplainParams{}, api.ErrorKindInvalid},
		{api.DetectExplainParams{Pane: sc.Pane, Session: sc.Session}, api.ErrorKindInvalid},
		{api.DetectExplainParams{Pane: "nope"}, api.ErrorKindInvalid},
		{api.DetectExplainParams{Pane: "00000000-0000-0000-0000-000000000001"}, api.ErrorKindNotFound},
		{api.DetectExplainParams{Session: "nonesuch"}, api.ErrorKindNotFound},
	} {
		if _, err := explain(t, e, tc.p); errKind(err) != tc.kind {
			t.Errorf("%+v: %v, want %s", tc.p, err, tc.kind)
		}
	}

	// The CLI.
	out := run(t, "detect", "explain", "-socket", e.sock, "explained")
	for _, want := range []string{
		"state     claude working, set by osc",
		"strongest this turn: osc",
		"process   group ",
		`claude matched by "claude"`,
		"manifest  claude ",
		"chosen by process",
		"screen    idle by rule live_prompt_box (priority",
		"region prompt_box_body",
		"            | ❯",
		"<- won",
		"verdict   working from osc. The screen says idle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CLI output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no    ") {
		t.Errorf("without -all the CLI lists rules that did not match:\n%s", out)
	}
	if all := run(t, "detect", "explain", "-socket", e.sock, "-all", sc.Pane); !strings.Contains(all, "no    ") {
		t.Errorf("-all lists no rule that missed:\n%s", all)
	}
	js := run(t, "detect", "explain", "-socket", e.sock, "-json", sc.Session)
	if !strings.Contains(js, `"chosen_by": "process"`) || !strings.Contains(js, `"rule": "live_prompt_box"`) {
		t.Errorf("JSON output:\n%s", js)
	}
	cmd := exec.Command(bin, "detect", "explain", "-socket", e.sock, "nonesuch")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 3 || !strings.Contains(stderr.String(), "nonesuch") {
		t.Errorf("unknown session: %v, exit %d, %s", err, cmd.ProcessState.ExitCode(), stderr.String())
	}
}

// TestExplainVerdict covers the verdict's cases without a hab.
func TestExplainVerdict(t *testing.T) {
	working := &api.AgentState{Harness: "claude", State: api.AgentStatusWorking, Source: api.StateSourceHook}
	tests := []struct {
		r    api.DetectExplain
		want string
	}{
		{api.DetectExplain{Process: api.DetectProcess{Match: "claude"}, Screen: &api.DetectScreen{State: "working"}, Agent: working},
			"working from hook. The screen agrees."},
		{api.DetectExplain{Process: api.DetectProcess{Match: "claude"}, Screen: &api.DetectScreen{State: "unknown"}, Agent: working},
			"working from hook."},
		{api.DetectExplain{Process: api.DetectProcess{Match: "codex"}, Agent: working},
			"working from hook. The foreground runs codex, not claude."},
		{api.DetectExplain{Agent: working}, "working from hook. No foreground process matches claude."},
		{api.DetectExplain{Process: api.DetectProcess{Error: "x"}, Agent: working}, "working from hook."},
		{api.DetectExplain{Process: api.DetectProcess{Match: "claude"}, Screen: &api.DetectScreen{State: "blocked"}},
			"no agent state. The screen says blocked; screen rules are a fallback this build shows but does not apply."},
	}
	for _, tt := range tests {
		if got := verdict(tt.r); got != tt.want {
			t.Errorf("verdict = %q, want %q", got, tt.want)
		}
	}
}

func TestRegionTail(t *testing.T) {
	if s, cut := regionTail("abc", 5); s != "abc" || cut {
		t.Errorf("short: %q %v", s, cut)
	}
	if s, cut := regionTail("line one\nline two\nend", 12); s != "end" || !cut {
		t.Errorf("line boundary: %q %v", s, cut)
	}
	if s, cut := regionTail("ééé", 3); s != "é" || !cut {
		t.Errorf("rune boundary: %q %v", s, cut)
	}
}

// staleDialog is a Claude permission dialog that scrolls into scrollback before the screen is
// cleared (ESC[H ESC[2J, as Ctrl-L does), leaving one line on screen.
var staleDialog = `● I'll remove the build directory.

──────────────────────────────────────────────────
 Bash command

   rm -rf build
   Remove build output

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for rm commands in this folder
   3. No, and tell Claude what to do differently (esc)

 Esc to cancel · Tab to amend
` + strings.Repeat("filler\n", 24) + "\033[H\033[2Jhello\n"

// TestDetectExplainVisibleOnly: the screen rules read the active screen only. A permission
// dialog in scrollback, above a cleared screen, does not make the screen say blocked, and the
// rules read one line.
func TestDetectExplainVisibleOnly(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	e := newHabEnv(t, 0)
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	writeFile(t, filepath.Join(e.work, "screen"), staleDialog)
	if err := os.WriteFile(filepath.Join(e.work, "claude"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var sc api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Argv: []string{"/bin/sh", "-c", `cat "$DIR/screen"; exec "$DIR/claude" 60`}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}}, &sc); err != nil {
		t.Fatal(err)
	}
	var res api.DetectExplain
	waitUntil(t, "explain to see claude in the foreground and hello on screen", func() bool {
		r, err := explain(t, e, api.DetectExplainParams{Pane: sc.Pane})
		res = r
		return err == nil && r.Process.Match == "claude" && r.Screen != nil && r.Screen.Lines > 0
	})
	if s := res.Screen; s.State != "unknown" || s.Rule != "" || s.Lines != 1 || s.Rows != 24 {
		t.Errorf("screen %+v; want unknown from the one line on screen", *s)
	}
	for _, r := range res.Screen.Rules {
		if strings.Contains(r.Text, "proceed") {
			t.Errorf("rule %s read scrollback: %q", r.ID, r.Text)
		}
	}
}
