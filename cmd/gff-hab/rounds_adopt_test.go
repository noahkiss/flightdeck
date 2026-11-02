package main

// Unit tests of the run watch in gff-hab over a fake holder link: adoption at start (waiting
// for holders, an exit while gff-hab was down, a lost start's session found again), one end
// per run, a pane that exited before its watch, retention that keeps live sessions, the
// deleted mark on round.updated, and the pane route's narrowing (rounds.md 8.4, 7.5, 7.6, 13).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// seedRound stores a manual command round in state and returns it, before any service opens.
func seedRound(t *testing.T, state string) rounds.Entry {
	t.Helper()
	st, _, err := rounds.Open(state, rounds.Options{Zone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.Create(rounds.Round{Name: "seeded", Mode: rounds.ModeFresh, Project: "p-1", Command: []string{"true"},
		Trigger: rounds.Trigger{Kind: rounds.TriggerManual}}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// seedRun stores a running run of round with session, as a gff-hab that stopped mid-run left it.
func seedRun(t *testing.T, state, roundID, session string) rounds.Run {
	t.Helper()
	st, _, err := rounds.Open(state, rounds.Options{Zone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := st.Get(roundID)
	now := time.Now()
	run := rounds.Run{ID: rounds.NewID("run"), Number: e.State.LastRun + 1, RoundID: roundID, Trigger: rounds.TriggerManual,
		State: rounds.RunRunning, Created: now, StartedAt: &now, SessionID: session}
	if _, err := st.QueueRun(run, func(rs *rounds.RoundState) { rs.LastRun = run.Number }); err != nil {
		t.Fatal(err)
	}
	return run
}

// fileRunSession files a session for pane in the structure state, marked with run, and closes
// the store again, so the service under test opens it.
func fileRunSession(t *testing.T, state string, holder *structure.FakeHolder, pane string, run rounds.Run) api.Session {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: state, Holders: holder})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := st.CreateSession(structure.SessionSpec{Name: "seeded", Pane: pane, Index: -1,
		Round: &api.SessionRound{Round: run.RoundID, Run: run.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func hpane(id string) structure.HolderPane {
	return structure.HolderPane{ID: id, HolderID: "one", HolderBuild: "test", Cols: 80, Rows: 24}
}

func runState(t *testing.T, svc *roundsService, roundID string) rounds.Run {
	t.Helper()
	runs, _, err := svc.store.Runs().Page(roundID, 0, 1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	return runs[0]
}

func watched(svc *roundsService, runID string) bool {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.watches[runID] != nil
}

// fakeLink is the holder link's view the service reads: which panes it lists, their status,
// and whether every holder is reached.
type fakeLink struct {
	mu       sync.Mutex
	panes    map[string]link.PaneStatus
	complete bool
}

func (f *fakeLink) pane(id string) (link.PaneStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.panes[id]
	return st, ok
}

func (f *fakeLink) set(id string, st link.PaneStatus, complete bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes == nil {
		f.panes = map[string]link.PaneStatus{}
	}
	f.panes[id] = st
	f.complete = complete
}

func (f *fakeLink) isComplete() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.complete }

func (f *fakeLink) config(c *roundsConfig) {
	c.pane = f.pane
	c.complete = f.isComplete
}

// TestRoundAdoptWaitsForHolders: at start, a run whose pane the link does not list yet waits
// while a holder is not reached; a pane that comes back is watched at once, well before the
// periodic look; and with every holder reached and no pane, the run fails "host restarted".
func TestRoundAdoptWaitsForHolders(t *testing.T) {
	state := t.TempDir()
	const u1 = "11111111-2222-3333-4444-555555555555"
	uid, _ := parseUUID(u1)
	e := seedRound(t, state)
	holder := structure.NewFakeHolder(hpane(u1))
	run := seedRun(t, state, e.Round.ID, "")
	ss := fileRunSession(t, state, holder, u1, run)
	seedRunSession(t, state, run, ss.ID)
	fl := &fakeLink{panes: map[string]link.PaneStatus{}}
	svc, _ := fakeRoundsServiceWith(t, state, holder, fl.config)
	svc.start()
	time.Sleep(100 * time.Millisecond)
	if got := runState(t, svc, e.Round.ID); got.State != rounds.RunRunning || watched(svc, run.ID) {
		t.Fatalf("with a holder not reached: %+v, watched %v", got, watched(svc, run.ID))
	}
	fl.set(u1, link.PaneStatus{}, false)
	at := time.Now()
	svc.notice(link.Notice{Kind: link.PaneAdded})
	waitUntil(t, "the run watched", func() bool { return watched(svc, run.ID) })
	if d := time.Since(at); d > roundsCheckEvery/2 {
		t.Errorf("the pane coming back was looked at after %v; a PaneAdded must look at once", d)
	}

	// The holder connection ends: the run waits again, and fails once every holder is
	// reached without the pane.
	fl.set(u1, link.PaneStatus{Lost: true}, false)
	svc.notice(link.Notice{Kind: link.PaneGone, Pane: link.PaneStatus{UUID: uid, Lost: true}})
	if watched(svc, run.ID) {
		t.Fatal("a lost pane is still watched")
	}
	time.Sleep(100 * time.Millisecond)
	if got := runState(t, svc, e.Round.ID); got.State != rounds.RunRunning {
		t.Fatalf("a lost pane ended the run: %+v", got)
	}
	fl.set(u1, link.PaneStatus{Lost: true}, true)
	svc.notice(link.Notice{Kind: link.PaneAdded})
	waitUntil(t, "host restarted", func() bool {
		r := runState(t, svc, e.Round.ID)
		return r.State == rounds.RunFailed && r.Reason == reasonHostRestarted && r.SessionID == ss.ID
	})
	svc.mu.Lock()
	stopped := svc.recheck == nil
	svc.mu.Unlock()
	if !stopped {
		t.Error("the recheck loop still runs with nothing pending")
	}
}

// seedRunSession sets the run's session id in the run log.
func seedRunSession(t *testing.T, state string, run rounds.Run, session string) {
	t.Helper()
	st, _, err := rounds.Open(state, rounds.Options{Zone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	run.SessionID = session
	if err := st.Runs().Update(run); err != nil {
		t.Fatal(err)
	}
}

// TestRoundAdoptExitWhileDown: a pane the holder released while gff-hab was down, whose
// session record holds the exit, ends the run by that exit at start.
func TestRoundAdoptExitWhileDown(t *testing.T) {
	state := t.TempDir()
	e := seedRound(t, state)
	holder := structure.NewFakeHolder(hpane("p1"))
	run := seedRun(t, state, e.Round.ID, "")
	ss := fileRunSession(t, state, holder, "p1", run)
	seedRunSession(t, state, run, ss.ID)
	code := 3
	holder.Exit("p1", api.ExitStatus{Code: &code})
	fl := &fakeLink{complete: true}
	svc, st := fakeRoundsServiceWith(t, state, holder, fl.config)
	if got, _ := st.Session(ss.ID); got.Exit == nil || got.Exit.Code == nil {
		t.Fatalf("session exit: %+v", got)
	}
	svc.start()
	waitUntil(t, "the run ended by its exit", func() bool {
		r := runState(t, svc, e.Round.ID)
		return r.State == rounds.RunFailed && r.Reason == "exit status 3"
	})
}

// TestRoundLostStartAttach: a run saved as running with no session id, whose session record
// names it (gff-hab stopped after the spawn), gets that session back and is watched; with no
// such session it fails "start lost".
func TestRoundLostStartAttach(t *testing.T) {
	state := t.TempDir()
	e := seedRound(t, state)
	holder := structure.NewFakeHolder(hpane("p1"))
	run := seedRun(t, state, e.Round.ID, "")
	ss := fileRunSession(t, state, holder, "p1", run)
	fl := &fakeLink{}
	fl.set("p1", link.PaneStatus{}, true)
	svc, _ := fakeRoundsServiceWith(t, state, holder, fl.config)
	svc.start()
	if got := runState(t, svc, e.Round.ID); got.State != rounds.RunRunning || got.SessionID != ss.ID || !watched(svc, run.ID) {
		t.Fatalf("lost start with its session: %+v watched %v", got, watched(svc, run.ID))
	}
	if _, err := svc.Run(api.RoundRef{ID: e.Round.ID}); err == nil {
		t.Error("a second run started beside the found one")
	}

	state2 := t.TempDir()
	e2 := seedRound(t, state2)
	seedRun(t, state2, e2.Round.ID, "")
	svc2, _ := fakeRoundsServiceWith(t, state2, structure.NewFakeHolder(), nil)
	svc2.start()
	if got := runState(t, svc2, e2.Round.ID); got.State != rounds.RunFailed || got.Reason != rounds.ReasonStartLost {
		t.Fatalf("lost start with no session: %+v", got)
	}
}

// TestRoundFinishOnce: the first end of a run wins; a later exit, timeout or take over finds no
// watch, so the run ends once.
func TestRoundFinishOnce(t *testing.T) {
	state := t.TempDir()
	e := seedRound(t, state)
	holder := structure.NewFakeHolder(hpane("p1"))
	run := seedRun(t, state, e.Round.ID, "")
	ss := fileRunSession(t, state, holder, "p1", run)
	seedRunSession(t, state, run, ss.ID)
	fl := &fakeLink{}
	fl.set("p1", link.PaneStatus{}, true)
	var ends atomic.Int32
	svc, _ := fakeRoundsServiceWith(t, state, holder, func(c *roundsConfig) {
		fl.config(c)
		c.publish = func(ev structure.Event) {
			if r, ok := ev.Data.(api.RoundRun); ok && r.ID == run.ID && rounds.RunState(r.State).Final() {
				ends.Add(1)
			}
		}
	})
	svc.start()
	if !watched(svc, run.ID) {
		t.Fatal("not watched")
	}
	if !svc.finish(run.ID, rounds.RunUpdate{State: rounds.RunTakenOver, Reason: reasonTyped}) {
		t.Fatal("the first end was refused")
	}
	if svc.finish(run.ID, rounds.RunUpdate{State: rounds.RunFailed}) {
		t.Fatal("a second end was taken")
	}
	svc.exited("p1", hp.ExitStatus{Code: 1})
	svc.timedOut(run.ID)
	waitUntil(t, "the end", func() bool { return runState(t, svc, e.Round.ID).State == rounds.RunTakenOver })
	time.Sleep(100 * time.Millisecond)
	if n := ends.Load(); n != 1 {
		t.Fatalf("%d final run events", n)
	}
}

// TestRoundWatchAlreadyExited: a program that exited before its watch began (the exit notice
// came first) ends its run at once, by its status.
func TestRoundWatchAlreadyExited(t *testing.T) {
	state := t.TempDir()
	e := seedRound(t, state)
	holder := structure.NewFakeHolder(hpane("p1"))
	fl := &fakeLink{}
	fl.set("p1", link.PaneStatus{Exited: true, Exit: hp.ExitStatus{Code: 0}}, true)
	var made api.Session
	svc, st := fakeRoundsServiceWith(t, state, holder, func(c *roundsConfig) {
		fl.config(c)
		c.create = func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			return api.SessionCreated{Session: made.ID, Pane: "p1"}, nil
		}
	})
	var err error
	if made, err = st.CreateSession(structure.SessionSpec{Name: "fast", Pane: "p1", Index: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(api.RoundRef{ID: e.Round.ID}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run succeeded", func() bool { return runState(t, svc, e.Round.ID).State == rounds.RunSucceeded })
}

// TestRoundPruneKeepsLiveSessions: retention removes the session records of the runs it drops
// only when they have exited; a live one (taken over, the user's now) stays.
func TestRoundPruneKeepsLiveSessions(t *testing.T) {
	state := t.TempDir()
	e := seedRound(t, state)
	holder := structure.NewFakeHolder(hpane("p1"), hpane("p2"), hpane("p3"))
	code := 0
	holder.Exit("p2", api.ExitStatus{Code: &code})
	holder.Exit("p3", api.ExitStatus{Code: &code})
	st, err := structure.Open(structure.Config{Dir: state, Holders: holder})
	if err != nil {
		t.Fatal(err)
	}
	live, _ := st.CreateSession(structure.SessionSpec{Name: "live", Pane: "p1", Index: -1})
	// Only a session a run made goes with it: a resume run's target is the user's.
	doneRun := rounds.NewID("run")
	done, _ := st.CreateSession(structure.SessionSpec{Name: "done", Pane: "p2", Index: -1, Round: &api.SessionRound{Round: e.Round.ID, Run: doneRun}})
	target, _ := st.CreateSession(structure.SessionSpec{Name: "target", Pane: "p3", Index: -1})
	st.Close()
	// More ended runs than the retention keeps, all older than KeepFor; runs 1 to 3 go.
	old := time.Now().Add(-rounds.KeepFor - 24*time.Hour)
	var runs []rounds.Run
	for n := 1; n <= rounds.KeepRuns+3; n++ {
		r := rounds.Run{ID: rounds.NewID("run"), Number: n, RoundID: e.Round.ID, Trigger: rounds.TriggerManual,
			State: rounds.RunSucceeded, Created: old, EndedAt: &old}
		switch n {
		case 1:
			r.State, r.SessionID = rounds.RunTakenOver, live.ID
		case 2:
			r.ID, r.SessionID = doneRun, done.ID
		case 3:
			r.SessionID = target.ID
		}
		runs = append(runs, r)
	}
	writeRuns(t, state, e.Round.ID, runs)
	svc, sst := fakeRoundsServiceWith(t, state, holder, nil)
	svc.prune(e.Round.ID)
	if _, ok := sst.Session(live.ID); !ok {
		t.Error("a live session was removed with its run")
	}
	if _, ok := sst.Session(done.ID); ok {
		t.Error("an exited session stayed after its run was dropped")
	}
	if _, ok := sst.Session(target.ID); !ok {
		t.Error("a session the run did not make (a resume target) was removed")
	}
	if got, _, _ := svc.store.Runs().Page(e.Round.ID, 0, 1000); len(got) != rounds.KeepRuns {
		t.Errorf("%d runs kept", len(got))
	}
}

// writeRuns puts runs in the rounds state file directly: many runs in one write.
func writeRuns(t *testing.T, state, roundID string, runs []rounds.Run) {
	t.Helper()
	p := filepath.Join(state, "rounds", "state.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	rb, _ := json.Marshal(map[string][]rounds.Run{roundID: runs})
	f["runs"] = rb
	out, _ := json.Marshal(f)
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRoundDeletedEvent: a delete sends round.updated with deleted set, at the next revision.
func TestRoundDeletedEvent(t *testing.T) {
	state := t.TempDir()
	e := seedRound(t, state)
	var mu sync.Mutex
	var last api.Round
	var rev uint64
	svc, _ := fakeRoundsServiceWith(t, state, structure.NewFakeHolder(), func(c *roundsConfig) {
		c.publish = func(ev structure.Event) {
			if r, ok := ev.Data.(api.Round); ok {
				mu.Lock()
				last, rev = r, ev.Revision
				mu.Unlock()
			}
		}
	})
	if err := svc.Delete(api.RoundRef{ID: e.Round.ID}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !last.Deleted || last.ID != e.Round.ID || rev < 2 || last.Revision != rev {
		t.Fatalf("delete event: %+v rev %d", last, rev)
	}
}

// TestRoundPaneNarrowing: from a pane, a round may target only the calling session, and only
// rounds that session created can be updated, deleted, paused, unpaused or run; each other
// call is refused with denied. Reads stay open. The control socket keeps full access.
func TestRoundPaneNarrowing(t *testing.T) {
	state := t.TempDir()
	svc, st := fakeRoundsServiceWith(t, state, structure.NewFakeHolder(), nil)
	me, _ := st.CreateSession(structure.SessionSpec{Name: "me", Pane: "pane-me", Index: -1, NewPane: true})
	other, _ := st.CreateSession(structure.SessionSpec{Name: "other", Pane: "pane-other", Index: -1, NewPane: true})
	h := roundHook(st, svc)
	ctx := context.Background()
	kind := func(err error) api.ErrorKind {
		var ce *control.Error
		if errors.As(err, &ce) {
			return ce.Data.Kind
		}
		return "none"
	}
	spec := func(name, target string) api.RoundSpec {
		return api.RoundSpec{Name: name, Mode: api.RoundModeResume, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
			Target: target, Prompt: "check"}
	}
	create := func(pane string, s api.RoundSpec) error {
		b, _ := json.Marshal(api.RoundCreateParams{Round: s})
		_, _, err := h(ctx, pane, "round.create", b)
		return err
	}
	// Targets.
	if err := create("pane-me", spec("mine", TargetSelf)); err != nil {
		t.Fatal(err)
	}
	if err := create("pane-me", spec("mine-by-id", me.ID)); err != nil {
		t.Fatalf("own id as target: %v", err)
	}
	if err := create("pane-me", spec("theirs", other.ID)); kind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "only the calling session") {
		t.Errorf("another session as target: %v", err)
	}
	if err := create("pane-unfiled", spec("x", TargetSelf)); kind(err) != api.ErrorKindInvalid {
		t.Errorf("self from a pane with no session: %v", err)
	}
	if err := create("pane-unfiled", spec("y", other.ID)); kind(err) != api.ErrorKindDenied {
		t.Errorf("a target from a pane with no session: %v", err)
	}
	// A round the user made on the control socket, and one another session made.
	user, err := svc.Create(ctx, api.RoundCreateParams{Round: spec("users", other.ID)}, "")
	if err != nil {
		t.Fatalf("the control socket targets any session: %v", err)
	}
	if err := create("pane-other", spec("others", TargetSelf)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"users", "others"} {
		ref, _ := json.Marshal(api.RoundRef{Name: name})
		for _, m := range []string{"round.delete", "round.pause", "round.unpause", "round.run"} {
			if _, _, err := h(ctx, "pane-me", m, ref); kind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "only rounds this session created") {
				t.Errorf("%s on %s: %v", m, name, err)
			}
		}
		r, _ := svc.Get(api.RoundRef{Name: name})
		up := r.RoundSpec
		up.ProjectRemote = ""
		b, _ := json.Marshal(api.RoundUpdateParams{ID: r.ID, Round: up})
		if _, _, err := h(ctx, "pane-me", "round.update", b); kind(err) != api.ErrorKindDenied {
			t.Errorf("round.update on %s: %v", name, err)
		}
		for _, m := range []string{"round.get", "round.runs"} {
			if _, _, err := h(ctx, "pane-me", m, ref); err != nil {
				t.Errorf("%s on %s: %v", m, name, err)
			}
		}
		// A pane with no session changes nothing.
		if _, _, err := h(ctx, "pane-unfiled", "round.pause", ref); kind(err) != api.ErrorKindDenied {
			t.Errorf("pause from a pane with no session: %v", err)
		}
	}
	// Its own round: every call works, and an update may not aim it at another session.
	mine, _ := svc.Get(api.RoundRef{Name: "mine"})
	up := mine.RoundSpec
	up.ProjectRemote, up.Target = "", other.ID
	b, _ := json.Marshal(api.RoundUpdateParams{ID: mine.ID, Round: up})
	if _, _, err := h(ctx, "pane-me", "round.update", b); kind(err) != api.ErrorKindDenied {
		t.Errorf("update of its own round to another target: %v", err)
	}
	up.Target, up.Prompt = TargetSelf, "check again"
	b, _ = json.Marshal(api.RoundUpdateParams{ID: mine.ID, Round: up})
	if _, _, err := h(ctx, "pane-me", "round.update", b); err != nil {
		t.Errorf("update of its own round: %v", err)
	}
	ref, _ := json.Marshal(api.RoundRef{Name: "mine"})
	for _, m := range []string{"round.pause", "round.unpause", "round.delete"} {
		if _, _, err := h(ctx, "pane-me", m, ref); err != nil {
			t.Errorf("%s on its own round: %v", m, err)
		}
	}
	// The control socket keeps full access.
	if _, err := svc.Pause(api.RoundRef{ID: user.Round.ID}); err != nil {
		t.Errorf("the control socket: %v", err)
	}
	if err := svc.Delete(api.RoundRef{Name: "others"}); err != nil {
		t.Errorf("the control socket: %v", err)
	}
}
