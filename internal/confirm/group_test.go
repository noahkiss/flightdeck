package confirm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

func groupStart(host string) *api.ConfirmStart {
	s := start(host)
	s.Group, s.Role, s.Item = "g3fa9c2", "lead", "cache"
	return s
}

// TestGroupStartAsks (groups.md 3.2, 4.2): while confirm.group_plan is on, a group start asks,
// also on the agent's own host and with confirm.start_elsewhere off; the run knows the user
// answered it. With confirm.group_plan off it runs at once as a record, and the run knows nobody
// answered it, so its trust cover names no confirmation. With confirm.start_elsewhere on and
// confirm.group_plan off, a group start on another host still asks once.
func TestGroupStartAsks(t *testing.T) {
	plan := true
	r := newRig(t, false, time.Hour)
	r.svc.cfg.GroupPlan = func() bool { return plan }

	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("self"), From: "coord"})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "open" || c.Record || c.Start.Group != "g3fa9c2" || c.Start.Role != "lead" || c.Start.Item != "cache" {
		t.Fatalf("a group start on this host with group_plan on: %+v", c)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"}); err != nil {
		t.Fatal(err)
	}
	if n := r.runner.count(); n != 1 || !r.runner.runs[0].confirmed || r.runner.runs[0].start.Group != "g3fa9c2" {
		t.Fatalf("runs: %+v", r.runner.runs)
	}

	// A start with no group on this host never asks, and nobody answered it.
	c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("self"), From: "coord"})
	if err != nil || !c.Record || r.runner.runs[1].confirmed {
		t.Fatalf("a plain start: %+v %v %+v", c, err, r.runner.runs)
	}

	plan = false
	c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("beta"), From: "coord"})
	if err != nil || !c.Record || c.State != "done" {
		t.Fatalf("group_plan off: %+v %v", c, err)
	}
	if run := r.runner.runs[2]; run.confirmed || run.start.Group != "g3fa9c2" {
		t.Fatalf("group_plan off run: %+v", run)
	}

	r.on = true // confirm.start_elsewhere
	c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("beta"), From: "coord"})
	if err != nil || c.State != "open" {
		t.Fatalf("start_elsewhere on, group_plan off, another host: %+v %v", c, err)
	}
	c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("self"), From: "coord"})
	if err != nil || !c.Record {
		t.Fatalf("start_elsewhere on, group_plan off, this host: %+v %v", c, err)
	}
}

// TestGroupFieldsChecked: the group and item ids and the role are checked before anything opens;
// role and item need a group.
func TestGroupFieldsChecked(t *testing.T) {
	r := newRig(t, false, time.Hour)
	for _, c := range []struct {
		name              string
		group, role, item string
	}{
		{"upper case group", "G1", "lead", ""},
		{"a slash in the group", "g/1", "lead", ""},
		{"a group of 65", "g" + strings.Repeat("a", 64), "", ""},
		{"a leading dash", "-g", "", ""},
		{"an unknown role", "g1", "boss", ""},
		{"the coordinator role", "g1", "coordinator", ""},
		{"a bad item", "g1", "lead", "Item 1"},
		{"a role with no group", "", "lead", ""},
		{"an item with no group", "", "", "cache"},
	} {
		s := start("self")
		s.Group, s.Role, s.Item = c.group, c.role, c.item
		if _, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: s, From: "coord"}); kind(err) != api.ErrorKindInvalid {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if r.runner.count() != 0 {
		t.Fatal("a refused start ran")
	}
	if got := GroupBranchPrefix("g3fa9c2"); got != "group/g3fa9c2/" {
		t.Errorf("prefix %q", got)
	}
}

type fakeGroups struct {
	mu        sync.Mutex
	elsewhere bool
	runs      []bool // confirmed per run
	plans     []api.GroupPlan
	closed    []api.Confirmation
}

func (f *fakeGroups) CheckPlan(g api.ConfirmGroup) (api.ConfirmGroup, bool, error) {
	if g.Plan.Title == "" {
		return g, false, control.Errorf(api.ErrorKindInvalid, "the plan needs a title")
	}
	return g, f.elsewhere, nil
}

func (f *fakeGroups) RunGroup(_ context.Context, _ string, g api.ConfirmGroup, confirmed bool) (api.ConfirmResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, confirmed)
	f.plans = append(f.plans, g.Plan)
	return api.ConfirmResult{Host: "dev"}, nil
}

func (f *fakeGroups) GroupClosed(c api.Confirmation) {
	f.mu.Lock()
	f.closed = append(f.closed, c)
	f.mu.Unlock()
}

func groupReq() api.ConfirmRequestParams {
	return api.ConfirmRequestParams{Kind: "group", From: "coord", Group: &api.ConfirmGroup{Group: "abcdef0123456789",
		Plan: api.GroupPlan{Title: "Add export", Project: "app"}}}
}

// TestGroupPlanConfirmation (groups.md 3.2): one confirmation of kind group per plan. It asks
// while confirm.group_plan is on, a plan on one host too; nothing runs before the answer; with
// the setting off it runs at once as a record; with it off and confirm.start_elsewhere on, a plan
// with a member elsewhere asks once. Edits replace the plan and are checked again. A cancel tells
// the group service, which runs nothing.
func TestGroupPlanConfirmation(t *testing.T) {
	plan := true
	r := newRig(t, false, time.Hour)
	fg := &fakeGroups{}
	r.svc.cfg.Groups, r.svc.cfg.GroupPlan = fg, func() bool { return plan }

	c, err := r.svc.Request(context.Background(), groupReq())
	if err != nil || c.State != "open" || c.Kind != "group" || c.Group == nil {
		t.Fatalf("plan on one host: %+v %v", c, err)
	}
	if len(fg.runs) != 0 {
		t.Fatal("a plan ran before its answer")
	}
	bad := api.GroupPlan{}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm", Edits: &api.ConfirmEdits{Plan: &bad}}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("a bad edited plan: %v", err)
	}
	edited := api.GroupPlan{Title: "Add export, edited", Project: "app"}
	done, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm", Edits: &api.ConfirmEdits{Plan: &edited}})
	if err != nil || done.State != "done" || !done.Edited {
		t.Fatalf("confirm: %+v %v", done, err)
	}
	if len(fg.runs) != 1 || !fg.runs[0] || fg.plans[0].Title != "Add export, edited" {
		t.Fatalf("runs %v plans %v", fg.runs, fg.plans)
	}

	c, _ = r.svc.Request(context.Background(), groupReq())
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if len(fg.runs) != 1 || len(fg.closed) != 1 || fg.closed[0].State != "cancelled" {
		t.Fatalf("cancel: runs %v closed %v", fg.runs, fg.closed)
	}

	plan = false
	c, err = r.svc.Request(context.Background(), groupReq())
	if err != nil || !c.Record || c.State != "done" || len(fg.runs) != 2 || fg.runs[1] {
		t.Fatalf("setting off: %+v %v %v", c, err, fg.runs)
	}
	r.on, fg.elsewhere = true, true
	c, err = r.svc.Request(context.Background(), groupReq())
	if err != nil || c.State != "open" {
		t.Fatalf("setting off, start_elsewhere on, a member elsewhere: %+v %v", c, err)
	}
	fg.elsewhere = false
	if c, _ = r.svc.Request(context.Background(), groupReq()); !c.Record {
		t.Fatalf("setting off, start_elsewhere on, one host: %+v", c)
	}
}

// TestGroupPlanExpiry (groups.md 3.2): a plan's confirmation expires at group.confirm_max_age,
// and the group service hears it.
func TestGroupPlanExpiry(t *testing.T) {
	r := newRig(t, false, time.Hour)
	fg := &fakeGroups{}
	now := time.Now()
	var nmu sync.Mutex
	r.svc.cfg.Groups, r.svc.cfg.GroupTTL = fg, func() time.Duration { return 2 * time.Minute }
	r.svc.cfg.Now = func() time.Time { nmu.Lock(); defer nmu.Unlock(); return now }
	c, err := r.svc.Request(context.Background(), groupReq())
	if err != nil {
		t.Fatal(err)
	}
	if exp, _ := time.Parse(time.RFC3339Nano, c.ExpiresAt); exp.Sub(now).Round(time.Second) != 2*time.Minute {
		t.Fatalf("expires at %s", c.ExpiresAt)
	}
	nmu.Lock()
	now = now.Add(2*time.Minute + time.Second)
	nmu.Unlock()
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"}); kind(err) != api.ErrorKindConflict {
		t.Fatalf("an answer after the expiry: %v", err)
	}
	waitFor(t, func() bool { fg.mu.Lock(); defer fg.mu.Unlock(); return len(fg.closed) == 1 })
	if fg.closed[0].State != "expired" || !strings.Contains(fg.closed[0].Reason, "2 minutes") || len(fg.runs) != 0 {
		t.Fatalf("closed %+v runs %v", fg.closed, fg.runs)
	}
}

// TestEnvelopeStart (groups.md 3.3): with an envelope check, a member's start inside it runs at
// once with no question; a member's start outside it is refused; a start from a session that
// is not a member follows the normal rules, so it asks while confirm.group_plan is on.
func TestEnvelopeStart(t *testing.T) {
	r := newRig(t, false, time.Hour)
	r.svc.cfg.Envelope = func(from string, s api.ConfirmStart) (bool, error) {
		if from != "lead-1" {
			return false, nil
		}
		if s.Host == "self" {
			return true, nil
		}
		return true, control.Errorf(api.ErrorKindInvalid, "outside the envelope")
	}
	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("self"), From: "coord"})
	if err != nil || c.Record || c.State != "open" {
		t.Fatalf("not a member: %+v %v", c, err)
	}
	if r.runner.count() != 0 {
		t.Fatalf("runs before the answer: %+v", r.runner.runs)
	}
	c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("self"), From: "lead-1"})
	if err != nil || !c.Record {
		t.Fatalf("inside: %+v %v", c, err)
	}
	if _, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: groupStart("beta"), From: "lead-1"}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("outside: %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
