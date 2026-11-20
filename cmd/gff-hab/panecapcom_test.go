package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// The Capcom service's pane access (hab-logic-api.md 3.11, 3.12; capcom-service.md 5.4, stage
// A10): it reads every pane at every agent.read_panes value but off; it types under
// agent.drive_panes with every project as its scope, with a raw line only under all; it starts a
// pane only under all; its forwards carry the mark capcom, which a far hab takes only from the
// peer that holds the Capcom mark.

// recordPeer is a peer link that records each call and answers with an empty result.
type recordPeer struct {
	mu    sync.Mutex
	calls []string // method and the params' as
}

func (p *recordPeer) Call(_ context.Context, method string, params, _ any) error {
	b, _ := json.Marshal(params)
	var m struct {
		As string `json:"as"`
	}
	json.Unmarshal(b, &m)
	p.mu.Lock()
	p.calls = append(p.calls, method+" as="+m.As)
	p.mu.Unlock()
	return nil
}

func (p *recordPeer) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

// TestPaneCapcomRead: the Capcom route reads every pane under project and all, unknown targets
// keep their kinds, a hand-set off refuses it with a hint that names the setting, and a
// label/name forward carries as=capcom at every value but off.
func TestPaneCapcomRead(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	rec := &recordPeer{}
	r.peer.Caps = append(r.peer.Caps, paneReadCapability)
	r.peer.Caller = rec
	readRule := readPanesProject
	r.s.rule = func() string { return readRule }
	for _, rule := range []string{readPanesProject, readPanesAll} {
		readRule = rule
		for _, s := range []string{"other", "loose", "shell", "mate"} {
			if _, _, _, err := r.s.prepare("pane.read", fromCap, "", s); err != nil {
				t.Errorf("%s: Capcom reads %s: %v", rule, s, err)
			}
		}
		if _, _, _, err := r.s.prepare("pane.read", fromCap, "", "ghost"); paneErrKind(err) != api.ErrorKindNotFound {
			t.Errorf("%s: an unknown session: %v, want not_found", rule, err)
		}
		if _, err := r.s.Read(context.Background(), fromCap, api.PaneReadParams{Session: "bee/far"}); err != nil {
			t.Errorf("%s: forward: %v", rule, err)
		}
		if _, err := r.s.Wait(context.Background(), fromCap, api.PaneWaitParams{Session: "bee/far", QuietMS: 1, TimeoutMS: 50}); err != nil {
			t.Errorf("%s: forwarded wait: %v", rule, err)
		}
		if _, err := r.s.List(context.Background(), fromCap, api.PaneListParams{Host: "bee"}); err != nil {
			t.Errorf("%s: list a peer: %v", rule, err)
		}
		for _, c := range rec.take() {
			if !strings.HasSuffix(c, " as=capcom") {
				t.Errorf("%s: forward %q carries no capcom mark", rule, c)
			}
		}
	}
	// An agent's forward carries no mark, even under all.
	readRule = readPanesAll
	if _, err := r.s.Read(context.Background(), fromMine, api.PaneReadParams{Session: "bee/far", As: asCapcom}); err != nil {
		t.Fatal(err)
	}
	if c := rec.take(); len(c) != 1 || c[0] != "pane.read as=" {
		t.Fatalf("an agent's forward: %q", c)
	}
	// off, set by hand: nothing here, no forward, no list.
	readRule = readPanesOff
	for _, s := range []string{"other", "ghost", "bee/far"} {
		_, err := r.s.Read(context.Background(), fromCap, api.PaneReadParams{Session: s})
		if paneErrKind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "agent.read_panes is off") || strings.Contains(err.Error(), s) {
			t.Errorf("off: Capcom reads %s: %v", s, err)
		}
	}
	if _, err := r.s.List(context.Background(), fromCap, api.PaneListParams{}); paneErrKind(err) != api.ErrorKindDenied {
		t.Errorf("off: list: %v", err)
	}
	if c := rec.take(); len(c) != 0 {
		t.Errorf("off: forwards %q", c)
	}
}

// TestPaneCapcomType: under all Capcom types a raw line and may type into a shell; under project
// into every pane, enveloped; under agents into every agent's pane, enveloped, never a shell;
// under off nothing. Control keys stay refused at all, and a forward carries the mark.
func TestPaneCapcomType(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	rec := &recordPeer{}
	r.peer.Caller = rec
	ctx := context.Background()
	type want struct {
		rule, session         string
		raw, requireAgent, ok bool
	}
	for _, w := range []want{
		{readPanesAll, "other", true, false, true}, {readPanesAll, "shell", true, false, true},
		{readPanesProject, "other", false, false, true}, {readPanesProject, "shell", false, false, true},
		{drivePanesAgents, "other", false, true, true}, {drivePanesAgents, "shell", false, false, false},
		{readPanesOff, "mate", false, false, false},
	} {
		r.setRule(w.rule)
		n := len(r.typed.writes)
		_, err := r.s.Type(ctx, fromCap, api.PaneTypeParams{Session: w.session, Text: "/compact", Enter: true})
		if (err == nil) != w.ok {
			t.Errorf("%s: Capcom types into %s: %v, want ok %v", w.rule, w.session, err, w.ok)
			continue
		}
		if !w.ok {
			if len(r.typed.writes) != n {
				t.Errorf("%s: a refused type wrote", w.rule)
			}
			continue
		}
		got := r.typed.writes[n]
		if got.raw != w.raw || got.requireAgent != w.requireAgent || !got.agentDoor || got.from != "capcom" {
			t.Errorf("%s into %s: write %+v", w.rule, w.session, got)
		}
	}
	// ESC, Ctrl-C and CR stay refused at all.
	r.setRule(readPanesAll)
	for _, text := range []string{"\x1b", "\x03", "x\r"} {
		if _, err := r.s.Type(ctx, fromCap, api.PaneTypeParams{Session: "mate", Text: text}); paneErrKind(err) != api.ErrorKindInvalid {
			t.Errorf("at all, %q: %v, want invalid", text, err)
		}
	}
	// Another agent's text at all stays enveloped.
	n := len(r.typed.writes)
	if _, err := r.s.Type(ctx, fromMine, api.PaneTypeParams{Session: "mate", Text: "/compact"}); err != nil || r.typed.writes[n].raw {
		t.Fatalf("an agent at all: %v %+v", err, r.typed.writes[n:])
	}
	// The forward carries the mark at every value but off.
	for _, rule := range []string{readPanesAll, readPanesProject, drivePanesAgents} {
		r.setRule(rule)
		if _, err := r.s.Type(ctx, fromCap, api.PaneTypeParams{Session: "bee/far", Text: "x"}); err != nil {
			t.Errorf("%s: forward: %v", rule, err)
		}
	}
	r.setRule(readPanesOff)
	if _, err := r.s.Type(ctx, fromCap, api.PaneTypeParams{Session: "bee/far", Text: "x"}); paneErrKind(err) != api.ErrorKindDenied {
		t.Errorf("off: forward: %v", err)
	}
	if c := rec.take(); len(c) != 3 || c[0] != "pane.type as=capcom" {
		t.Errorf("forwards %q", c)
	}
}

// TestPaneCapcomNew: Capcom starts a pane only under all, in any folder, with no caller pane
// for its folder and no project.
func TestPaneCapcomNew(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	ctx := context.Background()
	r.setRule(readPanesAll)
	if _, err := r.s.New(ctx, fromCap, api.PaneNewParams{Cwd: "/tmp", Argv: []string{"/bin/sh"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.New(ctx, fromCap, api.PaneNewParams{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range r.created {
		if p.CwdOf != "" || p.Project != "" {
			t.Errorf("created %+v: Capcom has no pane and no project", p)
		}
	}
	if _, err := r.s.New(ctx, fromCap, api.PaneNewParams{InProject: true}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Errorf("in_project: %v", err)
	}
	for _, rule := range []string{readPanesProject, drivePanesAgents, readPanesOff} {
		r.setRule(rule)
		if _, err := r.s.New(ctx, fromCap, api.PaneNewParams{Cwd: "/tmp"}); paneErrKind(err) != api.ErrorKindDenied {
			t.Errorf("%s: %v, want denied", rule, err)
		}
	}
	if len(r.created) != 2 {
		t.Fatalf("%d creates", len(r.created))
	}
}

// TestPaneCapcomMark: a far hab takes the capcom mark only over a peer link from the hab that
// holds the Capcom mark in its belief. From any other peer, without the mark, from itself, or on
// a pane's route, the call is judged under that door's own rule.
func TestPaneCapcomMark(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	home := "home-device"
	r.s.capcomHome = func() string { return home }
	r.setRule(readPanesProject) // the peer door may type nothing here; Capcom may type into a shell
	ctl := controlPanes{r.s}
	ctx := context.Background()
	typeAs := func(from control.PaneCaller, as string) error {
		_, err := ctl.Type(ctx, from, api.PaneTypeParams{Session: "shell", Text: "ls", As: as})
		return err
	}
	if err := typeAs(control.PaneCaller{Peer: true, Conn: "c1", Device: home}, asCapcom); err != nil {
		t.Fatalf("the Capcom home's forward: %v", err)
	}
	if w := r.typed.writes[len(r.typed.writes)-1]; w.from != "capcom" {
		t.Fatalf("the write names %q", w.from)
	}
	for _, c := range []struct {
		name string
		from control.PaneCaller
		as   string
	}{
		{"another peer", control.PaneCaller{Peer: true, Conn: "c2", Device: "intruder"}, asCapcom},
		{"no mark", control.PaneCaller{Peer: true, Conn: "c1", Device: home}, ""},
		{"no device", control.PaneCaller{Peer: true, Conn: "c3"}, asCapcom},
		{"itself", control.PaneCaller{Peer: true, Conn: "c4", Device: "near"}, asCapcom},
	} {
		if c.name == "itself" {
			home = "near"
		}
		if err := typeAs(c.from, c.as); paneErrKind(err) != api.ErrorKindDenied {
			t.Errorf("%s: %v, want denied", c.name, err)
		}
	}
	home = "home-device"
	// No Capcom home known: the mark counts from no one.
	r.s.capcomHome = func() string { return "" }
	if err := typeAs(control.PaneCaller{Peer: true, Conn: "c5", Device: ""}, asCapcom); paneErrKind(err) != api.ErrorKindDenied {
		t.Errorf("no home: %v", err)
	}
	r.s.capcomHome = func() string { return home }
	// The owner's door is the owner whatever as says; the pane route ignores as.
	if err := typeAs(control.PaneCaller{}, asCapcom); err != nil {
		t.Errorf("the owner: %v", err)
	}
	r.setRule(drivePanesAgents)
	params, _ := json.Marshal(api.PaneTypeParams{Session: "other", Text: "x", As: asCapcom})
	if _, _, err := paneHook(r.s)(ctx, paneMine, "pane.type", params); paneErrKind(err) != api.ErrorKindDenied {
		t.Errorf("a pane's route with as=capcom: %v, want denied", err)
	}
}

// TestPaneCapcomRawLine: the raw line reaches an agent's pane as typed, still as one paste; Enter
// alone into an agent's pane stays refused.
func TestPaneCapcomRawLine(t *testing.T) {
	t.Parallel()
	keys := &keyLog{}
	a := &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf}
	never := func(string) bool { return false }
	ctx := context.Background()
	a.agents.Hook("p1", claudeEnv("SessionStart", `{"session_id":"s","hook_event_name":"SessionStart"}`, 1))
	if _, err := a.typeInto(ctx, "p1", typeWrite{text: "/compact", enter: true, agentDoor: true, raw: true, from: "capcom", recent: never}); err != nil {
		t.Fatal(err)
	}
	keys.mu.Lock()
	if len(keys.writes) != 2 || string(keys.writes[0]) != string(harness.PasteWrite("/compact")) || string(keys.writes[1]) != "\r" {
		t.Fatalf("writes %q", keys.writes)
	}
	keys.mu.Unlock()
	a.agents.Hook("p2", claudeEnv("SessionStart", `{"session_id":"s2","hook_event_name":"SessionStart"}`, 1))
	if _, err := a.typeInto(ctx, "p2", typeWrite{enter: true, agentDoor: true, raw: true, recent: never}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Fatalf("Enter alone: %v", err)
	}
}
