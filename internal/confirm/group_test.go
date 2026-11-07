package confirm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
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
