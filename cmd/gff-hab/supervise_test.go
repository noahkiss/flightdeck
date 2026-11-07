package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// supFake is the hab around a supervisor: one member session, its agent, its readings, and
// what went in.
type supFake struct {
	mu       sync.Mutex
	ss       api.Session
	st       api.AgentState
	changed  chan struct{}
	pct      *int
	path     string
	inUse    string
	sent     []string
	follows  []string
	compacts int
	fresh    []string // seeds
	notes    []string // to the asker
	note     bool     // the member answers a hand-off ask with group_note
	sup      *superviseService
	settings map[string]string
}

func pctOf(n int) *int { return &n }

func newSupFake(t *testing.T, harnessKind string) *supFake {
	f := &supFake{changed: make(chan struct{}), settings: map[string]string{},
		ss: api.Session{ID: "s1", Name: "lead-a", Pane: "p1", State: api.SessionStateRunning,
			Group: &api.SessionGroup{ID: "g1", Role: groups.RoleLead, Item: "a"}},
		st: api.AgentState{Pane: "p1", Revision: 5, Harness: harnessKind, Session: "nat-1", State: api.AgentStatusIdle}}
	keys := groups.DefaultKeys
	keys.SiblingWait, keys.HandoffNoteWait = 300*time.Millisecond, 400*time.Millisecond
	cfg := superviseConfig{host: "dev",
		members: func() []api.Session {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.ss.Group == nil {
				return nil
			}
			return []api.Session{f.ss}
		},
		session: func(id string) (api.Session, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.ss, id == f.ss.ID
		},
		agents: f,
		reading: func(string, api.AgentState) (*int, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.pct, f.path
		},
		setPct: func(id string, p *int) bool { f.mu.Lock(); f.ss.ContextPct = p; f.mu.Unlock(); return true },
		addCompact: func(id string) (int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.ss.Compacts++
			return f.ss.Compacts, nil
		},
		inUse: func(api.Session) string { f.mu.Lock(); defer f.mu.Unlock(); return f.inUse },
		guard: func(string, string) error { return nil },
		send: func(_ context.Context, session, text string, _ func(string) error) error {
			f.mu.Lock()
			f.sent = append(f.sent, text)
			answer := f.note && strings.Contains(text, "hand-off")
			f.mu.Unlock()
			go f.turn(answer)
			return nil
		},
		record: func(context.Context, string) (api.Group, error) {
			return api.Group{ID: "g1", Title: "Shop", Items: []api.GroupItem{{ID: "a", Title: "Cart", Task: "task-7", Branch: "group/g1/a"}},
				Coordinator: api.GroupCoordinator{Name: "alice", Address: "dev/alice"}}, nil
		},
		fresh: func(_ context.Context, ss api.Session, _ api.AgentState, _ *api.Group, seed string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.fresh = append(f.fresh, seed)
			f.ss = api.Session{ID: "s2", Name: ss.Name, Pane: "p2", State: api.SessionStateRunning, Group: ss.Group, Replaced: ss.ID}
			f.pct = pctOf(1)
			return "s2", nil
		},
		note:    func(name, text string) error { f.mu.Lock(); f.notes = append(f.notes, name+": "+text); f.mu.Unlock(); return nil },
		setting: func(id string) json.RawMessage { f.mu.Lock(); defer f.mu.Unlock(); return json.RawMessage(f.settings[id]) },
		keys:    keys, tick: 20 * time.Millisecond, logf: t.Logf}
	if harnessKind == "claude" {
		cfg.compacts = f
	}
	f.sup = newSupervisor(cfg)
	t.Cleanup(f.sup.Stop)
	return f
}

func (f *supFake) Get(pane string) (api.AgentState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	st.Pane = pane
	return st, true
}

func (f *supFake) Changed(string) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *supFake) setState(s api.AgentStatus) {
	f.mu.Lock()
	f.st.State = s
	f.st.Revision++
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// turn plays the member's turn after a message: working, a note when asked, then idle.
func (f *supFake) turn(note bool) {
	time.Sleep(10 * time.Millisecond)
	f.setState(api.AgentStatusWorking)
	time.Sleep(10 * time.Millisecond)
	if note {
		f.mu.Lock()
		id := f.ss.ID
		f.mu.Unlock()
		f.sup.Noted(id)
	}
	f.setState(api.AgentStatusIdle)
}

// Start is the compaction service: it ends done at once, and the reading drops.
func (f *supFake) Start(pane, followUp string, guard func(string) error) (api.Compaction, <-chan api.Compaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.follows = append(f.follows, followUp)
	f.compacts++
	f.pct = pctOf(5)
	done := make(chan api.Compaction, 1)
	done <- api.Compaction{State: compactDone, Pane: pane}
	return api.Compaction{State: compactQueued}, done, nil
}

func (f *supFake) snapshot() (sent, follows, fresh, notes []string, compacts, counted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...), append([]string(nil), f.follows...), append([]string(nil), f.fresh...),
		append([]string(nil), f.notes...), f.compacts, f.ss.Compacts
}

func waitSup(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSuperviseCompactOnce (groups.md 9.2): a Claude Code member at 41% gets one compact: the
// progress ask with its percentage, then a compaction whose follow-up names the item and its
// task, counted on the member. The reading drops after it, so no second one follows.
func TestSuperviseCompactOnce(t *testing.T) {
	f := newSupFake(t, "claude")
	f.pct = pctOf(39)
	f.sup.Start()
	time.Sleep(100 * time.Millisecond)
	if sent, _, _, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("below group.compact_at: %q", sent)
	}
	f.mu.Lock()
	f.pct = pctOf(41)
	f.mu.Unlock()
	waitSup(t, "the compaction counted", func() bool { _, _, _, _, _, n := f.snapshot(); return n == 1 })
	time.Sleep(150 * time.Millisecond)
	sent, follows, fresh, _, compacts, counted := f.snapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "41%") || !strings.Contains(sent[0], "group_note (group g1, item a)") ||
		!strings.HasPrefix(sent[0], "[Flightdeck: a message from Flightdeck itself") {
		t.Fatalf("sent %q", sent)
	}
	if compacts != 1 || counted != 1 || len(fresh) != 0 || len(follows) != 1 || !strings.Contains(follows[0], "item a (Cart)") || !strings.Contains(follows[0], "task task-7") {
		t.Fatalf("compacts %d counted %d fresh %d follows %q", compacts, counted, len(fresh), follows)
	}
}

// TestSuperviseFreshAfterCompacts (groups.md 9.3): the trigger after group.fresh_after_compacts
// compacts asks for a hand-off note, waits for it and the turn's end, then starts a fresh
// session whose seed names the item's task.
func TestSuperviseFreshAfterCompacts(t *testing.T) {
	f := newSupFake(t, "claude")
	f.settings[settings.GroupFreshAfterCompacts] = "4"
	f.ss.Compacts, f.note = 4, true
	f.pct = pctOf(41)
	f.sup.Start()
	waitSup(t, "the fresh session", func() bool { _, _, fresh, _, _, _ := f.snapshot(); return len(fresh) == 1 })
	sent, follows, fresh, _, _, _ := f.snapshot()
	if len(follows) != 0 || len(sent) != 1 || !strings.Contains(sent[0], "hand-off") {
		t.Fatalf("follows %q sent %q", follows, sent)
	}
	if !strings.Contains(fresh[0], "Read the notes on task task-7 and continue") || !strings.Contains(fresh[0], "item a (Cart)") {
		t.Fatalf("seed %q", fresh[0])
	}
	var done api.MemberAction
	waitSup(t, "the action done", func() bool {
		f.sup.mu.Lock()
		defer f.sup.mu.Unlock()
		for _, a := range f.sup.byID {
			done = a.a
		}
		return done.State == actDone
	})
	if done.Fresh != "s2" || done.Reason != "" || done.Kind != actHandoff {
		t.Fatalf("action %+v", done)
	}
}

// TestSuperviseHandoffNoNote: with no note within [groups] handoff_note_wait, the fresh session
// starts anyway and the action says so.
func TestSuperviseHandoffNoNote(t *testing.T) {
	f := newSupFake(t, "claude")
	f.ss.Compacts = 4
	f.pct = pctOf(45)
	f.sup.Start()
	waitSup(t, "the fresh session", func() bool { _, _, fresh, _, _, _ := f.snapshot(); return len(fresh) == 1 })
	waitSup(t, "the flag", func() bool {
		f.sup.mu.Lock()
		defer f.sup.mu.Unlock()
		for _, a := range f.sup.byID {
			return a.a.State == actDone && strings.Contains(a.a.Reason, "no hand-off note")
		}
		return false
	})
}

// TestSuperviseCodexFresh (groups.md 9.1, 9.2): a Codex member, which this hab cannot compact,
// goes to a fresh session at group.handoff_at, read from its rollout's token_count.
func TestSuperviseCodexFresh(t *testing.T) {
	f := newSupFake(t, "codex")
	dir := t.TempDir()
	roll := filepath.Join(dir, "rollout.jsonl")
	write := func(total int) {
		line := `{"timestamp":"2026-01-01T10:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":9,"output_tokens":1},"last_token_usage":{"input_tokens":1,"output_tokens":1,"total_tokens":` +
			strconvItoa(total) + `},"model_context_window":200000},"rate_limits":null}}` + "\n"
		os.WriteFile(roll, []byte(`{"type":"session_meta","payload":{"id":"nat-1"}}`+"\n"+line), 0o600)
	}
	write(118000) // 59%
	read := memberReading(newNudgeService(nil), func(string, string) (string, error) { return roll, nil })
	f.sup.cfg.reading = read
	f.note = true
	f.sup.Start()
	time.Sleep(100 * time.Millisecond)
	if _, _, fresh, _, _, _ := f.snapshot(); len(fresh) != 0 {
		t.Fatal("a fresh session at 59%")
	}
	if p := func() *int { f.mu.Lock(); defer f.mu.Unlock(); return f.ss.ContextPct }(); p == nil || *p != 59 {
		t.Fatalf("context_pct %v", p)
	}
	write(120000) // 60%
	waitSup(t, "the fresh session", func() bool { _, _, fresh, _, _, _ := f.snapshot(); return len(fresh) == 1 })
	if _, follows, _, _, compacts, _ := f.snapshot(); compacts != 0 || len(follows) != 0 {
		t.Fatalf("a Codex member was compacted: %d", compacts)
	}
}

// TestSiblingWaitsThenSkips (groups.md 9.4): a sibling's member_compact against a member in use
// waits up to [groups] sibling_wait, then ends skipped with the reason, and the asker hears it.
// A second request while one runs gets conflict.
func TestSiblingWaitsThenSkips(t *testing.T) {
	f := newSupFake(t, "claude")
	f.inUse = "working"
	f.pct = pctOf(10)
	a, err := f.sup.Request(actCompact, f.ss, "lead-b", "lead-b")
	if err != nil || a.State != actQueued {
		t.Fatalf("request: %+v %v", a, err)
	}
	if _, err := f.sup.Request(actHandoff, f.ss, "lead-b", ""); kindOf(err) != api.ErrorKindConflict {
		t.Fatalf("second request: %v", err)
	}
	waitSup(t, "skipped", func() bool { got, _ := f.sup.Get(a.ID); return got.State == actSkipped })
	waitSup(t, "the asker told", func() bool { _, _, _, notes, _, _ := f.snapshot(); return len(notes) > 0 })
	got, _ := f.sup.Get(a.ID)
	if got.Reason != "working" {
		t.Fatalf("reason %q", got.Reason)
	}
	sent, _, _, notes, _, _ := f.snapshot()
	if len(sent) != 0 || len(notes) != 1 || !strings.HasPrefix(notes[0], "lead-b: member_compact for lead-a") || !strings.Contains(notes[0], "skipped: working") {
		t.Fatalf("sent %q notes %q", sent, notes)
	}
	// Free again: a new request runs.
	f.mu.Lock()
	f.inUse = ""
	f.mu.Unlock()
	b, err := f.sup.Request(actCompact, f.ss, "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitSup(t, "done", func() bool { got, _ := f.sup.Get(b.ID); return got.State == actDone })
}

// TestSuperviseCountsEveryCompact (groups.md 9.1): a member's own compaction counts; ours is
// counted once, at its end.
func TestSuperviseCountsEveryCompact(t *testing.T) {
	f := newSupFake(t, "claude")
	bySession := func(string) (api.Session, bool) { f.mu.Lock(); defer f.mu.Unlock(); return f.ss, true }
	hook := harness.Envelope{Harness: "claude", Event: "SessionStart", Payload: json.RawMessage(`{"source":"compact"}`)}
	f.sup.Observe("p1", hook, bySession)
	f.sup.Observe("p1", harness.Envelope{Harness: "claude", Event: "SessionStart", Payload: json.RawMessage(`{"source":"startup"}`)}, bySession)
	f.sup.Observe("p1", harness.Envelope{Harness: "claude", Event: "SessionStart", Payload: json.RawMessage(`{"source":"compact","agent_id":"x"}`)}, bySession)
	if _, _, _, _, _, n := f.snapshot(); n != 1 {
		t.Fatalf("counted %d, want the member's own one", n)
	}
	f.sup.mu.Lock()
	f.sup.ours["s1"] = time.Now()
	f.sup.mu.Unlock()
	f.sup.Observe("p1", hook, bySession)
	if _, _, _, _, _, n := f.snapshot(); n != 1 {
		t.Fatalf("ours counted twice: %d", n)
	}
}

// TestNudgeOffForMembers (groups.md 9.1): the context nudge says nothing to a member.
func TestNudgeOffForMembers(t *testing.T) {
	n := newNudgeService(func(id string) json.RawMessage {
		if id == settings.ContextNudge {
			return json.RawMessage(`10`)
		}
		return nil
	})
	status := harness.Envelope{Harness: "claude", Event: "Status", Payload: json.RawMessage(`{"session_id":"x","context_window":{"used_percentage":50}}`)}
	status.Event = "StatusLine"
	n.Observe("p1", status)
	n.Observe("p2", status)
	n.member = func(pane string) bool { return pane == "p1" }
	post := harness.Envelope{Harness: "claude", Event: "PostToolUse", Payload: json.RawMessage(`{"session_id":"x"}`)}
	if got := n.Context(context.Background(), "p1", post); got != "" {
		t.Fatalf("a member got the nudge: %q", got)
	}
	if pct, _, ok := n.Reading("p2"); !ok || pct != 50 {
		t.Fatalf("reading %v %v", pct, ok)
	}
}

// TestCodexContext reads the rollout's last token_count over the window, and the window from a
// task_started when the token_count has none.
func TestCodexContext(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.jsonl")
	os.WriteFile(p, []byte(strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":null}}`,
		`{"type":"event_msg","payload":{"type":"task_started","model_context_window":100000}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":20000,"output_tokens":5000}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":30000}}`, // cut: no newline
	}, "\n")), 0o600)
	if pct, ok := codexContext(p); !ok || pct != 25 {
		t.Fatalf("pct %v %v", pct, ok)
	}
	os.WriteFile(p, []byte(`{"type":"event_msg","payload":{"type":"token_count","info":null}}`+"\n"), 0o600)
	if _, ok := codexContext(p); ok {
		t.Fatal("a reading with no usage")
	}
}

func strconvItoa(n int) string { b, _ := json.Marshal(n); return string(b) }
