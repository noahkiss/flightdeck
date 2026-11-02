package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// lbClient is a control connection to a hab with a client label: the label is the deciding
// device of its decisions (logbook.md 10.5, Actor).
func lbClient(t *testing.T, sock, label string) *control.Client {
	t.Helper()
	c, err := control.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, label, ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func lbGet(t *testing.T, c *control.Client, id string) (api.LogbookRecord, bool) {
	t.Helper()
	var r api.LogbookRecordResult
	err := c.Call(context.Background(), "logbook.get", api.LogbookGetParams{ID: id}, &r)
	if err != nil {
		var ce *control.Error
		if errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound {
			return api.LogbookRecord{}, false
		}
		t.Fatalf("logbook.get %s: %v", id, err)
	}
	return r.Record, true
}

func hasFlag(r api.LogbookRecord, kind string) bool {
	return slices.ContainsFunc(r.Flags, func(f api.LogbookFlag) bool { return f.Kind == kind })
}

func setApproval(t *testing.T, c *control.Client, v string) {
	t.Helper()
	var cur api.SettingsResult
	call(t, c, "settings.get", api.SettingsGetParams{IDs: []string{"logbook.approval"}}, &cur)
	var set api.SettingsResult
	call(t, c, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: "logbook.approval", Value: json.RawMessage(`"` + v + `"`), Revision: cur.Settings[0].Revision}}}, &set)
}

// TestLogbookApprovalAgents: under logbook.approval = agents an agent's write from a pane is a
// proposal, with a note that it waits for approval, and lands only when a person approves it, as
// the agent's write with approved_by; a person's write lands at once; the pane cannot decide;
// clients get logbook.updated for the proposal as it opens and closes. Under off, the pane's
// write lands at once (logbook.md 18 row 7).
func TestLogbookApprovalAgents(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	made := createSession(t, e.sock, api.SessionCreateParams{Name: "worker", Argv: []string{"/bin/sh", "-c", fakeAgent}, Cwd: "/",
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}})
	addrFile := filepath.Join(e.work, "addr")
	waitUntil(t, "the pane's GFF_HAB", func() bool { b, err := os.ReadFile(addrFile); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addrFile)
	pane := []string{"GFF_HAB=" + string(b), "GFF_HAB_SOCKET=" + filepath.Join(e.work, "none.sock")}
	person := []string{"GFF_HAB_SOCKET=" + e.sock}
	c := lbClient(t, e.sock, "phone")
	var s api.StructureResult
	call(t, c, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, &s)

	setApproval(t, c, "agents")
	out, errs := lbOK(t, pane, "new", "task", "Rotate the keys")
	if !strings.HasPrefix(out, "proposed: proposal-") || !strings.Contains(out, "approval") || !strings.Contains(out, "do not ask") || errs != "" {
		t.Fatalf("a pane's write under agents: %q %q", out, errs)
	}
	prop := strings.Fields(out)[1]
	waitLogbookEvent(t, c, api.EventLogbookUpdated, prop)
	mcp := runEnv(t, pane, "act", "logbook_write", "-action", "create", "-type", "task", "-title", "Another")
	if !strings.HasPrefix(mcp, "Proposed:") || !strings.Contains(mcp, "approval") || !strings.Contains(mcp, "do not ask") {
		t.Fatalf("logbook_write under agents:\n%s", mcp)
	}
	var list api.LogbookRecords
	call(t, c, "logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}, &list)
	if len(list.Records) != 0 {
		t.Fatalf("an agent's write landed before approval: %+v", list.Records)
	}
	// A person's write lands at once, from the CLI outside a pane and from the app.
	if out, _ := lbOK(t, person, "new", "task", "By hand"); !strings.HasPrefix(out, "task-") {
		t.Fatalf("a person's write under agents: %q", out)
	}
	// The pane cannot decide, through the CLI or the route.
	if _, errs, code := lbRun(t, pane, "approve", prop); code != 3 || !strings.Contains(errs, "only a person decides") {
		t.Fatalf("approve from the pane: %d %s", code, errs)
	}
	if err := hookCall(string(b), "logbook.decide", api.LogbookDecideParams{ID: prop, Decision: "approve"}, nil); err == nil {
		t.Fatal("logbook.decide on the pane's route answered")
	}
	if r, _ := lbGet(t, c, prop); r.Status != "pending" {
		t.Fatalf("the proposal after a pane's decide: %q", r.Status)
	}
	// The person approves from the app; a repeated key applies once.
	p := api.LogbookDecideParams{ID: prop, Decision: api.LogbookDecisionApprove, IdempotencyKey: "app-decide-1"}
	var w api.LogbookWritten
	call(t, c, "logbook.decide", p, &w)
	var again api.LogbookWritten
	call(t, c, "logbook.decide", p, &again)
	if !again.Repeated || !slices.Equal(again.Ops, w.Ops) {
		t.Fatalf("a repeated decide: %+v, first %+v", again, w)
	}
	waitLogbookEvent(t, c, api.EventLogbookUpdated, prop)
	var task string
	for _, id := range w.IDs {
		if strings.HasPrefix(id, "task-") {
			task = id
		}
	}
	r, ok := lbGet(t, c, task)
	by := r.CreatedBy
	if !ok || r.Title != "Rotate the keys" || by == nil || by.Kind != "agent" || by.Session != made.Session || by.ApprovedBy != "phone" {
		t.Fatalf("the approved record: %+v by %+v", r, by)
	}
	if !slices.ContainsFunc(r.Links, func(l api.LogbookLink) bool { return l.Kind == "from" && l.To == prop }) {
		t.Fatalf("links %+v", r.Links)
	}
	if pr, _ := lbGet(t, c, prop); pr.Status != "approved" || pr.DecidedBy != "phone" {
		t.Fatalf("the proposal after approve: %+v", pr)
	}
	// gff lb proposals lists the other one; the CLI outside a pane rejects it.
	out, _ = lbOK(t, person, "proposals")
	rest := strings.Fields(out)[0]
	if strings.Contains(out, prop) || !strings.HasPrefix(rest, "proposal-") {
		t.Fatalf("proposals:\n%s", out)
	}
	if out, _ := lbOK(t, person, "reject", rest); out != "rejected: "+rest+"\n" {
		t.Fatalf("reject: %q", out)
	}
	call(t, c, "logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}, &list)
	if len(list.Records) != 2 {
		t.Fatalf("tasks after one approve, one reject and one person's write: %+v", list.Records)
	}

	// Off: the pane's write lands at once.
	setApproval(t, c, "off")
	out, _ = lbOK(t, pane, "new", "task", "Lands at once")
	if !strings.HasPrefix(out, "task-") {
		t.Fatalf("a pane's write under off: %q", out)
	}
}

// TestLogbookDecidedTwiceTwoHabs: E10 and 9.3 across two real gff-hab processes over the ssh
// stand-in. With B stopped, A approves X; then a person on B, through the CLI and B's file,
// rejects X and Y; then A approves Y. When B is back and the two sync: X stays approved, its
// record stays, and the reject is flagged decided twice; Y is rejected, the record A made from
// it is hidden on both habs and flagged approval overruled.
func TestLogbookDecidedTwiceTwoHabs(t *testing.T) {
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	path := sshStandIn(t, map[string]string{"hab-a": sockA, "hab-b": sockB})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	stateA, stateB := filepath.Join(d, "sa"), filepath.Join(d, "sb")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-a")
	peerHab(t, sockA, stateA, filesA, path, "host-a")
	b := peerHab(t, sockB, stateB, filesB, path, "host-b")
	waitPeer(t, sockA, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	ca, cb := lbClient(t, sockA, "phone"), lbClient(t, sockB, "mac")

	propose := func(title string) string {
		var w api.LogbookWritten
		call(t, ca, "logbook.write", api.LogbookWriteParams{Propose: true, Changes: []api.LogbookChange{{Op: "create", Type: "task", Title: title}}}, &w)
		return w.Proposal
	}
	x, y := propose("X"), propose("Y")
	waitFor(t, "b holds both proposals", scaleTimeout(10*time.Second), func() bool {
		_, okX := lbGet(t, cb, x)
		_, okY := lbGet(t, cb, y)
		return okX && okY
	})
	cb.Close()
	b.stop(t)

	approve := func(id string) string {
		var w api.LogbookWritten
		call(t, ca, "logbook.decide", api.LogbookDecideParams{ID: id, Decision: api.LogbookDecisionApprove}, &w)
		for _, r := range w.IDs {
			if strings.HasPrefix(r, "task-") {
				return r
			}
		}
		t.Fatalf("approve %s made no task: %+v", id, w)
		return ""
	}
	madeX := approve(x)
	time.Sleep(5 * time.Millisecond) // stamps in order: A's approve of X, B's rejects, A's approve of Y
	onB := []string{"GFF_HAB_SOCKET=" + sockB}
	for _, id := range []string{x, y} {
		out, errs := lbOK(t, onB, "--state", stateB, "reject", id)
		if out != "rejected: "+id+"\n" || !strings.Contains(errs, "gff-hab is not running") {
			t.Fatalf("reject %s on b's file: %q %q", id, out, errs)
		}
	}
	time.Sleep(5 * time.Millisecond)
	madeY := approve(y)
	if r, _ := lbGet(t, ca, madeY); r.Deleted {
		t.Fatal("a hides Y's record before it sees b's reject")
	}

	peerHab(t, sockB, stateB, filesB, path, "host-b")
	cb = lbClient(t, sockB, "mac")
	waitFor(t, "both habs hold every decision", scaleTimeout(20*time.Second), func() bool {
		for _, c := range []*control.Client{ca, cb} {
			rx, _ := lbGet(t, c, x)
			ry, _ := lbGet(t, c, y)
			if !hasFlag(rx, "decided_twice") || !hasFlag(ry, "decided_twice") {
				return false
			}
		}
		return true
	})
	for name, c := range map[string]*control.Client{"a": ca, "b": cb} {
		rx, _ := lbGet(t, c, x)
		if rx.Status != "approved" || rx.DecidedBy != "phone" {
			t.Fatalf("%s: X %q by %s, want the approve, the first decision", name, rx.Status, rx.DecidedBy)
		}
		mx, ok := lbGet(t, c, madeX)
		if !ok || mx.Deleted || hasFlag(mx, "approval_overruled") {
			t.Fatalf("%s: X's record %+v", name, mx)
		}
		ry, _ := lbGet(t, c, y)
		if ry.Status != "rejected" || ry.DecidedBy != "cli" {
			t.Fatalf("%s: Y %q by %s, want the reject, the first decision", name, ry.Status, ry.DecidedBy)
		}
		my, ok := lbGet(t, c, madeY)
		if !ok || !my.Deleted || !hasFlag(my, "approval_overruled") {
			t.Fatalf("%s: Y's record %+v, want hidden and flagged", name, my)
		}
		var list api.LogbookRecords
		call(t, c, "logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}, &list)
		if len(list.Records) != 1 || list.Records[0].ID != madeX {
			t.Fatalf("%s: tasks %+v, want only X's", name, list.Records)
		}
	}
}
