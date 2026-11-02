package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeChannelHab is the fake catalog of twoHosts with channel.send and channel.status: it
// records each send and answers as a hab would, handed for this hab, else queued.
type fakeChannelHab struct {
	*fakeReadHab
	sends []api.ChannelSendParams
}

func (f *fakeChannelHab) Call(ctx context.Context, method string, params, result any) error {
	switch method {
	case "channel.send":
		p := params.(api.ChannelSendParams)
		f.sends = append(f.sends, p)
		st := api.ChannelStateHanded
		if strings.Contains(p.To, "/") {
			st = api.ChannelStateQueued
		}
		*result.(*api.ChannelSent) = api.ChannelSent{ID: strings.Repeat("a", 32), To: p.To, State: st}
		return nil
	case "channel.status":
		*result.(*api.ChannelStatusResult) = api.ChannelStatusResult{Record: api.ChannelRecord{ID: strings.Repeat("a", 32), State: api.ChannelStateDelivered}}
		return nil
	}
	return f.fakeReadHab.Call(ctx, method, params, result)
}

func runAction(t *testing.T, c caller, name string, a toolArgs) (any, string, error) {
	t.Helper()
	tl, ok := findIn(actionTools, name)
	if !ok {
		t.Fatalf("no action tool %s", name)
	}
	if err := tl.check(a); err != nil {
		t.Fatal(err)
	}
	return tl.run(context.Background(), c, a)
}

// TestHandOffPick: hand_off finds the project's agents in the catalog, picks the one the user
// named, else an idle one, else the most recently active, sends to its address and says why;
// with no agent it sends nothing and points at suggest_host and start_session.
func TestHandOffPick(t *testing.T) {
	f := &fakeChannelHab{fakeReadHab: twoHosts()}
	cl := &f.catalog
	// A second agent on app, on devbox, idle and older; and an exited one that never counts.
	cl.Hosts[1].Catalog.Agents = append(cl.Hosts[1].Catalog.Agents,
		api.CatalogAgent{Session: "app-api", Harness: "codex", Project: "app", State: api.AgentStatusIdle, Since: "2026-03-01T09:00:00Z"},
		api.CatalogAgent{Session: "app-old", Harness: "codex", Project: "app", State: api.AgentStatusExited, Since: "2026-03-01T11:59:00Z"})

	// The idle agent wins over the more recent working one, and the idle one takes it now.
	res, text, err := runAction(t, f, "hand_off", toolArgs{"project": "/work/app", "message": "fix the cache"})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(handOffResult)
	if !r.Sent || r.Agent.Address != "devbox/app-api" || r.Delivery != "delivered" || len(f.sends) != 1 || f.sends[0].To != "devbox/app-api" || f.sends[0].Text != "fix the cache" {
		t.Fatalf("idle pick: %+v %+v\n%s", r, f.sends, text)
	}
	for _, want := range []string{"Delivered: devbox/app-api (codex, idle, on devbox)", "Why this agent: idle, so it takes the message now", "Also on app: app-ui (claude, working)"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "app-old") {
		t.Errorf("an exited agent was offered:\n%s", text)
	}

	// The agent the user named, though it works: queued until idle, no status wait.
	f.sends = nil
	res, text, err = runAction(t, f, "hand_off", toolArgs{"project": "app", "agent": "app-ui", "message": "and the docs"})
	if err != nil {
		t.Fatal(err)
	}
	r = res.(handOffResult)
	if r.Agent.Address != "app-ui" || r.Delivery != "queued" || r.Message.State != api.ChannelStateHanded ||
		!strings.Contains(text, "Queued: app-ui (claude, working, on host-a) is working; it gets the message when idle.") ||
		!strings.Contains(text, "the agent the user named") {
		t.Fatalf("named pick: %+v\n%s", r, text)
	}

	// A name that runs nowhere on the project, and a project with no agent: nothing is sent.
	f.sends = nil
	_, text, err = runAction(t, f, "hand_off", toolArgs{"project": "app", "agent": "nobody", "message": "x"})
	if err != nil || len(f.sends) != 0 || !strings.Contains(text, "No agent named nobody is running on app: nothing was sent") || !strings.Contains(text, "devbox/app-api") {
		t.Fatalf("unknown name: %v %+v\n%s", err, f.sends, text)
	}
	res, text, err = runAction(t, f, "hand_off", toolArgs{"project": "web", "message": "x"})
	if err != nil || len(f.sends) != 0 || res.(handOffResult).Sent ||
		!strings.Contains(text, "No agent is running on web: nothing was sent") || !strings.Contains(text, "suggest_host") || !strings.Contains(text, "start_session") {
		t.Fatalf("no agent: %v %+v\n%s", err, f.sends, text)
	}
	// host keeps one host: app on host-a only has the working agent.
	_, text, err = runAction(t, f, "hand_off", toolArgs{"project": "app", "host": "host-a", "message": "y"})
	if err != nil || len(f.sends) != 1 || f.sends[0].To != "app-ui" || !strings.Contains(text, "the only agent running on the project") {
		t.Fatalf("host filter: %v %+v\n%s", err, f.sends, text)
	}

	// An agent on an offline host is held in the outbox; only offline agents left: it says so.
	f.sends = nil
	cl.Hosts[1].Stale = true
	cl.Hosts[0].Catalog.Agents = nil
	res, text, err = runAction(t, f, "hand_off", toolArgs{"project": "app", "message": "z"})
	if err != nil || res.(handOffResult).Delivery != "held" || !strings.Contains(text, "Held: devbox/app-api") || !strings.Contains(text, "outbox") {
		t.Fatalf("offline: %v\n%s", err, text)
	}
}

// TestHandOffPickOrder: the order among candidates of one tier is the most recent first, and
// an online busy agent goes before one on an offline host.
func TestHandOffPickOrder(t *testing.T) {
	ts := []handOffTarget{
		{Address: "a", Offline: true, CatalogAgent: api.CatalogAgent{State: api.AgentStatusIdle, Since: "2026-03-01T12:00:00Z"}},
		{Address: "b", CatalogAgent: api.CatalogAgent{State: api.AgentStatusWorking, Since: "2026-03-01T10:00:00Z"}},
		{Address: "c", CatalogAgent: api.CatalogAgent{State: api.AgentStatusBlocked, Since: "2026-03-01T11:00:00Z"}},
	}
	if i, why := pickTarget(ts, ""); ts[i].Address != "c" || !strings.Contains(why, "no agent on the project is idle") {
		t.Fatalf("picked %s: %s", ts[i].Address, why)
	}
	ts = append(ts, handOffTarget{Address: "d", CatalogAgent: api.CatalogAgent{State: api.AgentStatusDone, Since: "2026-03-01T08:00:00Z"}})
	if i, _ := pickTarget(ts, ""); ts[i].Address != "d" {
		t.Fatalf("picked %s, want the done agent", ts[i].Address)
	}
}

// fakeLogbookHab is a hab with Logbook: it keeps records and proposals apart, as Part 12 will.
// A write with propose files a pending proposal; one without lands as a record.
type fakeLogbookHab struct {
	*fakeReadHab
	mu        sync.Mutex
	records   []api.LogbookRecord
	proposals []logbookWrite
}

func (f *fakeLogbookHab) Call(ctx context.Context, method string, params, result any) error {
	if method != "logbook.find" && method != "logbook.write" {
		return f.fakeReadHab.Call(ctx, method, params, result)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "logbook.find":
		*result.(*api.LogbookRecords) = api.LogbookRecords{Records: api.NonNil(f.records)}
		return nil
	case "logbook.write":
		b, _ := json.Marshal(params)
		var w logbookWrite
		if err := json.Unmarshal(b, &w); err != nil {
			return control.Errorf(api.ErrorKindInvalid, "%v", err)
		}
		if !w.Propose {
			c := w.Changes[0]
			f.records = append(f.records, api.LogbookRecord{ID: c.Type + "-k3m9q2xz", Type: c.Type, Title: c.Title, Status: "open"})
			*result.(*logbookWritten) = logbookWritten{State: "applied", IDs: []string{c.Type + "-k3m9q2xz"}}
			return nil
		}
		f.proposals = append(f.proposals, w)
		*result.(*logbookWritten) = logbookWritten{State: "proposed", Proposal: fmt.Sprintf("proposal-%08d", len(f.proposals))}
	}
	return nil
}

// TestFileRecord: without Logbook, file_record answers unavailable and sends no write; with a
// hab that has Logbook, the record lands as a pending proposal with the quote as its source,
// never as a record.
func TestFileRecord(t *testing.T) {
	plain := twoHosts()
	args := toolArgs{"type": "task", "title": "Cache the catalog", "quote": "note that we should cache the catalog", "project": "app", "body": "From the Capcom talk."}
	_, _, err := runAction(t, plain, "file_record", args)
	if errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("without Logbook: %v", err)
	}
	for _, m := range plain.calls {
		if m != "logbook.find" {
			t.Fatalf("without Logbook it called %s; only the capability check may run: %v", m, plain.calls)
		}
	}

	lb := &fakeLogbookHab{fakeReadHab: twoHosts()}
	res, text, err := runAction(t, lb, "file_record", args)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.(fileRecordResult); !r.Filed || r.State != "proposed" || r.Proposal != "proposal-00000001" || !strings.Contains(text, "waits for the user's review as proposal proposal-00000001") {
		t.Fatalf("proposal: %+v\n%s", r, text)
	}
	if len(lb.records) != 0 || len(lb.proposals) != 1 {
		t.Fatalf("records %v, proposals %v: the record landed", lb.records, lb.proposals)
	}
	w := lb.proposals[0]
	c := w.Changes[0]
	if !w.Propose || len(w.IdempotencyKey) != 32 || c.Op != "create" || c.Type != "task" || c.Title != "Cache the catalog" ||
		c.Project != "remote:example.com/app" || w.Source == nil || w.Source.Quote != "note that we should cache the catalog" {
		t.Fatalf("write: %+v %+v", w, c)
	}
	var found api.LogbookRecords
	if err := lb.Call(context.Background(), "logbook.find", api.LogbookFindParams{}, &found); err != nil || len(found.Records) != 0 {
		t.Fatalf("logbook.find shows a record: %+v %v", found, err)
	}
	// Bad drafts are refused before anything reaches the hab.
	for _, bad := range []toolArgs{
		{"type": "proposal", "title": "x", "quote": "q"},
		{"type": "task", "title": "two\nlines", "quote": "q"},
		{"type": "task", "title": strings.Repeat("x", 201), "quote": "q"},
		{"type": "task", "title": "x", "quote": "q", "project": "web"},
	} {
		if _, _, err := runAction(t, lb, "file_record", bad); err == nil {
			t.Errorf("%v: no refusal", bad)
		}
	}
	if len(lb.proposals) != 1 {
		t.Fatalf("a refused draft was filed: %v", lb.proposals)
	}
}

// startProjectReceiver files bob in the project at repo on e, running fakeReceiver for wants.
func startProjectReceiver(t *testing.T, e *habEnv, repo, start string, wants ...string) api.SessionCreated {
	t.Helper()
	for i, w := range wants {
		writeFile(t, filepath.Join(e.work, "len-"+strconv.Itoa(i+1)), strconv.Itoa(len(w)))
	}
	ss := createSession(t, e.sock, api.SessionCreateParams{
		Name: "bob", Argv: []string{"/bin/sh", "-c", fakeReceiver}, ProjectDir: repo, Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "START=" + start, "COUNT=" + strconv.Itoa(len(wants)), noRaceExitSleep}})
	waitState(t, e.sock, ss.Pane, "bob reports "+start, func(s api.AgentState) bool { return string(s.State) == start })
	return ss
}

// TestHandOffEndToEnd: the tool from a pane (alice, standing in for Capcom) through the real
// hab and holder: to the project's working agent it queues and says so, and the message
// arrives in the agent envelope once the agent is idle; to the idle agent it is delivered at
// once, with no confirmation.
func TestHandOffEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	repo := filepath.Join(e.work, "shop")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://example.com/shop.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v %s", err, out)
	}
	first, second := "Please fix the cart total.", "Now add a test for it."
	m1, m2 := pasted("alice", first), pasted("alice", second)
	startProjectReceiver(t, e, repo, "working", m1, m2)
	addr := startSender(t, e)
	agentIs := func(state api.AgentStatus) func(api.CatalogList) bool {
		return func(l api.CatalogList) bool {
			for _, a := range l.Hosts[0].Catalog.Agents {
				if a.Session == "bob" && a.Project == "shop" && a.State == state {
					return true
				}
			}
			return false
		}
	}
	waitCatalog(t, e.sock, "bob working on shop", agentIs(api.AgentStatusWorking))
	waitCatalog(t, e.sock, "shop's folder in the catalog", func(l api.CatalogList) bool {
		for _, p := range l.Projects {
			for _, loc := range p.Locations {
				for _, f := range loc.Folders {
					if r, _ := filepath.EvalSymlinks(repo); f.Path == r {
						return true
					}
				}
			}
		}
		return false
	})
	c := hookCaller{addr}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	tl, _ := findIn(actionTools, "hand_off")

	res, text, err := tl.run(ctx, c, toolArgs{"project": repo, "message": first})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(handOffResult)
	if r.Delivery != "queued" || r.Message.State != api.ChannelStateHanded || !strings.Contains(text, "Queued: bob (claude, working") || !strings.Contains(text, "gets the message when idle") {
		t.Fatalf("to a working agent: %+v\n%s", r, text)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(e.work, "msg-1")); err == nil {
		t.Fatal("delivered while working")
	}
	writeFile(t, filepath.Join(e.work, "go-idle"), "")
	waitUntil(t, "the first message after idle", fileIs(filepath.Join(e.work, "msg-1"), withID(m1, r.Message.ID)))
	if rec := channelStatus(t, e.sock, r.Message.ID, 10000); rec.State != api.ChannelStateDelivered || rec.From != "alice" {
		t.Fatalf("first record: %+v", rec)
	}

	waitCatalog(t, e.sock, "bob idle on shop", agentIs(api.AgentStatusIdle))
	res, text, err = tl.run(ctx, c, toolArgs{"project": "shop", "message": second})
	if err != nil {
		t.Fatal(err)
	}
	r = res.(handOffResult)
	if r.Delivery != "delivered" || !strings.Contains(text, "Delivered: bob (claude, idle") {
		t.Fatalf("to an idle agent: %+v\n%s", r, text)
	}
	waitUntil(t, "the second message", fileIs(filepath.Join(e.work, "msg-2"), withID(m2, r.Message.ID)))
	var cl api.ConfirmList
	if err := e.control().Call(ctx, "confirm.list", api.ConfirmListParams{}, &cl); err == nil && len(cl.Confirmations) != 0 {
		t.Fatalf("a hand-off asked for a confirmation: %+v", cl.Confirmations)
	}

	// No agent on another project: the clear none answer, nothing sent.
	_, text, err = tl.run(ctx, c, toolArgs{"project": "web", "message": "x"})
	if err != nil || !strings.Contains(text, "No agent is running on web: nothing was sent") {
		t.Fatalf("none: %v\n%s", err, text)
	}
	// file_record through the pane door: this hab has no Logbook, so unavailable.
	fr, _ := findIn(actionTools, "file_record")
	if _, _, err := fr.run(ctx, c, toolArgs{"type": "task", "title": "t", "quote": "q"}); errKind(err) != api.ErrorKindUnavailable {
		t.Fatalf("file_record without Logbook: %v", err)
	}
}
