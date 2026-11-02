package lbsvc

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// propose files one agent write from a pane as a proposal, as logbook.approval = agents makes
// every pane's write (9.1), and returns its id.
func propose(t *testing.T, r *rig, changes ...api.LogbookChange) string {
	t.Helper()
	pane := Pane{ID: "pane-1", Session: "sess-1", Harness: "claude", Host: "box-a", Propose: true}
	w, err := r.svc.WritePane(context.Background(), pane, api.LogbookWriteParams{Changes: changes,
		Source: &api.LogbookSource{Quote: "note that the cert expires"}})
	if err != nil || w.State != api.LogbookWriteStateProposed || w.Proposal == "" {
		t.Fatalf("propose: %+v %v", w, err)
	}
	if !strings.Contains(w.Note, w.Proposal) || !strings.Contains(w.Note, "approval") || !strings.Contains(w.Note, "do not ask") {
		t.Fatalf("the note an agent reads: %q", w.Note)
	}
	return w.Proposal
}

// TestApproveAppliesDraft: approve applies the draft in one batch with the decision, as the
// proposing agent with approved_by; the new record links to the proposal; a repeated key
// applies once.
func TestApproveAppliesDraft(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	tw, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "existing")}})
	if err != nil {
		t.Fatal(err)
	}
	existing := tw.IDs[0]
	x := propose(t, r, create("finding", "the cert expires in May"), set(existing, "status", "doing"))
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "finding"}}); len(l.Records) != 0 {
		t.Fatalf("a proposed record landed before approval: %+v", l.Records)
	}
	p := api.LogbookDecideParams{ID: x, Decision: api.LogbookDecisionApprove, IdempotencyKey: "decide-1"}
	w, err := r.svc.Decide(ctx, person, p)
	if err != nil || w.State != api.LogbookWriteStateApplied || w.Proposal != x {
		t.Fatalf("approve: %+v %v", w, err)
	}
	var made string
	for _, id := range w.IDs {
		if strings.HasPrefix(id, "finding-") {
			made = id
		}
	}
	if made == "" || !slices.Contains(w.IDs, x) || !slices.Contains(w.IDs, existing) {
		t.Fatalf("ids %v", w.IDs)
	}
	// One batch: every op shares one stamp.
	h, err := r.svc.History(ctx, api.LogbookHistoryParams{ID: made})
	if err != nil {
		t.Fatal(err)
	}
	ph, _ := r.svc.History(ctx, api.LogbookHistoryParams{ID: x})
	if h.Ops[0].At != ph.Ops[len(ph.Ops)-1].At {
		t.Fatalf("the record's create %s and the decision %s are not one batch", h.Ops[0].At, ph.Ops[len(ph.Ops)-1].At)
	}
	by := h.Ops[0].By
	if by == nil || by.Kind != "agent" || by.Session != "sess-1" || by.Harness != "claude" || by.ApprovedBy != "phone" {
		t.Fatalf("the approve's actor: %+v", by)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: made})
	if !slices.ContainsFunc(g.Record.Links, func(l api.LogbookLink) bool { return l.Kind == "from" && l.To == x }) {
		t.Fatalf("links %+v, want from %s", g.Record.Links, x)
	}
	if g.Record.Source == nil || g.Record.Source.Quote != "note that the cert expires" || g.Record.Source.Session != "sess-1" {
		t.Fatalf("source %+v", g.Record.Source)
	}
	pr, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: x})
	if pr.Record.Status != "approved" || pr.Record.DecidedBy != "phone" {
		t.Fatalf("proposal %+v", pr.Record)
	}
	if e, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: existing}); e.Record.Status != "doing" {
		t.Fatalf("the draft's set on %s: %q", existing, e.Record.Status)
	}
	// The same key again: nothing applied twice.
	again, err := r.svc.Decide(ctx, person, p)
	if err != nil || !again.Repeated || !slices.Equal(again.Ops, w.Ops) {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "finding"}}); len(l.Records) != 1 {
		t.Fatalf("findings after a repeated decide: %+v", l.Records)
	}
	// A new key on a decided proposal: refused.
	if _, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: x, Decision: api.LogbookDecisionReject}); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("a second decision: %v", err)
	}
}

// TestApproveIsAtomic: a draft that no longer applies (its record went away) applies nothing, and
// the proposal stays pending.
func TestApproveIsAtomic(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	tw, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if err != nil {
		t.Fatal(err)
	}
	id := tw.IDs[0]
	x := propose(t, r, create("task", "new one"), api.LogbookChange{Op: api.LogbookChangeOpNote, ID: id, Text: "a note"})
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{set(id, "deleted", true)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: x, Decision: api.LogbookDecisionApprove}); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("approve onto a deleted record: %v", err)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}); len(l.Records) != 0 {
		t.Fatalf("part of a refused approve landed: %+v", l.Records)
	}
	if pr, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: x}); pr.Record.Status != "pending" {
		t.Fatalf("proposal %q after a refused approve", pr.Record.Status)
	}
}

func TestApproveWithEditsAndReject(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	x := propose(t, r, create("task", "draft title"))
	edited := create("task", "the user's title")
	w, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: x, Decision: api.LogbookDecisionApprove, Edits: []api.LogbookChange{edited}})
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: x})
	if pr.Record.Status != "edited" {
		t.Fatalf("status %q, want edited", pr.Record.Status)
	}
	l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}})
	if len(l.Records) != 1 || l.Records[0].Title != "the user's title" || !slices.Contains(w.IDs, l.Records[0].ID) {
		t.Fatalf("tasks %+v", l.Records)
	}
	y := propose(t, r, create("task", "unwanted"))
	w, err = r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: y, Decision: api.LogbookDecisionReject})
	if err != nil || w.State != api.LogbookWriteStateRejected {
		t.Fatalf("reject: %+v %v", w, err)
	}
	if pr, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: y}); pr.Record.Status != "rejected" || pr.Record.DecidedBy != "phone" {
		t.Fatalf("rejected proposal %+v", pr.Record)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}); len(l.Records) != 1 {
		t.Fatalf("a reject landed something: %+v", l.Records)
	}
	// Refusals: a reject with edits, an unknown decision, not a proposal, a pane.
	z := propose(t, r, create("task", "z"))
	cases := []struct {
		name string
		p    api.LogbookDecideParams
	}{
		{"reject with edits", api.LogbookDecideParams{ID: z, Decision: api.LogbookDecisionReject, Edits: []api.LogbookChange{edited}}},
		{"unknown decision", api.LogbookDecideParams{ID: z, Decision: "maybe"}},
		{"not a proposal", api.LogbookDecideParams{ID: l.Records[0].ID, Decision: api.LogbookDecisionApprove}},
		{"no id", api.LogbookDecideParams{Decision: api.LogbookDecisionApprove}},
	}
	for _, c := range cases {
		if _, err := r.svc.Decide(ctx, person, c.p); kindOf(err) != api.ErrorKindInvalid {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	pane := Pane{ID: "pane-1", Session: "sess-1"}
	raw, _ := json.Marshal(api.LogbookDecideParams{ID: z, Decision: api.LogbookDecisionApprove})
	if _, err := r.svc.Dispatch(ctx, person, &pane, "logbook.decide", raw); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "only a person") {
		t.Fatalf("decide from a pane: %v", err)
	}
	if _, err := r.svc.Decide(ctx, PaneActor(pane), api.LogbookDecideParams{ID: z, Decision: api.LogbookDecisionApprove}); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("decide as an agent: %v", err)
	}
	if pr, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: z}); pr.Record.Status != "pending" {
		t.Fatalf("a refused decide changed %s: %q", z, pr.Record.Status)
	}
}

// TestDecideEvents: approve and reject emit logbook.updated for the proposal and for what the
// approve made, so clients see a proposal open and close.
func TestDecideEvents(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	x := propose(t, r, create("task", "t"))
	if ev := r.take(); !slices.ContainsFunc(ev, func(e event) bool { return e.typ == api.EventLogbookUpdated && e.id == x }) {
		t.Fatalf("no event for the new proposal: %+v", ev)
	}
	w, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: x, Decision: api.LogbookDecisionApprove})
	if err != nil {
		t.Fatal(err)
	}
	ev := r.take()
	for _, id := range w.IDs {
		if !slices.ContainsFunc(ev, func(e event) bool { return e.typ == api.EventLogbookUpdated && e.id == id }) {
			t.Fatalf("no logbook.updated for %s: %+v", id, ev)
		}
	}
}

// TestMemoryProposalExpiry: a memory proposal past its expires is no longer decided; one before
// it records the decision and applies nothing here (Part 8 writes the memory).
func TestMemoryProposalExpiry(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	mk := func(expires string) string {
		id := logbook.NewID(logbook.TypeProposal)
		if _, err := r.svc.Store().Write(ctx, logbook.Batch{Actor: &logbook.Actor{Kind: "agent", Session: "sess-1"}, Steps: []logbook.Step{{
			Kind: logbook.KindCreate, Record: id, Type: logbook.TypeProposal,
			Fields: map[string]any{"title": "remember the port", "target": "memory", "expires": expires,
				"draft": map[string]any{"text": "the dev server uses port 4100"}}}}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	soon := mk(r.now.Add(time.Hour).Format(time.RFC3339))
	past := mk(r.now.Add(-time.Minute).Format(time.RFC3339))
	if _, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: past, Decision: api.LogbookDecisionApprove}); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired proposal: %v", err)
	}
	w, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: soon, Decision: api.LogbookDecisionApprove})
	if err != nil || len(w.IDs) != 1 || w.IDs[0] != soon {
		t.Fatalf("approve a memory proposal: %+v %v", w, err)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: soon})
	if g.Record.Status != "approved" || g.Record.Expires == "" {
		t.Fatalf("memory proposal %+v", g.Record)
	}
}
