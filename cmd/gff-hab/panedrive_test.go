package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Driving panes (hab-logic-api.md 3.12), without a holder: the agent.drive_panes rule for
// pane.type and pane.new at each door, no existence oracle, the near rule on a forward, the
// Capcom route, the caller's own pane, what an agent's text may hold, and the send gate.

const (
	paneMine  = "00000000-0000-0000-0000-00000000000a" // the caller's pane, in project one; an agent
	paneMate  = "00000000-0000-0000-0000-00000000000b" // project one; an agent
	paneOther = "00000000-0000-0000-0000-00000000000c" // project two; an agent
	paneLoose = "00000000-0000-0000-0000-00000000000d" // unfiled, the loose caller's own
	paneShell = "00000000-0000-0000-0000-00000000000e" // project one; a plain shell
)

// typeLog is a paneTyper that records each type.
type typeLog struct {
	mu     sync.Mutex
	panes  []string
	writes []typeWrite
}

func (l *typeLog) typeInto(_ context.Context, pane string, w typeWrite) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.panes = append(l.panes, pane)
	l.writes = append(l.writes, w)
	return len(w.text), nil
}

// driveRig is a paneService over a structure store with four filed sessions and one unfiled.
type driveRig struct {
	s       *paneService
	rule    string
	typed   *typeLog
	created []api.SessionCreateParams
	peer    *channels.Link
	failNew error // create's answer, when set
}

func newDriveRig(t *testing.T) *driveRig {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder(
		structure.HolderPane{ID: paneMine}, structure.HolderPane{ID: paneMate}, structure.HolderPane{ID: paneOther},
		structure.HolderPane{ID: paneLoose}, structure.HolderPane{ID: paneShell})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	one, err := st.CreateProject("one", "git@example.invalid:one", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := st.CreateProject("two", "git@example.invalid:two", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []structure.SessionSpec{{Name: "me", Project: one.ID, Pane: paneMine, Index: -1},
		{Name: "mate", Project: one.ID, Pane: paneMate, Index: -1}, {Name: "other", Project: two.ID, Pane: paneOther, Index: -1},
		{Name: "loose", Pane: paneLoose, Index: -1}, {Name: "shell", Project: one.ID, Pane: paneShell, Index: -1}} {
		if _, err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	r := &driveRig{rule: readPanesAll, typed: &typeLog{}}
	r.peer = &channels.Link{Label: "bee", DeviceID: "far", Online: true, Caps: []string{paneDriveCapability}}
	r.s = &paneService{store: st, rule: func() string { return readPanesAll }, drive: func() string { return r.rule },
		links: func() []channels.Link { return []channels.Link{*r.peer} }, self: "near", host: "here", logf: t.Logf}
	agents := map[string]bool{paneMine: true, paneMate: true, paneOther: true}
	r.s.drv = paneDrive{typer: r.typed,
		agent: func(pane string) (api.AgentState, bool) {
			if !agents[pane] {
				return api.AgentState{}, false
			}
			return api.AgentState{Pane: pane, Revision: 1, Harness: "claude", State: api.AgentStatusIdle}, true
		},
		create: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			if r.failNew != nil {
				return api.SessionCreated{}, r.failNew
			}
			r.created = append(r.created, p)
			return api.SessionCreated{Session: "new-id", Pane: "00000000-0000-0000-0000-0000000000ee"}, nil
		}}
	return r
}

var (
	fromMine  = paneCaller{door: doorRoute, pane: paneMine}
	fromLoose = paneCaller{door: doorRoute, pane: paneLoose}
	fromPeer  = paneCaller{door: doorPeer, conn: "c1"}
	fromOwner = paneCaller{door: doorOwner}
	fromCap   = paneCaller{door: doorRoute, pane: capcomRoute}
)

// TestMayDrive is the rule's table (hab-logic-api.md 3.12).
func TestMayDrive(t *testing.T) {
	t.Parallel()
	agentIn := paneTarget{pane: paneMate, session: api.Session{Project: "p1"}, filed: true, agent: true}
	shellIn := paneTarget{pane: paneShell, session: api.Session{Project: "p1"}, filed: true}
	agentOut := paneTarget{pane: paneOther, session: api.Session{Project: "p2"}, filed: true, agent: true}
	self := paneTarget{pane: paneMine, session: api.Session{Project: "p1"}, filed: true, agent: true}
	unfiled := paneTarget{pane: paneLoose}
	for _, c := range []struct {
		rule    string
		caller  paneCaller
		project string
		target  paneTarget
		want    bool
	}{
		{readPanesAll, fromMine, "p1", agentOut, true},
		{readPanesAll, fromMine, "p1", self, false},
		{readPanesProject, fromMine, "p1", agentIn, true},
		{readPanesProject, fromMine, "p1", shellIn, true},
		{readPanesProject, fromMine, "p1", self, false},
		{readPanesProject, fromMine, "p1", agentOut, false},
		{readPanesProject, fromMine, "p1", unfiled, false},
		{readPanesProject, fromLoose, "", unfiled, false},
		{drivePanesAgents, fromMine, "p1", agentIn, true},
		{drivePanesAgents, fromMine, "p1", shellIn, false},
		{drivePanesAgents, fromMine, "p1", self, false},
		{drivePanesAgents, fromMine, "p1", agentOut, false},
		{readPanesOff, fromMine, "p1", agentIn, false},
		{readPanesAll, fromPeer, "", agentOut, true},
		{readPanesProject, fromPeer, "", agentIn, false},
		{drivePanesAgents, fromPeer, "", agentIn, false},
		{readPanesOff, fromOwner, "", agentOut, true},
		{readPanesAll, fromCap, "", agentIn, false},
	} {
		if got := mayDrive(c.rule, c.caller, c.project, c.target); got != c.want {
			t.Errorf("mayDrive(%s, door %d, project %q, %s) = %v, want %v", c.rule, c.caller.door, c.project, c.target.pane, got, c.want)
		}
	}
}

// TestPaneTypeRule: agent.drive_panes refuses pane.type with denied at each value and door,
// unknown targets alike under agents, project and off, the caller's own pane at every value, a
// label/name on a peer unless the near rule is all, and every call from the Capcom route. The
// hint names the setting, never the target.
func TestPaneTypeRule(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	ctx := context.Background()
	typeTo := func(c paneCaller, session string) error {
		_, err := r.s.Type(ctx, c, api.PaneTypeParams{Session: session, Text: "ls", Enter: true})
		return err
	}
	type want struct {
		caller  paneCaller
		session string
		kind    api.ErrorKind
	}
	for _, c := range []struct {
		rule  string
		cases []want
	}{
		{readPanesAll, []want{{fromMine, "other", ""}, {fromMine, "me", api.ErrorKindDenied}, {fromLoose, "mate", ""}, {fromPeer, "mate", ""},
			{fromMine, "ghost", api.ErrorKindNotFound}, {fromCap, "mate", api.ErrorKindDenied}}},
		{readPanesProject, []want{{fromMine, "mate", ""}, {fromMine, "shell", ""}, {fromMine, "me", api.ErrorKindDenied},
			{fromMine, "other", api.ErrorKindDenied}, {fromMine, "loose", api.ErrorKindDenied}, {fromMine, "ghost", api.ErrorKindDenied},
			{fromMine, "bee/far", api.ErrorKindDenied}, {fromLoose, "loose", api.ErrorKindDenied}, {fromPeer, "mate", api.ErrorKindDenied},
			{fromPeer, "ghost", api.ErrorKindDenied}, {fromOwner, "other", ""}, {fromCap, "mate", api.ErrorKindDenied}}},
		{drivePanesAgents, []want{{fromMine, "mate", ""}, {fromMine, "shell", api.ErrorKindDenied}, {fromMine, "me", api.ErrorKindDenied},
			{fromMine, "other", api.ErrorKindDenied}, {fromMine, "ghost", api.ErrorKindDenied}, {fromPeer, "mate", api.ErrorKindDenied},
			{fromOwner, "shell", ""}}},
		{readPanesOff, []want{{fromMine, "me", api.ErrorKindDenied}, {fromMine, "mate", api.ErrorKindDenied}, {fromMine, "ghost", api.ErrorKindDenied},
			{fromPeer, "mate", api.ErrorKindDenied}, {fromOwner, "mate", ""}}},
	} {
		r.rule = c.rule
		for _, w := range c.cases {
			err := typeTo(w.caller, w.session)
			if paneErrKind(err) != w.kind {
				t.Errorf("%s: door %d types into %s: %v, want %q", c.rule, w.caller.door, w.session, err, w.kind)
			}
			if w.kind == api.ErrorKindDenied && !w.caller.isCapcom() &&
				(strings.Contains(err.Error(), w.session) || !strings.Contains(err.Error(), "agent.drive_panes is "+c.rule)) {
				t.Errorf("%s: hint %q names the target, or not the setting", c.rule, err)
			}
		}
	}
	// Bad input is invalid before the rule.
	r.rule = readPanesAll
	if _, err := r.s.Type(ctx, fromMine, api.PaneTypeParams{Session: "mate"}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Errorf("no text, no enter: %v", err)
	}
	if _, err := r.s.Type(ctx, fromMine, api.PaneTypeParams{Session: "mate", Text: strings.Repeat("x", paneTypeMax+1)}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Errorf("text over the cap: %v", err)
	}
}

// TestPaneTypeAgentText: an agent's text holds no Esc and no C0 control but tab and newline, at
// every agent door; the owner's text goes as given (hab-logic-api.md 3.12).
func TestPaneTypeAgentText(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	ctx := context.Background()
	for _, text := range []string{"\x1b[Z", "/permissions\r", "x\x03", "\x1b", "a\u009bb"} {
		for _, c := range []paneCaller{fromMine, fromPeer} {
			if _, err := r.s.Type(ctx, c, api.PaneTypeParams{Session: "mate", Text: text}); paneErrKind(err) != api.ErrorKindInvalid {
				t.Errorf("door %d, %q: %v, want invalid", c.door, text, err)
			}
		}
	}
	n := len(r.typed.writes)
	if _, err := r.s.Type(ctx, fromMine, api.PaneTypeParams{Session: "mate", Text: "two\tcols\nlines"}); err != nil {
		t.Errorf("tab and newline: %v", err)
	}
	if _, err := r.s.Type(ctx, fromOwner, api.PaneTypeParams{Session: "mate", Text: "\x1b[Z"}); err != nil {
		t.Errorf("the owner's keys: %v", err)
	}
	if got := r.typed.writes[n:]; len(got) != 2 || !got[0].agentDoor || got[1].agentDoor {
		t.Errorf("writes %+v: the agent door is not marked", got)
	}
}

// TestPaneNewRule: agent.drive_panes for pane.new. all starts anywhere; project only in the
// caller's own project, from the caller's folder; agents and off nothing; a peer only under all;
// the owner always; the Capcom route never. A refused name tells a limited caller nothing.
func TestPaneNewRule(t *testing.T) {
	t.Parallel()
	r := newDriveRig(t)
	ctx := context.Background()
	newFrom := func(c paneCaller, p api.PaneNewParams) error {
		_, err := r.s.New(ctx, c, p)
		return err
	}
	r.rule = readPanesAll
	if err := newFrom(fromMine, api.PaneNewParams{Cwd: "/tmp", Argv: []string{"/bin/sh"}}); err != nil {
		t.Fatalf("all, cwd: %v", err)
	}
	if p := r.created[len(r.created)-1]; p.Cwd != "/tmp" || p.CwdOf != "" || p.Project != "" {
		t.Fatalf("all, cwd: %+v", p)
	}
	if err := newFrom(fromPeer, api.PaneNewParams{}); err != nil {
		t.Fatalf("all, peer: %v", err)
	}
	if err := newFrom(fromMine, api.PaneNewParams{Cwd: "rel"}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Fatalf("relative cwd: %v", err)
	}
	if err := newFrom(fromOwner, api.PaneNewParams{InProject: true}); paneErrKind(err) != api.ErrorKindInvalid {
		t.Fatalf("owner, in_project: %v", err)
	}
	r.rule = readPanesProject
	n := len(r.created)
	if err := newFrom(fromMine, api.PaneNewParams{}); err != nil {
		t.Fatalf("project: %v", err)
	}
	me, _ := r.s.store.SessionByPane(paneMine)
	if p := r.created[n]; p.CwdOf != paneMine || p.Project != me.Project {
		t.Fatalf("project: %+v, want cwd_of the caller and its project", p)
	}
	for _, c := range []struct {
		caller paneCaller
		p      api.PaneNewParams
	}{{fromMine, api.PaneNewParams{Cwd: "/tmp"}}, {fromLoose, api.PaneNewParams{}}, {fromPeer, api.PaneNewParams{}}, {fromCap, api.PaneNewParams{}}} {
		if err := newFrom(c.caller, c.p); paneErrKind(err) != api.ErrorKindDenied {
			t.Errorf("project, door %d %+v: %v, want denied", c.caller.door, c.p, err)
		}
	}
	if err := newFrom(fromOwner, api.PaneNewParams{Cwd: "/tmp"}); err != nil {
		t.Errorf("project, owner: %v", err)
	}
	// A taken name under project: a fixed text, never where the name is in use.
	r.failNew = control.Errorf(api.ErrorKindConflict, `name "x" in use on bee`)
	if err := newFrom(fromMine, api.PaneNewParams{Name: "x"}); paneErrKind(err) != api.ErrorKindConflict || strings.Contains(err.Error(), "bee") {
		t.Errorf("project, a taken name: %v", err)
	}
	if err := newFrom(fromOwner, api.PaneNewParams{Name: "x"}); err == nil || !strings.Contains(err.Error(), "bee") {
		t.Errorf("owner, a taken name: %v", err)
	}
	r.failNew = nil
	for _, rule := range []string{drivePanesAgents, readPanesOff} {
		r.rule = rule
		for _, c := range []paneCaller{fromMine, fromPeer, fromCap} {
			if err := newFrom(c, api.PaneNewParams{}); paneErrKind(err) != api.ErrorKindDenied {
				t.Errorf("%s, door %d: %v, want denied", rule, c.door, err)
			}
		}
	}
	if len(r.created) != n+2 {
		t.Errorf("%d creates, want %d: a refused call created a pane", len(r.created), n+2)
	}
}

// TestPaneTypeGate: the send gate for pane.type, under the pane's claim (hab-logic-api.md
// 3.12). A shell takes the text as typed; an open permission request, a blocked agent, an agent
// with no hooks (from an agent's door), a send in flight and a person's recent input each refuse
// with conflict and write nothing; text into an agent goes as one paste; Enter, or a newline in
// the text, holds an agent's pane as a send does; a type never marks the owner's interrupt.
func TestPaneTypeGate(t *testing.T) {
	t.Parallel()
	keys := &keyLog{}
	a := &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf}
	ctx := context.Background()
	never := func(string) bool { return false }
	writes := func() [][]byte { keys.mu.Lock(); defer keys.mu.Unlock(); return append([][]byte(nil), keys.writes...) }
	typeIn := func(pane string, w typeWrite) error {
		if w.recent == nil {
			w.recent = never
		}
		_, err := a.typeInto(ctx, pane, w)
		return err
	}

	// A shell: no agent state; the text goes as typed.
	if err := typeIn("shell", typeWrite{text: "ls", enter: true, agentDoor: true}); err != nil || len(writes()) != 2 || string(writes()[0]) != "ls" {
		t.Fatalf("shell: %v, %q", err, writes())
	}
	if a.sendHeld("shell") {
		t.Fatal("a type into a shell keeps the claim")
	}
	// A person typed just now.
	if err := typeIn("shell", typeWrite{text: "x", recent: func(string) bool { return true }}); paneErrKind(err) != api.ErrorKindConflict ||
		!strings.Contains(err.Error(), "person comes first") || len(writes()) != 2 {
		t.Fatalf("recent input: %v, %d writes", err, len(writes()))
	}
	// A send in flight.
	a.claim("shell")
	if err := typeIn("shell", typeWrite{text: "x"}); paneErrKind(err) != api.ErrorKindConflict || len(writes()) != 2 {
		t.Fatalf("send in flight: %v", err)
	}
	a.unclaim("shell")
	// An open permission request.
	a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	if _, req := a.agents.Hook("p", claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make"}}`, 2)); req == "" {
		t.Fatal("no request opened")
	}
	if err := typeIn("p", typeWrite{text: "y", enter: true}); paneErrKind(err) != api.ErrorKindConflict ||
		!strings.Contains(err.Error(), "permission request") || len(writes()) != 2 {
		t.Fatalf("open request: %v", err)
	}
	if a.sendHeld("p") {
		t.Fatal("a refused type keeps the claim")
	}
	// Blocked without an open request, and an agent with no hooks.
	a.agents.Apply("b", "claude", api.StateSourceOsc, 1, harness.Update{State: api.AgentStatusBlocked})
	if err := typeIn("b", typeWrite{text: "1"}); paneErrKind(err) != api.ErrorKindConflict || len(writes()) != 2 {
		t.Fatalf("blocked: %v", err)
	}
	a.agents.Apply("o", "claude", api.StateSourceOsc, 1, harness.Update{State: api.AgentStatusIdle})
	if err := typeIn("o", typeWrite{text: "hi", agentDoor: true}); paneErrKind(err) != api.ErrorKindConflict || len(writes()) != 2 {
		t.Fatalf("no hooks, agent door: %v", err)
	}
	// An idle agent with hooks: one paste, then Enter; the pane holds until the turn starts.
	a.agents.Hook("q", claudeEnv("SessionStart", `{"session_id":"b","hook_event_name":"SessionStart"}`, 1))
	if err := typeIn("q", typeWrite{text: "hello\x1b[201~x", enter: true, agentDoor: true}); err != nil {
		t.Fatalf("idle agent: %v", err)
	}
	w := writes()
	if len(w) != 4 || string(w[2]) != "\x1b[200~hellox\x1b[201~" || string(w[3]) != "\r" {
		t.Fatalf("idle agent writes %q", w[2:])
	}
	if !a.sendHeld("q") {
		t.Fatal("Enter into an idle agent does not hold the pane")
	}
	if err := typeIn("q", typeWrite{text: "again"}); paneErrKind(err) != api.ErrorKindConflict {
		t.Fatalf("a second type while the first holds: %v", err)
	}
	waitUntil(t, "the hold to end", func() bool { return !a.sendHeld("q") })
	// A newline in the text holds as Enter does.
	if err := typeIn("q", typeWrite{text: "one\ntwo", agentDoor: true}); err != nil || !a.sendHeld("q") {
		t.Fatalf("newline: %v, held %v", err, a.sendHeld("q"))
	}
	waitUntil(t, "the hold to end", func() bool { return !a.sendHeld("q") })
	// Ctrl-C from the owner's door into a working agent is not the owner's interrupt.
	a.agents.Hook("q", claudeEnv("UserPromptSubmit", `{"session_id":"b","hook_event_name":"UserPromptSubmit","prompt":"go"}`, 5))
	if st, _ := a.agents.Get("q"); st.State != api.AgentStatusWorking {
		t.Fatalf("q is %s, want working", st.State)
	}
	if err := typeIn("q", typeWrite{text: "\x03"}); err != nil {
		t.Fatalf("owner Ctrl-C: %v", err)
	}
	if st, _ := a.agents.Get("q"); st.State != api.AgentStatusWorking {
		t.Fatalf("after a typed Ctrl-C q is %s: a type marked an interrupt", st.State)
	}
}

// TestInputTimes: a client's pane.input counts for the gate as it is queued and once written;
// the window ends.
func TestInputTimes(t *testing.T) {
	t.Parallel()
	var it inputTimes
	if it.within(paneMate, time.Second) {
		t.Fatal("no input yet")
	}
	it.note(strings.ToUpper(paneMate))
	if !it.within(paneMate, time.Second) {
		t.Fatal("input in another spelling of the id is not seen")
	}
	if it.within(paneMate, 0) {
		t.Fatal("an empty window holds")
	}
}

// TestPaneToolsUntrusted: the five pane tools are in the kit, the read ones say their text is
// untrusted program output, and the two that drive are marked destructive and open-world.
func TestPaneToolsUntrusted(t *testing.T) {
	t.Parallel()
	ann := map[string]map[string]any{}
	for _, m := range mcpToolsOf(allTools()) {
		ann[m["name"].(string)] = m["annotations"].(map[string]any)
	}
	for _, name := range []string{"pane_list", "pane_read", "pane_wait", "pane_type", "pane_new"} {
		tool, ok := findIn(allTools(), name)
		if !ok {
			t.Fatalf("no tool %s", name)
		}
		if (name == "pane_read" || name == "pane_wait") && !strings.Contains(tool.Doc, "untrusted") {
			t.Errorf("%s: %q does not say the text is untrusted", name, tool.Doc)
		}
		drives := name == "pane_type" || name == "pane_new"
		if tool.Action != drives || ann[name]["destructiveHint"] != drives || ann[name]["openWorldHint"] != drives {
			t.Errorf("%s: action %v, hints %v", name, tool.Action, ann[name])
		}
	}
	if !strings.Contains(skillText(), "untrusted data") || !strings.Contains((&mcpServer{}).instructions(), "pane_type") {
		t.Error("the manual or the instructions lack the pane rules")
	}
}

// ownerSocket is a caller that reaches the hab through the owner's socket, as the MCP server
// does outside a pane. It records every call.
type ownerSocket struct{ calls []string }

func (o *ownerSocket) Call(_ context.Context, method string, _, _ any) error {
	o.calls = append(o.calls, method)
	return nil
}

// TestPaneToolsOutsideAPane: without the pane route, every pane tool is refused before any call,
// so a harness that runs the kit outside a pane cannot type or start through the owner's door.
func TestPaneToolsOutsideAPane(t *testing.T) {
	t.Parallel()
	o := &ownerSocket{}
	args := map[string]toolArgs{"pane_list": {}, "pane_read": {"target": "work"}, "pane_wait": {"target": "work", "quiet_ms": 10.0},
		"pane_type": {"target": "work", "text": "ls", "enter": true}, "pane_new": {"argv": []any{"/bin/sh"}}}
	for name, a := range args {
		tool, _ := findIn(allTools(), name)
		if _, _, err := tool.run(context.Background(), o, a); paneErrKind(err) != api.ErrorKindDenied {
			t.Errorf("%s outside a pane: %v, want denied", name, err)
		}
	}
	if len(o.calls) != 0 {
		t.Errorf("calls reached the hab: %v", o.calls)
	}
	// From a pane they call.
	p := &paneFake{}
	tool, _ := findIn(allTools(), "pane_list")
	if _, _, err := tool.run(context.Background(), p, toolArgs{}); err != nil {
		t.Errorf("pane_list from a pane: %v", err)
	}
}
