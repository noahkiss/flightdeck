package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

type contextFixture struct {
	Cases []struct {
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
		Text    string          `json:"text"`
		Output  string          `json:"output"`
	} `json:"cases"`
}

// TestContextFixtures: for each harness's hand-written fixture, gff-hab hook prints exactly the
// fixture's output for the text the hab answers (logbook.md 11.3), and the hab's context handler
// is asked only at a context event, after the sink took the envelope.
func TestContextFixtures(t *testing.T) {
	for _, h := range []string{"claude", "codex", "opencode"} {
		t.Run(h, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "context", h+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var fx contextFixture
			if err := json.Unmarshal(b, &fx); err != nil {
				t.Fatal(err)
			}
			reg := newTestPanes()
			var mu sync.Mutex
			sunk := map[string]bool{}
			var asked []string
			answer := ""
			sock := filepath.Join(shortDir(t), "hook.sock")
			l, err := control.Listen(sock)
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer(reg, func(pane string, env harness.Envelope) string {
				mu.Lock()
				sunk[env.Event] = true
				mu.Unlock()
				return ""
			}, t.Logf)
			s.SetContext(func(ctx context.Context, pane string, env harness.Envelope) string {
				mu.Lock()
				defer mu.Unlock()
				if !sunk[env.Event] || pane != "pane-1" {
					t.Errorf("context for %s before the sink, or for pane %q", env.Event, pane)
				}
				asked = append(asked, env.Event)
				return answer
			})
			go s.Serve(l)
			t.Cleanup(func() { s.Close() })
			addr := Addr(sock, reg.mint("pane-1"))
			for i, c := range fx.Cases {
				mu.Lock()
				answer, asked = c.Text, nil
				mu.Unlock()
				var out bytes.Buffer
				err := Run(context.Background(), Invocation{Harness: h, Event: c.Event, Stdin: bytes.NewReader(c.Payload),
					Getenv: env(map[string]string{Env: addr}), Stdout: &out,
					FileBrief: func(context.Context, string) (string, error) {
						t.Error("the file read while gff-hab answers")
						return "", nil
					}})
				if err != nil {
					t.Fatalf("case %d (%s): %v", i, c.Event, err)
				}
				if out.String() != c.Output {
					t.Errorf("case %d (%s): printed\n%q\nwant\n%q", i, c.Event, out.String(), c.Output)
				}
				_, isContext := ContextEvent(h, c.Event)
				mu.Lock()
				if got := len(asked); (got == 1) != isContext || got > 1 {
					t.Errorf("case %d (%s): asked %v", i, c.Event, asked)
				}
				mu.Unlock()
				if c.Output != "" && h != "opencode" && !json.Valid([]byte(c.Output)) {
					t.Errorf("case %d: output is not JSON", i)
				}
			}
		})
	}
}

// TestContextHabDown: with gff-hab down, the brief's event builds the brief from the file for
// the payload's folder and prints it within the budget; the reminder and the closeout print
// nothing and never read the file; a FileBrief error prints nothing.
func TestContextHabDown(t *testing.T) {
	addr := Addr(filepath.Join(shortDir(t), "gone.sock"), strings.Repeat("ab", 16))
	var dirs []string
	fb := func(_ context.Context, cwd string) (string, error) {
		dirs = append(dirs, cwd)
		return "brief from the file\n", nil
	}
	run := func(h, ev, payload string, fb func(context.Context, string) (string, error)) string {
		t.Helper()
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), Budget)
		defer cancel()
		start := time.Now()
		Run(ctx, Invocation{Harness: h, Event: ev, Stdin: strings.NewReader(payload), Getenv: env(map[string]string{Env: addr}),
			Stdout: &out, FileBrief: fb})
		if d := time.Since(start); d > Budget {
			t.Errorf("%s %s took %v", h, ev, d)
		}
		return out.String()
	}
	if got := run("claude", "SessionStart", `{"session_id":"s","cwd":"/work/app"}`, fb); got != string(ContextOutput("claude", "SessionStart", "brief from the file\n")) {
		t.Fatalf("claude brief from the file: %q", got)
	}
	// opencode's brief goes through its server (logbook.md 11.4): its hook never reads the file.
	if got := run("opencode", "session.created", `{"type":"session.created","properties":{"info":{"id":"ses_1"}}}`, fb); got != "" {
		t.Fatalf("opencode brief from the file: %q", got)
	}
	if len(dirs) != 1 || dirs[0] != "/work/app" {
		t.Fatalf("folders %q", dirs)
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop"} {
		if got := run("codex", ev, `{"session_id":"s","cwd":"/work/app"}`, fb); got != "" {
			t.Fatalf("%s with gff-hab down printed %q", ev, got)
		}
	}
	if len(dirs) != 1 {
		t.Fatalf("the reminder or closeout read the file: %q", dirs)
	}
	if got := run("claude", "SessionStart", `{}`, func(context.Context, string) (string, error) { return "", errors.New("no file") }); got != "" {
		t.Fatalf("a failed file brief printed %q", got)
	}
}

// TestContextEvents names which events carry which habit.
func TestContextEvents(t *testing.T) {
	for _, c := range []struct{ h, ev, want string }{
		{"claude", "SessionStart", "brief"}, {"claude", "UserPromptSubmit", "reminder"}, {"claude", "Stop", "closeout"},
		{"codex", "SessionStart", "brief"}, {"codex", "UserPromptSubmit", "reminder"}, {"codex", "Stop", "closeout"},
		{"opencode", "session.created", ""}, {"opencode", "session.idle", ""}, {"claude", "PreToolUse", ""},
		{"claude", "SessionEnd", ""}, {"pi", "SessionStart", ""},
	} {
		got, ok := ContextEvent(c.h, c.ev)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s %s: %q %v", c.h, c.ev, got, ok)
		}
	}
	// The opencode plugin only posts events: gff-hab adds the brief through the server.
	js, _ := Config("opencode", "")
	if strings.Contains(js, "client.session") || strings.Contains(js, "noReply") || !strings.Contains(js, `stdout: "ignore"`) {
		t.Errorf("the opencode plugin still adds the brief:\n%s", js)
	}
}
