package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// recreateRig is a memberSwap over the drive rig's store, with recording fakes.
type recreateRig struct {
	w          *memberSwap
	st         *structure.Store
	log        []string
	created    []api.SessionCreateParams
	failCreate error
	makeAnyway bool // the failed create still made the session
	starts     map[string]resumeEntry
}

func newRecreateRig(t *testing.T) *recreateRig {
	dr := newDriveRig(t)
	r := &recreateRig{st: dr.s.store, starts: map[string]resumeEntry{}}
	r.w = &memberSwap{store: r.st, logf: t.Logf,
		hold: func(_ context.Context, session, name string) (func(), error) {
			r.log = append(r.log, "hold "+name)
			return func() { r.log = append(r.log, "release") }, nil
		},
		move: func(from, to string) (int, error) { r.log = append(r.log, "move to "+to); return 1, nil },
		rename: func(session, name string) error {
			r.log = append(r.log, "rename "+name)
			_, err := r.st.RenameSession(session, name)
			return err
		},
		closePane: func(_ context.Context, pane string) error { r.log = append(r.log, "close "+pane); return nil },
		folder:    func(context.Context, string) string { return "/work/one" },
		start:     func(id string) (resumeEntry, bool) { e, ok := r.starts[id]; return e, ok },
		create: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			if r.failCreate != nil {
				if r.makeAnyway {
					made, err := r.st.CreateSession(structure.SessionSpec{Name: p.Name, Pane: "00000000-0000-0000-0000-0000000000f1", Index: -1, NewPane: true})
					if err != nil {
						t.Fatal(err)
					}
					return api.SessionCreated{}, errors.Join(r.failCreate, errors.New(made.ID))
				}
				return api.SessionCreated{}, r.failCreate
			}
			r.created = append(r.created, p)
			return api.SessionCreated{Session: "fresh"}, nil
		}}
	return r
}

func (r *recreateRig) session(t *testing.T, name string) api.Session {
	t.Helper()
	ss, ok := r.st.SessionByName(name)
	if !ok {
		t.Fatalf("no session %s", name)
	}
	return ss
}

// TestRecreate: a fresh session under the same name, started as the old one was with the
// session-picking flags out, its seed from Capcom; the old one renamed aside, its held messages
// moved and its pane closed, the swap mark set and cleared; a failed create puts the name back.
func TestRecreate(t *testing.T) {
	t.Parallel()
	r := newRecreateRig(t)
	ss := r.session(t, "mate")
	r.starts[ss.ID] = resumeEntry{Harness: "claude", Model: "opus", Argv: []string{"claude", "--permission-mode", "plan", "--session-id", "abc", "-c"}}
	id, err := r.w.recreate(context.Background(), ss, api.AgentState{Harness: "claude"}, "carry on", "here/capcom")
	if err != nil || id != "fresh" {
		t.Fatalf("recreate %s %v", id, err)
	}
	p := r.created[0]
	if p.Name != "mate" || p.ProjectDir != "/work/one" || p.Harness != "claude" || p.Model != "opus" || p.Seed != "carry on" ||
		p.SeedFrom != "here/capcom" || !slices.Equal(p.Argv, []string{"claude", "--permission-mode", "plan"}) {
		t.Fatalf("created %+v", p)
	}
	if got := strings.Join(r.log, "|"); got != "hold mate|rename mate-1|move to fresh|close "+paneMate+"|release" {
		t.Fatalf("steps %s", got)
	}
	if len(r.st.Swaps()) != 0 {
		t.Fatalf("the swap mark stayed: %v", r.st.Swaps())
	}
	// A failed create puts the name back and clears the mark.
	other := r.session(t, "other")
	r.starts[other.ID] = resumeEntry{Harness: "claude"}
	r.failCreate = errors.New("no harness")
	if _, err := r.w.recreate(context.Background(), other, api.AgentState{Harness: "claude"}, "", ""); err == nil {
		t.Fatal("a failed create succeeded")
	}
	if _, ok := r.st.SessionByName("other"); !ok || len(r.st.Swaps()) != 0 {
		t.Fatalf("the old name was not put back, or the mark stayed: %v", r.log)
	}
}

// TestRecreateTimedOutCreate: a create that answers with an error but made the session all the
// same: the recreate goes on with it, so two sessions never run under one name.
func TestRecreateTimedOutCreate(t *testing.T) {
	t.Parallel()
	r := newRecreateRig(t)
	ss := r.session(t, "mate")
	r.starts[ss.ID] = resumeEntry{Harness: "claude"}
	r.failCreate, r.makeAnyway = context.DeadlineExceeded, true
	id, err := r.w.recreate(context.Background(), ss, api.AgentState{Harness: "claude"}, "", "")
	if err != nil || id == "" || id == ss.ID {
		t.Fatalf("recreate %q %v", id, err)
	}
	if s, ok := r.st.SessionByName("mate-1"); !ok || s.ID != ss.ID {
		t.Fatal("the old session is not aside")
	}
	if len(r.st.Swaps()) != 0 || !slices.Contains(r.log, "close "+paneMate) {
		t.Fatalf("not finished: %v", r.log)
	}
}

// TestRecreateRefusals: recreate never starts a session wider, or as something else, than it was
// started: Capcom's session, a round's run, a harness started by hand, and Codex or opencode
// with flags answer unavailable with a hint to restart. A shell starts again as it was.
func TestRecreateRefusals(t *testing.T) {
	t.Parallel()
	r := newRecreateRig(t)
	base := r.session(t, "mate")
	for _, c := range []struct {
		name  string
		ss    func(api.Session) api.Session
		start *resumeEntry
		st    api.AgentState
	}{
		{"capcom", func(s api.Session) api.Session { s.Kind = api.SessionKindCapcom; return s }, &resumeEntry{Harness: "claude"}, api.AgentState{Harness: "claude"}},
		{"round", func(s api.Session) api.Session { s.Round = &api.SessionRound{Round: "r1"}; return s }, &resumeEntry{Harness: "claude"}, api.AgentState{Harness: "claude"}},
		{"by hand", nil, &resumeEntry{Argv: []string{"/bin/zsh"}}, api.AgentState{Harness: "claude"}},
		{"unknown", nil, nil, api.AgentState{Harness: "claude"}},
		{"codex flags", nil, &resumeEntry{Harness: "codex", Argv: []string{"codex", "--sandbox", "read-only"}}, api.AgentState{Harness: "codex"}},
		{"opencode flags", nil, &resumeEntry{Harness: "opencode", Argv: []string{"opencode", "--agent", "plan"}}, api.AgentState{Harness: "opencode"}},
	} {
		ss := base
		if c.ss != nil {
			ss = c.ss(ss)
		}
		delete(r.starts, ss.ID)
		if c.start != nil {
			r.starts[ss.ID] = *c.start
		}
		err := r.w.recreatable(ss, c.st)
		var ce *control.Error
		if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "restart") {
			t.Errorf("%s: %v, want unavailable naming restart", c.name, err)
		}
	}
	// Allowed: bare harnesses, Codex with only --no-daemon, a plain shell.
	for _, c := range []struct {
		start resumeEntry
		st    api.AgentState
		argv  []string
	}{
		{resumeEntry{Harness: "codex", Argv: []string{"codex", "--no-daemon"}}, api.AgentState{Harness: "codex"}, []string{"codex"}},
		{resumeEntry{Harness: "claude"}, api.AgentState{Harness: "claude"}, nil},
		{resumeEntry{Argv: []string{"/bin/sh", "-l"}}, api.AgentState{}, []string{"/bin/sh", "-l"}},
	} {
		r.starts[base.ID] = c.start
		plan, err := r.w.recreatePlan(base, c.st)
		if err != nil || !slices.Equal(plan.argv, c.argv) {
			t.Errorf("%+v: %+v %v", c.start, plan, err)
		}
	}
}
