package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// serverFixture is a hand-written opencode server event sequence and the agent.state stream it
// must give (harness-adapters.md, Fixtures).
type serverFixture struct {
	Note           string `json:"note"`
	HarnessVersion string `json:"harness_version"`
	Events         []struct {
		Type    string          `json:"type"`
		T       int64           `json:"t"`
		Dir     string          `json:"dir"`
		Child   bool            `json:"child"`
		Running bool            `json:"running"`
		Data    json.RawMessage `json:"data"`
	} `json:"events"`
	Expect []expected `json:"expect"`
}

// TestOpencodeServerFixtures feeds each server event sequence to one pane with source server,
// as gff-hab's follower does, and asserts the exact agent.state stream.
func TestOpencodeServerFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/opencode-server/*.json")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var fx serverFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			if fx.HarnessVersion == "" {
				t.Fatal("fixture does not state the harness version its shape follows")
			}
			var events []structure.Event
			s := newTestStore(&events)
			for _, e := range fx.Events {
				var d struct {
					SessionID string `json:"sessionID"`
				}
				json.Unmarshal(e.Data, &d)
				u, err := NormaliseOpencodeServer(ServerEvent{Type: e.Type, Data: e.Data, Dir: e.Dir},
					OpencodeSession{Child: e.Child, Running: e.Running, Bound: s.OpencodeBound("pane-1", d.SessionID)})
				if err != nil {
					if !errors.Is(err, ErrUnknownEvent) {
						t.Fatalf("%s: %v", e.Type, err)
					}
					continue
				}
				s.Apply("pane-1", "opencode", api.StateSourceServer, e.T, u)
			}
			var got []expected
			for i, e := range events {
				st := e.Data.(api.AgentState)
				if e.Type != EventAgentState || e.Revision != uint64(i+1) || st.Source != api.StateSourceServer {
					t.Errorf("event %d envelope: %+v", i, e)
				}
				got = append(got, project(st))
			}
			if !reflect.DeepEqual(got, fx.Expect) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(fx.Expect, "", "  ")
				t.Fatalf("stream:\n%s\nwant:\n%s", g, w)
			}
		})
	}
}

// TestOpencodeServerRequest: a request carries the event's data as its input, and a large one is
// left out and flagged; an action that is neither shell nor a file tool keeps its resources
// only in the input.
func TestOpencodeServerRequest(t *testing.T) {
	data := json.RawMessage(`{"id":"per_9","sessionID":"ses_a","action":"webfetch","resources":["https://example.com"]}`)
	u, err := NormaliseOpencodeServer(ServerEvent{Type: "permission.asked", Data: data, Dir: "/work"}, OpencodeSession{})
	if err != nil {
		t.Fatal(err)
	}
	r := u.Request
	if r.ID != "per_9" || r.Tool != "webfetch" || r.Command != "" || r.Files != nil || string(r.Input) != string(data) || r.Cwd != "/work" {
		t.Fatalf("request %+v", r)
	}
	big := json.RawMessage(`{"id":"per_10","action":"write","resources":["/work/a"],"pad":"` + string(bigString(MaxInput)) + `"}`)
	u, err = NormaliseOpencodeServer(ServerEvent{Type: "permission.asked", Data: big}, OpencodeSession{})
	if err != nil || !u.Request.InputTruncated || u.Request.Input != nil || len(u.Request.Files) != 1 {
		t.Fatalf("large request %+v %v", u.Request, err)
	}
	if _, err := NormaliseOpencodeServer(ServerEvent{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"c"}`)}, OpencodeSession{Child: true}); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("a child session's turn: %v", err)
	}
	if _, err := NormaliseOpencodeServer(ServerEvent{Type: "permission.asked", Data: json.RawMessage(`not json`)}, OpencodeSession{}); err == nil || errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("unreadable data: %v", err)
	}
}

func bigString(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}
