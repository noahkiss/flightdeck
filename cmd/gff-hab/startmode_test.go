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
// mode its record holds, not the current setting: auto stays auto at bypass, and a session
// started at ask stays at ask.
func TestResumeStartMode(t *testing.T) {
	t.Parallel()
	const p1, p2 = "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000a2"
	r := newStartRig(t, structure.HolderPane{ID: p1, Exit: &api.ExitStatus{}}, structure.HolderPane{ID: p2, Exit: &api.ExitStatus{}})
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
}

// TestRecreateStartMode: recreate starts the fresh session with the old one's recorded mode, a
// recorded ask included, never the current setting; and its argv carries no flag the hab added,
// so the create adds one flag, not two.
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
		name, recorded string
		want           []string
	}{
		{"mate", startAuto, []string{"claude", "--permission-mode", "auto"}},
		{"other", "", []string{"claude"}},
	} {
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
