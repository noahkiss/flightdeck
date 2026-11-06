package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habits"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
)

// habitHook runs `gff-hab hook <harness> <event>` with payload on stdin and returns what it
// printed and how long it took. It always exits 0.
func habitHook(t *testing.T, env []string, harness, event, payload string) (string, time.Duration) {
	t.Helper()
	cmd := exec.Command(bin, "hook", harness, event)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("gff-hab hook %s %s: %v", harness, event, err)
	}
	return out.String(), time.Since(start)
}

func setHabit(t *testing.T, c *control.Client, id string, on bool) {
	t.Helper()
	var cur api.SettingsResult
	call(t, c, "settings.get", api.SettingsGetParams{IDs: []string{id}}, &cur)
	var set api.SettingsResult
	v, _ := json.Marshal(on)
	call(t, c, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{{ID: id, Value: v, Revision: cur.Settings[0].Revision}}}, &set)
}

// TestLogbookHabits: the recording habits (logbook.md 11, 18 row 6) in a real gff-hab with its
// holder, through the real `gff-hab hook` command from a session's pane in a repository. The
// brief at a Claude Code, Codex and opencode start is the project's brief in each harness's
// shape; 20 prompts after an edit bring the reminder; the first stop after an edit with no
// Logbook write brings the closeout once, and a session that wrote gets none; each habit's
// setting turns it off, and so does [logbook.habits_off] for the project; with gff-hab stopped
// the brief still prints from the file within the hook's budget, under the same switches, and
// the reminder and closeout print nothing.
func TestLogbookHabits(t *testing.T) {
	g := newGitRepos(t)
	e := newHolderEnv(t, 0)
	cfg := filepath.Join(e.work, "hab.toml")
	e.env = append(g.env, "GFF_HAB_CONFIG="+cfg)
	e.serve()
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	app := g.repo(filepath.Join(e.work, "app"), "git@example.com:o/app.git")
	c := e.control()
	pane := startIn(t, e, "main", app)
	p := waitProject(t, c, "example.com/o/app", nil)
	task := lbTask(t, c, p.ID, "Fix the cart")
	// The pane's env as the hab's: the config file named, so the file brief reads its switches.
	inPane := []string{"GFF_HAB=" + pane, "GFF_HAB_STATE=" + e.state, "GFF_HAB_CONFIG=" + cfg, "CLAUDE_CODE_ENTRYPOINT=cli"}
	start := `{"session_id":"s1","cwd":"` + app + `","hook_event_name":"SessionStart","source":"startup"}`

	var brief api.LogbookBrief
	call(t, c, "logbook.brief", api.LogbookBriefParams{Project: p.ID}, &brief)
	if !strings.Contains(brief.Text, task+"  open  Fix the cart\n") {
		t.Fatalf("brief:\n%s", brief.Text)
	}
	if out, _ := habitHook(t, inPane, "claude", "SessionStart", start); out != string(hook.ContextOutput("claude", "SessionStart", brief.Text)) {
		t.Fatalf("claude brief:\n%q\nwant the brief\n%q", out, brief.Text)
	}
	if out, _ := habitHook(t, inPane, "codex", "SessionStart", start); out != string(hook.ContextOutput("codex", "SessionStart", brief.Text)) {
		t.Fatalf("codex brief: %q", out)
	}
	// opencode's brief goes through its server (TestOpencodeBrief), never through the hook.
	if out, _ := habitHook(t, inPane, "opencode", "session.created", `{"type":"session.created","properties":{"info":{"id":"ses_1"}}}`); out != "" {
		t.Fatalf("opencode brief through the hook: %q", out)
	}
	// The brief through the CLI in the pane is the same text: the pane's project by default.
	if out, _ := lbOK(t, inPane, "brief"); out != brief.Text {
		t.Fatalf("gff lb brief in the pane:\n%s", out)
	}

	// No change yet: no reminder and no closeout.
	prompt := `{"session_id":"s1","cwd":"` + app + `","hook_event_name":"UserPromptSubmit","prompt":"go"}`
	stop := `{"session_id":"s1","cwd":"` + app + `","hook_event_name":"Stop","stop_hook_active":false}`
	edit := `{"session_id":"s1","cwd":"` + app + `","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"` + app + `/a.go"}}`
	if out, _ := habitHook(t, inPane, "claude", "Stop", stop); out != "" {
		t.Fatalf("closeout with no change: %q", out)
	}
	if out, _ := habitHook(t, inPane, "claude", "PostToolUse", edit); out != "" {
		t.Fatalf("PostToolUse printed %q", out)
	}
	for i := 1; i < habits.ReminderTurns; i++ {
		if out, _ := habitHook(t, inPane, "claude", "UserPromptSubmit", prompt); out != "" {
			t.Fatalf("prompt %d: %q", i, out)
		}
	}
	if out, _ := habitHook(t, inPane, "claude", "UserPromptSubmit", prompt); out != string(hook.ContextOutput("claude", "UserPromptSubmit", habits.ReminderText)) {
		t.Fatalf("no reminder at prompt %d: %q", habits.ReminderTurns, out)
	}
	if out, _ := habitHook(t, inPane, "claude", "UserPromptSubmit", prompt); out != "" {
		t.Fatalf("a second reminder within the hour: %q", out)
	}
	if out, _ := habitHook(t, inPane, "claude", "Stop", stop); out != string(hook.ContextOutput("claude", "Stop", habits.CloseoutText)) {
		t.Fatalf("no closeout: %q", out)
	}
	habitHook(t, inPane, "claude", "PostToolUse", edit)
	if out, _ := habitHook(t, inPane, "claude", "Stop", stop); out != "" {
		t.Fatalf("a second closeout: %q", out)
	}

	// A session that wrote to Logbook gets no closeout.
	other := []string{"GFF_HAB=" + startIn(t, e, "other", app), "GFF_HAB_STATE=" + e.state, "CLAUDE_CODE_ENTRYPOINT=cli"}
	habitHook(t, other, "codex", "PostToolUse", `{"session_id":"x","hook_event_name":"PostToolUse","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch"}}`)
	lbOK(t, other, "note", task, "found the cause")
	if out, _ := habitHook(t, other, "codex", "Stop", stop); out != "" {
		t.Fatalf("closeout after a write: %q", out)
	}

	// Each setting turns its habit off; so does the project's switch.
	third := []string{"GFF_HAB=" + startIn(t, e, "third", app), "GFF_HAB_STATE=" + e.state, "CLAUDE_CODE_ENTRYPOINT=cli"}
	setHabit(t, c, "logbook.habits.brief", false)
	setHabit(t, c, "logbook.habits.closeout", false)
	habitHook(t, third, "claude", "PostToolUse", edit)
	if out, _ := habitHook(t, third, "claude", "SessionStart", start); out != "" {
		t.Fatalf("brief with its setting off: %q", out)
	}
	if out, _ := habitHook(t, third, "claude", "Stop", stop); out != "" {
		t.Fatalf("closeout with its setting off: %q", out)
	}
	setHabit(t, c, "logbook.habits.brief", true)
	setHabit(t, c, "logbook.habits.closeout", true)
	writeFile(t, cfg, "[logbook.habits_off]\n\"example.com/o/app\" = [\"brief\", \"closeout\"]\n")
	if out, _ := habitHook(t, third, "claude", "SessionStart", start); out != "" {
		t.Fatalf("brief turned off for the project: %q", out)
	}
	if out, _ := habitHook(t, third, "claude", "Stop", stop); out != "" {
		t.Fatalf("closeout turned off for the project: %q", out)
	}
	writeFile(t, cfg, "")
	if out, _ := habitHook(t, third, "claude", "Stop", stop); out != string(hook.ContextOutput("claude", "Stop", habits.CloseoutText)) {
		t.Fatalf("closeout back on: %q", out)
	}

	// gff-hab stopped, with 1,000 more open tasks: the brief comes from the file, within the
	// budget and under 8 KB; nothing else prints.
	for range 5 {
		var chs []api.LogbookChange
		for i := range 200 {
			chs = append(chs, api.LogbookChange{Op: "create", Type: "task", Title: fmt.Sprintf("open task %d with a longer title", i), Project: p.ID})
		}
		var w api.LogbookWritten
		call(t, c, "logbook.write", api.LogbookWriteParams{Changes: chs}, &w)
	}
	// The file brief honours the same switches (habits.On): the setting gff-hab saved in
	// settings.json, a [settings] lock over it, the project's [logbook.habits_off].
	setHabit(t, c, "logbook.habits.brief", false)
	e.stopHab(false)
	if out, _ := habitHook(t, inPane, "claude", "SessionStart", start); out != "" {
		t.Fatalf("brief from the file with its setting off: %q", out)
	}
	writeFile(t, cfg, "[settings]\n\"logbook.habits.brief\" = true\n[logbook.habits_off]\n\"example.com/o/app\" = [\"brief\"]\n")
	if out, _ := habitHook(t, inPane, "claude", "SessionStart", start); out != "" {
		t.Fatalf("brief from the file turned off for the project: %q", out)
	}
	writeFile(t, cfg, "[settings]\n\"logbook.habits.brief\" = true\n")
	out, took := habitHook(t, inPane, "claude", "SessionStart", start)
	var got struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	json.Unmarshal([]byte(out), &got)
	text := got.HookSpecificOutput.AdditionalContext
	if !strings.HasPrefix(text, "Logbook brief for app") || len(text) > lbsvc.BriefCap || !strings.HasSuffix(text, lbsvc.BriefMore) {
		t.Fatalf("brief from the file (%v, %d bytes): %q", took, len(text), out)
	}
	if took > hook.Budget+100*time.Millisecond {
		t.Fatalf("the brief from the file took %v", took)
	}
	for _, ev := range []struct{ name, payload string }{{"UserPromptSubmit", prompt}, {"Stop", stop}} {
		if out, _ := habitHook(t, inPane, "claude", ev.name, ev.payload); out != "" {
			t.Fatalf("%s with gff-hab stopped: %q", ev.name, out)
		}
	}
}
