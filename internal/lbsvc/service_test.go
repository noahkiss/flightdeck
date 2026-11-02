package lbsvc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

type event struct {
	typ  api.EventType
	id   string
	rev  uint64
	data any
}

type rig struct {
	svc    *Service
	path   string
	now    time.Time
	mu     sync.Mutex
	events []event
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	r := &rig{path: filepath.Join(t.TempDir(), "logbook.db"), now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	st, err := logbook.Open(ctx, r.path, logbook.Options{Origin: "SHA256:habA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.svc, err = New(ctx, Config{Store: st, Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		Publish: func(typ api.EventType, id string, rev uint64, data any) {
			r.mu.Lock()
			r.events = append(r.events, event{typ, id, rev, data})
			r.mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *rig) take() []event {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.events
	r.events = nil
	return e
}

func kindOf(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

var person = api.LogbookActor{Kind: "person", Device: "phone"}

func create(typ, title string) api.LogbookChange {
	return api.LogbookChange{Op: api.LogbookChangeOpCreate, Type: typ, Title: title}
}

func TestWriteReadAndKeys(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	c := create("task", "fix the build")
	c.Tags, c.Body, c.Priority = []string{"ci"}, "it fails on arm64", 2
	w, err := r.svc.Write(ctx, person, api.LogbookWriteParams{IdempotencyKey: "key-1", Changes: []api.LogbookChange{c}})
	if err != nil || w.State != api.LogbookWriteStateApplied || len(w.IDs) != 1 || len(w.Ops) != 2 || w.Repeated {
		t.Fatalf("write: %+v %v", w, err)
	}
	id := w.IDs[0]
	again, err := r.svc.Write(ctx, person, api.LogbookWriteParams{IdempotencyKey: "key-1", Changes: []api.LogbookChange{c}})
	if err != nil || !again.Repeated || !slices.Equal(again.IDs, w.IDs) || !slices.Equal(again.Ops, w.Ops) {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{}); len(l.Records) != 1 {
		t.Fatalf("a repeated key applied twice: %+v", l.Records)
	}
	other := c
	other.Title = "another"
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{IdempotencyKey: "key-1", Changes: []api.LogbookChange{other}}); kindOf(err) != api.ErrorKindConflict {
		t.Fatalf("same key, other request: %v", err)
	}
	g, err := r.svc.Get(ctx, api.LogbookGetParams{ID: id})
	rec := g.Record
	if err != nil || rec.Title != "fix the build" || rec.Status != "open" || rec.Body != "it fails on arm64" || rec.Priority != 2 ||
		!slices.Equal(rec.Tags, []string{"ci"}) || rec.CreatedBy == nil || rec.CreatedBy.Device != "phone" || rec.Revision != 2 || rec.Updated == "" {
		t.Fatalf("get: %+v %v", rec, err)
	}
	// Each change kind.
	note := api.LogbookChange{Op: api.LogbookChangeOpNote, ID: id, Text: "tried a clean build"}
	set := api.LogbookChange{Op: api.LogbookChangeOpSet, ID: id, Field: "status", Value: api.RawJSON(`"doing"`)}
	rm := api.LogbookChange{Op: api.LogbookChangeOpRemove, ID: id, Tag: "ci"}
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{note, set, rm}}); err != nil {
		t.Fatal(err)
	}
	g, _ = r.svc.Get(ctx, api.LogbookGetParams{ID: id})
	if g.Record.Status != "doing" || len(g.Record.Tags) != 0 || len(g.Record.Notes) != 1 || g.Record.Notes[0].Text != "tried a clean build" {
		t.Fatalf("after changes: %+v", g.Record)
	}
	h, err := r.svc.History(ctx, api.LogbookHistoryParams{ID: id})
	if err != nil || len(h.Ops) != 5 || h.Ops[0].Kind != "create" || !h.Ops[0].Folded {
		t.Fatalf("history: %+v %v", h, err)
	}
	// Refusals keep their kind and hint.
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "move"}}}); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("unknown op: %v", err)
	}
	f, _ := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("finding", "a finding")}})
	_, err = r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpSet, ID: f.IDs[0], Field: "body", Value: api.RawJSON(`"x"`)}}})
	if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "supersede") {
		t.Fatalf("a finding body set: %v", err)
	}
	if _, err := r.svc.Get(ctx, api.LogbookGetParams{ID: "task-zzzzzzzz"}); kindOf(err) != api.ErrorKindNotFound {
		t.Fatalf("missing record: %v", err)
	}
	big := create("task", "big")
	big.Body = strings.Repeat("x", logbook.MaxBody+1)
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{big}}); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "file") {
		t.Fatalf("a body over 256 KiB: %v", err)
	}
	st, err := r.svc.Status(ctx)
	if err != nil || st.Origin != "SHA256:habA" || st.Records != 2 || len(st.Vector) != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
}

func TestPaneWrite(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	pane := Pane{ID: "pane-1", Session: "sess-1", Harness: "claude", Host: "box-a"}
	src := &api.LogbookSource{Session: "forged", Message: "m-1", Quote: "note that the cache is stale"}
	w, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{create("finding", "the cache is stale")}, Source: src})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: w.IDs[0]})
	by := g.Record.CreatedBy
	if by == nil || by.Kind != "agent" || by.Harness != "claude" || by.Session != "sess-1" || by.Pane != "pane-1" || by.Host != "box-a" {
		t.Fatalf("actor: %+v", by)
	}
	if s := g.Record.Source; s == nil || s.Session != "sess-1" || s.Quote != src.Quote || s.Message != "m-1" {
		t.Fatalf("source: %+v", s)
	}
	// The limit: 60 a minute, the 61st refused, the next minute open again.
	for i := 1; i < PaneWrites; i++ {
		if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: w.IDs[0], Text: "n"}}}); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	_, err = r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "the 61st")}})
	if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the 61st write: %v", err)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}); len(l.Records) != 0 {
		t.Fatalf("the refused write landed: %+v", l.Records)
	}
	// Another pane is not held up, and neither is a person.
	if _, err := r.svc.WritePane(ctx, Pane{ID: "pane-2"}, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "other pane")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "a person")}}); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "a minute later")}}); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
	// Resolve through the pane route counts too.
	r.svc.cfg.PaneWrites = 1
	r.advance(time.Minute)
	if _, err := r.svc.Dispatch(ctx, person, &pane, "logbook.write", json.RawMessage(`{"changes":[{"op":"create","type":"task","title":"one"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Dispatch(ctx, person, &pane, "logbook.resolve", json.RawMessage(`{"id":"task-x","field":"title","value":"\"t\""}`)); kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("resolve past the limit: %v", err)
	}
}

func TestPropose(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	pane := Pane{ID: "pane-1", Session: "capcom-1", Harness: "claude", Host: "box-a"}
	c := create("task", "renew the certificate")
	c.Project = "proj-aaaaaaaa"
	p := api.LogbookWriteParams{IdempotencyKey: "k", Propose: true, Changes: []api.LogbookChange{c}, Source: &api.LogbookSource{Quote: "remind me to renew it"}}
	w, err := r.svc.WritePane(ctx, pane, p)
	if err != nil || w.State != api.LogbookWriteStateProposed || w.Proposal == "" || !strings.HasPrefix(w.Proposal, "proposal-") || !slices.Equal(w.IDs, []string{w.Proposal}) {
		t.Fatalf("propose: %+v %v", w, err)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: w.Proposal})
	rec := g.Record
	if rec.Type != "proposal" || rec.Status != "pending" || rec.Target != "record" || rec.Project != "proj-aaaaaaaa" ||
		!strings.Contains(rec.Title, "renew the certificate") || rec.Source == nil || rec.Source.Session != "capcom-1" {
		t.Fatalf("proposal: %+v", rec)
	}
	var draft struct {
		Changes []api.LogbookChange `json:"changes"`
	}
	if err := json.Unmarshal(rec.Draft, &draft); err != nil || len(draft.Changes) != 1 || draft.Changes[0].Title != "renew the certificate" {
		t.Fatalf("draft: %s %v", rec.Draft, err)
	}
	// Nothing landed as a task; a retry is the same proposal.
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}); len(l.Records) != 0 {
		t.Fatalf("a proposed task landed: %+v", l.Records)
	}
	again, err := r.svc.WritePane(ctx, pane, p)
	if err != nil || again.Proposal != w.Proposal || again.State != api.LogbookWriteStateProposed || !again.Repeated {
		t.Fatalf("retry: %+v %v", again, err)
	}
	// Capcom lists one conversation's proposals.
	l, err := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "proposal", Status: "pending", Session: "capcom-1"}})
	if err != nil || len(l.Records) != 1 || l.Records[0].ID != w.Proposal {
		t.Fatalf("list by session: %+v %v", l, err)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Session: "another"}}); len(l.Records) != 0 {
		t.Fatalf("another session: %+v", l.Records)
	}
}

func TestEventsForEveryWriter(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	w, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{create("task", "one")}})
	if err != nil {
		t.Fatal(err)
	}
	ev := r.take()
	if len(ev) != 1 || ev[0].typ != api.EventLogbookUpdated || ev[0].id != w.IDs[0] || ev[0].rev != 1 {
		t.Fatalf("events after a write: %+v", ev)
	}
	// Another writer of the file (the CLI): seen at the next Changed.
	cli, err := logbook.Open(ctx, r.path, logbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.Write(ctx, logbook.Batch{Steps: []logbook.Step{{Kind: logbook.KindAppend, Record: w.IDs[0], Field: "notes", Value: "from the CLI"}}}); err != nil {
		t.Fatal(err)
	}
	if ev := r.take(); len(ev) != 0 {
		t.Fatalf("events before Changed: %+v", ev)
	}
	r.svc.Changed(ctx)
	ev = r.take()
	if len(ev) != 1 || ev[0].id != w.IDs[0] || ev[0].rev != 2 {
		t.Fatalf("events for the CLI's write: %+v", ev)
	}
	// A conflict: two sets of one field made apart, one through each writer's view. The second
	// writer sets from the same heads by writing before it reads the first's set.
	id := w.IDs[0]
	set := func(s *logbook.Store, v string) {
		if _, err := s.Write(ctx, logbook.Batch{Steps: []logbook.Step{{Kind: logbook.KindSet, Record: id, Field: "title", Value: v}}}); err != nil {
			t.Fatal(err)
		}
	}
	// One file folds both writers' sets in order, so a real conflict needs a peer's op. Apply
	// one from a second replica that never saw the first set.
	peer, err := logbook.Open(ctx, filepath.Join(t.TempDir(), "peer.db"), logbook.Options{Origin: "SHA256:habB"})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	have, _ := peer.Vector(ctx)
	ops, _, _ := r.svc.Store().OpsAfter(ctx, have, 100, 0)
	if _, err := peer.Apply(ctx, ops); err != nil {
		t.Fatal(err)
	}
	set(r.svc.Store(), "title A")
	set(peer, "title B")
	have, _ = r.svc.Store().Vector(ctx)
	ops, _, _ = peer.OpsAfter(ctx, have, 100, 0)
	if _, err := r.svc.Store().Apply(ctx, ops); err != nil {
		t.Fatal(err)
	}
	r.take()
	r.svc.Changed(ctx)
	ev = r.take()
	if len(ev) != 2 || ev[1].typ != api.EventLogbookConflict || !slices.Equal(ev[1].data.(api.LogbookConflict).Flags, []string{"conflict"}) {
		t.Fatalf("conflict events: %+v", ev)
	}
	// Resolve by op id clears it, and the event says so.
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: id})
	if len(g.Record.Flags) != 1 || len(g.Record.Flags[0].Values) != 2 {
		t.Fatalf("flags: %+v", g.Record.Flags)
	}
	keep := g.Record.Flags[0].Values[1].Op
	if _, err := r.svc.Resolve(ctx, person, api.LogbookResolveParams{ID: id, Field: "title", Op: keep}); err != nil {
		t.Fatal(err)
	}
	ev = r.take()
	if len(ev) != 2 || ev[1].typ != api.EventLogbookConflict || len(ev[1].data.(api.LogbookConflict).Flags) != 0 {
		t.Fatalf("resolve events: %+v", ev)
	}
	g, _ = r.svc.Get(ctx, api.LogbookGetParams{ID: id})
	if len(g.Record.Flags) != 0 {
		t.Fatalf("still flagged: %+v", g.Record.Flags)
	}
	if l, _ := r.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Flagged: true}}); len(l.Records) != 0 {
		t.Fatalf("flagged list: %+v", l.Records)
	}
}

func TestDispatchAndReady(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	call := func(method, params string) any {
		t.Helper()
		res, err := r.svc.Dispatch(ctx, person, nil, method, json.RawMessage(params))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return res
	}
	w := call("logbook.write", `{"changes":[{"op":"create","type":"task","title":"blocker"},{"op":"create","type":"task","title":"blocked"}]}`).(api.LogbookWritten)
	call("logbook.write", `{"changes":[{"op":"add","id":"`+w.IDs[0]+`","link":{"kind":"blocks","to":"`+w.IDs[1]+`"}}]}`)
	ready := call("logbook.ready", `{}`).(api.LogbookRecords)
	if len(ready.Records) != 1 || ready.Records[0].ID != w.IDs[0] {
		t.Fatalf("ready: %+v", ready)
	}
	found := call("logbook.find", `{"words":"blocked"}`).(api.LogbookRecords)
	if len(found.Records) != 1 || found.Records[0].ID != w.IDs[1] || found.Records[0].Body != "" {
		t.Fatalf("find: %+v", found)
	}
	if _, err := r.svc.Dispatch(ctx, person, nil, "logbook.decide", nil); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("decide: %v", err)
	}
	if _, err := r.svc.Dispatch(ctx, person, nil, "logbook.list", json.RawMessage(`{"since":"yesterday"}`)); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("bad since: %v", err)
	}
	call("logbook.write", `{"changes":[{"op":"create","id":"proj-abcdefgh","type":"project","title":"flightdeck"},{"op":"add","id":"proj-abcdefgh","tag":"go"}]}`)
	pr := call("logbook.projects", `{}`).(api.LogbookProjects)
	if len(pr.Projects) != 1 || pr.Projects[0].Name != "flightdeck" {
		t.Fatalf("projects: %+v", pr)
	}
}
