package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type structureEvent = structure.Event

// agentsOnly is a control source with no structure but the agent states.
type agentsOnly struct{ s *harness.Store }

func (a agentsOnly) Snapshot(host api.HostInfo) api.Structure {
	return api.NonNil(api.Structure{Host: host, Agents: a.s.Snapshot()})
}

// shortDir returns a private directory with a short path (macOS socket paths max 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffk")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o700)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestAddr(t *testing.T) {
	a := Addr("/run/u/gff/hook.sock", "0123456789abcdef0123456789abcdef")
	sock, tok, err := ParseAddr(a)
	if err != nil || sock != "/run/u/gff/hook.sock" || tok != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("round trip: %q %q %v", sock, tok, err)
	}
	for _, bad := range []string{
		"", "http://x/hooks/0123456789abcdef", "http+unix://relative.sock/hooks/0123456789abcdef",
		"http+unix:///s.sock/hooks/", "http+unix:///s.sock/hooks/short", "http+unix:///s.sock/hooks/NOTHEX0123456789",
		"http+unix:///s.sock",
	} {
		if _, _, err := ParseAddr(bad); err == nil {
			t.Errorf("ParseAddr(%q) accepted", bad)
		}
	}
}

type recorder struct {
	mu   sync.Mutex
	got  []harness.Envelope
	pane []string
}

func (r *recorder) sink(pane string, env harness.Envelope) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, env)
	r.pane = append(r.pane, pane)
	return ""
}

func startHooks(t *testing.T, reg Resolver, sink Sink) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "hook.sock")
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, sink, t.Logf)
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	return sock
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestRunPostsEnvelope(t *testing.T) {
	reg := newTestPanes()
	rec := &recorder{}
	sock := startHooks(t, reg, rec.sink)
	tok := reg.mint("pane-7")
	ctx := context.Background()
	clock := func() time.Time { return time.UnixMilli(1234) }

	payload := `{"session_id":"s1","hook_event_name":"Stop"}`
	err := Run(ctx, Invocation{Harness: "claude", Stdin: strings.NewReader(payload + "\n"), Now: clock,
		Getenv: env(map[string]string{Env: Addr(sock, tok), "CLAUDE_CODE_ENTRYPOINT": "cli"})})
	if err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	if len(rec.got) != 1 || rec.pane[0] != "pane-7" {
		t.Fatalf("got %+v for %v", rec.got, rec.pane)
	}
	e := rec.got[0]
	rec.mu.Unlock()
	if e.Harness != "claude" || e.Event != "Stop" || e.T != 1234 || e.N != 1 || string(e.Payload) != payload {
		t.Fatalf("envelope %+v", e)
	}

	// The event named on the command line wins; opencode's event type is read when none is.
	Run(ctx, Invocation{Harness: "codex", Event: "SessionStart", Stdin: strings.NewReader(`{"session_id":"x"}`), Getenv: env(map[string]string{Env: Addr(sock, tok)})})
	Run(ctx, Invocation{Harness: "opencode", T: 777, Stdin: strings.NewReader(`{"type":"session.idle","properties":{}}`), Getenv: env(map[string]string{Env: Addr(sock, tok)})})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 3 || rec.got[1].Event != "SessionStart" || rec.got[2].Event != "session.idle" || rec.got[2].T != 777 {
		t.Fatalf("events %+v", rec.got)
	}
}

func TestRunSendsNothing(t *testing.T) {
	reg := newTestPanes()
	rec := &recorder{}
	sock := startHooks(t, reg, rec.sink)
	tok := reg.mint("p")
	ctx := context.Background()
	in := func() *strings.Reader { return strings.NewReader(`{"hook_event_name":"Stop"}`) }

	if err := Run(ctx, Invocation{Harness: "claude", Stdin: in(), Getenv: env(nil)}); !errors.Is(err, ErrNoHab) {
		t.Errorf("no GFF_HAB: %v", err)
	}
	if err := Run(ctx, Invocation{Harness: "claude", Stdin: in(), Getenv: env(map[string]string{Env: Addr(sock, tok), "CLAUDE_CODE_ENTRYPOINT": "sdk-cli"})}); err == nil {
		t.Error("nested claude -p was sent")
	}
	if err := Run(ctx, Invocation{Harness: "claude", Stdin: strings.NewReader("not json"), Getenv: env(map[string]string{Env: Addr(sock, tok)})}); err == nil {
		t.Error("non-JSON payload was sent")
	}
	if err := Run(ctx, Invocation{Harness: "claude", Stdin: in(), Getenv: env(map[string]string{Env: Addr(sock, strings.Repeat("ab", 16))})}); err == nil {
		t.Error("unknown token reported success")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 0 {
		t.Fatalf("sink got %+v", rec.got)
	}
}

// scaleTimeout multiplies d by $GFF_TEST_TIMEOUT_SCALE, a positive number (docs/testing.md).
// Unset, it returns d.
func scaleTimeout(d time.Duration) time.Duration {
	v := os.Getenv("GFF_TEST_TIMEOUT_SCALE")
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		panic(fmt.Sprintf("GFF_TEST_TIMEOUT_SCALE=%q: want a positive number", v))
	}
	return time.Duration(float64(d) * f)
}

// TestDeadHabIsFast: with nothing listening, a post fails at once.
func TestDeadHabIsFast(t *testing.T) {
	sock := filepath.Join(shortDir(t), "hook.sock")
	start := time.Now()
	err := Run(context.Background(), Invocation{Harness: "claude", Stdin: strings.NewReader(`{"hook_event_name":"Stop"}`),
		Getenv: env(map[string]string{Env: Addr(sock, strings.Repeat("ab", 16))})})
	if err == nil {
		t.Fatal("post to a dead hab succeeded")
	}
	if d := time.Since(start); d > scaleTimeout(200*time.Millisecond) {
		t.Fatalf("post to a dead hab took %v", d)
	}
}

func TestServerRejects(t *testing.T) {
	reg := newTestPanes()
	rec := &recorder{}
	sock := startHooks(t, reg, rec.sink)
	tok := reg.mint("p")
	addr := Addr(sock, tok)
	ctx := context.Background()
	for name, e := range map[string]harness.Envelope{
		"no harness": {Event: "Stop", Payload: json.RawMessage(`{}`)},
		"no event":   {Harness: "claude", Payload: json.RawMessage(`{}`)},
	} {
		if err := Post(ctx, addr, e); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A missing client clock is stamped by the server.
	if err := Post(ctx, addr, harness.Envelope{Harness: "claude", Event: "Stop", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 1 || rec.got[0].T <= 0 {
		t.Fatalf("got %+v", rec.got)
	}
}

func TestConfig(t *testing.T) {
	for _, h := range []struct {
		name   string
		events []string
	}{{"claude", ClaudeEvents}, {"codex", CodexEvents}} {
		out, err := Config(h.name, "")
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Hooks map[string][]struct {
				Matcher *string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("%s config is not JSON: %v\n%s", h.name, err, out)
		}
		if len(cfg.Hooks) != len(h.events) {
			t.Fatalf("%s: %d events, want %d", h.name, len(cfg.Hooks), len(h.events))
		}
		for _, ev := range h.events {
			g := cfg.Hooks[ev]
			want := "gff-hab hook " + h.name + " " + ev
			if h.name == "claude" && ev == "PermissionRequest" {
				want = "gff-hab hook -wait claude PermissionRequest"
			}
			if len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0].Type != "command" ||
				g[0].Hooks[0].Command != want || g[0].Hooks[0].Timeout <= 0 {
				t.Errorf("%s %s: %+v", h.name, ev, g)
			}
		}
	}
	out, _ := Config("claude", "/opt/my tools/gff-hab")
	if !strings.Contains(out, `"'/opt/my tools/gff-hab' hook claude Stop"`) {
		t.Errorf("command with a space is not quoted:\n%s", out)
	}
	js, err := Config("opencode", "")
	if err != nil || !strings.Contains(js, `"hook", "-t", String(Date.now()), "opencode", event.type`) || !strings.Contains(js, "permission.asked") {
		t.Errorf("opencode plugin: %v\n%s", err, js)
	}
	if _, err := Config("nonesuch", ""); err == nil {
		t.Error("config for an unknown harness")
	}
}

// TestHookToEventStream: a hook posted to the endpoint becomes an agent.state event on a
// subscribed control connection, with the connection's seq, and the snapshot holds it.
func TestHookToEventStream(t *testing.T) {
	var srv *control.Server
	var mu sync.Mutex
	agents := harness.NewStore(harness.Config{Publish: func(e structureEvent) {
		mu.Lock()
		s := srv
		mu.Unlock()
		if s != nil {
			s.Publish(e)
		}
	}})
	mu.Lock()
	srv = control.New(control.Config{Build: "test", Host: api.HostInfo{ID: "h"}, Source: agentsOnly{agents}})
	mu.Unlock()
	dir := shortDir(t)
	cl, err := control.Listen(filepath.Join(dir, "hab.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(cl)
	t.Cleanup(srv.Close)

	reg := newTestPanes()
	sock := startHooks(t, reg, func(p string, e harness.Envelope) string { _, opened := agents.Hook(p, e); return opened })
	tok := reg.mint("pane-1")

	c, err := control.Dial(filepath.Join(dir, "hab.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "t", Version: "1", Kind: api.ClientKindCli}, "", ""); err != nil {
		t.Fatal(err)
	}
	snap, err := api.Client{Caller: c}.EventsSubscribe(ctx, api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Agents) != 0 {
		t.Fatalf("snapshot agents %+v", snap.Agents)
	}

	getenv := env(map[string]string{Env: Addr(sock, tok)})
	for _, p := range []string{
		`{"session_id":"s1","hook_event_name":"SessionStart","source":"startup"}`,
		`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt_id":"p1"}`,
	} {
		if err := Run(ctx, Invocation{Harness: "claude", Stdin: strings.NewReader(p), Getenv: getenv}); err != nil {
			t.Fatal(err)
		}
	}
	var states []api.AgentState
	for len(states) < 2 {
		select {
		case n := <-c.Notifications():
			if n.Method != "event" {
				continue
			}
			var e api.Event
			if err := json.Unmarshal(n.Params, &e); err != nil {
				t.Fatal(err)
			}
			if e.Type != "agent.state" || e.ID != "pane-1" || e.Seq != snap.Seq.Seq+uint64(len(states))+1 || e.Revision == nil || *e.Revision != uint64(len(states))+1 {
				t.Fatalf("event %+v", e)
			}
			var st api.AgentState
			if err := json.Unmarshal(e.Data, &st); err != nil {
				t.Fatal(err)
			}
			states = append(states, st)
		case <-ctx.Done():
			t.Fatal("no agent.state event")
		}
	}
	if states[0].State != api.AgentStatusIdle || states[1].State != api.AgentStatusWorking || states[1].Turn != "p1" || states[1].Session != "s1" {
		t.Fatalf("states %+v", states)
	}
	var res api.StructureResult
	if err := c.Call(ctx, "structure.get", struct{}{}, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Agents) != 1 || res.Agents[0].State != api.AgentStatusWorking {
		t.Fatalf("structure agents %+v", res.Agents)
	}
}
