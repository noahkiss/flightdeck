package capcomengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

// The Codex engine (A6): its default login is the user's own codex login, which gff-hab hands
// on by name and never reads; an API key comes from api_key_command, else OPENAI_API_KEY; the
// job's environment names the engine kind and CODEX_HOME, and never a key.
func TestSupervisorCodexCredential(t *testing.T) {
	s := newSup(t, func(s *supT) {
		s.cfg.Engine, s.cfg.Auth = capcom.EngineCodex, capcom.AuthLogin
		s.env["CODEX_HOME"] = "/codex-home"
		s.env["OPENAI_API_KEY"] = "planted-openai"
	})
	s.sup.Ensure(context.Background())
	s.started()
	if c, ok := s.sup.Credential(); !ok || c.Kind != capcom.AuthLogin || c.Value != "" {
		t.Fatalf("credential %v", c)
	}
	env, _ := os.ReadFile(filepath.Join(s.dir, EnvFile))
	if !strings.Contains(string(env), KindVar+"=codex") || !strings.Contains(string(env), "CODEX_HOME=/codex-home") {
		t.Fatalf("env file %q", env)
	}
	s.noFileHolds("planted-openai")

	s2 := newSup(t, func(s *supT) {
		s.cfg.Engine, s.cfg.Auth = capcom.EngineCodex, capcom.AuthAPIKey
		s.env["OPENAI_API_KEY"] = "planted-openai"
		s.env["ANTHROPIC_API_KEY"] = "planted-anthropic"
	})
	s2.sup.Ensure(context.Background())
	s2.started()
	if c, _ := s2.sup.Credential(); c.Kind != capcom.AuthAPIKey || c.Value != "planted-openai" {
		t.Fatalf("credential %v", c)
	}
	for _, e := range s2.job.starts[0].Env {
		if strings.Contains(e, "planted") {
			t.Fatalf("the job's environment holds a key: %s", e)
		}
	}
	s3 := newSup(t, func(s *supT) { s.cfg.Engine, s.cfg.Auth = capcom.EngineCodex, capcom.AuthAPIKey })
	s3.sup.Ensure(context.Background())
	if st := s3.waitState(StateError); st.Reason != "no API key: set [capcom] api_key_command" {
		t.Fatal(st.Reason)
	}
}

// codexCaps are the Codex engine's capabilities (4.1): no mid-turn input, no approval callback,
// no compaction or usage events.
var codexCaps = []string{CapImages, CapInterrupt, CapResumeByID}

func codexReady() Message {
	return Message{Type: TReady, Engine: "codex", Version: "test", Capabilities: codexCaps}
}

// quiet fails when the hab sends anything but an ack within d.
func (e *fakeEngine) quiet(d time.Duration, what string) {
	e.t.Helper()
	select {
	case m := <-e.in:
		e.t.Fatalf("%s: got %+v", what, m)
	case <-time.After(d):
	}
}

func dividers(t *testing.T, th *capcomthread.Store) []string {
	t.Helper()
	page, _ := th.Page("", 0)
	var out []string
	for _, it := range page {
		if it.Kind == capcomthread.KindDivider {
			out = append(out, it.Text)
		}
	}
	return out
}

// With no mid_turn_input the hab holds a message that comes during a turn until the turn ends,
// then sends what waits in thread order (4.1).
func TestLinkHoldsInputWithoutMidTurn(t *testing.T) {
	h := newHarness(t)
	e, _ := h.connect()
	e.send(codexReady())
	a := h.user("a")
	if m := e.next(); inputID(m) != a.ID {
		t.Fatal("a not sent")
	}
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user", Items: []string{a.ID}})
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + a.ID + `"`)})
	e.acked(e.seq)
	b, c := h.user("b"), h.user("c")
	e.quiet(150*time.Millisecond, "an input during the turn")
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess})
	if m := e.next(); inputID(m) != b.ID {
		t.Fatalf("after the turn got %s, want %s", inputID(m), b.ID)
	}
	if m := e.next(); inputID(m) != c.ID {
		t.Fatalf("got %s, want %s", inputID(m), c.ID)
	}
}

// With no images capability an image goes in as its path with a note (4.1); with it, as an
// image ref.
func TestLinkImagesWithoutCapability(t *testing.T) {
	for _, caps := range [][]string{{CapInterrupt}, codexCaps} {
		h := newHarness(t)
		e, _ := h.connect()
		e.send(Message{Type: TReady, Engine: "x", Capabilities: caps})
		it, _, err := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindMessage, Author: capcomthread.Author{Kind: capcomthread.AuthorUser, Device: "d1"},
			Text: "what is this", Images: []string{"/state/capcom/attachments/flyer.png"}})
		if err != nil {
			t.Fatal(err)
		}
		m := e.next()
		var in InputItem
		json.Unmarshal(m.Item, &in)
		if in.ID != it.ID {
			t.Fatalf("input %s", in.ID)
		}
		if len(caps) == 1 {
			if len(in.Images) != 0 || !strings.Contains(in.Text, "[image /state/capcom/attachments/flyer.png: attached") || !strings.HasPrefix(in.Text, "what is this") {
				t.Fatalf("no images capability: %+v", in)
			}
		} else if len(in.Images) != 1 || in.Text != "what is this" {
			t.Fatalf("images capability: %+v", in)
		}
	}
}

// An engine that cannot resume by id gets a re-seed once it is ready, when hello named a
// session (4.1); one that can resume gets none.
func TestLinkNoResumeByIDReseeds(t *testing.T) {
	for _, resume := range []bool{false, true} {
		h := newHarness(t)
		h.rec.Update(func(r *Record) { r.Sessions = []SDKSession{{ID: "s1", Why: "first", Engine: "x"}} })
		e, hello := h.connect()
		if hello.SDKSession != "s1" {
			t.Fatalf("hello %+v", hello)
		}
		caps := []string{CapInterrupt}
		if resume {
			caps = append(caps, CapResumeByID)
		}
		e.send(Message{Type: TReady, Engine: "x", Capabilities: caps})
		if resume {
			e.quiet(150*time.Millisecond, "a re-seed for an engine that resumes")
			continue
		}
		if m := e.next(); m.Type != TReseed || m.Reason != ReseedRestartFailed {
			t.Fatalf("got %+v", m)
		}
	}
}

// A switch of engines is a re-seed: hello names no session to resume and asks for the switch;
// the fresh session is recorded as the new engine's, with one divider, and the thread stays
// (18 A6).
func TestLinkEngineSwitch(t *testing.T) {
	kind := "codex"
	h := newHarnessWith(t, func(c *LinkConfig) { c.EngineKind = func() string { return kind } })
	h.user("before the switch")
	// A session from before A6 has no engine: it was the Claude engine's.
	h.rec.Update(func(r *Record) { r.Sessions = []SDKSession{{ID: "claude-1", Why: "first"}} })
	e, hello := h.connect()
	if hello.SDKSession != "" || hello.Reseed != ReseedSwitch {
		t.Fatalf("hello %+v", hello)
	}
	e.send(codexReady())
	e.send(Message{Type: TSessionStarted, Session: "", Why: "reseed"})
	e.acked(e.seq)
	if got := dividers(t, h.thread); len(got) != 1 || got[0] != "Earlier messages summarised" {
		t.Fatalf("dividers %q", got)
	}
	if r := h.rec.Get(); r.Current() != "" || r.Sessions[0].End.IsZero() || r.Opening != "reseed" {
		t.Fatalf("record after the announcement %+v", r)
	}
	e.send(Message{Type: TSessionStarted, Session: "codex-1", Why: "reseed"})
	e.acked(e.seq)
	r := h.rec.Get()
	if r.Current() != "codex-1" || r.Sessions[1].EngineOf() != "codex" || r.Opening != "" {
		t.Fatalf("record %+v", r)
	}
	if got := dividers(t, h.thread); len(got) != 1 {
		t.Fatalf("the id added a second divider: %q", got)
	}
	// The same engine again resumes; switching back is a switch again.
	e.c.Close()
	waitFor(t, "offline", func() bool { return !h.link.Status().Connected })
	_, hello = h.connect()
	if hello.SDKSession != "codex-1" || hello.Reseed != "" {
		t.Fatalf("hello for the same engine %+v", hello)
	}
	kind = "claude"
	_, hello = h.connect()
	if hello.SDKSession != "" || hello.Reseed != ReseedSwitch {
		t.Fatalf("hello for a switch back %+v", hello)
	}
	kept := false
	page, _ := h.thread.Page("", 0)
	for _, it := range page {
		kept = kept || it.Text == "before the switch"
	}
	if !kept {
		t.Fatalf("the thread lost its message: %+v", page)
	}
}

// A fresh session announced with no id whose engine stops before the id comes is re-seeded
// again by the next engine.
func TestLinkOpeningWithoutIDSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.rec.Update(func(r *Record) { r.Sessions = []SDKSession{{ID: "c1", Why: "first", Engine: "codex"}} })
	e, _ := h.connect()
	e.send(codexReady())
	e.send(Message{Type: TSessionStarted, Session: "", Why: "new"})
	e.acked(e.seq)
	e.c.Close()
	waitFor(t, "offline", func() bool { return !h.link.Status().Connected })
	e2, hello := h.connect()
	if hello.SDKSession != "" || hello.Reseed != ReseedNew {
		t.Fatalf("hello %+v", hello)
	}
	e2.send(codexReady())
	e2.send(Message{Type: TSessionStarted, Session: "", Why: "new"})
	e2.send(Message{Type: TSessionStarted, Session: "c2", Why: "new"})
	e2.acked(e2.seq)
	if got := dividers(t, h.thread); len(got) != 1 || got[0] != "New conversation" {
		t.Fatalf("dividers %q", got)
	}
	if r := h.rec.Get(); r.Current() != "c2" || len(r.Sessions) != 2 {
		t.Fatalf("record %+v", r)
	}
}

// With no compaction events the size trigger is reseed_early_at, below reseed_at (4.1).
func TestContextReseedEarlyWithoutCompaction(t *testing.T) {
	for _, tc := range []struct {
		caps []string
		want bool
	}{{claudeCaps, false}, {codexCaps, true}} {
		h := newHarness(t)
		e, _ := h.connect()
		e.send(Message{Type: TReady, Engine: "x", Capabilities: tc.caps})
		e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user"})
		e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess, ContextUsed: 0.5})
		if !tc.want {
			e.quiet(150*time.Millisecond, "a re-seed at 50% with compaction events")
			continue
		}
		if m := e.next(); m.Type != TReseed || m.Reason != ReseedLimit {
			t.Fatalf("got %+v", m)
		}
	}
}
