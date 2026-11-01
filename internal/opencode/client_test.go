package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is an opencode server stand-in: it checks the password and records each request.
type fake struct {
	mu   sync.Mutex
	reqs []string // "METHOD path?query body"
	srv  *httptest.Server
}

func newFake(t *testing.T, h http.HandlerFunc) (*fake, *Client) {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != User || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"_tag":"UnauthorizedError","message":"Authentication required"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(b)))
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	c, err := New(f.srv.URL, "pw")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func TestNewRefusesOtherHosts(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1:1", "http://example.com:80", "http://10.1.2.3:4", "http://127.0.0.1:5/api", "not a url", "http://127.0.0.1:5?x=1"} {
		if _, err := New(u, "pw"); err == nil {
			t.Errorf("New(%q) accepted", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:4096", "http://localhost:4096/", "http://[::1]:4096"} {
		if _, err := New(u, "pw"); err != nil {
			t.Errorf("New(%q): %v", u, err)
		}
	}
}

func TestCalls(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/info":
			io.WriteString(w, `{"version":"2.2.5","pid":1}`)
		case r.URL.Path == "/api/session/active":
			io.WriteString(w, `{"data":{"ses_a":{"type":"running"}}}`)
		case r.URL.Path == "/api/session/ses_c":
			io.WriteString(w, `{"data":{"id":"ses_c","parentID":"ses_a","projectID":"p"}}`)
		case r.URL.Path == "/api/permission/request":
			io.WriteString(w, `{"data":[{"id":"per_1","sessionID":"ses_a","action":"shell","resources":["ls"]}]}`)
		case r.URL.Path == "/api/session/ses_a/message":
			io.WriteString(w, `{"data":[{"id":"msg_2","type":"user","text":"hi"}],"cursor":{"previous":"p","next":"n1"}}`)
		case r.URL.Path == "/api/session/ses_a/prompt":
			io.WriteString(w, `{"data":{"id":"msg_3","type":"user"}}`)
		case r.URL.Path == "/api/session/ses_a/permission/per_1/reply":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/session/ses_a/permission/per_2/reply":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"_tag":"PermissionNotFoundError","message":"Permission request not found: per_2"}`)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	if info, err := c.Info(ctx); err != nil || info.Version != "2.2.5" || info.PID != 1 {
		t.Fatalf("info %+v %v", info, err)
	}
	if ids, err := c.Active(ctx); err != nil || len(ids) != 1 || ids[0] != "ses_a" {
		t.Fatalf("active %v %v", ids, err)
	}
	if s, err := c.Session(ctx, "ses_c"); err != nil || s.ParentID != "ses_a" {
		t.Fatalf("session %+v %v", s, err)
	}
	if p, err := c.Permissions(ctx); err != nil || len(p) != 1 {
		t.Fatalf("permissions %s %v", p, err)
	}
	page, err := c.Messages(ctx, "ses_a", "", 100)
	if err != nil || len(page.Data) != 1 || page.Cursor.Next != "n1" {
		t.Fatalf("messages %+v %v", page, err)
	}
	if _, err := c.Messages(ctx, "ses_a", "n1", 100); err != nil {
		t.Fatal(err)
	}
	if err := c.Prompt(ctx, "ses_a", "line one\nline \"two\""); err != nil {
		t.Fatal(err)
	}
	if err := c.Reply(ctx, "ses_a", "per_1", ReplyOnce, ""); err != nil {
		t.Fatal(err)
	}
	err = c.Reply(ctx, "ses_a", "per_2", ReplyReject, "no")
	var ae *APIError
	if !NotFound(err, TagPermissionNotFound) || Drift(err) || !errors.As(err, &ae) || ae.Tag != "PermissionNotFoundError" {
		t.Fatalf("reply to a gone request: %v", err)
	}
	if NotFound(err, TagSessionNotFound) {
		t.Fatalf("a missing request taken as a missing session: %v", err)
	}
	// A route the server lacks is a 404 with no tag: API drift, never a missing request.
	err = c.Reply(ctx, "ses_b", "per_1", ReplyOnce, "")
	if NotFound(err, TagSessionNotFound, TagPermissionNotFound) || !Drift(err) {
		t.Fatalf("an unknown route taken as a missing request: %v", err)
	}
	for _, bad := range []string{"", "../x", "a/b", "ses a", strings.Repeat("a", 129)} {
		if err := c.Prompt(ctx, bad, "x"); err == nil {
			t.Errorf("Prompt(%q) accepted the id", bad)
		}
		if err := c.Reply(ctx, "ses_a", bad, ReplyOnce, ""); err == nil {
			t.Errorf("Reply(%q) accepted the id", bad)
		}
	}
	want := []string{
		"GET /api/info",
		"GET /api/session/active",
		"GET /api/session/ses_c",
		"GET /api/permission/request",
		"GET /api/session/ses_a/message?limit=100&order=desc",
		"GET /api/session/ses_a/message?cursor=n1&limit=100",
		`POST /api/session/ses_a/prompt {"text":"line one\nline \"two\""}`,
		`POST /api/session/ses_a/permission/per_1/reply {"decision":"once"}`,
		`POST /api/session/ses_a/permission/per_2/reply {"decision":"reject","message":"no"}`,
		`POST /api/session/ses_b/permission/per_1/reply {"decision":"once"}`,
	}
	if got := f.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestWrongPassword(t *testing.T) {
	f, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {})
	c, _ := New(f.srv.URL, "other")
	_, err := c.Info(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || ae.Tag != "UnauthorizedError" {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestCheckDoc(t *testing.T) {
	paths := map[string]map[string]map[string]string{}
	for _, op := range Operations {
		if paths[op.Path] == nil {
			paths[op.Path] = map[string]map[string]string{}
		}
		paths[op.Path][strings.ToLower(op.Method)] = map[string]string{"operationId": op.ID}
	}
	doc, _ := json.Marshal(map[string]any{"openapi": "3.1.0", "paths": paths})
	if err := CheckDoc(doc); err != nil {
		t.Fatal(err)
	}
	delete(paths, "/api/session/{sessionID}/prompt")
	paths["/api/info"]["get"]["operationId"] = "server.identity"
	doc, _ = json.Marshal(map[string]any{"paths": paths})
	err := CheckDoc(doc)
	if err == nil || !strings.Contains(err.Error(), "session.prompt") || !strings.Contains(err.Error(), "server.info") {
		t.Fatalf("a document without two operations: %v", err)
	}
	if CheckDoc([]byte("<html>")) == nil {
		t.Fatal("an HTML page passed as a document")
	}
}

// sse serves an event stream from a channel of raw chunks.
func sse(chunks <-chan string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case c, ok := <-chunks:
				if !ok {
					return
				}
				io.WriteString(w, c)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

func TestEvents(t *testing.T) {
	chunks := make(chan string, 16)
	f, c := newFake(t, sse(chunks))
	chunks <- "data: {\"id\":\"e0\",\"type\":\"server.connected\",\"data\":{}}\n\n: heartbeat\n\n"
	chunks <- "data: {\"id\":\"e1\",\"created\":5,\"type\":\"session.execution.started\",\r\ndata: \"data\":{\"sessionID\":\"ses_a\"}}\r\n\r\n"
	chunks <- "event: x\nid: 9\ndata: {\"id\":\"e2\",\"type\":\"permission.asked\",\"location\":{\"directory\":\"/work\"},"
	chunks <- "\"data\":{\"id\":\"per_1\",\"sessionID\":\"ses_a\"}}\n\n"
	close(chunks)
	var got []Event
	connected := false
	err := c.Events(context.Background(), []string{"session.execution.started", "permission.asked"}, func() {
		if len(got) > 0 {
			t.Error("connected after an event")
		}
		connected = true
	}, func(e Event) { got = append(got, e) })
	if !errors.Is(err, io.EOF) || !connected {
		t.Fatalf("end of stream: %v, connected %v", err, connected)
	}
	if len(got) != 2 || got[0].Type != "session.execution.started" || got[0].Created != 5 || got[0].SessionID() != "ses_a" ||
		got[1].Type != "permission.asked" || got[1].Location.Directory != "/work" || got[1].SessionID() != "ses_a" {
		t.Fatalf("events %+v", got)
	}
	if r := f.requests(); len(r) != 1 || r[0] != "GET /api/event?types=session.execution.started&types=permission.asked" {
		t.Fatalf("subscribe %q", r)
	}
}

func TestEventsFailures(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		old := StreamIdle
		StreamIdle = 100 * time.Millisecond
		defer func() { StreamIdle = old }()
		chunks := make(chan string)
		_, c := newFake(t, sse(chunks))
		start := time.Now()
		err := c.Events(context.Background(), nil, nil, func(Event) {})
		if !errors.Is(err, ErrStreamIdle) || time.Since(start) > 5*time.Second {
			t.Fatalf("a silent stream: %v after %v", err, time.Since(start))
		}
	})
	t.Run("bad event", func(t *testing.T) {
		chunks := make(chan string, 1)
		chunks <- "data: {not json\n\n"
		_, c := newFake(t, sse(chunks))
		if err := c.Events(context.Background(), nil, nil, func(Event) {}); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("a bad event: %v", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		chunks := make(chan string, 2)
		chunks <- "data: " + strings.Repeat("x", maxEvent+200<<10)
		_, c := newFake(t, sse(chunks))
		if err := c.Events(context.Background(), nil, nil, func(Event) {}); err == nil || !strings.Contains(err.Error(), "over") {
			t.Fatalf("an oversize event: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		chunks := make(chan string)
		_, c := newFake(t, sse(chunks))
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		if err := c.Events(ctx, nil, nil, func(Event) {}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) })
		_ = f
		var ae *APIError
		if err := c.Events(context.Background(), []string{"nope"}, nil, func(Event) {}); !errors.As(err, &ae) || ae.Status != 400 {
			t.Fatalf("a refused subscription: %v", err)
		}
	})
}

// TestOperationsUnique: every operation is named once, with a path the document form uses.
func TestOperationsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range Operations {
		key := fmt.Sprintf("%s %s", op.Method, op.Path)
		if seen[op.ID] || seen[key] || !strings.HasPrefix(op.Path, "/api/") {
			t.Errorf("operation %+v", op)
		}
		seen[op.ID], seen[key] = true, true
	}
}
