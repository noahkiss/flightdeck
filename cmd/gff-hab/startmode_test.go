package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestWithStartMode: each start mode adds its flags right after the program for Claude Code and
// Codex; ask adds nothing; an argv that names a mode already gets nothing; opencode has no
// checked row, so it gets nothing and a reason to log.
func TestWithStartMode(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		harness, mode string
		argv          []string
		want          []string
		added         string
		why           bool
	}{
		{"claude", "ask", []string{"claude"}, []string{"claude"}, "", false},
		{"claude", "", []string{"claude"}, []string{"claude"}, "", false},
		{"claude", "auto", []string{"claude", "--model", "opus"}, []string{"claude", "--permission-mode", "auto", "--model", "opus"}, "auto", false},
		{"claude", "bypass", []string{"claude"}, []string{"claude", "--permission-mode", "bypassPermissions"}, "bypass", false},
		{"codex", "auto", []string{"codex", "--no-daemon"}, []string{"codex", "--approve-for-me", "--no-daemon"}, "auto", false},
		{"codex", "bypass", []string{"codex", "--no-daemon", "resume", "x"}, []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "--no-daemon", "resume", "x"}, "bypass", false},
		// An explicit flag wins.
		{"claude", "bypass", []string{"claude", "--permission-mode", "plan"}, []string{"claude", "--permission-mode", "plan"}, "", false},
		{"claude", "bypass", []string{"claude", "--permission-mode=default"}, []string{"claude", "--permission-mode=default"}, "", false},
		{"claude", "auto", []string{"claude", "--dangerously-skip-permissions"}, []string{"claude", "--dangerously-skip-permissions"}, "", false},
		{"codex", "bypass", []string{"codex", "-s", "read-only"}, []string{"codex", "-s", "read-only"}, "", false},
		{"codex", "bypass", []string{"codex", "--ask-for-approval=on-request"}, []string{"codex", "--ask-for-approval=on-request"}, "", false},
		{"codex", "auto", []string{"codex", "--full-auto"}, []string{"codex", "--full-auto"}, "", false},
		{"codex", "auto", []string{"codex", "-c", "sandbox_mode=\"read-only\""}, []string{"codex", "-c", "sandbox_mode=\"read-only\""}, "", false},
		{"codex", "auto", []string{"codex", "--config=approval_policy=untrusted"}, []string{"codex", "--config=approval_policy=untrusted"}, "", false},
		// A -c that sets something else is no mode.
		{"codex", "bypass", []string{"codex", "-c", "model=\"o3\""}, []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "-c", "model=\"o3\""}, "bypass", false},
		// The flag's own value is no flag: a claude prompt that mentions it still gets the mode.
		{"claude", "auto", []string{"claude", "explain --permission-mode"}, []string{"claude", "--permission-mode", "auto", "explain --permission-mode"}, "auto", false},
		// Attached short values, a Codex profile and Claude Code's --settings name a mode too.
		{"codex", "bypass", []string{"codex", "-sread-only"}, []string{"codex", "-sread-only"}, "", false},
		{"codex", "bypass", []string{"codex", "-auntrusted"}, []string{"codex", "-auntrusted"}, "", false},
		{"codex", "bypass", []string{"codex", "-csandbox_mode=\"read-only\""}, []string{"codex", "-csandbox_mode=\"read-only\""}, "", false},
		{"codex", "bypass", []string{"codex", "-capproval_policy=never"}, []string{"codex", "-capproval_policy=never"}, "", false},
		{"codex", "bypass", []string{"codex", "-c=approval_policy=never"}, []string{"codex", "-c=approval_policy=never"}, "", false},
		{"codex", "bypass", []string{"codex", "-p", "careful"}, []string{"codex", "-p", "careful"}, "", false},
		{"codex", "bypass", []string{"codex", "-pcareful"}, []string{"codex", "-pcareful"}, "", false},
		{"codex", "bypass", []string{"codex", "--profile=careful"}, []string{"codex", "--profile=careful"}, "", false},
		{"claude", "bypass", []string{"claude", "--settings", "/s.json"}, []string{"claude", "--settings", "/s.json"}, "", false},
		{"claude", "bypass", []string{"claude", "--settings={}"}, []string{"claude", "--settings={}"}, "", false},
		// Other short flags are no mode: -m, and a -c that sets something else.
		{"codex", "auto", []string{"codex", "-mo3", "-cmodel=o3"}, []string{"codex", "--approve-for-me", "-mo3", "-cmodel=o3"}, "auto", false},
		// Unverified rows add nothing.
		{"opencode", "bypass", []string{"opencode"}, []string{"opencode"}, "", true},
		{"opencode", "ask", []string{"opencode"}, []string{"opencode"}, "", false},
		{"claude", "yolo", []string{"claude"}, []string{"claude"}, "", true},
	} {
		got, added, why := withStartMode(c.harness, c.argv, c.mode)
		if !slices.Equal(got, c.want) || added != c.added || (why != "") != c.why {
			t.Errorf("%s %s %q: got %q added %q why %q; want %q added %q why %v", c.harness, c.mode, c.argv, got, added, why, c.want, c.added, c.why)
		}
	}
	argv := []string{"claude"}
	withStartMode("claude", argv, "bypass")
	if !slices.Equal(argv, []string{"claude"}) {
		t.Fatalf("the caller's argv changed: %q", argv)
	}
}

// startRig is actions over a store with a fake holder that records each spawn's argv, and an
// agent.start_mode the test sets.
type startRig struct {
	a    *actions
	st   *structure.Store
	mu   sync.Mutex
	mode string
	argv [][]string
	logs []string
}

func newStartRig(t *testing.T, panes ...structure.HolderPane) *startRig {
	t.Helper()
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder(panes...)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	r := &startRig{st: st, mode: startAsk}
	sp := &spawner{tokens: hook.NewTokens(make([]byte, 32), nil), hookSock: "/nonexistent/hook.sock", spawning: newSpawning(),
		run: func(_ context.Context, _ string, s hp.Spawn) (link.PaneStatus, error) {
			r.mu.Lock()
			r.argv = append(r.argv, slices.Clone(s.Argv))
			r.mu.Unlock()
			return link.PaneStatus{UUID: s.UUID}, nil
		},
		logf: func(f string, args ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(f, args...))
			r.mu.Unlock()
		}}
	r.a = &actions{sp: sp, store: st, getenv: func(string) string { return "" }, environ: func() []string { return nil }, home: t.TempDir(),
		startMode: func() string { r.mu.Lock(); defer r.mu.Unlock(); return r.mode }}
	return r
}

func (r *startRig) set(mode string) { r.mu.Lock(); r.mode = mode; r.mu.Unlock() }

func (r *startRig) last(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.argv) == 0 {
		t.Fatal("nothing spawned")
	}
	return r.argv[len(r.argv)-1]
}

func (r *startRig) create(t *testing.T, ctx context.Context, p api.SessionCreateParams) api.Session {
	t.Helper()
	id, _, err := r.a.CreateSession(ctx, p)
	if err != nil {
		t.Fatalf("create %+v: %v", p, err)
	}
	ss, ok := r.st.Session(id)
	if !ok {
		t.Fatalf("no session %s", id)
	}
	return ss
}

// TestCreateStartMode: session.create with a harness reads agent.start_mode at each start, adds
// its flags and records the mode on the session; a plain program gets nothing, even one named
// claude; an explicit flag in the argv (a round's harness_args) suppresses it; a recorded mode
// on the context wins over the setting, a recorded ask included; an unverified harness adds
// nothing and logs one line per start.
func TestCreateStartMode(t *testing.T) {
	t.Parallel()
	r := newStartRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		harness, mode string
		want          []string
	}{
		{"claude", startAsk, []string{"claude"}},
		{"claude", startAuto, []string{"claude", "--permission-mode", "auto"}},
		{"claude", startBypass, []string{"claude", "--permission-mode", "bypassPermissions"}},
		{"codex", startAsk, []string{"codex", "--no-daemon"}},
		{"codex", startAuto, []string{"codex", "--approve-for-me", "--no-daemon"}},
		{"codex", startBypass, []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "--no-daemon"}},
	} {
		r.set(c.mode)
		ss := r.create(t, ctx, api.SessionCreateParams{Harness: c.harness, Cwd: r.a.home})
		if got := r.last(t); !slices.Equal(got, c.want) {
			t.Errorf("%s at %s: argv %q, want %q", c.harness, c.mode, got, c.want)
		}
		want := c.mode
		if want == startAsk {
			want = ""
		}
		if ss.StartMode != want {
			t.Errorf("%s at %s: recorded %q, want %q", c.harness, c.mode, ss.StartMode, want)
		}
	}

	r.set(startBypass)
	ss := r.create(t, ctx, api.SessionCreateParams{Argv: []string{"claude"}, Cwd: r.a.home})
	if got := r.last(t); !slices.Equal(got, []string{"claude"}) || ss.StartMode != "" {
		t.Fatalf("a plain program: argv %q, recorded %q", got, ss.StartMode)
	}
	ss = r.create(t, ctx, api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--permission-mode", "plan"}, Cwd: r.a.home})
	if got := r.last(t); !slices.Equal(got, []string{"claude", "--permission-mode", "plan"}) || ss.StartMode != "" {
		t.Fatalf("an explicit flag: argv %q, recorded %q", got, ss.StartMode)
	}

	// A recorded mode wins over the setting, and a recorded ask adds nothing.
	ss = r.create(t, withRecordedMode(ctx, startAuto), api.SessionCreateParams{Harness: "claude", Cwd: r.a.home})
	if got := r.last(t); !slices.Equal(got, []string{"claude", "--permission-mode", "auto"}) || ss.StartMode != startAuto {
		t.Fatalf("recorded auto at bypass: argv %q, recorded %q", got, ss.StartMode)
	}
	ss = r.create(t, withRecordedMode(ctx, ""), api.SessionCreateParams{Harness: "claude", Cwd: r.a.home})
	if got := r.last(t); !slices.Equal(got, []string{"claude"}) || ss.StartMode != "" {
		t.Fatalf("recorded ask at bypass: argv %q, recorded %q", got, ss.StartMode)
	}
	// A lowered setting lowers a recorded mode: the lower of the two wins.
	r.set(startAuto)
	ss = r.create(t, withRecordedMode(ctx, startBypass), api.SessionCreateParams{Harness: "claude", Cwd: r.a.home})
	if got := r.last(t); !slices.Equal(got, []string{"claude", "--permission-mode", "auto"}) || ss.StartMode != startAuto {
		t.Fatalf("recorded bypass at auto: argv %q, recorded %q", got, ss.StartMode)
	}
	r.set(startAsk)
	ss = r.create(t, withRecordedMode(ctx, startBypass), api.SessionCreateParams{Harness: "claude", Cwd: r.a.home})
	if got := r.last(t); !slices.Equal(got, []string{"claude"}) || ss.StartMode != "" {
		t.Fatalf("recorded bypass at ask: argv %q, recorded %q", got, ss.StartMode)
	}

	// The Capcom service's own session gets no flag, whatever the setting.
	r.set(startBypass)
	id, _, err := r.a.createSession(ctx, api.SessionCreateParams{Name: "capcom-test", Harness: "claude", Cwd: r.a.home}, string(api.SessionKindCapcom), r.a.home)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ = r.st.Session(id)
	if got := r.last(t); !slices.Equal(got, []string{"claude"}) || ss.StartMode != "" {
		t.Fatalf("the Capcom session at bypass: argv %q, recorded %q", got, ss.StartMode)
	}

	// opencode's row is unverified: nothing added, one line per start. This rig cannot start
	// opencode, so the start fails after the mode is read.
	r.mu.Lock()
	r.logs = nil
	r.mu.Unlock()
	for range 2 {
		if _, _, err := r.a.CreateSession(ctx, api.SessionCreateParams{Harness: "opencode", Cwd: r.a.home}); errKind(err) != api.ErrorKindUnavailable {
			t.Fatalf("opencode: %v", err)
		}
	}
	r.mu.Lock()
	logs := slices.Clone(r.logs)
	r.mu.Unlock()
	if len(logs) != 2 || !strings.Contains(logs[0], "opencode has no checked flag") {
		t.Fatalf("opencode logs %q, want one line per start", logs)
	}
}

// TestResumeStartMode: session.resume (and so session.restart) starts a session again in the
// lower of the mode its record holds and the current setting: auto stays auto at bypass, a
// session started at ask stays at ask, and bypass drops to auto at auto and to none at ask.
func TestResumeStartMode(t *testing.T) {
	t.Parallel()
	const p1, p2 = "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000a2"
	const p3, p4 = "00000000-0000-0000-0000-0000000000a3", "00000000-0000-0000-0000-0000000000a4"
	r := newStartRig(t, structure.HolderPane{ID: p1, Exit: &api.ExitStatus{}}, structure.HolderPane{ID: p2, Exit: &api.ExitStatus{}},
		structure.HolderPane{ID: p3, Exit: &api.ExitStatus{}}, structure.HolderPane{ID: p4, Exit: &api.ExitStatus{}})
	auto1, err := r.st.CreateSession(structure.SessionSpec{Name: "auto", Pane: p1, Index: -1, StartMode: startAuto})
	if err != nil || auto1.State != api.SessionStateExited || auto1.StartMode != startAuto {
		t.Fatalf("session: %+v %v", auto1, err)
	}
	ask, err := r.st.CreateSession(structure.SessionSpec{Name: "ask", Pane: p2, Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	r.set(startBypass)
	ctx := context.Background()
	if _, err := r.a.resumeSession(ctx, auto1.ID, api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--resume", "n1"}}, r.a.home); err != nil {
		t.Fatal(err)
	}
	if got := r.last(t); !slices.Equal(got, []string{"claude", "--permission-mode", "auto", "--resume", "n1"}) {
		t.Fatalf("resume of an auto session at bypass: %q", got)
	}
	if _, err := r.a.resumeSession(ctx, ask.ID, api.SessionCreateParams{Harness: "codex", Argv: []string{"codex", "resume", "n2"}}, r.a.home); err != nil {
		t.Fatal(err)
	}
	if got := r.last(t); !slices.Equal(got, []string{"codex", "--no-daemon", "resume", "n2"}) {
		t.Fatalf("resume of an ask session at bypass: %q", got)
	}
	if ss, _ := r.st.Session(auto1.ID); ss.StartMode != startAuto {
		t.Fatalf("the record lost its mode: %+v", ss)
	}

	byp, err := r.st.CreateSession(structure.SessionSpec{Name: "byp", Pane: p3, Index: -1, StartMode: startBypass})
	if err != nil {
		t.Fatal(err)
	}
	r.set(startAuto)
	if _, err := r.a.resumeSession(ctx, byp.ID, api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--resume", "n3"}}, r.a.home); err != nil {
		t.Fatal(err)
	}
	if got := r.last(t); !slices.Equal(got, []string{"claude", "--permission-mode", "auto", "--resume", "n3"}) {
		t.Fatalf("resume of a bypass session at auto: %q", got)
	}
	byp2, err := r.st.CreateSession(structure.SessionSpec{Name: "byp2", Pane: p4, Index: -1, StartMode: startBypass})
	if err != nil {
		t.Fatal(err)
	}
	r.set(startAsk)
	if _, err := r.a.resumeSession(ctx, byp2.ID, api.SessionCreateParams{Harness: "claude", Argv: []string{"claude", "--resume", "n3"}}, r.a.home); err != nil {
		t.Fatal(err)
	}
	if got := r.last(t); !slices.Equal(got, []string{"claude", "--resume", "n3"}) {
		t.Fatalf("resume of a bypass session at ask: %q", got)
	}
}

// TestRecreateStartMode: recreate starts the fresh session with the lower of the old one's
// recorded mode and the current setting: a recorded ask stays ask at bypass, a recorded bypass
// drops to auto at auto. Its argv carries no flag the hab added, so the create adds one, not two.
func TestRecreateStartMode(t *testing.T) {
	t.Parallel()
	r := newRecreateRig(t)
	sr := newStartRig(t)
	sr.set(startBypass)
	r.w.create = func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
		p.Name, p.ProjectDir, p.Cwd = "", "", sr.a.home // the start rig has no project
		id, _, err := sr.a.CreateSession(ctx, p)
		return api.SessionCreated{Session: id}, err
	}
	for _, c := range []struct {
		name, recorded, setting string
		want                    []string
	}{
		{"mate", startAuto, startBypass, []string{"claude", "--permission-mode", "auto"}},
		{"other", "", startBypass, []string{"claude"}},
		{"loose", startBypass, startAuto, []string{"claude", "--permission-mode", "auto"}},
	} {
		sr.set(c.setting)
		ss := r.session(t, c.name)
		ss.StartMode = c.recorded // as the record holds it
		r.starts[ss.ID] = resumeEntry{Harness: "claude", Argv: []string{"claude"}}
		if _, err := r.w.recreate(context.Background(), ss, api.AgentState{Harness: "claude"}, "", ""); err != nil {
			t.Fatalf("recreate %s: %v", c.name, err)
		}
		if got := sr.last(t); !slices.Equal(got, c.want) {
			t.Fatalf("recreate of %s (recorded %q) at bypass: %q, want %q", c.name, c.recorded, got, c.want)
		}
	}
}

// TestSwapStartMode: a group member's fresh session (the group swap, also a member's recreate)
// starts with the lower of the old member's recorded mode and the current setting: a recorded
// ask and a recorded auto stay so at bypass.
func TestSwapStartMode(t *testing.T) {
	t.Parallel()
	r := newRecreateRig(t)
	sr := newStartRig(t)
	sr.set(startBypass)
	r.w.create = func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
		// The start rig has no project, group or peers: keep only what decides the mode.
		id, _, err := sr.a.CreateSession(ctx, api.SessionCreateParams{Harness: p.Harness, Model: p.Model, Cwd: sr.a.home})
		return api.SessionCreated{Session: id}, err
	}
	for i, c := range []struct {
		recorded string
		want     []string
	}{
		{"", []string{"claude"}},
		{startAuto, []string{"claude", "--permission-mode", "auto"}},
	} {
		name := fmt.Sprintf("member%d", i)
		pane := fmt.Sprintf("00000000-0000-0000-0000-0000000000c%d", i)
		ss, err := r.st.CreateSession(structure.SessionSpec{Name: name, Pane: pane, Index: -1, NewPane: true,
			Group: &api.SessionGroup{ID: "g1", Role: "worker", Item: "i1"}, StartMode: c.recorded})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.w.recreate(context.Background(), ss, api.AgentState{Harness: "claude"}, "", ""); err != nil {
			t.Fatalf("swap of %s: %v", name, err)
		}
		if got := sr.last(t); !slices.Equal(got, c.want) {
			t.Fatalf("swap of a member recorded %q at bypass: %q, want %q", c.recorded, got, c.want)
		}
	}
}
