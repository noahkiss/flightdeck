package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
	habusage "github.com/noahkiss/flightdeck/internal/usage"
)

// usageScript runs in a pane: Claude Code's status line through gff-hab with a wrapped command,
// a status-line input with no rate limits, an opencode hook (no usage source), then a Codex hook
// whose session's rollout the test wrote. Each step leaves a file.
const usageScript = `
while [ ! -e "$DIR/go" ]; do sleep 0.02; done
"$HAB" statusline claude -- 'cat > "$DIR/wrapped"; echo LINE; exit 3' < "$DIR/status.json" > "$DIR/out.tmp"
echo $? > "$DIR/code" && mv "$DIR/out.tmp" "$DIR/out"
"$HAB" statusline claude < "$DIR/none.json" > "$DIR/none.out"
"$HAB" hook opencode session.idle < "$DIR/opencode.json"
"$HAB" hook codex Stop < "$DIR/codex.json"
touch "$DIR/done"
exec sleep 600
`

// TestUsageAdapters is the usage path end to end with the real holder and gff-hab: `gff-hab
// statusline` posts Claude Code's status-line input and still prints the wrapped command's
// output with its exit code; a Codex hook makes gff-hab read the end of that session's rollout.
// A host-scoped and a session-scoped client both get usage.updated, with no revision, and
// usage.list answers each account: a status-line input with no rate limits and an opencode hook
// add none. The hab says it has the usage capability.
func TestUsageAdapters(t *testing.T) {
	e := newHabEnv(t, 0)
	codexHome := filepath.Join(e.work, "codex")
	rollout := filepath.Join(codexHome, "sessions", "2026", "01", "01", "rollout-2026-01-01T10-00-00-019a-codex-usage.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../internal/usage/testdata/codex-0.166.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// A long first part: the read takes only the end, cutting a line.
	pad := strings.Repeat(`{"timestamp":"2026-01-01T09:00:00.000Z","type":"response_item","payload":{"type":"message","text":"`+strings.Repeat("x", 900)+`"}}`+"\n", 400)
	writeFile(t, rollout, pad+string(fixture))
	e.env = append(e.env, "CODEX_HOME="+codexHome)
	e.restart(false)

	status, err := os.ReadFile("../../internal/usage/testdata/claude-2.1.316.json")
	if err != nil {
		t.Fatal(err)
	}
	none, err := os.ReadFile("../../internal/usage/testdata/claude-none.json")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.work, "status.json"), string(status))
	writeFile(t, filepath.Join(e.work, "none.json"), string(none))
	writeFile(t, filepath.Join(e.work, "opencode.json"), `{"type":"session.idle","properties":{"sessionID":"ses_1"}}`)
	writeFile(t, filepath.Join(e.work, "codex.json"), `{"session_id":"019a-codex-usage","turn_id":"turn-1","cwd":"/work/app","hook_event_name":"Stop","model":"model-x"}`)

	c := e.control()
	var w api.Welcome
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	ch, err := control.Dial(e.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if w, err = ch.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(w.Capabilities, ","), "usage") {
		t.Fatalf("capabilities %v lack usage", w.Capabilities)
	}

	var ss api.SessionCreated
	call(t, c, "session.create", api.SessionCreateParams{
		Name: "usage", Argv: []string{"/bin/sh", "-c", usageScript}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "HAB=" + bin, noRaceExitSleep}}, &ss)

	// A host client and a client scoped to this one session subscribe; then the script goes on.
	type got struct {
		mu     sync.Mutex
		events map[string][]api.UsageAccount
	}
	collect := func(cl *control.Client, scope api.Scope) *got {
		g := &got{events: map[string][]api.UsageAccount{}}
		call(t, cl, "events.subscribe", api.SubscribeParams{Scope: scope}, nil)
		go func() {
			for n := range cl.Notifications() {
				ev, err := n.Event()
				if n.Method != "event" || err != nil || ev.Type != api.EventUsageUpdated {
					continue
				}
				if ev.Revision != nil {
					t.Errorf("usage.updated carries a revision")
				}
				var a api.UsageAccount
				if err := json.Unmarshal(ev.Data, &a); err != nil || a.Key != ev.ID {
					t.Errorf("usage.updated %s: %s (%v)", ev.ID, ev.Data, err)
				}
				g.mu.Lock()
				g.events[a.Harness] = append(g.events[a.Harness], a)
				g.mu.Unlock()
			}
		}()
		return g
	}
	host := collect(ch, api.Scope{Kind: api.ScopeKindHost})
	scoped := collect(c, api.Scope{Kind: api.ScopeKindSessions, IDs: []string{ss.Session}})
	writeFile(t, filepath.Join(e.work, "go"), "")

	waitUntil(t, "the script's steps", func() bool { _, err := os.Stat(filepath.Join(e.work, "done")); return err == nil })
	if out, _ := os.ReadFile(filepath.Join(e.work, "out")); string(out) != "LINE\n" {
		t.Errorf("status line output %q", out)
	}
	if code, _ := os.ReadFile(filepath.Join(e.work, "code")); strings.TrimSpace(string(code)) != "3" {
		t.Errorf("status line exit code %q, want the wrapped command's 3", code)
	}
	if wrapped, _ := os.ReadFile(filepath.Join(e.work, "wrapped")); string(wrapped) != string(status) {
		t.Errorf("the wrapped command got %q", wrapped)
	}
	if out, _ := os.ReadFile(filepath.Join(e.work, "none.out")); len(out) != 0 {
		t.Errorf("a status line with no command printed %q", out)
	}

	// The Codex read waits for the debounce after the hook.
	var list api.UsageReadings
	defer func() {
		if t.Failed() {
			t.Logf("usage.list: %+v; agents %+v", list, agentStates(t, e.sock))
		}
	}()
	waitUntil(t, "both accounts in usage.list", func() bool {
		list = api.UsageReadings{}
		call(t, c, "usage.list", api.UsageListParams{}, &list)
		return len(list.Accounts) == 2
	})
	byHarness := map[string]api.UsageAccount{}
	for _, a := range list.Accounts {
		byHarness[a.Harness] = a
	}
	cl, cx := byHarness["claude"], byHarness["codex"]
	if cl.Key != habusage.Key(w.Host.ID, "claude", "/srv/agent/.claude") || len(cl.Windows) != 2 ||
		cl.Windows[0] != (api.UsageWindow{Name: "5h", UsedPct: 42, ResetsAt: "2026-01-01T12:00:00Z", ReadAt: cl.Windows[0].ReadAt,
			LengthS: 18000, StartsAt: "2026-01-01T07:00:00Z"}) {
		t.Errorf("claude account %+v", cl)
	}
	if at, err := time.Parse(time.RFC3339, cl.Windows[0].ReadAt); err != nil || time.Since(at) > time.Minute {
		t.Errorf("claude read_at %q is not the status line's time", cl.Windows[0].ReadAt)
	}
	if cx.Label != "Plus" || cx.Key != habusage.Key(w.Host.ID, "codex", codexHome) || len(cx.Windows) != 3 ||
		cx.Windows[0] != (api.UsageWindow{Name: "5h", UsedPct: 12, ResetsAt: "2026-01-01T12:00:00Z", ReadAt: "2026-01-01T10:00:20Z",
			LengthS: 18000, StartsAt: "2026-01-01T07:00:00Z"}) {
		t.Errorf("codex account %+v", cx)
	}
	for name, g := range map[string]*got{"host": host, "session-scoped": scoped} {
		waitUntil(t, name+" client's usage events", func() bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.events["claude"]) > 0 && len(g.events["codex"]) > 0
		})
		g.mu.Lock()
		if len(g.events) != 2 || len(g.events["claude"]) != 1 {
			t.Errorf("%s client: %+v", name, g.events)
		}
		g.mu.Unlock()
	}
	if !structure.Ephemeral(api.EventUsageUpdated) {
		t.Error("usage.updated is not ephemeral")
	}
}

// TestStatuslineOutsidePane: outside any pane gff-hab owns, or with gff-hab stopped, `gff-hab
// statusline` still prints the wrapped command's output at once; with no command it prints
// nothing and exits 0; -print gives the setting.
func TestStatuslineOutsidePane(t *testing.T) {
	dir := shortDir(t)
	base := []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep}
	run := func(env []string, args ...string) (string, int, time.Duration) {
		t.Helper()
		out, d, code := runCmd(t, env, `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1767268800}}}`, append([]string{"statusline"}, args...)...)
		return out, code, d
	}
	run(base, "claude") // the first exec of a fresh binary can be slow; do not time it
	for name, env := range map[string][]string{
		"outside any pane": base,
		"gff-hab stopped":  append(base, hook.Env+"="+hook.Addr(filepath.Join(dir, "hook.sock"), strings.Repeat("ab", 16))),
	} {
		out, code, d := run(env, "claude", "--", "head -c 5; echo")
		if out != "{\"rat\n" || code != 0 || d > scaleTimeout(500*time.Millisecond) {
			t.Errorf("%s: %q, exit %d after %v", name, out, code, d)
		}
		if out, code, _ := run(env, "claude"); out != "" || code != 0 {
			t.Errorf("%s with no command: %q, exit %d", name, out, code)
		}
	}
	out, code, _ := run(base, "-print", "claude", "-wrap", "~/bin/my status")
	var cfg struct {
		StatusLine struct{ Type, Command string } `json:"statusLine"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &cfg) != nil || cfg.StatusLine.Type != "command" ||
		cfg.StatusLine.Command != `gff-hab statusline claude -- '~/bin/my status'` {
		t.Errorf("-print: %q, exit %d", out, code)
	}
	if _, code, _ := run(base, "-print", "opencode"); code != 2 {
		t.Errorf("-print opencode: exit %d, want 2", code)
	}
}

// runCmd runs gff-hab with args, payload on stdin and the given environment.
func runCmd(t *testing.T, env []string, payload string, args ...string) (stdout string, d time.Duration, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	err := cmd.Run()
	d = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), d, code
}
