package main

// Unit tests of the rounds pieces in gff-hab: the [rounds] settings, a state file set aside
// with one notice, the export and apply plan, and the pane route of the round methods.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/structure"
)

func TestRoundSettings(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "hab.toml")
	if s, err := loadRoundsSettings(p); err != nil || s != (roundsSettings{}) {
		t.Fatalf("no file: %+v %v", s, err)
	}
	writeFile(t, p, "[rounds]\nmax_running = 3\nwake_settle = \"10s\"\n")
	if s, err := loadRoundsSettings(p); err != nil || s.MaxRunning != 3 || s.WakeSettle != 10*time.Second {
		t.Fatalf("set: %+v %v", s, err)
	}
	writeFile(t, p, "[rounds]\nwake_settle = \"0s\"\n")
	if s, err := loadRoundsSettings(p); err != nil || s.WakeSettle >= 0 {
		t.Fatalf("0s must mean no settle: %+v %v", s, err)
	}
	for _, bad := range []string{"[rounds]\nmax_running = 0\n", "[rounds]\nwake_settle = \"soon\"\n", "[rounds]\nwake_settle = \"-1s\"\n", "[rounds\n"} {
		writeFile(t, p, bad)
		if _, err := loadRoundsSettings(p); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

// fakeRoundsService is a rounds service with no holder link: runs never start.
func fakeRoundsService(t *testing.T, state string, notices *[]string) *roundsService {
	t.Helper()
	svc, _ := fakeRoundsServiceWith(t, state, structure.NewFakeHolder(), func(c *roundsConfig) {
		c.raise = func(ev push.RoundEvent, _, _, _ string) { *notices = append(*notices, ev.Kind) }
	})
	return svc
}

// fakeRoundsServiceWith is fakeRoundsService over holder, with edit changing the config
// before the service opens.
func fakeRoundsServiceWith(t *testing.T, state string, holder *structure.FakeHolder, edit func(*roundsConfig)) (*roundsService, *structure.Store) {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: state, Holders: holder})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	cfg := roundsConfig{
		state: state, store: st, settings: roundsSettings{WakeSettle: -1},
		create: func(context.Context, api.SessionCreateParams) (api.SessionCreated, error) {
			return api.SessionCreated{}, errors.New("no holder")
		},
		pane:       func(string) (link.PaneStatus, bool) { return link.PaneStatus{}, false },
		complete:   func() bool { return true },
		closePane:  func(context.Context, string) error { return nil },
		projectFor: func(context.Context, string) (string, error) { return "", errors.New("no git") },
		publish:    func(structure.Event) {},
		raise:      func(push.RoundEvent, string, string, string) {},
		logf:       t.Logf,
		// Only claude is installed.
		lookHarness: func(name string) (string, error) {
			if name == "claude" {
				return "/bin/claude", nil
			}
			return "", errors.New("not on PATH")
		},
	}
	if edit != nil {
		edit(&cfg)
	}
	svc, err := newRounds(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, st
}

func TestRoundStateSetAside(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "rounds"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(d, "rounds", "state.json"), "{not json")
	var notices []string
	svc := fakeRoundsService(t, d, &notices)
	if len(notices) != 1 || len(svc.List()) != 0 {
		t.Fatalf("notices %v, rounds %d", notices, len(svc.List()))
	}
	if m, _ := filepath.Glob(filepath.Join(d, "rounds", "state.json.bad-*")); len(m) != 1 {
		t.Fatalf("set-aside files: %v", m)
	}
	// A file from a newer build stops gff-hab instead.
	d2 := t.TempDir()
	os.MkdirAll(filepath.Join(d2, "rounds"), 0o700)
	writeFile(t, filepath.Join(d2, "rounds", "state.json"), `{"version": 99}`)
	st, err := structure.Open(structure.Config{Dir: d2, Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := newRounds(roundsConfig{state: d2, store: st, logf: t.Logf}); err == nil {
		t.Fatal("a newer state file was taken")
	}
}

// TestRoundServiceChecks covers the API checks that need no run: the closed word sets, one
// project field, a target that must exist, a stale revision, a prompt round whose run fails as
// not built yet, and the error kinds.
func TestRoundServiceChecks(t *testing.T) {
	d := t.TempDir()
	var notices []string
	svc := fakeRoundsService(t, d, &notices)
	p, err := svc.cfg.store.CreateProject("app", "https://example.com/team/app.git", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	kind := func(err error) api.ErrorKind {
		var ce *control.Error
		if errors.As(err, &ce) {
			return ce.Data.Kind
		}
		return "none: " + api.ErrorKind(func() string {
			if err == nil {
				return "nil"
			}
			return err.Error()
		}())
	}
	base := func() api.RoundSpec {
		return api.RoundSpec{Name: "r", Mode: api.RoundModeFresh, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
			Project: p.ID, Command: []string{"true"}}
	}
	for _, c := range []struct {
		name string
		edit func(*api.RoundSpec)
		want api.ErrorKind
	}{
		{"bad mode", func(s *api.RoundSpec) { s.Mode = "later" }, api.ErrorKindInvalid},
		{"bad trigger kind", func(s *api.RoundSpec) { s.Trigger.Kind = "event" }, api.ErrorKindInvalid},
		{"two project fields", func(s *api.RoundSpec) { s.ProjectRemote = "example.com/team/app" }, api.ErrorKindInvalid},
		{"unknown project", func(s *api.RoundSpec) { s.Project = "p-none" }, api.ErrorKindNotFound},
		{"unknown remote", func(s *api.RoundSpec) { s.Project, s.ProjectRemote = "", "example.com/other" }, api.ErrorKindNotFound},
		{"relative project_dir", func(s *api.RoundSpec) { s.Project, s.ProjectDir = "", "app" }, api.ErrorKindInvalid},
		{"missing target", func(s *api.RoundSpec) {
			s.Mode, s.Command, s.Project, s.Target, s.Prompt = api.RoundModeResume, nil, "", "s-none", "hi"
		}, api.ErrorKindNotFound},
		{"every and cron", func(s *api.RoundSpec) {
			s.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: "* * * * *", Every: "5m"}
		}, api.ErrorKindInvalid},
		{"bad preset", func(s *api.RoundSpec) { s.Trigger = api.RoundTrigger{Every: "7m"} }, api.ErrorKindInvalid},
		{"past once", func(s *api.RoundSpec) {
			s.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindOnce, At: "2001-01-01T00:00:00Z"}
		}, api.ErrorKindInvalid},
		{"no name", func(s *api.RoundSpec) { s.Name = "" }, api.ErrorKindInvalid},
	} {
		s := base()
		c.edit(&s)
		if _, err := svc.Create(ctx, api.RoundCreateParams{Round: s}, ""); kind(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	s := base()
	s.ProjectRemote, s.Project = "https://example.com/team/app", ""
	made, err := svc.Create(ctx, api.RoundCreateParams{Round: s}, "s-agent")
	if err != nil {
		t.Fatal(err)
	}
	if made.Round.Project != p.ID || made.Round.ProjectRemote != p.Remote || made.Round.CreatedBy != "s-agent" || made.Round.Revision != 1 {
		t.Fatalf("created: %+v", made.Round)
	}
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: base()}, ""); kind(err) != api.ErrorKindConflict {
		t.Errorf("a taken name: %v", err)
	}
	upd := made.Round.RoundSpec
	upd.ProjectRemote, upd.TimeoutMS = "", 60_000
	if _, err := svc.Update(ctx, api.RoundUpdateParams{ID: made.Round.ID, Round: upd, Revision: 7}); kind(err) != api.ErrorKindConflict {
		t.Errorf("a stale revision: %v", err)
	}
	got, err := svc.Update(ctx, api.RoundUpdateParams{ID: made.Round.ID, Round: upd, Revision: 1})
	if err != nil || got.TimeoutMS != 60_000 || got.Revision != 2 || got.CreatedBy != "s-agent" {
		t.Fatalf("update: %+v %v", got, err)
	}
	if _, err := svc.Get(api.RoundRef{ID: made.Round.ID, Name: "r"}); kind(err) != api.ErrorKindInvalid {
		t.Errorf("id and name: %v", err)
	}
	if _, err := svc.Get(api.RoundRef{Name: "none"}); kind(err) != api.ErrorKindNotFound {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := svc.Runs(api.RoundRunsParams{RoundRef: api.RoundRef{Name: "r"}, Limit: -1}); kind(err) != api.ErrorKindInvalid {
		t.Errorf("negative limit: %v", err)
	}
	if _, err := svc.Pause(api.RoundRef{Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(api.RoundRef{Name: "r"}); kind(err) != api.ErrorKindConflict {
		t.Errorf("run of a paused round: %v", err)
	}
	// A run whose session cannot start fails with the reason.
	if _, err := svc.Unpause(api.RoundRef{Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(api.RoundRef{Name: "r"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run to fail", func() bool {
		rs, _ := svc.Runs(api.RoundRunsParams{RoundRef: api.RoundRef{Name: "r"}})
		return len(rs.Runs) == 1 && rs.Runs[0].State == api.RoundRunStateFailed && strings.Contains(rs.Runs[0].Reason, "no holder")
	})
	// A resume round whose target runs no agent waits for it, then ends skipped (stage 5).
	target, err := svc.cfg.store.CreateSession(structure.SessionSpec{Name: "target", Pane: newUUID().String(), Index: -1, NewPane: true})
	if err != nil {
		t.Fatal(err)
	}
	ps := api.RoundSpec{Name: "asks", Mode: api.RoundModeResume, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Target: target.ID, Prompt: "triage", WaitMS: 300}
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: ps, RunNow: true}, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the resume run to be skipped", func() bool {
		rs, _ := svc.Runs(api.RoundRunsParams{RoundRef: api.RoundRef{Name: "asks"}})
		return len(rs.Runs) == 1 && rs.Runs[0].State == api.RoundRunStateSkipped && rs.Runs[0].Reason == lockNoAgent
	})
	if err := svc.Delete(api.RoundRef{Name: "asks"}); err != nil {
		t.Fatal(err)
	}
	// A fresh harness round: refused with no adapter or no program; with both, its run starts
	// the harness (here the start fails: no holder).
	hs := api.RoundSpec{Name: "fresh", Mode: api.RoundModeFresh, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Project: p.ID, Harness: "pi", Prompt: "triage"}
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: hs}, ""); kind(err) != api.ErrorKindInvalid {
		t.Errorf("a harness with no adapter: %v", err)
	}
	hs.Harness = "codex"
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: hs}, ""); kind(err) != api.ErrorKindUnavailable {
		t.Errorf("a harness not installed: %v", err)
	}
	hs.Harness = "claude"
	if _, err := svc.Create(ctx, api.RoundCreateParams{Round: hs, RunNow: true}, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the harness run to fail at its start", func() bool {
		rs, _ := svc.Runs(api.RoundRunsParams{RoundRef: api.RoundRef{Name: "fresh"}})
		return len(rs.Runs) == 1 && rs.Runs[0].State == api.RoundRunStateFailed && strings.Contains(rs.Runs[0].Reason, "no holder")
	})
	if err := svc.Delete(api.RoundRef{Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if len(svc.List()) != 1 {
		t.Errorf("list after delete: %+v", svc.List())
	}
}

// TestRoundPlan: the export of a round, read back, plans nothing; defaults written or left out
// compare equal; each change is planned once.
func TestRoundPlan(t *testing.T) {
	have := []api.Round{{ID: "round-1", Revision: 3, RoundSpec: api.RoundSpec{
		Name: "nightly", Mode: api.RoundModeFresh, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: "0 3 * * *", Zone: "Europe/Berlin"},
		Command: []string{"make", "check"}, Project: "p-1", ProjectRemote: "example.com/team/app", TimeoutMS: 3600000,
		CatchUp: "once", OnFinish: "close", Notify: "failures"}}}
	b, err := exportRounds(have)
	if err != nil {
		t.Fatal(err)
	}
	var f roundsFile
	if _, err := decodeTOML(b, &f); err != nil {
		t.Fatal(err)
	}
	if plan, err := planApply(f, have); err != nil || len(plan) != 0 {
		t.Fatalf("plan of the export: %+v %v", plan, err)
	}
	// Defaults left out, a preset for the same cron, no zone, the remote as a URL: still equal.
	short := roundsFile{Rounds: []fileRound{{Name: "nightly", Cron: "0 3 * * *", Command: []string{"make", "check"},
		ProjectRemote: "https://example.com/team/app.git"}}}
	if plan, err := planApply(short, have); err != nil || len(plan) != 0 {
		t.Fatalf("plan of the short form: %+v %v", plan, err)
	}
	daily := have
	daily[0].Trigger.Cron = "0 0 * * *"
	every := roundsFile{Rounds: []fileRound{{Name: "nightly", Every: "1d", Command: []string{"make", "check"}, ProjectRemote: "example.com/team/app"}}}
	if plan, err := planApply(every, daily); err != nil || len(plan) != 0 {
		t.Fatalf("plan of a preset: %+v %v", plan, err)
	}
	for _, bad := range []roundsFile{
		{Rounds: []fileRound{{Name: "a", Manual: true}, {Name: "a", Manual: true}}},
		{Rounds: []fileRound{{Manual: true}}},
		{Rounds: []fileRound{{Name: "a", Every: "7m"}}},
		{Rounds: []fileRound{{Name: "a", Manual: true, Timeout: "soon"}}},
	} {
		if _, err := planApply(bad, nil); err == nil {
			t.Errorf("planned %+v", bad)
		}
	}
	if _, err := (fileRound{Name: "a", Cron: "* * * * *", Manual: true}).spec(); err == nil {
		t.Error("two triggers made a spec")
	}
}

func decodeTOML(b []byte, v any) (toml.MetaData, error) { return toml.Decode(string(b), v) }

// TestRoundHookRoute: the round methods on the pane route take the pane's session as the
// creator and as target self; a pane with no session creates as a user and cannot say self.
func TestRoundHookRoute(t *testing.T) {
	d := t.TempDir()
	var notices []string
	svc := fakeRoundsService(t, d, &notices)
	st := svc.cfg.store
	ss, err := st.CreateSession(structure.SessionSpec{Name: "agent", Pane: "pane-1", Index: -1, NewPane: true})
	if err != nil {
		t.Fatal(err)
	}
	h := roundHook(st, svc)
	ctx := context.Background()
	spec := api.RoundSpec{Name: "self", Mode: api.RoundModeResume, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Target: TargetSelf, Prompt: "check"}
	params, _ := json.Marshal(api.RoundCreateParams{Round: spec})
	res, _, err := h(ctx, "pane-1", "round.create", params)
	if err != nil {
		t.Fatal(err)
	}
	made := res.(api.RoundCreated)
	if made.Round.Target != ss.ID || made.Round.CreatedBy != ss.ID {
		t.Fatalf("from the pane: %+v", made.Round)
	}
	spec.Name = "other"
	params, _ = json.Marshal(api.RoundCreateParams{Round: spec})
	if _, _, err := h(ctx, "pane-unfiled", "round.create", params); err == nil {
		t.Error("target self from a pane with no session")
	}
	for _, m := range []string{"round.get", "round.pause", "round.unpause", "round.runs"} {
		if _, _, err := h(ctx, "pane-1", m, json.RawMessage(`{"name":"self"}`)); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	if res, _, err := h(ctx, "pane-1", "round.list", json.RawMessage(`{}`)); err != nil || len(res.(api.RoundList).Rounds) != 1 {
		t.Errorf("list: %+v %v", res, err)
	}
	if _, _, err := h(ctx, "pane-1", "round.delete", json.RawMessage(`{"name":"self"}`)); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, _, err := h(ctx, "pane-1", "round.get", json.RawMessage(`{bad`)); err == nil {
		t.Error("bad params")
	}
}
