package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/aliases"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/confirm"
)

// TestSpawnAliases: the runner turns an alias into its harness and model once, an alias may
// shadow a harness word, the request's model wins, and an unknown word is refused naming the
// harnesses and the aliases. A teammate start carries report_to, and the result its address.
func TestSpawnAliases(t *testing.T) {
	table := aliases.Table{"gpt": {Harness: "codex", Model: "gpt-5.5"}, "codex": {Harness: "codex", Model: "gpt-5.5-codex"}, "opus": {Harness: "claude", Model: "opus"}}
	link := &fakeLinkCaller{}
	var created []api.SessionCreateParams
	r := &confirmRunner{host: "mac", aliases: func() aliases.Table { return table },
		links: func() []confirmLink {
			return []confirmLink{{Label: "devbox", Online: true, Caps: []string{"confirm"}, Caller: link}}
		},
		local: func(_ context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
			created = append(created, p)
			return api.SessionCreated{Session: "s-9", Pane: "p-9"}, nil
		}}
	s, _, err := r.CheckStart(api.ConfirmStart{Folder: "/a", Harness: " GPT "})
	if err != nil || s.Harness != "codex" || s.Model != "gpt-5.5" || s.Alias != "gpt" {
		t.Fatalf("gpt: %+v %v", s, err)
	}
	// Checked again (after an edit): the alias is not applied twice.
	table["codex"] = aliases.Alias{Harness: "claude"}
	if again, _, err := r.CheckStart(s); err != nil || again.Harness != "codex" || again.Model != "gpt-5.5" {
		t.Fatalf("again: %+v %v", again, err)
	}
	table["codex"] = aliases.Alias{Harness: "codex", Model: "gpt-5.5-codex"}
	if s, _, err := r.CheckStart(api.ConfirmStart{Folder: "/a", Harness: "codex"}); err != nil || s.Model != "gpt-5.5-codex" || s.Alias != "codex" {
		t.Fatalf("shadowed codex: %+v %v", s, err)
	}
	if s, _, err := r.CheckStart(api.ConfirmStart{Folder: "/a", Harness: "opus", Model: "sonnet"}); err != nil || s.Harness != "claude" || s.Model != "sonnet" {
		t.Fatalf("the request's model: %+v %v", s, err)
	}
	if s, _, err := r.CheckStart(api.ConfirmStart{Folder: "/a", Harness: "opencode"}); err != nil || s.Alias != "" || s.Model != "" {
		t.Fatalf("a plain harness: %+v %v", s, err)
	}
	_, _, err = r.CheckStart(api.ConfirmStart{Folder: "/a", Harness: "sol"})
	if errKind(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "claude, codex, opencode; aliases from this hab's [aliases] table: codex, gpt, opus") {
		t.Fatalf("unknown: %v", err)
	}

	// A teammate on this hab and on a peer: report_to names the requester; the address is how
	// this hab reaches the new session.
	s.Teammate, s.Name, s.Host = true, "codex-abc123", "mac"
	res, err := r.RunStart(context.Background(), "id-1", "alice", s)
	if err != nil || res.Address != "codex-abc123" || created[0].ReportTo != "alice" {
		t.Fatalf("local teammate: %+v %+v %v", res, created, err)
	}
	s.Host = "devbox"
	res, err = r.RunStart(context.Background(), "id-2", "alice", s)
	if err != nil || res.Address != "devbox/codex-abc123" || link.calls[0].ReportTo != "alice" {
		t.Fatalf("peer teammate: %+v %+v %v", res, link.calls, err)
	}
	s.Teammate = false
	if _, err := r.RunStart(context.Background(), "id-3", "alice", s); err != nil || link.calls[1].ReportTo != "" {
		t.Fatalf("detached: %+v %v", link.calls, err)
	}
}

// TestSpawnTeammateNeedsRequester: a teammate start from outside a pane names no session to
// report to, and is refused before anything runs.
func TestSpawnTeammateNeedsRequester(t *testing.T) {
	ran := 0
	svc, err := confirm.New(confirm.Config{Runner: runnerFunc(func() { ran++ })})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	_, err = svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", Start: &api.ConfirmStart{Folder: "/a", Harness: "codex", Teammate: true}})
	if errKind(err) != api.ErrorKindInvalid || ran != 0 {
		t.Fatalf("no requester: %v, ran %d", err, ran)
	}
	c, err := svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "start", From: "alice", Start: &api.ConfirmStart{Folder: "/a", Harness: "codex", Teammate: true, Alias: "forged"}})
	if err != nil || ran != 1 || c.Start.Alias != "" {
		t.Fatalf("from a session: %+v %v, ran %d", c, err, ran)
	}
}

// runnerFunc is a confirm.Runner that accepts every start and counts runs.
type runnerFunc func()

func (f runnerFunc) CheckStart(s api.ConfirmStart) (api.ConfirmStart, bool, error) {
	return s, true, nil
}
func (f runnerFunc) RunStart(context.Context, string, string, api.ConfirmStart) (api.ConfirmResult, error) {
	f()
	return api.ConfirmResult{Session: "s", Name: "n"}, nil
}

// fakeSpawnHab is a pane door that answers confirm.request with a done start.
type fakeSpawnHab struct {
	pane bool
	got  []api.ConfirmRequestParams
}

func (f *fakeSpawnHab) fromPane() bool { return f.pane }
func (f *fakeSpawnHab) Call(_ context.Context, method string, params, result any) error {
	p := params.(api.ConfirmRequestParams)
	f.got = append(f.got, p)
	st := *p.Start
	st.Harness, st.Model, st.Alias, st.Name, st.Host = "codex", "gpt-5.5", "gpt", "codex-abc123", "devbox"
	*(result.(*api.ConfirmRequested)) = api.ConfirmRequested{Confirmation: api.Confirmation{ID: "abc123", State: "done", Record: true, Start: &st,
		Result: &api.ConfirmResult{Host: "devbox", Session: "s-1", Name: "codex-abc123", Address: "devbox/codex-abc123"}}}
	return nil
}

func (f *fakeSpawnHab) paneKey() string { return "" }

// TestSpawnTool: the tool runs only from a pane, sends a teammate start by default and a
// detached one when asked, refuses another mode, and says the address and how reports come.
func TestSpawnTool(t *testing.T) {
	tl, ok := findIn(actionTools, "spawn")
	if !ok {
		t.Fatal("no spawn tool")
	}
	ctx := context.Background()
	if _, _, err := tl.run(ctx, &fakeSpawnHab{}, toolArgs{"harness": "gpt", "seed": "Review."}); errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("outside a pane: %v", err)
	}
	f := &fakeSpawnHab{pane: true}
	res, text, err := tl.run(ctx, f, toolArgs{"harness": "gpt", "seed": "Review the diff.", "host": "devbox"})
	if err != nil {
		t.Fatal(err)
	}
	if p := f.got[0]; p.Kind != "start" || !p.Start.Teammate || p.Start.Harness != "gpt" || p.Start.Folder != "" || p.Start.Seed != "Review the diff." || p.WaitMS != 45000 {
		t.Fatalf("request %+v %+v", p, p.Start)
	}
	if r := res.(spawnResult); r.Address != "devbox/codex-abc123" || r.Mode != "teammate" {
		t.Fatalf("result %+v", r)
	}
	for _, w := range []string{"Started session codex-abc123 on devbox", "gpt is the owner's alias for codex with gpt-5.5", "Its channel address: devbox/codex-abc123", "It is your teammate"} {
		if !strings.Contains(text, w) {
			t.Errorf("text lacks %q:\n%s", w, text)
		}
	}
	_, text, err = tl.run(ctx, f, toolArgs{"harness": "claude", "seed": "x", "mode": "detached"})
	if err != nil || f.got[1].Start.Teammate || !strings.Contains(text, "It runs detached") {
		t.Fatalf("detached: %v %+v\n%s", err, f.got[1].Start, text)
	}
	if _, _, err := tl.run(ctx, f, toolArgs{"harness": "claude", "seed": "x", "mode": "boss"}); errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("a bad mode: %v", err)
	}
}

// TestSpawnFolders: a start with no folder takes the requester's own folder on this hab, and
// the same project's folder on a peer, on the same branch when there is one.
func TestSpawnFolders(t *testing.T) {
	own := t.TempDir()
	cl := api.CatalogList{
		Hosts: []api.CatalogEntry{{Own: true, Catalog: api.Catalog{DeviceID: "d-mac"}}, {Label: "devbox", Catalog: api.Catalog{DeviceID: "d-dev"}}, {Label: "bare", Catalog: api.Catalog{DeviceID: "d-bare"}}},
		Projects: []api.CatalogProjectGroup{
			{Name: "web", Locations: []api.CatalogLocation{{DeviceID: "d-dev", Folders: []api.CatalogFolder{{Path: "/srv/web", Branch: "feature"}}}}},
			{Name: "app", Locations: []api.CatalogLocation{
				{DeviceID: "d-mac", Folders: []api.CatalogFolder{{Path: own, Branch: "feature"}}},
				{DeviceID: "d-dev", Folders: []api.CatalogFolder{{Path: "/srv/app", Branch: "main"}, {Path: "/srv/app-wt", Branch: "feature"}}},
			}},
		}}
	d := spawnFolders{host: "mac", catalog: func() api.CatalogList { return cl },
		folderOf: func(_ context.Context, session string) string {
			if session == "s-alice" {
				return own
			}
			return ""
		}}
	ctx := context.Background()
	alice := api.Session{ID: "s-alice", Name: "alice"}
	for host, want := range map[string]string{"": own, "mac": own, "devbox": "/srv/app-wt"} {
		if got, err := d.fill(ctx, alice, host); err != nil || got != want {
			t.Errorf("%q: %q %v", host, got, err)
		}
	}
	cl.Projects[1].Locations[1].Folders = cl.Projects[1].Locations[1].Folders[:1]
	if got, _ := d.fill(ctx, alice, "devbox"); got != "/srv/app" {
		t.Errorf("another branch: %q", got)
	}
	if _, err := d.fill(ctx, alice, "bare"); errKind(err) != api.ErrorKindNotFound || !strings.Contains(err.Error(), "project app has no folder on bare") {
		t.Errorf("not set up: %v", err)
	}
	if _, err := d.fill(ctx, alice, "nowhere"); errKind(err) != api.ErrorKindNotFound {
		t.Errorf("unknown host: %v", err)
	}
	if _, err := d.fill(ctx, api.Session{ID: "s-bob"}, ""); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("no folder known: %v", err)
	}
}

// TestTeammates: a seed, or a message from the spawner, arms the next turn; a working state
// after the write and then done sends the final message to the spawner, once; another
// sender's message, a turn before the write, and an undelivered write send nothing; an error
// and an exit say so; a resumed session reports from its new pane; the list survives a reopen.
func TestTeammates(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	rev := map[string]uint64{}
	var sent []string
	final := "LGTM, one nit in cart.go."
	mk := func() *teammates {
		m := openTeammates(dir, t.Logf)
		m.readFor = 500 * time.Millisecond
		m.agent = func(pane string) (api.AgentState, bool) {
			mu.Lock()
			defer mu.Unlock()
			return api.AgentState{Pane: pane, Revision: rev[pane]}, true
		}
		m.name = func(id string) (string, bool) { return strings.TrimPrefix(id, "s-"), id != "s-gone" }
		m.read = func(_ context.Context, session, harness, native string) ([]api.ChatMessage, error) {
			if native == "unreadable" {
				return nil, errors.New("no transcript")
			}
			return []api.ChatMessage{{Kind: api.ChatKindUser, Text: "seed"}, {Kind: api.ChatKindAgent, Text: final}}, nil
		}
		m.send = func(_ context.Context, from string, to api.ChannelEnd, text string) error {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, from+">"+to.DeviceID+"/"+to.Session+": "+text)
			return nil
		}
		return m
	}
	m := mk()
	spawner := api.ChannelEnd{DeviceID: "d-a", Host: "ha", Session: "alice"}
	m.add("s-rev", "p1", spawner)
	state := func(pane string, st api.AgentStatus, native string) {
		mu.Lock()
		rev[pane]++
		r := rev[pane]
		mu.Unlock()
		m.agentState(api.AgentState{Pane: pane, Revision: r, State: st, Harness: "codex", Session: native, Error: map[bool]string{true: "server_error"}[st == api.AgentStatusError]})
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(sent)
	}
	waitSent := func(n int) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for count() < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(sent) != n {
			t.Fatalf("sent %d, want %d: %q", len(sent), n, sent)
		}
		return sent[n-1]
	}

	// A turn before any write reports nothing.
	state("p1", api.AgentStatusWorking, "n1")
	state("p1", api.AgentStatusDone, "n1")
	// The seed: done before working (a stale state) ends nothing; working then done reports.
	heard := m.delivering("s-rev", api.ChannelEnd{DeviceID: "d-b", Session: "ha/alice"}, true)
	heard(true)
	state("p1", api.AgentStatusDone, "n1")
	state("p1", api.AgentStatusWorking, "n1")
	state("p1", api.AgentStatusBlocked, "n1")
	state("p1", api.AgentStatusDone, "n1")
	if got := waitSent(1); got != "rev>d-a/alice: (Flightdeck: my turn ended. My final message follows.)\n\n"+final {
		t.Fatalf("report %q", got)
	}
	state("p1", api.AgentStatusWorking, "n1")
	state("p1", api.AgentStatusDone, "n1")
	// Another sender's message, and a write that was not delivered, arm nothing.
	if h := m.delivering("s-rev", api.ChannelEnd{DeviceID: "d-a", Session: "bob"}, false); h != nil {
		t.Fatal("bob's message armed a report")
	}
	m.delivering("s-rev", spawner, false)(false)
	state("p1", api.AgentStatusWorking, "n1")
	state("p1", api.AgentStatusDone, "n1")
	time.Sleep(50 * time.Millisecond)
	if count() != 1 {
		t.Fatalf("reports %d", count())
	}
	// The spawner's message: an error turn, with an unreadable transcript.
	m.delivering("s-rev", spawner, false)(true)
	state("p1", api.AgentStatusWorking, "unreadable")
	state("p1", api.AgentStatusError, "unreadable")
	if got := waitSent(2); !strings.Contains(got, "ended with an error (server_error), and my final message could not be read") {
		t.Fatalf("error report %q", got)
	}
	// Resumed in a new pane after a reopen; then an exit mid-turn.
	m.Close()
	m = mk()
	m.repane("s-rev", "p2")
	state("p1", api.AgentStatusWorking, "n1") // the old pane is not followed
	m.delivering("s-rev", spawner, false)(true)
	state("p2", api.AgentStatusWorking, "n2")
	state("p2", api.AgentStatusExited, "n2")
	if got := waitSent(3); !strings.Contains(got, "exited before the turn your message began ended") {
		t.Fatalf("exit report %q", got)
	}
	// A long final message is cut to one channel message.
	final = strings.Repeat("é", channels.MaxText)
	m.delivering("s-rev", spawner, false)(true)
	state("p2", api.AgentStatusWorking, "n2")
	state("p2", api.AgentStatusDone, "n2")
	if got := waitSent(4); len(got) > channels.MaxText+len("rev>d-a/alice: ") || !strings.HasSuffix(got, "é [cut]") {
		t.Fatalf("cut report: %d bytes, ends %q", len(got), got[len(got)-20:])
	}
	m.forget("s-rev")
	if h := m.delivering("s-rev", spawner, true); h != nil {
		t.Fatal("a forgotten session armed")
	}
	m.Close()
	if b, err := os.ReadFile(filepath.Join(dir, teammatesFile)); err != nil || strings.Contains(string(b), "s-rev") {
		t.Fatalf("file after forget: %s %v", b, err)
	}
}
