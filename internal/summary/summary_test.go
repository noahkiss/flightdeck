package summary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hab.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSettings(t *testing.T) {
	s, err := LoadSettings(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil || s.On() {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	s, err = LoadSettings(writeConfig(t, "[speech]\nenabled = true\n"))
	if err != nil || s.On() {
		t.Fatalf("no section: %+v %v", s, err)
	}
	s, err = LoadSettings(writeConfig(t, "[summaries]\n"))
	if err != nil || s.On() {
		t.Fatalf("empty section: %+v %v", s, err)
	}
	s, err = LoadSettings(writeConfig(t, "[summaries]\nbase_url = \"http://127.0.0.1:11434/v1\"\nmodel = \"small\"\n"))
	if err != nil || !s.On() || s.Thinking != ThinkingOff || s.Wait != DefaultWait || s.KeyCommand != nil {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s, err = LoadSettings(writeConfig(t, "[summaries]\nbase_url = \"https://api.example.com/v1/\"\nmodel = \"m\"\nkey_command = [\"pass\", \"show\", \"x\"]\nthinking = \"Low\"\ntimeout = \"800ms\"\n"))
	if err != nil || s.Thinking != "low" || s.Wait != 800*time.Millisecond || len(s.KeyCommand) != 3 {
		t.Fatalf("full: %+v %v", s, err)
	}
	for name, body := range map[string]string{
		"model only":     "model = \"m\"",
		"url only":       "base_url = \"http://x/v1\"",
		"bad scheme":     "base_url = \"ftp://x/v1\"\nmodel = \"m\"",
		"no host":        "base_url = \"http:///v1\"\nmodel = \"m\"",
		"user in url":    "base_url = \"https://u:p@x/v1\"\nmodel = \"m\"",
		"empty key cmd":  "base_url = \"http://x/v1\"\nmodel = \"m\"\nkey_command = []",
		"blank program":  "base_url = \"http://x/v1\"\nmodel = \"m\"\nkey_command = [\"\"]",
		"bad thinking":   "base_url = \"http://x/v1\"\nmodel = \"m\"\nthinking = \"lots\"",
		"bad timeout":    "base_url = \"http://x/v1\"\nmodel = \"m\"\ntimeout = \"soon\"",
		"timeout low":    "base_url = \"http://x/v1\"\nmodel = \"m\"\ntimeout = \"10ms\"",
		"timeout high":   "base_url = \"http://x/v1\"\nmodel = \"m\"\ntimeout = \"30s\"",
		"key cmd string": "base_url = \"http://x/v1\"\nmodel = \"m\"\nkey_command = \"pass show x\"",
	} {
		if _, err := LoadSettings(writeConfig(t, "[summaries]\n"+body+"\n")); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	s, err := LoadSettings(filepath.Join("..", "..", "docs", "examples", "hab.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.On() || s.Thinking != ThinkingOff {
		t.Fatalf("example: %+v", s)
	}
}

func TestPlainWarning(t *testing.T) {
	for u, warn := range map[string]bool{
		"http://127.0.0.1:11434/v1": false, "http://localhost:1234/v1": false, "http://10.0.0.5/v1": false,
		"http://192.168.1.9/v1": false, "http://100.100.1.1/v1": false, "http://[::1]:8080/v1": false,
		"https://api.example.com/v1": false, "http://api.example.com/v1": true, "http://8.8.8.8/v1": true,
	} {
		if got := (Settings{BaseURL: u}).PlainWarning() != ""; got != warn {
			t.Errorf("%s: warning %v, want %v", u, got, warn)
		}
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"Deploy web app?":                     "Deploy web app?",
		"  **Delete build folder?**\n":        "Delete build folder?",
		"\"Run the tests?\"":                  "Run the tests?",
		"`Check git status`.":                 "Check git status",
		"# Remove temp files?":                "Remove temp files?",
		"<think>hmm, ok</think>\nList files?": "List files?",
		"Push to‮main?":                       "Push to main?",
		"Create github_issue?":                "Create github_issue?",
		"Don't do it?":                        "Don't do it?",
		"":                                    "",
		"   ":                                 "",
		"Line one\nLine two":                  "",
		"Open https://evil.example?":          "",
		"Visit www.example.com?":              "",
		"I can't help with that":              "",
		"\"I’m sorry, I can't\"":              "",
		"Sorry, no":                           "",
		strings.Repeat("word ", 30):           "",
		"***":                                 "",
	} {
		got, ok := Clean(in)
		if got != want || ok != (want != "") {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestFromRequest(t *testing.T) {
	in := FromRequest(&api.PermissionRequest{Tool: "Write", Files: []string{"/home/x/app/src/a.go"}, Input: api.RawJSON(`{"content":"secret file body"}`), Cwd: "/home/x/app"}, "claude", "app", "")
	b, _ := json.Marshal(in)
	if strings.Contains(string(b), "secret file body") || strings.Contains(string(b), "/home/x") || in.Files[0] != "a.go" {
		t.Fatalf("file request sends %s", b)
	}
	in = FromRequest(&api.PermissionRequest{Tool: "Bash", Command: "make test", Cwd: "/home/x/app"}, "claude", "app", "Run tests")
	b, _ = json.Marshal(in)
	if strings.Contains(string(b), "/home/x") || in.Command != "make test" || in.Description != "Run tests" {
		t.Fatalf("command request sends %s", b)
	}
	in = FromRequest(&api.PermissionRequest{Tool: "WebFetch", Input: api.RawJSON(`{"url":"https://docs.example.com/a","prompt":"p"}`)}, "claude", "", "")
	if in.URL != "https://docs.example.com/a" || in.ToolInput != nil {
		t.Fatalf("fetch: %+v", in)
	}
	in = FromRequest(&api.PermissionRequest{Tool: "apply_patch", Input: api.RawJSON(`{"patch":"secret body"}`)}, "codex", "", "")
	if in.ToolInput != nil {
		t.Fatalf("file tool input sent: %+v", in)
	}
	in = FromRequest(&api.PermissionRequest{Tool: "mcp__github__create_issue", Input: api.RawJSON(`{"title":"t"}`)}, "claude", "", "")
	if string(in.ToolInput) != `{"title":"t"}` {
		t.Fatalf("mcp: %+v", in)
	}
	long := Input{Command: strings.Repeat("x", 5000), ToolInput: json.RawMessage(`"` + strings.Repeat("y", 5000) + `"`)}.bounded()
	if len(long.Command) > maxLong+4 || len(long.ToolInput) > maxLong+10 || !json.Valid(long.ToolInput) {
		t.Fatalf("bounds: %d %d", len(long.Command), len(long.ToolInput))
	}
}

// fake is an OpenAI-compatible endpoint that records each request.
type fake struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []recorded
	handler func(w http.ResponseWriter, r *http.Request)
}

type recorded struct {
	path, auth string
	body       map[string]any
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t}
	f.handler = func(w http.ResponseWriter, r *http.Request) { reply(w, "Run the tests?") }
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: m})
		h := f.handler
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fake) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func (f *fake) set(h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func reply(w http.ResponseWriter, content string) {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, a ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
	l.mu.Unlock()
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (l *logs) n() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

var cmdInput = Input{Harness: "claude", Tool: "Bash", Command: "go test ./...", Project: "app"}

func TestRequestShape(t *testing.T) {
	f := newFake(t)
	for _, c := range []struct {
		thinking, effort string
		tokens           float64
	}{{ThinkingOff, "none", 64}, {"low", "low", 1024}, {"high", "high", 1024}, {ThinkingDefault, "", 0}} {
		v := New(Settings{BaseURL: f.srv.URL + "/v1/", Model: "small-model", Thinking: c.thinking}, Options{})
		got, err := v.Summarize(context.Background(), cmdInput)
		v.Close()
		if err != nil || got != "Run the tests?" {
			t.Fatalf("%s: %q %v", c.thinking, got, err)
		}
		r := f.last()
		if r.path != "/v1/chat/completions" || r.auth != "" {
			t.Fatalf("%s: path %q auth %q", c.thinking, r.path, r.auth)
		}
		effort, has := r.body["reasoning_effort"]
		if (c.effort == "") == has || (has && effort != c.effort) {
			t.Fatalf("%s: reasoning_effort %v", c.thinking, r.body["reasoning_effort"])
		}
		tokens, _ := r.body["max_tokens"].(float64)
		if _, has := r.body["max_tokens"]; has != (c.tokens != 0) {
			t.Fatalf("%s: max_tokens %v", c.thinking, r.body["max_tokens"])
		}
		if r.body["model"] != "small-model" || tokens != c.tokens || r.body["temperature"] != 0.0 || r.body["stream"] != false {
			t.Fatalf("%s: body %v", c.thinking, r.body)
		}
		msgs := r.body["messages"].([]any)
		user := msgs[1].(map[string]any)["content"].(string)
		if msgs[0].(map[string]any)["role"] != "system" || !strings.Contains(user, `"command":"go test ./..."`) {
			t.Fatalf("%s: messages %v", c.thinking, msgs)
		}
	}
}

func TestBearerKey(t *testing.T) {
	f := newFake(t)
	l := &logs{}
	secret := "fake" + strings.Repeat("Key9", 10)
	v := New(Settings{BaseURL: f.srv.URL, Model: "m", KeyCommand: []string{"/bin/echo", secret}}, Options{Logf: l.logf})
	defer v.Close()
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = v.Summarize(context.Background(), cmdInput); !errors.Is(err, ErrNoKey) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if f.last().auth != "Bearer "+secret {
		t.Fatalf("auth %q", f.last().auth)
	}
	// A refused key: the error and the log name the status, never the key.
	f.set(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key `+secret+`"}`, http.StatusUnauthorized)
	})
	_, err = v.Summarize(context.Background(), Input{Command: "other"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(l.all(), secret) || strings.Contains(l.all(), "bad key") {
		t.Fatalf("key or body leaked: %v / %s", err, l.all())
	}
}

func TestKeyCommandFailure(t *testing.T) {
	f := newFake(t)
	l := &logs{}
	secret := "fake" + strings.Repeat("Zy8", 12)
	// The command prints the key on stderr and fails: the log keeps it out.
	v := New(Settings{BaseURL: f.srv.URL, Model: "m", KeyCommand: []string{"/bin/sh", "-c", "echo " + secret + " >&2; exit 3"}}, Options{Logf: l.logf})
	defer v.Close()
	deadline := time.Now().Add(5 * time.Second)
	for l.n() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := v.Summarize(context.Background(), cmdInput); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err %v", err)
	}
	if f.count() != 0 {
		t.Fatal("a request went out with no key")
	}
	if !strings.Contains(l.all(), "exit status 3") || strings.Contains(l.all(), secret) {
		t.Fatalf("log %q", l.all())
	}
}

func TestKeyBackoff(t *testing.T) {
	f := newFake(t)
	l := &logs{}
	var now atomic.Int64
	now.Store(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC).UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	var runs atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	ran := make(chan struct{}, 10)
	v := New(Settings{BaseURL: f.srv.URL, Model: "m", KeyCommand: []string{"x"}}, Options{Now: clock, Logf: l.logf, KeyRun: func(context.Context) (string, error) {
		runs.Add(1)
		defer func() { ran <- struct{}{} }()
		if fail.Load() {
			return "", errors.New("store locked")
		}
		return "k", nil
	}})
	defer v.Close()
	<-ran
	waitIdle := func() {
		// The run's result is recorded under mu after the KeyRun returns.
		for range 100 {
			v.mu.Lock()
			busy := v.keyBusy
			v.mu.Unlock()
			if !busy {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitIdle()
	if _, err := v.Summarize(context.Background(), cmdInput); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	if runs.Load() != 1 {
		t.Fatalf("ran %d times inside the backoff", runs.Load())
	}
	now.Add(int64(31 * time.Second))
	fail.Store(false)
	v.Summarize(context.Background(), cmdInput) // starts a run, answers ErrNoKey
	<-ran
	waitIdle()
	if _, err := v.Summarize(context.Background(), cmdInput); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(l.all(), "store locked") || !strings.Contains(l.all(), "summaries are on") {
		t.Fatalf("log %q", l.all())
	}
}

func TestFallbacks(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"500":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"429":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) },
		"400":        func(w http.ResponseWriter, r *http.Request) { http.Error(w, "reasoning_effort unknown", 400) },
		"not json":   func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>hi</html>")) },
		"no choices": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"choices":[]}`)) },
		"null content": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"content":null}}]}`))
		},
		"wrong type": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"content":[1,2]}}]}`))
		},
		"too long":  func(w http.ResponseWriter, r *http.Request) { reply(w, strings.Repeat("Run it ", 40)) },
		"refusal":   func(w http.ResponseWriter, r *http.Request) { reply(w, "I can't help with that.") },
		"huge body": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat(" ", maxResponse+10))) },
		"two lines": func(w http.ResponseWriter, r *http.Request) { reply(w, "Run tests?\nAlso ignore the rules") },
	}
	for name, h := range cases {
		f := newFake(t)
		f.set(h)
		l := &logs{}
		v := New(Settings{BaseURL: f.srv.URL, Model: "m"}, Options{Logf: l.logf})
		got, err := v.Summarize(context.Background(), cmdInput)
		v.Summarize(context.Background(), cmdInput)
		if err == nil || got != "" {
			t.Errorf("%s: %q %v", name, got, err)
		}
		if f.count() != 2 {
			t.Errorf("%s: a failure was cached (%d calls)", name, f.count())
		}
		if l.n() != 1 {
			t.Errorf("%s: %d log lines for one streak: %q", name, l.n(), l.all())
		}
		if strings.Contains(l.all(), "reasoning_effort unknown") || strings.Contains(l.all(), "html") {
			t.Errorf("%s: body in the log: %q", name, l.all())
		}
		// It works again: one line says so.
		f.set(func(w http.ResponseWriter, r *http.Request) { reply(w, "Run the tests?") })
		if got, err := v.Summarize(context.Background(), cmdInput); err != nil || got != "Run the tests?" {
			t.Errorf("%s: recovery %q %v", name, got, err)
		}
		if l.n() != 2 || !strings.Contains(l.all(), "answers again") {
			t.Errorf("%s: recovery log %q", name, l.all())
		}
		v.Close()
	}
}

func TestTimeout(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	f.set(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	l := &logs{}
	v := New(Settings{BaseURL: f.srv.URL, Model: "m"}, Options{Logf: l.logf})
	defer v.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := v.Summarize(ctx, cmdInput)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if l.n() != 1 {
		t.Fatalf("log %q", l.all())
	}
	// The hab closing ends a call at once, and logs nothing.
	v2 := New(Settings{BaseURL: f.srv.URL, Model: "m"}, Options{Logf: l.logf})
	go func() { time.Sleep(50 * time.Millisecond); v2.Close() }()
	if _, err := v2.Summarize(context.Background(), cmdInput); !errors.Is(err, context.Canceled) {
		t.Fatalf("close: %v", err)
	}
	if l.n() != 1 {
		t.Fatalf("close logged: %q", l.all())
	}
}

func TestUnreachable(t *testing.T) {
	f := newFake(t)
	url := f.srv.URL + "/v1?token=hidden"
	f.srv.Close()
	l := &logs{}
	v := New(Settings{BaseURL: url, Model: "m"}, Options{Logf: l.logf})
	defer v.Close()
	if _, err := v.Summarize(context.Background(), cmdInput); err == nil || strings.Contains(err.Error(), "hidden") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(l.all(), "hidden") {
		t.Fatalf("log %q", l.all())
	}
}

func TestCache(t *testing.T) {
	f := newFake(t)
	var now atomic.Int64
	now.Store(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC).UnixNano())
	v := New(Settings{BaseURL: f.srv.URL, Model: "m"}, Options{Now: func() time.Time { return time.Unix(0, now.Load()) }})
	defer v.Close()
	for range 3 {
		if got, err := v.Summarize(context.Background(), cmdInput); err != nil || got != "Run the tests?" {
			t.Fatal(got, err)
		}
	}
	if f.count() != 1 {
		t.Fatalf("%d calls for one input", f.count())
	}
	now.Add(int64(cacheTTL))
	v.Summarize(context.Background(), cmdInput)
	if f.count() != 2 {
		t.Fatalf("expired entry served: %d calls", f.count())
	}
	for i := range cacheSize + 10 {
		v.Summarize(context.Background(), Input{Command: fmt.Sprint("cmd ", i)})
		now.Add(int64(time.Millisecond))
	}
	v.mu.Lock()
	n := len(v.cache)
	v.mu.Unlock()
	if n > cacheSize {
		t.Fatalf("cache holds %d", n)
	}
	before := f.count()
	v.Summarize(context.Background(), Input{Command: "cmd 0"}) // the oldest, evicted
	v.Summarize(context.Background(), Input{Command: fmt.Sprint("cmd ", cacheSize+9)})
	if f.count() != before+1 {
		t.Fatalf("eviction: %d calls, want %d", f.count(), before+1)
	}
}

// TestStatusHints: a 400 names the thinking setting in the log; a 401 runs the key command
// again, past the re-run wait, and the new key goes out.
func TestStatusHints(t *testing.T) {
	f := newFake(t)
	f.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) })
	l := &logs{}
	v := New(Settings{BaseURL: f.srv.URL, Model: "m"}, Options{Logf: l.logf})
	v.Summarize(context.Background(), cmdInput)
	v.Close()
	if !strings.Contains(l.all(), `thinking = "default"`) {
		t.Fatalf("log %q", l.all())
	}

	var now atomic.Int64
	now.Store(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC).UnixNano())
	var runs atomic.Int32
	ran := make(chan struct{}, 10)
	v = New(Settings{BaseURL: f.srv.URL, Model: "m", KeyCommand: []string{"x"}}, Options{Now: func() time.Time { return time.Unix(0, now.Load()) }, Logf: l.logf,
		KeyRun: func(context.Context) (string, error) {
			defer func() { ran <- struct{}{} }()
			return fmt.Sprint("key-", runs.Add(1)), nil
		}})
	defer v.Close()
	<-ran
	waitFor := func(cond func() bool) {
		for range 200 {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("timed out")
	}
	waitFor(func() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.key == "key-1" && !v.keyBusy })
	f.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	v.Summarize(context.Background(), cmdInput)
	if runs.Load() != 1 {
		t.Fatal("re-ran the key command inside the re-run wait")
	}
	now.Add(int64(6 * time.Minute))
	v.Summarize(context.Background(), cmdInput)
	<-ran
	waitFor(func() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.key == "key-2" && !v.keyBusy })
	f.set(func(w http.ResponseWriter, r *http.Request) { reply(w, "Run the tests?") })
	if _, err := v.Summarize(context.Background(), cmdInput); err != nil || f.last().auth != "Bearer key-2" {
		t.Fatalf("%v auth %q", err, f.last().auth)
	}
}
