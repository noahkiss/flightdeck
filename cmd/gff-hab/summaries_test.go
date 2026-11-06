package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestPushSummaries: with a [summaries] section, gff-hab asks the endpoint (a fake, never a
// real provider) for an approval's title with the key the key command printed, and the key
// is in no log line. A request in a folder the read scope denies, and a hab with no section,
// send nothing to the endpoint and keep today's title.
func TestPushSummaries(t *testing.T) {
	secret := "fake" + strings.Repeat("key", 12)
	var mu sync.Mutex
	var bodies []string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, auths = append(bodies, string(b)), append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "Run the test suite?"}}}})
		w.Write(out)
	}))
	defer srv.Close()
	calls := func() int { mu.Lock(); defer mu.Unlock(); return len(bodies) }

	for _, c := range []struct {
		name, section string
		cwd           string
		want          string
		calls         int
	}{
		{"on", fmt.Sprintf("[summaries]\nbase_url = %q\nmodel = \"m\"\nkey_command = [\"/bin/echo\", %q]\n", srv.URL+"/v1", secret), "/work/app", "Run the test suite?", 1},
		{"denied", fmt.Sprintf("[summaries]\nbase_url = %q\nmodel = \"m\"\n", srv.URL+"/v1"), "/work/secret", "Run make test?", 0},
		{"off", "[speech]\nenabled = false\n", "/work/app", "Run make test?", 0},
		{"no folder", fmt.Sprintf("[summaries]\nbase_url = %q\nmodel = \"m\"\n", srv.URL+"/v1"), "", "Run make test?", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := calls()
			state := t.TempDir()
			writePushConfig(t, state)
			cfg := filepath.Join(state, "hab.toml")
			if err := os.WriteFile(cfg, []byte(c.section), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := structure.Open(structure.Config{Dir: filepath.Join(state, "structure"), Holders: structure.NewFakeHolder()})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			sent := &sentLog{}
			logs := &lockedLog{}
			var pushes *pushService
			agents := harness.NewStore(harness.Config{Publish: func(e structure.Event) {
				if s, ok := e.Data.(api.AgentState); ok && pushes != nil {
					pushes.notifier.State(s)
				}
			}})
			pushes = newPushService(pushSetup{
				state: state, hab: "laptop", getenv: func(string) string { return "" }, logf: logs.logf, sender: sent,
				connected: func(string) bool { return false }, label: func(string) string { return "" }, publish: func(structure.Event) {},
				denied:    func(f string) bool { return strings.HasPrefix(f, "/work/secret") },
				habConfig: cfg,
			}, st, agents)
			defer pushes.Close()
			if err := pushes.Register("c-phone", api.PushRegisterParams{DeviceID: "dev1", Token: strings.Repeat("ab", 32), Environment: "development", Kinds: []api.PushKind{"permission"}}); err != nil {
				t.Fatal(err)
			}
			if c.name == "on" {
				// The key command runs in the background at start.
				waitUntil(t, "the summaries key", func() bool { return strings.Contains(logs.String(), "summaries are on") })
			}
			agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
			agents.Hook("p", claudeEnv("PermissionRequest", fmt.Sprintf(`{"session_id":"a","hook_event_name":"PermissionRequest","cwd":%q,"tool_name":"Bash","tool_input":{"command":"make test"}}`, c.cwd), 2))
			start := time.Now()
			pushes.notifier.Sync()
			if time.Since(start) > 3*time.Second {
				t.Fatalf("the banner waited %v", time.Since(start))
			}
			al := sent.alerts()
			if len(al) != 1 || title(al[0]) != c.want {
				t.Fatalf("banner: %v", al)
			}
			if got := calls() - before; got != c.calls {
				t.Fatalf("%d calls to the endpoint, want %d", got, c.calls)
			}
			if c.calls > 0 {
				mu.Lock()
				body, auth := bodies[len(bodies)-1], auths[len(auths)-1]
				mu.Unlock()
				if auth != "Bearer "+secret || !strings.Contains(body, "make test") || strings.Contains(body, "/work/app") {
					t.Fatalf("request auth %q body %s", auth, body)
				}
			}
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("the key is in the log: %s", logs.String())
			}
		})
	}
}
