package main

import (
	"io"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
)

func roundCard(state api.ConfirmState) api.Confirmation {
	c := api.Confirmation{ID: "c1", Kind: "round", State: state, From: "agent", Reason: "the link dropped",
		Round: &api.ConfirmRound{Host: "bee", Trigger: "every 1h in UTC", Project: "app", RunNow: true,
			Round: api.RoundSpec{Name: "nightly", Mode: api.RoundModeFresh, Command: []string{"make", "check"}, Prompt: ""}}}
	if state == "done" {
		c.Result = &api.ConfirmResult{Host: "bee", Name: "nightly", Round: "round-1"}
	}
	return c
}

// TestRoundConfirmationText: confirmation_read's text for a round card in every state, never
// the start's words (a round card has no start); a card of an unknown kind with no start.
func TestRoundConfirmationText(t *testing.T) {
	for _, st := range []api.ConfirmState{"open", "running", "done", "failed", "cancelled", "expired", "unknown"} {
		got := confirmationText(roundCard(st))
		if !strings.Contains(strings.ToLower(got), "round nightly (every 1h in utc) on bee") || !strings.Contains(got, "c1") {
			t.Errorf("%s: %q", st, got)
		}
	}
	if got := confirmationText(roundCard("done")); !strings.Contains(got, "round-1") {
		t.Errorf("done: %q", got)
	}
	if got := confirmationText(api.Confirmation{ID: "c2", Kind: "later", State: "open"}); !strings.Contains(got, "c2") {
		t.Errorf("unknown kind: %q", got)
	}
}

// TestRoundConfirmationCard: gff-hab confirm show prints what a round card would make.
func TestRoundConfirmationCard(t *testing.T) {
	c := roundCard("open")
	c.Round.Round.Prompt, c.Round.Round.Command, c.Round.Round.Harness = "check the build", nil, "claude"
	got := confirmationCard(c)
	for _, w := range []string{"runner:    bee", "round:     nightly", "mode:      fresh", "trigger:   every 1h in UTC",
		"project:   app", "harness:   claude", "check the build", "run now:   yes", "asked by:  agent"} {
		if !strings.Contains(got, w) {
			t.Errorf("open card lacks %q:\n%s", w, got)
		}
	}
	if got := confirmationCard(roundCard("done")); !strings.Contains(got, "command:   make check") || !strings.Contains(got, "made:      round nightly on bee (round-1)") {
		t.Errorf("done card:\n%s", got)
	}
}

// TestRoundToolNulls: a null argument is no argument.
func TestRoundToolNulls(t *testing.T) {
	spec := api.RoundSpec{Name: "x", Mode: api.RoundModeFresh, Prompt: "keep me", ProjectDir: "/work/app",
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}}
	named, err := roundSpecArgs(&spec, toolArgs{"prompt": nil, "project": nil, "command": nil, "every": nil, "manual": nil, "max_runs": nil}, true)
	if err != nil || named || spec.Prompt != "keep me" || spec.ProjectDir != "/work/app" || spec.Trigger.Kind != api.RoundTriggerKindManual {
		t.Fatalf("nulls changed the round: %+v %v %v", spec, named, err)
	}
	if _, err := roundSpecArgs(&spec, toolArgs{"prompt": "new", "command": nil}, false); err != nil || spec.Prompt != "new" {
		t.Fatalf("a prompt beside a null command: %+v %v", spec, err)
	}
}

// TestRoundHostFromPane: from a pane, -socket or -via never turns -host into a direct dial.
func TestRoundHostFromPane(t *testing.T) {
	t.Setenv("GFF_HAB_SOCKET", "")
	t.Setenv(hook.Env, "http+unix:///nonexistent/hook.sock")
	for _, args := range [][]string{
		{"add", "far", "-host", "bee", "-socket", "/nonexistent/hab.sock", "-manual", "-command", "--", "true"},
		{"pause", "far", "-host", "bee", "-socket", "/nonexistent/hab.sock"},
	} {
		if err := roundCmd(args, strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "not available from a pane") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// TestRoundToolEvents: the round tools take stage 9's event trigger and message modes as the
// CLI's -on, -where, -debounce, -cooldown, -mode and -group do.
func TestRoundToolEvents(t *testing.T) {
	spec := api.RoundSpec{Name: "x", Mode: api.RoundModeFresh}
	if _, err := roundSpecArgs(&spec, toolArgs{"on": "logbook.updated", "where": []any{"status=done,dropped"},
		"debounce": "10s", "cooldown": "2m", "mode": "group", "group": "g-1", "prompt": "look"}, false); err != nil {
		t.Fatal(err)
	}
	tr := spec.Trigger
	if tr.Kind != api.RoundTriggerKindEvent || tr.Event != "logbook.updated" || tr.Filter == nil ||
		spec.DebounceMS != 10000 || spec.CooldownMS != 120000 || spec.Mode != "group" || spec.Group != "g-1" {
		t.Fatalf("event round: %+v", spec)
	}
	if _, err := roundSpecArgs(&api.RoundSpec{Name: "y"}, toolArgs{"where": []any{"a=b"}, "manual": true}, false); err == nil {
		t.Error("where with no on")
	}
}
