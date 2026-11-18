package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// TestStaleWatch: with no render record nothing is read and nothing is stale. With a record of
// another digest than the one the Claude Code session loaded, the sweep flags it; an opencode
// session is never flagged; a flightrules.list agrees; after a restart only a load since then
// counts; an ended session loses the flag.
func TestStaleWatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "flightrules", "testdata", "claude-2.1.327.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-1.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	h := structure.NewFakeHolder(structure.HolderPane{ID: "p1", HolderID: "h1"}, structure.HolderPane{ID: "p2", HolderID: "h1"})
	st, err := structure.Open(structure.Config{Dir: t.TempDir(), Holders: h})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	claude, _ := st.CreateSession(structure.SessionSpec{Name: "claude", Pane: "p1"})
	oc, _ := st.CreateSession(structure.SessionSpec{Name: "oc", Pane: "p2"})
	agents := map[string]api.AgentState{"p1": {Harness: "claude", Session: "native-1"}, "p2": {Harness: "opencode", Session: "ses_1"}}
	agent := func(p string) (api.AgentState, bool) { a, ok := agents[p]; return a, ok }
	state := t.TempDir()
	w := &staleWatch{store: st, agent: agent, state: state}
	f := &flightRulesService{session: st.Session, agent: agent, state: state, now: time.Now,
		ledgers: map[string]*sessionLedger{}, mark: w.listed,
		find: func(_, native string) (string, error) {
			p := filepath.Join(dir, native+".jsonl")
			if _, err := os.Stat(p); err != nil {
				return "", transcript.ErrNoTranscript
			}
			return p, nil
		}}
	w.rules = f
	ctx := context.Background()
	staleOf := func(id string) []string { s, _ := st.Session(id); return s.Stale }

	w.sweep(ctx)
	if staleOf(claude.ID) != nil || len(f.ledgers) != 0 {
		t.Fatalf("with no record: stale %v, %d ledgers read", staleOf(claude.ID), len(f.ledgers))
	}

	rec := `{"harness":"claude","target":"/home/user/.claude/rules/flightdeck.md","digest":"` + strings.Repeat("cd", 32) + `","renderer":1,"sources":[]}`
	if err := os.MkdirAll(filepath.Join(state, "instructions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "instructions", "claude.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	w.sweep(ctx)
	if got := staleOf(claude.ID); !slices.Equal(got, []string{"instructions"}) || staleOf(oc.ID) != nil {
		t.Fatalf("after a new render: claude %v, opencode %v", got, staleOf(oc.ID))
	}
	st.SetStale(claude.ID, nil)
	if r, err := f.List(ctx, api.FlightRulesParams{Session: claude.ID}); err != nil || !r.Stale || staleOf(claude.ID) == nil {
		t.Fatalf("list: %v %v, session %v", r.Stale, err, staleOf(claude.ID))
	}
	// After a restart, the old program's load no longer counts: the flag stays clear until the
	// new program records a stale load of its own.
	w.restarted(claude.ID, time.Now())
	w.sweep(ctx)
	if staleOf(claude.ID) != nil {
		t.Fatalf("stale again right after a restart: %v", staleOf(claude.ID))
	}
	w.restarted(claude.ID, time.Time{})
	w.sweep(ctx)
	if staleOf(claude.ID) == nil {
		t.Fatal("a stale load after the restart is not flagged")
	}
	h.Exit("p1", api.ExitStatus{})
	waitUntil(t, "the exit", func() bool { s, _ := st.Session(claude.ID); return s.State == api.SessionStateExited })
	w.sweep(ctx)
	if staleOf(claude.ID) != nil {
		t.Fatalf("an ended session stays stale: %v", staleOf(claude.ID))
	}
}
