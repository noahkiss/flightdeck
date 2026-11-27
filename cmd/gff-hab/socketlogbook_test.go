package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

func lbSet(id, field string, v any) api.LogbookChange {
	b, _ := json.Marshal(v)
	return api.LogbookChange{Op: api.LogbookChangeOpSet, ID: id, Field: field, Value: api.RawJSON(b)}
}

// TestSocketLogbookPaneCaller: on the control socket, a process inside a pane writes Logbook as
// an agent, under the pane route's rules: no decision on a proposal, no project marked private,
// nothing in a private project, and a proposal under logbook.approval = agents. The person's
// calls are unchanged (hab-logic-api.md 4.1; logbook.md 10.5).
func TestSocketLogbookPaneCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := logbook.Open(ctx, filepath.Join(t.TempDir(), "logbook.db"), logbook.Options{Origin: "SHA256:habA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc, stop, err := startLogbookVerbs(ctx, st, nil, func() {}, func(structure.Event) {}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	approval := false
	lb := socketLogbook{Service: svc, host: "test-host", approval: func() bool { return approval }}
	person := api.LogbookActor{Kind: "person", Device: "phone"}
	agent := api.LogbookActor{Kind: "agent", Device: "gff"} // what the control server passes
	pane := control.WithPaneCall(ctx)

	secret, err := svc.Sight(ctx, lbsvc.Sighting{Dir: "/src/diary", Identity: lbsvc.Identity("git@example.com:me/diary.git", ""), Host: "test-host"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := svc.Sight(ctx, lbsvc.Sighting{Dir: "/src/app", Identity: lbsvc.Identity("git@example.com:o/app.git", ""), Host: "test-host"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := lb.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{
		{Op: api.LogbookChangeOpCreate, Type: "task", Title: "secret work", Project: secret},
	}})
	if err != nil {
		t.Fatal(err)
	}
	secretTask := w.IDs[0]
	if _, err := lb.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{lbSet(secret, "private", true)}}); err != nil {
		t.Fatalf("the person marks a project private: %v", err)
	}
	w, err = svc.WritePane(ctx, lbsvc.Pane{ID: "p1", Session: "s1"}, api.LogbookWriteParams{Propose: true,
		Changes: []api.LogbookChange{{Op: api.LogbookChangeOpCreate, Type: "task", Title: "proposed", Project: open}}})
	if err != nil {
		t.Fatal(err)
	}
	prop := w.Proposal

	for name, ch := range map[string]api.LogbookChange{
		"a proposal's status":      lbSet(prop, "status", "approved"),
		"decided_by":               lbSet(prop, "decided_by", "phone"),
		"private":                  lbSet(open, "private", true),
		"a private project's task": lbSet(secretTask, "title", "seen"),
	} {
		if _, err := lb.Write(pane, agent, api.LogbookWriteParams{Changes: []api.LogbookChange{ch}}); err == nil {
			t.Errorf("logbook.write of %s from a pane worked", name)
		}
	}
	if _, err := lb.Resolve(pane, agent, api.LogbookResolveParams{ID: prop, Field: "status", Value: api.RawJSON(`"approved"`)}); err == nil || !strings.Contains(err.Error(), "proposal") {
		t.Errorf("logbook.resolve of a proposal's status from a pane: %v", err)
	}
	if _, err := lb.Resolve(pane, agent, api.LogbookResolveParams{ID: secretTask, Field: "title", Value: api.RawJSON(`"seen"`)}); err == nil {
		t.Error("logbook.resolve in a private project from a pane worked")
	}
	if _, err := lb.Decide(pane, agent, api.LogbookDecideParams{ID: prop, Decision: api.LogbookDecisionApprove}); err == nil {
		t.Error("logbook.decide from a pane worked")
	}

	// A plain write from a pane lands as an agent's, or as a proposal under approval = agents.
	w, err = lb.Write(pane, agent, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpCreate, Type: "task", Title: "an agent's task", Project: open}}})
	if err != nil || w.State != api.LogbookWriteStateApplied {
		t.Fatalf("a plain write from a pane: %+v %v", w, err)
	}
	g, err := svc.Get(ctx, api.LogbookGetParams{ID: w.IDs[0]})
	if err != nil || g.Record.CreatedBy == nil || g.Record.CreatedBy.Kind != "agent" {
		t.Fatalf("the pane's write is not an agent's: %+v %v", g.Record.CreatedBy, err)
	}
	approval = true
	w, err = lb.Write(pane, agent, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpCreate, Type: "task", Title: "waits", Project: open}}})
	if err != nil || w.State != api.LogbookWriteStateProposed {
		t.Fatalf("a write from a pane under approval = agents: %+v %v", w, err)
	}

	// The person's calls are unchanged.
	if _, err := lb.Resolve(ctx, person, api.LogbookResolveParams{ID: secretTask, Field: "title", Value: api.RawJSON(`"renamed"`)}); err != nil {
		t.Fatalf("the person's resolve: %v", err)
	}
	if _, err := lb.Decide(ctx, person, api.LogbookDecideParams{ID: prop, Decision: api.LogbookDecisionReject}); err != nil {
		t.Fatalf("the person's decide: %v", err)
	}
}
