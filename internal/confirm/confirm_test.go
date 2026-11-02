package confirm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakeRunner records every run. Host "self" is this hab; "gone" fails the check; a run on
// "lost" loses its answer; "refuses" fails.
type fakeRunner struct {
	mu   sync.Mutex
	runs []run
}

type run struct {
	id, from string
	start    api.ConfirmStart
}

func (f *fakeRunner) CheckStart(s api.ConfirmStart) (api.ConfirmStart, bool, error) {
	if s.Host == "gone" {
		return s, false, control.Errorf(api.ErrorKindNotFound, "no peer gone")
	}
	if err := CheckSeed(s.Seed); err != nil {
		return s, false, err
	}
	s.Project = "app"
	return s, s.Host == "self" || s.Host == "", nil
}

func (f *fakeRunner) RunStart(_ context.Context, id, from string, s api.ConfirmStart) (api.ConfirmResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, run{id, from, s})
	f.mu.Unlock()
	switch s.Host {
	case "lost":
		return api.ConfirmResult{}, fmt.Errorf("%w: the link to lost dropped", ErrUnknown)
	case "refuses":
		return api.ConfirmResult{}, control.Errorf(api.ErrorKindInvalid, "cwd is not a directory")
	}
	return api.ConfirmResult{Host: s.Host, Session: "s-" + id[:4], Name: "app-1", Pane: "p1"}, nil
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

type rig struct {
	svc    *Service
	runner *fakeRunner
	on     bool
	mu     sync.Mutex
	events []structure.Event
	notes  []string
}

func newRig(t *testing.T, on bool, ttl time.Duration) *rig {
	t.Helper()
	r := &rig{runner: &fakeRunner{}, on: on}
	svc, err := New(Config{
		Runner:  r.runner,
		Enabled: func() bool { return r.on },
		Capcom:  func(s string) bool { return s == "capcom" },
		Publish: func(e structure.Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() },
		Notify: func(session, text string) error {
			r.mu.Lock()
			r.notes = append(r.notes, session+": "+text)
			r.mu.Unlock()
			return nil
		},
		TTL: ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	r.svc = svc
	return r
}

func (r *rig) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, string(e.Type))
	}
	return out
}

func (r *rig) notesNow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notes...)
}

func start(host string) *api.ConfirmStart {
	return &api.ConfirmStart{Host: host, Folder: "/src/app", Harness: "claude", Seed: "Build the cache."}
}

func kind(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

func TestOnNothingRunsWithoutAConfirm(t *testing.T) {
	r := newRig(t, true, time.Hour)
	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta"), From: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "open" || c.Record || c.ExpiresAt == "" || c.Start.Project != "app" {
		t.Fatalf("request: %+v", c)
	}
	if r.runner.count() != 0 {
		t.Fatal("a start ran before the confirm")
	}
	if open := r.svc.Open(); len(open) != 1 || open[0].ID != c.ID {
		t.Fatalf("open: %+v", open)
	}
	got, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" || got.AnsweredBy != "phone" || got.Result == nil || got.Result.Name != "app-1" {
		t.Fatalf("answer: %+v", got)
	}
	if r.runner.count() != 1 || r.runner.runs[0].id != c.ID || r.runner.runs[0].from != "agent" {
		t.Fatalf("runs: %+v", r.runner.runs)
	}
	// The first answer won: a second one gets conflict, and nothing runs again.
	if _, err := r.svc.Answer(context.Background(), "mac", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"}); kind(err) != api.ErrorKindConflict {
		t.Fatalf("second answer: %v", err)
	}
	if r.runner.count() != 1 {
		t.Fatal("a second answer ran the start again")
	}
	if ts := r.types(); strings.Join(ts, ",") != "confirm.requested,confirm.closed" {
		t.Fatalf("events: %v", ts)
	}
	if len(r.svc.Open()) != 0 {
		t.Fatal("still open after the answer")
	}
	// The request asked for no wait, so the requesting session hears the outcome.
	if n := r.notesNow(); len(n) != 1 || !strings.HasPrefix(n[0], "agent: Confirmation "+c.ID+": the owner confirmed") {
		t.Fatalf("notes: %v", n)
	}
}

func TestOffRunsAtOnceAsARecord(t *testing.T) {
	r := newRig(t, false, time.Hour)
	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta"), From: "capcom", WaitMS: 120000})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "done" || !c.Record || c.Result == nil {
		t.Fatalf("record: %+v", c)
	}
	if r.runner.count() != 1 {
		t.Fatalf("runs: %d", r.runner.count())
	}
	if ts := r.types(); strings.Join(ts, ",") != "confirm.requested,confirm.closed" {
		t.Fatalf("events: %v", ts)
	}
	if n := r.notesNow(); len(n) != 0 {
		t.Fatalf("the request returned the outcome, yet a note went: %v", n)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "cancel"}); kind(err) != api.ErrorKindConflict {
		t.Fatalf("answer to a record: %v", err)
	}
}

// An agent's start on its own host runs at once; Capcom's needs a confirm while the setting is on.
func TestOwnHostAndCapcom(t *testing.T) {
	r := newRig(t, true, time.Hour)
	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("self"), From: "agent"})
	if err != nil || !c.Record || c.State != "done" {
		t.Fatalf("own host: %+v %v", c, err)
	}
	c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("self"), From: "capcom"})
	if err != nil || c.Record || c.State != "open" {
		t.Fatalf("capcom on its own host: %+v %v", c, err)
	}
}

func TestExpiredNeverRuns(t *testing.T) {
	r := newRig(t, true, 40*time.Millisecond)
	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta"), From: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := r.svc.Get(c.ID)
		if got.State == "expired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not expired: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"}); kind(err) != api.ErrorKindConflict {
		t.Fatalf("answer after expiry: %v", err)
	}
	if r.runner.count() != 0 {
		t.Fatal("an expired confirmation ran")
	}
	if n := r.notesNow(); len(n) != 1 || !strings.Contains(n[0], "expired") {
		t.Fatalf("notes: %v", n)
	}
}

// The tool waits: an answer within the wait comes back in the call and no note goes; after
// the wait ends, the answer reaches the session as a note.
func TestWaitThenNote(t *testing.T) {
	r := newRig(t, true, time.Hour)
	var c api.Confirmation
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		c, err = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta"), From: "agent", WaitMS: 5000})
		if err != nil {
			t.Error(err)
		}
	}()
	var id string
	for id == "" {
		if open := r.svc.Open(); len(open) == 1 {
			id = open[0].ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: id, Choice: "cancel"}); err != nil {
		t.Fatal(err)
	}
	<-done
	if c.State != "cancelled" {
		t.Fatalf("the wait returned %+v", c)
	}
	if n := r.notesNow(); len(n) != 0 {
		t.Fatalf("notes: %v", n)
	}

	c, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta"), From: "agent", WaitMS: 30})
	if err != nil || c.State != "open" {
		t.Fatalf("still waiting: %+v %v", c, err)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"}); err != nil {
		t.Fatal(err)
	}
	if n := r.notesNow(); len(n) != 1 || !strings.Contains(n[0], "session app-1 started on beta") {
		t.Fatalf("notes: %v", n)
	}
}

func TestEditsRunAsEdited(t *testing.T) {
	r := newRig(t, true, time.Hour)
	c, _ := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta")})
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm",
		Edits: &api.ConfirmEdits{Host: "gamma"}}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("a new host with no folder: %v", err)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm",
		Edits: &api.ConfirmEdits{Seed: strings.Repeat("é", MaxSeed+1)}}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("a long seed: %v", err)
	}
	got, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm",
		Edits: &api.ConfirmEdits{Host: "gamma", Folder: "/work/app", Seed: "Fix the cache.", Harness: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Edited || got.Start.Host != "gamma" || r.runner.runs[0].start.Seed != "Fix the cache." || r.runner.runs[0].start.Harness != "codex" {
		t.Fatalf("edited: %+v runs %+v", got, r.runner.runs)
	}
}

func TestOutcomes(t *testing.T) {
	r := newRig(t, false, time.Hour)
	c, _ := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("lost")})
	if c.State != "unknown" || !strings.Contains(c.Reason, "dropped") {
		t.Fatalf("lost: %+v", c)
	}
	c, _ = r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("refuses")})
	if c.State != "failed" || !strings.Contains(c.Reason, "cwd") {
		t.Fatalf("refused: %+v", c)
	}
	if _, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("gone")}); kind(err) != api.ErrorKindNotFound {
		t.Fatalf("unknown host: %v", err)
	}
	if _, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "setup", Setup: &api.ConfirmSetup{Host: "beta", Repository: "r"}}); kind(err) != api.ErrorKindUnavailable {
		t.Fatalf("setup: %v", err)
	}
	if _, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "launch"}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("bad kind: %v", err)
	}
	if all := r.svc.List(true); len(all) != 2 || all[0].State != "failed" {
		t.Fatalf("list all: %+v", all)
	}
}

func TestCancelRunsNothing(t *testing.T) {
	r := newRig(t, true, time.Hour)
	c, _ := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: start("beta")})
	got, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "cancel"})
	if err != nil || got.State != "cancelled" || r.runner.count() != 0 {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: "nope", Choice: "cancel"}); kind(err) != api.ErrorKindNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := r.svc.Answer(context.Background(), "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "maybe"}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("bad choice: %v", err)
	}
}
