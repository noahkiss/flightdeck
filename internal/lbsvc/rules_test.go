package lbsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// The pane rules of logbook.md 10.5: an agent never decides a proposal, forgets only its own
// session's ops, claims only with a session; the change caps; the limit's window; the key hash
// whatever the door; the sync nudge after each write.

func set(id, field string, v any) api.LogbookChange {
	b, _ := json.Marshal(v)
	return api.LogbookChange{Op: api.LogbookChangeOpSet, ID: id, Field: field, Value: api.RawJSON(b)}
}

func TestPaneNeverDecidesProposal(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	pane := Pane{ID: "pane-1", Session: "sess-1", Harness: "claude", Host: "box-a"}
	w, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Propose: true, Changes: []api.LogbookChange{create("task", "renew it")}})
	if err != nil {
		t.Fatal(err)
	}
	prop := w.Proposal
	for name, ch := range map[string]api.LogbookChange{
		"status":     set(prop, "status", "rejected"),
		"decided_by": set(prop, "decided_by", "phone"),
		"create":     {Op: api.LogbookChangeOpCreate, Type: "proposal", Title: "a forged proposal"},
	} {
		_, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{ch}})
		if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "proposal") {
			t.Fatalf("%s from a pane: %v", name, err)
		}
	}
	// Through the dispatcher too, and resolve.
	if _, err := r.svc.Dispatch(ctx, person, &pane, "logbook.write", json.RawMessage(`{"changes":[{"op":"set","id":"`+prop+`","field":"status","value":"\"approved\""}]}`)); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := r.svc.Dispatch(ctx, person, &pane, "logbook.resolve", json.RawMessage(`{"id":"`+prop+`","field":"status","value":"\"approved\""}`)); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "proposal") {
		t.Fatalf("resolve: %v", err)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: prop})
	if g.Record.Status != "pending" || g.Record.DecidedBy != "" {
		t.Fatalf("the proposal changed: %+v", g.Record)
	}
	// A pane still sets a task's status; a person decides the proposal.
	task, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "a task")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{set(task.IDs[0], "status", "doing")}}); err != nil {
		t.Fatalf("a task's status: %v", err)
	}
	if _, err := r.svc.Decide(ctx, person, api.LogbookDecideParams{ID: prop, Decision: api.LogbookDecisionReject}); err != nil {
		t.Fatalf("a person decides: %v", err)
	}
}

// A Capcom pane's writes are proposals, and its resolve is refused; another pane's resolve
// records that pane as the writer.
func TestCapcomPaneAndResolveActor(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	capcom := Pane{ID: "pane-c", Session: "capcom-1", Harness: "claude", Host: "box-a", Propose: true}
	w, err := r.svc.WritePane(ctx, capcom, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "from capcom")}})
	if err != nil || w.State != api.LogbookWriteStateProposed || w.Proposal == "" {
		t.Fatalf("capcom write: %+v %v", w, err)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}); len(l.Records) != 0 {
		t.Fatalf("a Capcom write landed: %+v", l.Records)
	}
	// A record with a conflict to resolve: two sets of the title that do not see each other.
	tw, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if err != nil {
		t.Fatal(err)
	}
	id := tw.IDs[0]
	if _, err := r.svc.Dispatch(ctx, person, &capcom, "logbook.resolve", json.RawMessage(`{"id":"`+id+`","field":"title","value":"\"x\""}`)); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "review") {
		t.Fatalf("capcom resolve: %v", err)
	}
	pane := Pane{ID: "pane-1", Session: "sess-1", Harness: "codex", Host: "box-a"}
	if _, err := r.svc.Dispatch(ctx, person, &pane, "logbook.resolve", json.RawMessage(`{"id":"`+id+`","field":"title","value":"\"resolved\""}`)); err != nil {
		t.Fatal(err)
	}
	h, err := r.svc.History(ctx, api.LogbookHistoryParams{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	last := h.Ops[len(h.Ops)-1]
	if last.By == nil || last.By.Kind != "agent" || last.By.Session != "sess-1" || last.By.Pane != "pane-1" || last.By.Harness != "codex" {
		t.Fatalf("resolve's writer: %+v", last.By)
	}
}

func TestPaneForgetsOwnOpsOnly(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	mine := Pane{ID: "pane-1", Session: "sess-1", Harness: "claude"}
	other := Pane{ID: "pane-2", Session: "sess-2", Harness: "claude"}
	w, err := r.svc.WritePane(ctx, mine, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if err != nil {
		t.Fatal(err)
	}
	id := w.IDs[0]
	own, err := r.svc.WritePane(ctx, mine, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: id, Text: "a token: abc"}}})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := r.svc.WritePane(ctx, other, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: id, Text: "their note"}}})
	if err != nil {
		t.Fatal(err)
	}
	persons, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: id, Text: "a person's note"}}})
	if err != nil {
		t.Fatal(err)
	}
	forget := func(op string) api.LogbookWriteParams {
		return api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpForget, OpID: op}}}
	}
	for _, op := range []string{theirs.Ops[0], persons.Ops[0]} {
		_, err := r.svc.WritePane(ctx, mine, forget(op))
		if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "A person can forget it") {
			t.Fatalf("forget %s from another pane's session: %v", op, err)
		}
	}
	// A pane outside a session forgets nothing.
	if _, err := r.svc.WritePane(ctx, Pane{ID: "pane-3"}, forget(own.Ops[0])); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("forget from a pane with no session: %v", err)
	}
	if _, err := r.svc.WritePane(ctx, mine, forget(own.Ops[0])); err != nil {
		t.Fatalf("forget its own op: %v", err)
	}
	if _, err := r.svc.Write(ctx, person, forget(theirs.Ops[0])); err != nil {
		t.Fatalf("a person forgets: %v", err)
	}
}

func TestSelfNeedsSession(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	w, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if err != nil {
		t.Fatal(err)
	}
	id := w.IDs[0]
	lone := Pane{ID: "pane-9", Harness: "claude"}
	for _, ch := range []api.LogbookChange{set(id, "assignee", Self), {Op: api.LogbookChangeOpCreate, Type: "task", Title: "x", Assignee: Self}} {
		if _, err := r.svc.WritePane(ctx, lone, api.LogbookWriteParams{Changes: []api.LogbookChange{ch}}); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "has none") {
			t.Fatalf("self from a pane with no session: %v", err)
		}
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: id})
	if g.Record.Assignee != "" {
		t.Fatalf("assignee: %q", g.Record.Assignee)
	}
	// A spaced JSON value is the same word.
	in := Pane{ID: "pane-1", Session: "sess-1"}
	if _, err := r.svc.WritePane(ctx, in, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpSet, ID: id, Field: "assignee", Value: api.RawJSON(` "self" `)}}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: id}); g.Record.Assignee != "sess-1" {
		t.Fatalf("assignee: %q", g.Record.Assignee)
	}
}

func TestChangeCaps(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	many := func(n int) []api.LogbookChange {
		out := make([]api.LogbookChange, n)
		for i := range out {
			out[i] = create("task", "t")
		}
		return out
	}
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: many(MaxChanges + 1)}); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("a write over the cap: %v", err)
	}
	pane := Pane{ID: "pane-1", Session: "sess-1"}
	for i := 0; i < PaneChanges/MaxChanges; i++ {
		if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: many(MaxChanges)}); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: many(1)}); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "changes in the last minute") {
		t.Fatalf("past the change limit: %v", err)
	}
	if st, _ := r.svc.Status(ctx); st.Records != PaneChanges {
		t.Fatalf("records: %d", st.Records)
	}
	r.advance(time.Minute)
	if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: many(1)}); err != nil {
		t.Fatalf("a minute later: %v", err)
	}
}

// The limit's window is a minute: a write 59 seconds after the last one allowed is still refused.
func TestPaneLimitWindow(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.svc.cfg.PaneWrites = 1
	pane := Pane{ID: "pane-1", Session: "sess-1"}
	one := api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}}
	if _, err := r.svc.WritePane(ctx, pane, one); err != nil {
		t.Fatal(err)
	}
	r.advance(59 * time.Second)
	if _, err := r.svc.WritePane(ctx, pane, one); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "Wait 1s") {
		t.Fatalf("59 s later: %v", err)
	}
	r.advance(time.Second)
	if _, err := r.svc.WritePane(ctx, pane, one); err != nil {
		t.Fatalf("a minute later: %v", err)
	}
}

// A key means the same request through the pane route and any other door: the hash is of the
// request as sent, before the pane's session goes into its source.
func TestKeyHashWhateverDoor(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	p := api.LogbookWriteParams{IdempotencyKey: "k-1", Changes: []api.LogbookChange{create("task", "once")},
		Source: &api.LogbookSource{Session: "as-sent", Quote: "q"}}
	first, err := r.svc.WritePane(ctx, Pane{ID: "pane-1", Session: "sess-1"}, p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.svc.Write(ctx, person, p)
	if err != nil || !again.Repeated || again.IDs[0] != first.IDs[0] {
		t.Fatalf("the same request through another door: %+v %v", again, err)
	}
}

// Each write that lands nudges sync once; a refused one does not.
func TestWroteNudgesSync(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	n := 0
	r.svc.cfg.Wrote = func() { n++ }
	w, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if err != nil || n != 1 {
		t.Fatalf("write: %d %v", n, err)
	}
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: "task-zzzzzzzz", Text: "x"}}}); err == nil || n != 1 {
		t.Fatalf("a refused write: %d %v", n, err)
	}
	if _, err := r.svc.Resolve(ctx, person, api.LogbookResolveParams{ID: w.IDs[0], Field: "title", Value: api.RawJSON(`"u"`)}); err != nil || n != 2 {
		t.Fatalf("resolve: %d %v", n, err)
	}
}

// A restored file refuses writes as unavailable, with the reason.
func TestRestoringUnavailable(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if err := r.svc.Store().NoteOwnSeen(ctx, 50); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("write to a restored file: %v", err)
	}
}

// A pane's write with no source still gets one naming the pane's session.
func TestPaneSourceSession(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	w, err := r.svc.WritePane(ctx, Pane{ID: "pane-1", Session: "sess-1"}, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "t")}})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: w.IDs[0]})
	if g.Record.Source == nil || g.Record.Source.Session != "sess-1" {
		t.Fatalf("source: %+v", g.Record.Source)
	}
	// With no session and no source, none.
	w, err = r.svc.WritePane(ctx, Pane{ID: "pane-2"}, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "u")}})
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: w.IDs[0]}); g.Record.Source != nil {
		t.Fatalf("source with no session: %+v", g.Record.Source)
	}
}
