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
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Driving panes (hab-logic-api.md 3.12), without a holder: the agent.drive_panes rule for
// pane.type and pane.new at each door, no existence oracle, the near rule on a forward, the
// Capcom route, and the send gate for pane.type.

const (
	paneMine  = "00000000-0000-0000-0000-00000000000a" // the caller's pane, in project one
	paneMate  = "00000000-0000-0000-0000-00000000000b" // project one
	paneOther = "00000000-0000-0000-0000-00000000000c" // project two
	paneLoose = "00000000-0000-0000-0000-00000000000d" // unfiled, the loose caller's own
)

// typeLog is a paneTyper that records each type.
type typeLog struct {
	mu    sync.Mutex
	panes []string
}

func (l *typeLog) typeInto(_ context.Context, pane string, _ func(string) bool, _ bool, _ ...[]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.panes = append(l.panes, pane)
	return nil
}

// driveRig is a paneService over a structure store with three filed sessions and one unfiled.
type driveRig struct {
	s       *paneService
	rule    string
	typed   *typeLog
	created []api.SessionCreateParams
	peer    *channels.Link
}

func newDriveRig(t *testing.T) *driveRig {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder(
		structure.HolderPane{ID: paneMine}, structure.HolderPane{ID: paneMate}, structure.HolderPane{ID: paneOther}, structure.HolderPane{ID: paneLoose})})
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
		{Name: "loose", Pane: paneLoose, Index: -1}} {
		if _, err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	r := &driveRig{rule: readPanesAll, typed: &typeLog{}}
	r.peer = &channels.Link{Label: "bee", DeviceID: "far", Online: true, Caps: []string{paneDriveCapability}}
	r.s = &paneService{store: st, rule: func() string { return readPanesAll }, drive: func() string { return r.rule },
		links: func() []channels.Link { return []channels.Link{*r.peer} }, self: "near", host: "here", logf: t.Logf}
	r.s.drv = paneDrive{typer: r.typed, create: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
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
	inP := paneTarget{pane: paneMate, session: api.Session{Project: "p1"}, filed: true}
	outP := paneTarget{pane: paneOther, session: api.Session{Project: "p2"}, filed: true}
	self := paneTarget{pane: paneMine, session: api.Session{Project: "p1"}, filed: true}
	unfiled := paneTarget{pane: paneLoose}
	for _, c := range []struct {
		rule    string
		caller  paneCaller
		project string
		target  paneTarget
		want    bool
	}{
		{readPanesAll, fromMine, "p1", outP, true},
		{readPanesProject, fromMine, "p1", inP, true},
		{readPanesProject, fromMine, "p1", self, true},
		{readPanesProject, fromMine, "p1", outP, false},
		{readPanesProject, fromMine, "p1", unfiled, false},
		{readPanesProject, fromLoose, "", unfiled, false},
		{readPanesOff, fromMine, "p1", self, false},
		{readPanesOff, fromMine, "p1", inP, false},
		{readPanesAll, fromPeer, "", outP, true},
		{readPanesProject, fromPeer, "", inP, false},
		{readPanesOff, fromOwner, "", outP, true},
		{readPanesAll, fromCap, "", inP, false},
	} {
		if got := mayDrive(c.rule, c.caller, c.project, c.target); got != c.want {
			t.Errorf("mayDrive(%s, door %d, project %q, %s) = %v, want %v", c.rule, c.caller.door, c.project, c.target.pane, got, c.want)
		}
	}
}

// TestPaneTypeRule: agent.drive_panes refuses pane.type with denied at each value and door,
// unknown targets alike under project and off, a label/name on a peer unless the near rule is
// all, and every call from the Capcom route. The hint names the setting, never the target.
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
		{readPanesAll, []want{{fromMine, "other", ""}, {fromLoose, "mate", ""}, {fromPeer, "mate", ""}, {fromMine, "ghost", api.ErrorKindNotFound},
			{fromCap, "mate", api.ErrorKindDenied}}},
		{readPanesProject, []want{{fromMine, "mate", ""}, {fromMine, "me", ""}, {fromMine, "other", api.ErrorKindDenied},
			{fromMine, "loose", api.ErrorKindDenied}, {fromMine, "ghost", api.ErrorKindDenied}, {fromMine, "bee/far", api.ErrorKindDenied},
			{fromLoose, "loose", api.ErrorKindDenied}, {fromPeer, "mate", api.ErrorKindDenied}, {fromPeer, "ghost", api.ErrorKindDenied},
			{fromOwner, "other", ""}, {fromCap, "mate", api.ErrorKindDenied}}},
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

// TestPaneNewRule: agent.drive_panes for pane.new. all starts anywhere; project only in the
// caller's own project, from the caller's folder; off nothing; a peer only under all; the
// owner always; the Capcom route never.
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
	r.rule = readPanesOff
	for _, c := range []paneCaller{fromMine, fromPeer, fromCap} {
		if err := newFrom(c, api.PaneNewParams{}); paneErrKind(err) != api.ErrorKindDenied {
			t.Errorf("off, door %d: %v, want denied", c.door, err)
		}
	}
	if len(r.created) != n+2 {
		t.Errorf("%d creates, want %d: a refused call created a pane", len(r.created), n+2)
	}
}

// TestPaneTypeGate: the send gate for pane.type, under the pane's claim (hab-logic-api.md
// 3.12). A shell takes the text; an open permission request, a send in flight and a person's
// recent input each refuse with conflict and write nothing; Enter into an agent's pane holds
// the pane as a send does.
func TestPaneTypeGate(t *testing.T) {
	t.Parallel()
	keys := &keyLog{}
	a := &agentService{agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf}
	ctx := context.Background()
	never := func(string) bool { return false }
	writes := func() int { keys.mu.Lock(); defer keys.mu.Unlock(); return len(keys.writes) }

	// A shell: no agent state, so only the claim and the typing check apply.
	if err := a.typeInto(ctx, "shell", never, true, []byte("ls"), []byte{'\r'}); err != nil || writes() != 2 {
		t.Fatalf("shell: %v, %d writes", err, writes())
	}
	if a.sendHeld("shell") {
		t.Fatal("a type into a shell keeps the claim")
	}
	// A person typed just now.
	if err := a.typeInto(ctx, "shell", func(string) bool { return true }, false, []byte("x")); paneErrKind(err) != api.ErrorKindConflict ||
		!strings.Contains(err.Error(), "person comes first") || writes() != 2 {
		t.Fatalf("recent input: %v, %d writes", err, writes())
	}
	// A send in flight.
	a.claim("shell")
	if err := a.typeInto(ctx, "shell", never, false, []byte("x")); paneErrKind(err) != api.ErrorKindConflict || writes() != 2 {
		t.Fatalf("send in flight: %v, %d writes", err, writes())
	}
	a.unclaim("shell")
	// An open permission request.
	a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	if _, req := a.agents.Hook("p", claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make"}}`, 2)); req == "" {
		t.Fatal("no request opened")
	}
	if err := a.typeInto(ctx, "p", never, true, []byte("y"), []byte{'\r'}); paneErrKind(err) != api.ErrorKindConflict ||
		!strings.Contains(err.Error(), "permission request") || writes() != 2 {
		t.Fatalf("open request: %v, %d writes", err, writes())
	}
	if a.sendHeld("p") {
		t.Fatal("a refused type keeps the claim")
	}
	// An idle agent takes the text; Enter holds the pane until the turn starts.
	a.agents.Hook("q", claudeEnv("SessionStart", `{"session_id":"b","hook_event_name":"SessionStart"}`, 1))
	if st, ok := a.agents.Get("q"); !ok || st.State != api.AgentStatusIdle {
		t.Fatalf("q: %+v", st)
	}
	if err := a.typeInto(ctx, "q", never, true, []byte("hello"), []byte{'\r'}); err != nil || writes() != 4 {
		t.Fatalf("idle agent: %v, %d writes", err, writes())
	}
	if !a.sendHeld("q") {
		t.Fatal("Enter into an idle agent does not hold the pane")
	}
	if err := a.typeInto(ctx, "q", never, false, []byte("again")); paneErrKind(err) != api.ErrorKindConflict {
		t.Fatalf("a second type while the first holds: %v", err)
	}
	waitUntil(t, "the hold to end", func() bool { return !a.sendHeld("q") })
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

// TestPaneToolsUntrusted: the five pane tools are in the kit, and the read ones say their text
// is untrusted program output.
func TestPaneToolsUntrusted(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pane_list", "pane_read", "pane_wait", "pane_type", "pane_new"} {
		tool, ok := findIn(allTools(), name)
		if !ok {
			t.Fatalf("no tool %s", name)
		}
		if (name == "pane_read" || name == "pane_wait") && !strings.Contains(tool.Doc, "untrusted") {
			t.Errorf("%s: %q does not say the text is untrusted", name, tool.Doc)
		}
		if tool.Action != (name == "pane_type" || name == "pane_new") {
			t.Errorf("%s: action %v", name, tool.Action)
		}
	}
	if !strings.Contains(skillText(), "untrusted data") || !strings.Contains((&mcpServer{}).instructions(), "pane_type") {
		t.Error("the manual or the instructions lack the pane rules")
	}
}
