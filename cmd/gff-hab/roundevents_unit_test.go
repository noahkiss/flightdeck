package main

// Event triggers and the capcom and group modes in gff-hab, over the fake rounds service
// (rounds.md 4.3, 4.5, 5.2): the matcher on each of the six events with its filters and its
// no-loop rule, Logbook ops counted once by fold number, a private record told only as a count;
// the message runs against fake channels (accepted, the outcome once, unknown after
// outcome_wait, a too-old coordinator skipped with its batch kept, an ended group completing
// the round with one notice); a move's batch and an event filter's project by remote.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

const testSelf = "SHA256:self"

// eventRound creates a fresh command round on an event trigger and returns its id.
func eventRound(t *testing.T, svc *roundsService, name, event string, f *api.RoundEventFilter) string {
	t.Helper()
	pr, err := svc.cfg.store.CreateProject("run-"+name, "example.com/run/"+name, "")
	if err != nil {
		t.Fatal(err)
	}
	made, err := svc.Create(context.Background(), api.RoundCreateParams{Round: api.RoundSpec{Name: name, Mode: api.RoundModeFresh,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindEvent, Event: event, Filter: f},
		Command: []string{"true"}, Project: pr.ID}}, "")
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return made.Round.ID
}

// waitPending waits until round id's pending batch holds n events.
func waitPending(t *testing.T, svc *roundsService, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for svc.sched.Pending(id) != n {
		if time.Now().After(deadline) {
			t.Fatalf("round %s: %d pending, want %d", id, svc.sched.Pending(id), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settled sends a probe event that only the probe round takes, and waits for it: every event
// observed before it has been handled.
type eventProber struct {
	t     *testing.T
	svc   *roundsService
	probe string
	n     int
}

func (p *eventProber) settle() {
	p.t.Helper()
	p.n++
	p.svc.matcher.observe(structure.Event{Type: api.EventType(rounds.EventHabWoke), Data: api.HabWoke{SleptS: int64(p.n)}})
	waitPending(p.t, p.svc, p.probe, p.n)
}

func TestRoundMatcher(t *testing.T) {
	catalog := api.CatalogList{Hosts: []api.CatalogEntry{{Catalog: api.Catalog{DeviceID: testSelf,
		Capabilities: []api.CatalogCapability{{Name: capCapcomService}, {Name: capRoundLock}},
		Capcom:       &api.CatalogCapcom{Home: testSelf, Assignment: 1, Service: "capcom-actor"}}}}}
	svc, st := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.self = testSelf
		c.catalog = func() api.CatalogList { return catalog }
	})
	proj, err := st.CreateProject("app", "example.com/team/app", "")
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.CreateSession(structure.SessionSpec{Name: "worker", Project: proj.ID, Pane: "pane-w", Index: -1, NewPane: true})
	if err != nil {
		t.Fatal(err)
	}
	p := &eventProber{t: t, svc: svc, probe: eventRound(t, svc, "probe", rounds.EventHabWoke, nil)}
	obs := func(typ, id string, data any) {
		svc.matcher.observe(structure.Event{Type: api.EventType(typ), ID: id, Data: data})
	}

	t.Run("project.updated", func(t *testing.T) {
		anyField := eventRound(t, svc, "proj-any", rounds.EventProjectUpdated, nil)
		branch := eventRound(t, svc, "proj-branch", rounds.EventProjectUpdated, &api.RoundEventFilter{Project: proj.ID, Field: []string{"branch"}})
		other := eventRound(t, svc, "proj-other", rounds.EventProjectUpdated, &api.RoundEventFilter{Project: "nope"})
		pr := proj
		pr.Head, pr.Branch = "aaaa", "main"
		obs(rounds.EventProjectUpdated, pr.ID, pr) // first sight: the matcher has nothing to compare
		pr.Head = "bbbb"
		obs(rounds.EventProjectUpdated, pr.ID, pr) // head only
		obs(rounds.EventProjectUpdated, pr.ID, pr) // nothing changed
		pr.Branch = "dev"
		obs(rounds.EventProjectUpdated, pr.ID, pr) // branch
		p.settle()
		if n := svc.sched.Pending(anyField); n != 2 {
			t.Errorf("any field: %d events, want 2", n)
		}
		if n := svc.sched.Pending(branch); n != 1 {
			t.Errorf("branch only: %d events, want 1", n)
		}
		if n := svc.sched.Pending(other); n != 0 {
			t.Errorf("another project: %d events", n)
		}
	})

	t.Run("agent.state", func(t *testing.T) {
		all := eventRound(t, svc, "agent-all", rounds.EventAgentState, nil)
		errs := eventRound(t, svc, "agent-error", rounds.EventAgentState, &api.RoundEventFilter{State: []string{"error"}, Session: ss.ID})
		obs(rounds.EventAgentState, "pane-w", api.AgentState{Pane: "pane-w", State: api.AgentStatusWorking, Turn: "t1"})
		obs(rounds.EventAgentState, "pane-w", api.AgentState{Pane: "pane-w", State: api.AgentStatusDone, Turn: "t1"})
		obs(rounds.EventAgentState, "pane-w", api.AgentState{Pane: "pane-w", State: api.AgentStatusDone, Turn: "t1"}) // the same end again
		obs(rounds.EventAgentState, "pane-w", api.AgentState{Pane: "pane-w", State: api.AgentStatusError, Turn: "t2", Error: "server_error"})
		obs(rounds.EventAgentState, "pane-x", api.AgentState{Pane: "pane-x", State: api.AgentStatusDone, Turn: "t1"}) // no session
		p.settle()
		if n := svc.sched.Pending(all); n != 2 {
			t.Errorf("done and error: %d events, want 2", n)
		}
		if n := svc.sched.Pending(errs); n != 1 {
			t.Errorf("error only: %d events, want 1", n)
		}
		b := svc.sched.PendingBatch(errs)
		if len(b.Events) != 1 || !strings.Contains(b.Events[0].Title, "worker: agent error (server_error)") {
			t.Errorf("event %+v", b.Events)
		}
	})

	t.Run("command.updated", func(t *testing.T) {
		id := eventRound(t, svc, "cmd", rounds.EventCommandUpdated, nil)
		one, zero := 1, 0
		end := uint64(100)
		obs(rounds.EventCommandUpdated, "pane-w", api.Command{ID: 1, Text: "make", End: &end, Exit: &one})
		obs(rounds.EventCommandUpdated, "pane-w", api.Command{ID: 1, Text: "make", End: &end, Exit: &one}) // again
		obs(rounds.EventCommandUpdated, "pane-w", api.Command{ID: 2, Text: "ls", End: &end, Exit: &zero})
		obs(rounds.EventCommandUpdated, "pane-w", api.Command{ID: 3, Text: "sleep 9"})
		p.settle()
		if n := svc.sched.Pending(id); n != 1 {
			t.Errorf("%d events, want the one failure once", n)
		}
	})

	t.Run("notice.raised", func(t *testing.T) {
		active := eventRound(t, svc, "notice-active", rounds.EventNoticeRaised, &api.RoundEventFilter{Level: "active", Text: "DISK"})
		capcom, err := svc.Create(context.Background(), api.RoundCreateParams{Round: api.RoundSpec{Name: "notice-capcom", Mode: api.RoundModeCapcom,
			Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindEvent, Event: rounds.EventNoticeRaised}, Prompt: "look"}}, "")
		if err != nil {
			t.Fatal(err)
		}
		obs(rounds.EventNoticeRaised, "n1", api.Notice{ID: "n1", Title: "disk full", Level: api.NoticeLevelPassive})
		obs(rounds.EventNoticeRaised, "n2", api.Notice{ID: "n2", Title: "Disk full", Level: api.NoticeLevelTimeSensitive})
		obs(rounds.EventNoticeRaised, "n3", api.Notice{ID: "n3", Title: "build failed", Level: api.NoticeLevelActive})
		// Capcom's own notice and a notice about the round itself never trigger it.
		obs(rounds.EventNoticeRaised, "n4", api.Notice{ID: "n4", Title: "from capcom", Level: api.NoticeLevelActive, Session: "capcom-actor"})
		obs(rounds.EventNoticeRaised, "n5", api.Notice{ID: "n5", Title: "round failed", Level: api.NoticeLevelActive,
			Round: &api.NoticeRound{Kind: "run_failed", Round: capcom.Round.ID}})
		p.settle()
		if n := svc.sched.Pending(active); n != 1 {
			t.Errorf("level and text: %d events, want 1", n)
		}
		if n := svc.sched.Pending(capcom.Round.ID); n != 3 {
			t.Errorf("capcom: %d events, want 3 (not its service's, not its own)", n)
		}
		if l := svc.sched.PendingBatch(active).Level(); l != "time-sensitive" {
			t.Errorf("level hint %q", l)
		}
	})

	t.Run("a paused round takes none", func(t *testing.T) {
		id := eventRound(t, svc, "woke-paused", rounds.EventHabWoke, nil)
		if _, err := svc.Pause(api.RoundRef{ID: id}); err != nil {
			t.Fatal(err)
		}
		p.settle()
		if n := svc.sched.Pending(id); n != 0 {
			t.Errorf("%d events", n)
		}
	})
}

// TestRoundMatcherLogbook: logbook.updated with a real Logbook: a created record counts once,
// a status change counts for a status filter and a title change does not, a peer's op folded
// later counts once, and a private project's record is told as a count only.
func TestRoundMatcherLogbook(t *testing.T) {
	ctx := context.Background()
	lst, err := logbook.Open(ctx, filepath.Join(t.TempDir(), "logbook.db"), logbook.Options{Origin: "SHA256:habA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lst.Close() })
	var svc *roundsService
	var mu sync.Mutex
	lb, stop, err := startLogbookVerbs(ctx, lst, nil, func() {}, func(e structure.Event) {
		mu.Lock()
		s := svc
		mu.Unlock()
		if s != nil {
			s.matcher.observe(e)
		}
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	s, _ := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) { c.logbook = lb })
	mu.Lock()
	svc = s
	mu.Unlock()
	p := &eventProber{t: t, svc: svc, probe: eventRound(t, svc, "probe", rounds.EventHabWoke, nil)}

	created := eventRound(t, svc, "lb-created", rounds.EventLogbookUpdated, &api.RoundEventFilter{Type: "task"})
	done := eventRound(t, svc, "lb-done", rounds.EventLogbookUpdated, &api.RoundEventFilter{Change: "status", Status: []string{"done"}})
	// A project Logbook has not filed: a general record is not in it.
	unfiled, err := svc.cfg.store.CreateProject("unfiled", "example.com/team/unfiled", "")
	if err != nil {
		t.Fatal(err)
	}
	inProject := eventRound(t, svc, "lb-project", rounds.EventLogbookUpdated, &api.RoundEventFilter{Project: unfiled.ID})
	person := api.LogbookActor{Kind: "person", Device: "phone"}
	w, err := lb.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpCreate, Type: "task", Title: "ship it"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := w.IDs[0]
	set := func(field, value string) {
		t.Helper()
		v, _ := json.Marshal(value)
		if _, err := lb.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpSet, ID: id, Field: field, Value: api.RawJSON(v)}}}); err != nil {
			t.Fatal(err)
		}
	}
	set("title", "ship it today")
	set("status", "done")
	p.settle()
	if n := svc.sched.Pending(created); n != 1 {
		t.Errorf("created: %d events, want 1", n)
	}
	if n := svc.sched.Pending(done); n != 1 {
		t.Errorf("status done: %d events, want 1 (the title change is not one)", n)
	}
	if n := svc.sched.Pending(inProject); n != 0 {
		t.Errorf("a general record reached a round on an unfiled project: %d events", n)
	}
	if b := svc.sched.PendingBatch(done); len(b.Events) != 1 || !strings.Contains(b.Events[0].Title, "ship it today") {
		t.Errorf("event %+v", b.Events)
	}
	// The same record's event again, with no new op: nothing more.
	svc.matcher.observe(structure.Event{Type: api.EventType(rounds.EventLogbookUpdated), ID: id, Data: api.LogbookUpdated{ID: id, Type: "task"}})
	p.settle()
	if n := svc.sched.Pending(done) + svc.sched.Pending(created); n != 2 {
		t.Errorf("a repeated event added events: %d", n)
	}
}

// fakeRoundChannels records round sends and answers records it is told.
type fakeRoundChannels struct {
	mu   sync.Mutex
	sent []channels.RoundSend
	recs map[string]api.ChannelRecord
}

func (f *fakeRoundChannels) SendRound(_ context.Context, p channels.RoundSend) (api.ChannelSent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
	if f.recs == nil {
		f.recs = map[string]api.ChannelRecord{}
	}
	f.recs[p.ID] = api.ChannelRecord{ID: p.ID, From: "round:" + p.Name, State: api.ChannelStateHanded}
	return api.ChannelSent{ID: p.ID, State: api.ChannelStateHanded}, nil
}

func (f *fakeRoundChannels) Record(id string) (api.ChannelRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recs[id]
	return r, ok
}

func (f *fakeRoundChannels) sends() []channels.RoundSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]channels.RoundSend(nil), f.sent...)
}

// waitRun waits until the round's newest run is in state and returns it.
func waitMsgRun(t *testing.T, svc *roundsService, roundID string, state rounds.RunState, ok func(api.RoundRun) bool) api.RoundRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rs, err := svc.Runs(api.RoundRunsParams{RoundRef: api.RoundRef{ID: roundID}, Limit: 1})
		if err == nil && len(rs.Runs) == 1 && rs.Runs[0].State == api.RoundRunState(state) && (ok == nil || ok(rs.Runs[0])) {
			return rs.Runs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("round %s: runs %+v %v, want the newest %s", roundID, rs.Runs, err, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRoundMessageRuns(t *testing.T) {
	var mu sync.Mutex
	group := api.CatalogGroup{ID: "g1", State: "running", Coordinator: &api.CatalogMember{Device: "SHA256:old", Name: "lead"}}
	withCapcom := true
	catalog := func() api.CatalogList {
		mu.Lock()
		defer mu.Unlock()
		self := api.CatalogEntry{Catalog: api.Catalog{DeviceID: testSelf, Groups: []api.CatalogGroup{group},
			Capabilities: []api.CatalogCapability{{Name: capRoundLock}}}}
		if withCapcom {
			self.Catalog.Capabilities = append(self.Catalog.Capabilities, api.CatalogCapability{Name: capCapcomService})
			self.Catalog.Capcom = &api.CatalogCapcom{Home: testSelf, Assignment: 1, Service: "capcom-actor"}
		}
		return api.CatalogList{Hosts: []api.CatalogEntry{self, {Label: "old", Catalog: api.Catalog{DeviceID: "SHA256:old"}}}}
	}
	ch := &fakeRoundChannels{}
	var notices []push.RoundEvent
	svc, _ := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) {
		c.self, c.catalog, c.channels = testSelf, catalog, ch
		c.settings.OutcomeWait = 300 * time.Millisecond
		c.raise = func(ev push.RoundEvent, _, _, _ string) { mu.Lock(); notices = append(notices, ev); mu.Unlock() }
	})
	create := func(spec api.RoundSpec) (api.RoundCreated, error) {
		return svc.Create(context.Background(), api.RoundCreateParams{Round: spec}, "")
	}
	manual := api.RoundTrigger{Kind: api.RoundTriggerKindManual}

	t.Run("capcom: accepted, then the outcome once", func(t *testing.T) {
		made, err := create(api.RoundSpec{Name: "cc", Mode: api.RoundModeCapcom, Trigger: manual, Prompt: "check the builds"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := svc.Run(api.RoundRef{ID: made.Round.ID})
		if err != nil {
			t.Fatal(err)
		}
		got := waitMsgRun(t, svc, made.Round.ID, rounds.RunRunning, func(r api.RoundRun) bool { return r.Message != "" })
		s := ch.sends()
		if len(s) != 1 || s[0].Session != channels.CapcomName || s[0].Text != "check the builds" || s[0].Round.Run != run.ID ||
			s[0].ID != roundMessageID(run.ID) || s[0].Round.Deadline != "" {
			t.Fatalf("sent %+v", s)
		}
		if got.Destination != "Capcom on this host" {
			t.Errorf("destination %q", got.Destination)
		}
		svc.channelRecord(api.ChannelRecord{ID: got.Message, From: "round:cc", State: api.ChannelStateAccepted})
		waitMsgRun(t, svc, made.Round.ID, rounds.RunSucceeded, func(r api.RoundRun) bool { return r.Reason == reasonAccepted })
		svc.outcome(got.Message, "told", "2026-11-06T10:00:00Z")
		waitMsgRun(t, svc, made.Round.ID, rounds.RunSucceeded, func(r api.RoundRun) bool { return r.Outcome != nil && r.Outcome.Word == "told" })
		svc.outcome(got.Message, "acted", "2026-11-06T10:01:00Z") // a second one is dropped
		time.Sleep(50 * time.Millisecond)
		if r := waitMsgRun(t, svc, made.Round.ID, rounds.RunSucceeded, nil); r.Outcome.Word != "told" {
			t.Errorf("outcome %+v", r.Outcome)
		}
	})

	t.Run("capcom: unknown after outcome_wait", func(t *testing.T) {
		made, err := create(api.RoundSpec{Name: "cc2", Mode: api.RoundModeCapcom, Trigger: manual, Prompt: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Run(api.RoundRef{ID: made.Round.ID}); err != nil {
			t.Fatal(err)
		}
		got := waitMsgRun(t, svc, made.Round.ID, rounds.RunRunning, func(r api.RoundRun) bool { return r.Message != "" })
		svc.channelRecord(api.ChannelRecord{ID: got.Message, From: "round:cc2", State: api.ChannelStateAccepted})
		if r := waitMsgRun(t, svc, made.Round.ID, rounds.RunSucceeded, nil); r.Outcome != nil {
			t.Errorf("an outcome before outcome_wait: %+v", r.Outcome)
		}
		waitMsgRun(t, svc, made.Round.ID, rounds.RunSucceeded, func(r api.RoundRun) bool { return r.Outcome != nil && r.Outcome.Word == "unknown" })
	})

	t.Run("capcom: refused at create with no service", func(t *testing.T) {
		mu.Lock()
		withCapcom = false
		mu.Unlock()
		defer func() { mu.Lock(); withCapcom = true; mu.Unlock() }()
		if _, err := create(api.RoundSpec{Name: "cc3", Mode: api.RoundModeCapcom, Trigger: manual, Prompt: "x"}); err == nil ||
			!strings.Contains(err.Error(), "no Capcom service") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("group: a too-old coordinator's hab, then the group ends", func(t *testing.T) {
		made, err := create(api.RoundSpec{Name: "gr", Mode: api.RoundModeGroup, Group: "g1", Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindEvent,
			Event: rounds.EventHabWoke}, Prompt: "sum up"})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.sched.AddEvents(made.Round.ID, []rounds.Event{{At: time.Now(), Kind: rounds.EventHabWoke, Title: "woke"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Run(api.RoundRef{ID: made.Round.ID}); err != nil {
			t.Fatal(err)
		}
		waitMsgRun(t, svc, made.Round.ID, rounds.RunSkipped, func(r api.RoundRun) bool { return r.Reason == reasonTooOld })
		waitPending(t, svc, made.Round.ID, 1) // the batch goes back
		before := len(ch.sends())
		mu.Lock()
		group.State = "ended"
		mu.Unlock()
		if _, err := svc.Run(api.RoundRef{ID: made.Round.ID}); err != nil {
			t.Fatal(err)
		}
		waitMsgRun(t, svc, made.Round.ID, rounds.RunSkipped, func(r api.RoundRun) bool { return r.Reason == "group ended" })
		r, err := svc.Get(api.RoundRef{ID: made.Round.ID})
		if err != nil || !r.Completed {
			t.Fatalf("round %+v %v: not completed", r, err)
		}
		if len(ch.sends()) != before {
			t.Error("a message went to an ended group")
		}
		mu.Lock()
		defer mu.Unlock()
		gone := 0
		for _, n := range notices {
			if n.Kind == push.RoundTargetGone && n.Round == made.Round.ID {
				gone++
				if n.Reason != reasonGroupEnded {
					t.Errorf("notice reason %q", n.Reason)
				}
			}
		}
		if gone != 1 {
			t.Errorf("%d target_gone notices, want 1: %+v", gone, notices)
		}
	})
}

// TestRoundBatchCarried: a create with a batch (a move's) starts with it pending, an event
// filter's project goes by remote both ways, and round.get with batch reads it back; a batch
// for a round with no event trigger, or with a bad event, is refused.
func TestRoundBatchCarried(t *testing.T) {
	svc, st := fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), nil)
	proj, err := st.CreateProject("app", "example.com/team/app", "")
	if err != nil {
		t.Fatal(err)
	}
	b := &api.RoundBatch{Events: []api.RoundEvent{{At: "2026-11-06T10:00:00Z", Kind: rounds.EventAgentState, Title: "a done", Data: api.RawJSON(`{"x":1}`)}}, Dropped: 2}
	spec := api.RoundSpec{Name: "moved", Mode: api.RoundModeFresh, Command: []string{"true"}, Project: proj.ID, Paused: true,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindEvent, Event: rounds.EventAgentState, Filter: &api.RoundEventFilter{ProjectRemote: "example.com/team/app"}}}
	made, err := svc.Create(context.Background(), api.RoundCreateParams{Round: spec, Batch: b}, "")
	if err != nil {
		t.Fatal(err)
	}
	if f := made.Round.Trigger.Filter; f == nil || f.Project != proj.ID || f.ProjectRemote != "example.com/team/app" {
		t.Errorf("filter %+v", f)
	}
	if made.Round.Pending != 3 {
		t.Errorf("pending %d, want 3", made.Round.Pending)
	}
	got, err := svc.Batch(api.RoundRef{ID: made.Round.ID})
	if err != nil || got == nil || len(got.Events) != 1 || got.Dropped != 2 || string(got.Events[0].Data) != `{"x":1}` {
		t.Fatalf("batch %+v %v", got, err)
	}
	for _, c := range []struct {
		name string
		spec api.RoundSpec
		b    api.RoundBatch
	}{
		{"a cron round", api.RoundSpec{Name: "c1", Mode: api.RoundModeFresh, Command: []string{"true"}, Project: proj.ID,
			Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}}, *b},
		{"an unknown event", func() api.RoundSpec { s := spec; s.Name = "c2"; return s }(), api.RoundBatch{Events: []api.RoundEvent{{At: "2026-11-06T10:00:00Z", Kind: "pane.updated"}}}},
		{"a bad time", func() api.RoundSpec { s := spec; s.Name = "c3"; return s }(), api.RoundBatch{Events: []api.RoundEvent{{At: "now", Kind: rounds.EventHabWoke}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Create(context.Background(), api.RoundCreateParams{Round: c.spec, Batch: &c.b}, ""); err == nil {
				t.Fatal("created")
			}
			if _, err := svc.Get(api.RoundRef{Name: c.spec.Name}); err == nil {
				t.Error("the round exists after a refused create")
			}
		})
	}
	// The pane route refuses a batch.
	h := roundHook(st, svc)
	params, _ := json.Marshal(api.RoundCreateParams{Round: func() api.RoundSpec { s := spec; s.Name = "c4"; return s }(), Batch: b})
	if _, _, err := h(context.Background(), "pane-z", "round.create", params); err == nil || !strings.Contains(err.Error(), "only a move") {
		t.Errorf("pane create with a batch: %v", err)
	}
}

// TestRoundsExampleSettings: the [rounds] keys of docs/examples/hab.toml load with no complaint.
func TestRoundsExampleSettings(t *testing.T) {
	var logged []string
	s, err := loadRoundsSettings(filepath.Join("..", "..", "docs", "examples", "hab.toml"), func(f string, a ...any) { logged = append(logged, f) })
	if err != nil || len(logged) > 0 {
		t.Fatalf("%v %v", err, logged)
	}
	if s.OutcomeWait != 7*24*time.Hour || s.Limits.BatchMaxEvents != 200 || s.BlockMaxBytes != 32<<10 || s.Limits.StateSaveEvery != 10*time.Second {
		t.Errorf("settings %+v", s)
	}
}
