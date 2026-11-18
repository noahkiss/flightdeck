package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// TestFlightRulesService: a session's ledger comes from the transcript its agent's native id
// names; it stays readable after the agent leaves; a transcript not written yet is an empty
// ledger; a harness with no reader, an unknown session and an unknown entry answer with their
// error kinds.
func TestFlightRulesService(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "flightrules", "testdata", "claude-2.1.327.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "native-1.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	agents := map[string]api.AgentState{"p1": {Harness: "claude", Session: "native-1"}, "p3": {Harness: "claude", Session: "native-3"}, "p4": {Harness: "pi", Session: "x"}}
	sessions := map[string]api.Session{"s1": {ID: "s1", Pane: "p1"}, "s2": {ID: "s2"}, "s3": {ID: "s3", Pane: "p3"}, "s4": {ID: "s4", Pane: "p4"}}
	f := &flightRulesService{
		session: func(id string) (api.Session, bool) { s, ok := sessions[id]; return s, ok },
		agent:   func(pane string) (api.AgentState, bool) { a, ok := agents[pane]; return a, ok },
		find: func(_, native string) (string, error) {
			p := filepath.Join(dir, native+".jsonl")
			if _, err := os.Stat(p); err != nil {
				return "", transcript.ErrNoTranscript
			}
			return p, nil
		},
		state: t.TempDir(), now: time.Now, ledgers: map[string]*sessionLedger{},
	}
	ctx := context.Background()
	kind := func(err error) api.ErrorKind {
		var ce *control.Error
		if errors.As(err, &ce) {
			return ce.Data.Kind
		}
		return ""
	}

	// A read that runs out of time answers with what it read and a warning.
	cut, cancel := context.WithCancel(ctx)
	cancel()
	if r, err := f.List(cut, api.FlightRulesParams{Session: "s1"}); err != nil || len(r.Entries) != 0 || len(r.Warnings) != 1 {
		t.Fatalf("cut read: %+v %v", r, err)
	}
	r, err := f.List(ctx, api.FlightRulesParams{Session: "s1"})
	if err != nil || r.Harness != "claude" || len(r.Entries) != 26 || len(r.Warnings) != 1 {
		t.Fatalf("list: %d entries, %v", len(r.Entries), err)
	}
	e := r.Entries[0]
	tx, err := f.Text(ctx, api.FlightRuleTextParams{Session: "s1", ID: e.ID})
	if err != nil || tx.Text != "Managed policy text." || tx.Bytes != int64(len(tx.Text)) {
		t.Fatalf("text: %+v %v", tx, err)
	}
	if _, err := f.Text(ctx, api.FlightRuleTextParams{Session: "s1", ID: "nope"}); kind(err) != api.ErrorKindNotFound {
		t.Errorf("unknown entry: %v", err)
	}

	// The agent left: the last ledger stays readable.
	delete(agents, "p1")
	if r, err := f.List(ctx, api.FlightRulesParams{Session: "s1"}); err != nil || len(r.Entries) != 26 {
		t.Errorf("after the agent left: %d %v", len(r.Entries), err)
	}

	if r, err := f.List(ctx, api.FlightRulesParams{Session: "s3"}); err != nil || len(r.Entries) != 0 || r.Entries == nil || r.Harness != "claude" {
		t.Errorf("not written yet: %+v %v", r, err)
	}
	// Once written, the same session reads it.
	if err := os.WriteFile(filepath.Join(dir, "native-3.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := f.List(ctx, api.FlightRulesParams{Session: "s3"}); err != nil || len(r.Entries) != 26 {
		t.Errorf("written later: %d %v", len(r.Entries), err)
	}

	if _, err := f.List(ctx, api.FlightRulesParams{Session: "s2"}); kind(err) != api.ErrorKindUnavailable {
		t.Errorf("no agent: %v", err)
	}
	if _, err := f.List(ctx, api.FlightRulesParams{Session: "s4"}); kind(err) != api.ErrorKindUnavailable {
		t.Errorf("pi: %v", err)
	}

	// A Codex session reads its rollout; an opencode session whose server is not running is
	// an empty ledger with a warning.
	rollout, err := os.ReadFile(filepath.Join("..", "..", "internal", "flightrules", "testdata", "codex-0.169.0.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-5.jsonl"), rollout, 0o600); err != nil {
		t.Fatal(err)
	}
	f.codexHome = t.TempDir()
	agents["p5"], sessions["s5"] = api.AgentState{Harness: "codex", Session: "native-5"}, api.Session{ID: "s5", Pane: "p5"}
	if r, err := f.List(ctx, api.FlightRulesParams{Session: "s5"}); err != nil || r.Harness != "codex" || len(r.Entries) == 0 {
		t.Errorf("codex: %+v %v", r, err)
	}
	agents["p6"], sessions["s6"] = api.AgentState{Harness: "opencode", Session: "ses_01"}, api.Session{ID: "s6", Pane: "p6"}
	if r, err := f.List(ctx, api.FlightRulesParams{Session: "s6"}); err != nil || r.Harness != "opencode" || len(r.Entries) != 0 || len(r.Warnings) != 1 {
		t.Errorf("opencode with no server: %+v %v", r, err)
	}

	// A render record of another digest than the fixture's rendered rules file makes the
	// Claude session stale.
	rec := `{"harness":"claude","target":"/home/user/.claude/rules/flightdeck.md","digest":"` + strings.Repeat("cd", 32) + `","renderer":1,"sources":[]}`
	if err := os.MkdirAll(filepath.Join(f.state, "instructions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, "instructions", "claude.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	agents["p1"] = api.AgentState{Harness: "claude", Session: "native-1"}
	if r, err := f.List(ctx, api.FlightRulesParams{Session: "s1"}); err != nil || !r.Stale {
		t.Errorf("stale claude session: stale %v, %v", r.Stale, err)
	}
	if _, err := f.List(ctx, api.FlightRulesParams{Session: "nope"}); kind(err) != api.ErrorKindNotFound {
		t.Errorf("unknown session: %v", err)
	}

	// A removed session's ledger is dropped.
	delete(sessions, "s3")
	f.List(ctx, api.FlightRulesParams{Session: "s1"})
	if _, ok := f.ledgers["s3"]; ok {
		t.Error("a removed session's ledger stayed")
	}
}
