package main

// The start mode (harness-adapters.md, Start mode): the harness's own permission flag the hab
// adds when it starts a harness through session.create. agent.start_mode is a levelled setting
// (app.md 20.3); the session keeps the mode it started with, and resume, restart and recreate
// start it again with that mode.

import (
	"context"
	"slices"
	"strings"

	"github.com/noahkiss/flightdeck/internal/detect"
)

// The values of agent.start_mode.
const (
	startAsk    = "ask"
	startAuto   = "auto"
	startBypass = "bypass"
)

// startFlags are the flags each start mode adds, per harness, checked against each harness's
// --help (Claude Code 2.1.329, Codex 0.171.0). A harness with no row is unverified: the hab adds
// nothing and logs a line at each start.
var startFlags = map[string]map[string][]string{
	detect.KindClaude: {
		startAuto:   {"--permission-mode", "auto"},
		startBypass: {"--permission-mode", "bypassPermissions"},
	},
	detect.KindCodex: {
		// Codex's automatic review of approval requests, in the workspace-write sandbox: the
		// closest to Claude Code's auto mode. Codex 0.171.0 has no --full-auto.
		startAuto:   {"--approve-for-me"},
		startBypass: {"--dangerously-bypass-approvals-and-sandbox"},
	},
}

// claudeModeFlags and codexModeFlags name a permission mode when argv already holds one: then
// the hab adds nothing. Codex's -c keys count too.
var (
	claudeModeFlags = []string{"--permission-mode", "--dangerously-skip-permissions"}
	codexModeFlags  = []string{"-s", "--sandbox", "-a", "--ask-for-approval", "--full-auto",
		"--dangerously-bypass-approvals-and-sandbox", "--yolo", "--approve-for-me"}
	// codexModeKeys are the config keys a -c override may set the mode with.
	codexModeKeys = []string{"sandbox_mode", "approval_policy"}
)

// namesMode reports whether argv (past the program) already names a permission mode for the
// harness.
func namesMode(harness string, argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	args := argv[1:]
	switch harness {
	case detect.KindClaude:
		return slices.ContainsFunc(args, func(a string) bool {
			name, _, _ := strings.Cut(a, "=")
			return slices.Contains(claudeModeFlags, name)
		})
	case detect.KindCodex:
		for i, a := range args {
			name, val, inline := strings.Cut(a, "=")
			if slices.Contains(codexModeFlags, name) {
				return true
			}
			if name == "-c" || name == "--config" {
				if !inline {
					if i+1 >= len(args) {
						continue
					}
					val = args[i+1]
				}
				key, _, _ := strings.Cut(val, "=")
				if slices.Contains(codexModeKeys, strings.TrimSpace(key)) {
					return true
				}
			}
		}
	}
	return false
}

// withStartMode adds a harness's flags for mode to argv, right after the program. It returns
// the new argv and the mode it added: "" when it added nothing (ask, a harness with no row, or
// argv that names a mode already). why is set when a mode was asked for and not added because
// the harness's row is unverified.
func withStartMode(harness string, argv []string, mode string) (out []string, added string, why string) {
	if mode == "" || mode == startAsk || len(argv) == 0 {
		return argv, "", ""
	}
	rows, ok := startFlags[harness]
	if !ok {
		return argv, "", harness + " has no checked flag for start mode " + mode + "; it starts in its own default"
	}
	flags, ok := rows[mode]
	if !ok {
		return argv, "", "start mode " + mode + " is not known; " + harness + " starts in its own default"
	}
	if namesMode(harness, argv) {
		return argv, "", ""
	}
	out = slices.Clone(argv)
	out = slices.Insert(out, 1, flags...)
	return out, mode, ""
}

// startModeKey carries a start mode a start must use in place of the current setting: the
// mode a recreated or swapped session was recorded with, or ask for an adopt.
type startModeKey struct{}

// withRecordedMode makes the create on ctx start with mode, not with agent.start_mode. An empty
// mode is a recorded ask: the create adds nothing.
func withRecordedMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, startModeKey{}, mode)
}

// recordedModeFrom returns the mode withRecordedMode set, and whether one was set.
func recordedModeFrom(ctx context.Context) (string, bool) {
	m, ok := ctx.Value(startModeKey{}).(string)
	return m, ok
}
