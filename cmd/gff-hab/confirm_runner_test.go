package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeLinkCaller is a peer link: it records session.create calls, and can lose its answer or
// refuse.
type fakeLinkCaller struct {
	calls []api.SessionCreateParams
	lose  bool
	deny  bool
}

func (f *fakeLinkCaller) Call(_ context.Context, method string, params, result any) error {
	p := params.(api.SessionCreateParams)
	f.calls = append(f.calls, p)
	switch {
	case f.lose:
		return errors.New("link closed")
	case f.deny:
		return control.Errorf(api.ErrorKindConflict, "a session named %q exists", p.Name)
	}
	*(result.(*api.SessionCreated)) = api.SessionCreated{Session: "s-1", Pane: "p-1"}
	return nil
}

// TestConfirmRunnerPeers: a start on a peer goes over its link with the confirmation id as the
// key and the seed's sender; an offline peer, an unknown one and one without the confirm
// capability are refused before a card opens; a lost answer is unknown, a refusal a failure.
func TestConfirmRunnerPeers(t *testing.T) {
	link := &fakeLinkCaller{}
	links := []confirmLink{
		{Label: "devbox", Online: true, Caps: []string{"confirm"}, Caller: link},
		{Label: "laptop", Online: false},
		{Label: "old", Online: true, Caps: []string{"catalog"}, Caller: link},
	}
	r := &confirmRunner{host: "mac", links: func() []confirmLink { return links },
		catalog: func() api.CatalogList {
			return api.CatalogList{Hosts: []api.CatalogEntry{{Own: true, Catalog: api.Catalog{DeviceID: "d-mac"}}, {Label: "devbox", Catalog: api.Catalog{DeviceID: "d-dev"}}},
				Projects: []api.CatalogProjectGroup{{Name: "app", Locations: []api.CatalogLocation{{DeviceID: "d-dev", Folders: []api.CatalogFolder{{Path: "/srv/app", Branch: "main"}}}}}}}
		}}
	s, self, err := r.CheckStart(api.ConfirmStart{Host: "devbox", Folder: "/srv/app", Harness: "codex", Seed: "Fix it."})
	if err != nil || self || s.Project != "app" || s.Branch != "main" {
		t.Fatalf("check: %+v %v %v", s, self, err)
	}
	for host, want := range map[string]api.ErrorKind{"laptop": api.ErrorKindUnavailable, "old": api.ErrorKindUnavailable, "nowhere": api.ErrorKindNotFound} {
		if _, _, err := r.CheckStart(api.ConfirmStart{Host: host, Folder: "/srv/app", Harness: "codex"}); errKind(err) != want {
			t.Errorf("%s: %v", host, err)
		}
	}
	for _, bad := range []api.ConfirmStart{{Folder: "rel", Harness: "codex"}, {Folder: "/a", Harness: "vim"}, {Folder: "/a", Harness: "codex", Model: "--yolo"},
		{Folder: "/a", Harness: "codex", Name: "a b"}, {Folder: "/a", Harness: "codex", Seed: strings.Repeat("x", 8001)}} {
		if _, _, err := r.CheckStart(bad); errKind(err) != api.ErrorKindInvalid {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if s, self, err := r.CheckStart(api.ConfirmStart{Folder: "/a", Harness: "claude"}); err != nil || !self || s.Host != "mac" {
		t.Errorf("own host: %+v %v %v", s, self, err)
	}
	s.Name = "codex-abc123"
	res, err := r.RunStart(context.Background(), "id-1", "capcom", s)
	if err != nil || res.Host != "devbox" || res.Session != "s-1" || res.Name != "codex-abc123" {
		t.Fatalf("run: %+v %v", res, err)
	}
	if c := link.calls[0]; c.IdempotencyKey != "id-1" || c.SeedFrom != "mac/capcom" || c.ProjectDir != "/srv/app" || c.Harness != "codex" {
		t.Fatalf("create: %+v", c)
	}
	link.lose = true
	if _, err := r.RunStart(context.Background(), "id-2", "capcom", s); !errors.Is(err, confirm.ErrUnknown) || !strings.Contains(err.Error(), "codex-abc123") {
		t.Fatalf("lost: %v", err)
	}
	link.lose, link.deny = false, true
	if _, err := r.RunStart(context.Background(), "id-3", "capcom", s); errors.Is(err, confirm.ErrUnknown) || errKind(err) != api.ErrorKindConflict {
		t.Fatalf("refused: %v", err)
	}
	links[0].Online = false
	if _, err := r.RunStart(context.Background(), "id-4", "capcom", s); err == nil || errors.Is(err, confirm.ErrUnknown) {
		t.Fatalf("offline at run: %v", err)
	}
	links[0].Online = true
	link.deny = false
	ts := confirmTargets("mac", func() []confirmLink { return links })()
	if len(ts) != 4 || !ts[0].Ready || ts[0].Host != "mac" || !ts[1].Ready || ts[2].Ready || !strings.Contains(ts[3].Reason, "confirm capability") {
		t.Fatalf("targets: %+v", ts)
	}
	if !validSeedFrom("mac/capcom") || validSeedFrom("mac/cap com") || validSeedFrom("capcom") || validSeedFrom("a]/b") ||
		!validSeedFrom("SHA256:q3+Zk/9xW2pLm7/capcom") || !validSeedFrom("box.example.com:2/capcom") || validSeedFrom("my mac/capcom") {
		t.Fatal("validSeedFrom")
	}
	// A host id shaped like a device id (':', '/', '+') goes through as seed_from; one the
	// envelope cannot carry is left out, and the start still runs.
	link.calls = nil
	r.host = "SHA256:q3+Zk/9xW2pLm7"
	if _, err := r.RunStart(context.Background(), "id-5", "capcom", s); err != nil {
		t.Fatal(err)
	}
	if got := link.calls[0].SeedFrom; got != "SHA256:q3+Zk/9xW2pLm7/capcom" {
		t.Fatalf("seed_from %q", got)
	}
	r.host = "my mac"
	if _, err := r.RunStart(context.Background(), "id-6", "capcom", s); err != nil || link.calls[1].SeedFrom != "" {
		t.Fatalf("a host id with a space: %v %+v", err, link.calls[1])
	}
}

// TestMCPCancelled: notifications/cancelled ends that call's hab call, and the cancelled
// request gets no answer.
func TestMCPCancelled(t *testing.T) {
	entered := make(chan struct{})
	ended := make(chan error, 1)
	blocking := callerFunc(func(ctx context.Context, method string, _, _ any) error {
		if method != "confirm.request" {
			return errors.New("unexpected " + method)
		}
		close(entered)
		<-ctx.Done()
		ended <- ctx.Err()
		return ctx.Err()
	})
	m := &mcpServer{dial: func(context.Context) (caller, func(), error) { return blocking, func() {}, nil }}
	r, w := io.Pipe()
	var out bytes.Buffer
	done := make(chan error)
	go func() { done <- m.serve(r, &out) }()
	io.WriteString(w, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"start_session","arguments":{"folder":"/a","harness":"claude"}}}`+"\n")
	<-entered
	io.WriteString(w, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"timeout"}}`+"\n")
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hab call ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel did not reach the hab call")
	}
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("a cancelled request was answered: %s", out.String())
	}
}

// TestSnapshotOpenOnly: the host snapshot carries only open confirmations; a running one has
// its answer already.
func TestSnapshotOpenOnly(t *testing.T) {
	got := openOnly([]api.Confirmation{{ID: "a", State: "open"}, {ID: "b", State: "running"}, {ID: "c", State: "open"}})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("%+v", got)
	}
}

type callerFunc func(ctx context.Context, method string, params, result any) error

func (f callerFunc) Call(ctx context.Context, method string, params, result any) error {
	return f(ctx, method, params, result)
}

// TestActionToolText: the start tool's text tells the agent to wait for the message, never to
// ask again, and names the session after a start.
func TestActionToolText(t *testing.T) {
	st := &api.ConfirmStart{Host: "devbox", Folder: "/srv/app", Harness: "codex", Name: "codex-abc123", Seed: "x"}
	open := confirmationText(api.Confirmation{ID: "c1", State: "open", Start: st})
	if !strings.Contains(open, "Still waiting") || !strings.Contains(open, "Do not ask again") {
		t.Fatalf("open: %q", open)
	}
	done := confirmationText(api.Confirmation{ID: "c1", State: "done", Record: true, Start: st, Result: &api.ConfirmResult{Host: "devbox", Name: "codex-abc123"}})
	if !strings.Contains(done, "Started session codex-abc123 on devbox, at once") {
		t.Fatalf("done: %q", done)
	}
	unknown := confirmationText(api.Confirmation{ID: "c1", State: "unknown", Start: st, Reason: "lost"})
	if !strings.Contains(unknown, "Check the agents tool for codex-abc123") {
		t.Fatalf("unknown: %q", unknown)
	}
	for name, action := range map[string]bool{"start_session": true, "set_up_project": true, "suggest_host": false, "confirmation_read": false} {
		tl, ok := findIn(allTools(), name)
		if !ok || tl.Action != action {
			t.Errorf("%s: %+v", name, tl)
		}
	}
	if _, ok := findTool("start_session"); ok {
		t.Error("gff-hab read runs an action tool")
	}
	// A suggestion through the tool, on the fake catalog: devbox lacks xcode, so the Mac is it.
	text := runTool(t, twoHosts(), "suggest_host", toolArgs{"needs": "xcode", "project": "app"})
	if !strings.Contains(text, "suggest host-a (this host)") && !strings.Contains(text, "suggest host-a") {
		t.Fatalf("suggest: %q", text)
	}
	if !strings.Contains(text, "not devbox: lacks xcode") {
		t.Fatalf("suggest: %q", text)
	}
}
