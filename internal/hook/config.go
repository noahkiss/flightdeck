package hook

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/noahkiss/flightdeck/internal/detect"
)

// Events each harness's hook config subscribes, in the order printed.
var (
	ClaudeEvents   = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "Notification", "Stop", "StopFailure", "SessionEnd"}
	CodexEvents    = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "Stop", "Interrupt", "SessionEnd"}
	OpencodeEvents = []string{"session.created", "session.deleted", "session.status", "session.idle", "session.error", "permission.asked", "permission.updated", "permission.replied"}
)

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroup struct {
	Hooks []hookCmd `json:"hooks"`
}

// Config returns the hook config for harness, ready to merge into its settings by hand.
// command is how the harness should run gff-hab ("gff-hab" when on PATH). Claude Code and Codex
// get JSON in their shared hooks shape; opencode gets a plugin file in JavaScript. Config only
// prints; writing a harness's settings is a separate step the owner approves.
func Config(harness, command string) (string, error) {
	if command == "" {
		command = "gff-hab"
	}
	switch harness {
	case detect.KindClaude:
		// PermissionRequest runs with -wait: it returns at once unless the owner turned the
		// blocking permission hook on in gff-hab, and its timeout covers the longest wait.
		return hooksJSON(command, harness, ClaudeEvents, map[string]int{"PermissionRequest": PermissionTimeout}, map[string]string{"PermissionRequest": "-wait "})
	case detect.KindCodex:
		// Codex caps its end hooks at three seconds. PermissionRequest waits as Claude's does.
		return hooksJSON(command, harness, CodexEvents, map[string]int{"SessionEnd": 2, "Interrupt": 2, "PermissionRequest": PermissionTimeout}, map[string]string{"PermissionRequest": "-wait "})
	case detect.KindOpencode:
		return opencodePlugin(command), nil
	}
	return "", fmt.Errorf("no hook config for harness %q; known: claude, codex, opencode", harness)
}

// PermissionTimeout is the hook timeout, in seconds, of the PermissionRequest hook: above
// the longest wait the hab holds a hook (harness.MaxPermissionWait plus a margin).
const PermissionTimeout = 130

func hooksJSON(command, harness string, events []string, timeouts map[string]int, flags map[string]string) (string, error) {
	// Build the object by hand to keep the events in a stable, readable order.
	var b strings.Builder
	b.WriteString("{\n  \"hooks\": {\n")
	for i, ev := range events {
		t := 5
		if v, ok := timeouts[ev]; ok {
			t = v
		}
		g, err := json.Marshal([]hookGroup{{Hooks: []hookCmd{{Type: "command", Command: shellQuote(command) + " hook " + flags[ev] + harness + " " + ev, Timeout: t}}}})
		if err != nil {
			return "", err
		}
		key, _ := json.Marshal(ev)
		fmt.Fprintf(&b, "    %s: %s", key, g)
		if i < len(events)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  }\n}\n")
	return b.String(), nil
}

// shellQuote quotes a command path for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func opencodePlugin(command string) string {
	events, _ := json.Marshal(OpencodeEvents)
	cmd, _ := json.Marshal(command)
	return `// gff-hab hook plugin for opencode. Save it as a plugin file, for example
// .opencode/plugin/gff-hab.js in a project or the plugin directory of your opencode config.
// It posts the events below to gff-hab and does nothing outside a pane gff-hab owns.
const EVENTS = new Set(` + string(events) + `)

export const GffHab = async () => ({
  event: async ({ event }) => {
    if (!process.env.GFF_HAB || !EVENTS.has(event.type)) return
    try {
      // Stamp the time here: each event spawns its own process, and those may run out of order.
      const proc = Bun.spawn([` + string(cmd) + `, "hook", "-t", String(Date.now()), "opencode", event.type], {
        stdin: "pipe",
        stdout: "ignore",
        stderr: "ignore",
      })
      proc.stdin.write(JSON.stringify(event))
      proc.stdin.end()
    } catch {}
  },
})
`
}
