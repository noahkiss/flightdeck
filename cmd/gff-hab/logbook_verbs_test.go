package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// lbRun runs `gff-hab lb` with extra environment and returns stdout, stderr and the exit code.
func lbRun(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"lb"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// lbOK is lbRun that must succeed; it returns stdout and stderr.
func lbOK(t *testing.T, env []string, args ...string) (string, string) {
	t.Helper()
	out, errs, code := lbRun(t, env, args...)
	if code != 0 {
		t.Fatalf("gff-hab lb %v: exit %d\n%s%s", args, code, out, errs)
	}
	return out, errs
}

// waitLogbookEvent waits for a Logbook event about id on a host subscriber.
func waitLogbookEvent(t *testing.T, c *control.Client, typ api.EventType, id string) api.Event {
	t.Helper()
	deadline := time.After(scaleTimeout(10 * time.Second))
	for {
		select {
		case n := <-c.Notifications():
			if n.Method != "event" {
				continue
			}
			e, err := n.Event()
			if err != nil {
				t.Fatal(err)
			}
			if e.Type == typ && e.ID == id {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s for %s", typ, id)
		}
	}
}

// TestLogbookServe: gff-hab with Logbook end to end (logbook.md 18 row 4). The capability; a CLI
// write through the API and its event; a write to the file by another writer while gff-hab runs
// reaching subscribers; a repeated key applied once; the CLI writing while gff-hab is stopped and
// gff-hab serving those ops after its start; the read tools and file_record against it.
func TestLogbookServe(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	state := filepath.Join(d, "state")
	s := startServe(t, sock, state)
	if !slices.Contains(welcome(t, sock).Capabilities, "logbook") {
		t.Fatal("no logbook capability")
	}
	host := settingsClient(t, sock)
	var snap api.StructureResult
	call(t, host, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, &snap)
	narrow := settingsClient(t, sock)
	call(t, narrow, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"none"}}}, &snap)

	// Through the API.
	out, errs := lbOK(t, nil, "--socket", sock, "new", "task", "Fix the build", "--tag", "ci", "--body", "It fails on arm64.")
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "task-") || errs != "" {
		t.Fatalf("new: %q %q", out, errs)
	}
	e := waitLogbookEvent(t, host, api.EventLogbookUpdated, id)
	var upd api.LogbookUpdated
	if err := json.Unmarshal(e.Data, &upd); err != nil || upd.Type != "task" || upd.Revision != 2 || e.Revision == nil || *e.Revision != 2 {
		t.Fatalf("event: %+v %+v %v", e, upd, err)
	}
	// Another writer of the file while gff-hab runs: the CLI's fallback, forced by a socket
	// nothing listens on. Its op reaches the subscriber.
	out, errs = lbOK(t, nil, "--socket", filepath.Join(d, "none.sock"), "--state", state, "note", id, "Tried a clean build.")
	if strings.TrimSpace(out) != id || !strings.Contains(errs, "used "+filepath.Join(state, "logbook.db")+" directly") {
		t.Fatalf("note through the file: %q %q", out, errs)
	}
	if e := waitLogbookEvent(t, host, api.EventLogbookUpdated, id); e.Revision == nil || *e.Revision != 3 {
		t.Fatalf("the file's write: %+v", e)
	}
	// A sessions scope gets no Logbook event.
	select {
	case n := <-narrow.Notifications():
		if ev, _ := n.Event(); strings.HasPrefix(string(ev.Type), "logbook.") {
			t.Fatalf("a sessions scope got %s", ev.Type)
		}
	case <-time.After(200 * time.Millisecond):
	}

	// A repeated key applies once.
	params := `{"idempotency_key":"k-1","changes":[{"op":"create","type":"finding","title":"The cache is stale"}]}`
	var w1, w2 api.LogbookWritten
	json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "logbook.write", params)), &w1)
	json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "logbook.write", params)), &w2)
	if len(w1.IDs) != 1 || w1.Repeated || !w2.Repeated || !slices.Equal(w1.IDs, w2.IDs) {
		t.Fatalf("repeat: %+v %+v", w1, w2)
	}
	out, _ = lbOK(t, nil, "--socket", sock, "ls", "--type", "finding")
	if strings.Count(out, "The cache is stale") != 1 {
		t.Fatalf("a repeated key applied twice:\n%s", out)
	}
	var rec api.LogbookRecordResult
	json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "logbook.get", `{"id":"`+id+`"}`)), &rec)
	if by := rec.Record.CreatedBy; by == nil || by.Kind != "person" || by.Device != "gff-hab" || len(rec.Record.Notes) != 1 || rec.Record.Notes[0].By.Device != "cli" {
		t.Fatalf("writers: %+v", rec.Record)
	}

	// gff-hab stopped: the CLI writes to the file; gff-hab serves the op after its start.
	s.stop(t)
	out, errs = lbOK(t, []string{"GFF_HAB_STATE=" + state}, "--socket", sock, "new", "task", "Written while the hab was down")
	offline := strings.TrimSpace(out)
	if !strings.HasPrefix(offline, "task-") || !strings.Contains(errs, "gff-hab is not running") {
		t.Fatalf("while stopped: %q %q", out, errs)
	}
	if out, _ := lbOK(t, []string{"GFF_HAB_STATE=" + state}, "--socket", sock, "show", offline); !strings.Contains(out, "Written while the hab was down") {
		t.Fatalf("show while stopped:\n%s", out)
	}
	startServe(t, sock, state)
	json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "logbook.get", `{"id":"`+offline+`"}`)), &rec)
	if rec.Record.Title != "Written while the hab was down" {
		t.Fatalf("after the start: %+v", rec.Record)
	}
	if out, errs := lbOK(t, nil, "--socket", sock, "ready"); !strings.Contains(out, offline) || errs != "" {
		t.Fatalf("ready after the start: %q %q", out, errs)
	}

	// The read tools and file_record against a hab with Logbook.
	if out := run(t, "read", "-socket", sock, "logbook_search", "-words", "build"); !strings.Contains(out, id) {
		t.Fatalf("logbook_search:\n%s", out)
	}
	if out := run(t, "read", "-socket", sock, "logbook_read", id); !strings.Contains(out, "It fails on arm64.") || !strings.Contains(out, "Tried a clean build.") {
		t.Fatalf("logbook_read:\n%s", out)
	}
	out = run(t, "act", "-socket", sock, "-json", "file_record", "-type", "task", "-title", "Renew the certificate", "-quote", "remind me to renew it")
	var filed fileRecordResult
	if err := json.Unmarshal([]byte(out), &filed); err != nil || filed.State != "proposed" || filed.Proposal == "" {
		t.Fatalf("file_record: %s %v", out, err)
	}
	if out, _ := lbOK(t, nil, "--socket", sock, "proposals"); !strings.Contains(out, filed.Proposal) {
		t.Fatalf("proposals:\n%s", out)
	}
	// A person approves Capcom's proposal from the CLI outside a pane (logbook.md 9.4).
	if out, errs := lbOK(t, nil, "--socket", sock, "approve", filed.Proposal); !strings.HasPrefix(out, "approved: "+filed.Proposal+"\n") || !strings.Contains(out, "task-") {
		t.Fatalf("approve: %q %q", out, errs)
	}
	// Later stages' verbs say so.
	if _, errs, code := lbRun(t, nil, "--socket", sock, "export", "x"); code != 1 || !strings.Contains(errs, "stage 9") {
		t.Fatalf("export: %d %s", code, errs)
	}
	// An API error exits 3.
	if _, errs, code := lbRun(t, nil, "--socket", sock, "show", "task-zzzzzzzz"); code != 3 || !strings.Contains(errs, "no such record") {
		t.Fatalf("show a missing record: %d %s", code, errs)
	}
}

// fakeAgent reports itself as an idle Claude Code over OSC 7501 and writes its GFF_HAB.
const fakeAgent = `stty raw -echo
printf '\033]7501;state=idle:app=claude\033\\'
printf '%s' "$GFF_HAB_STATE" > "$DIR/state"
printf '%s' "$GFF_HAB" > "$DIR/addr.tmp" && mv "$DIR/addr.tmp" "$DIR/addr"
while [ ! -e "$DIR/end" ]; do sleep 0.05; done
`

// TestLogbookPaneWrites: a write through a pane's token records the pane's harness, session and
// host; the pane's 61st write in a minute is refused; logbook_write and file_record from the pane.
func TestLogbookPaneWrites(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	made := createSession(t, e.sock, api.SessionCreateParams{Name: "worker", Argv: []string{"/bin/sh", "-c", fakeAgent}, Cwd: "/",
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}})
	addrFile := filepath.Join(e.work, "addr")
	waitUntil(t, "the pane's GFF_HAB", func() bool { b, err := os.ReadFile(addrFile); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addrFile)
	pane := []string{"GFF_HAB=" + string(b), "GFF_HAB_SOCKET=" + filepath.Join(e.work, "none.sock")}
	// gff-hab names its state folder in the pane, -state included, so the CLI's reads find the
	// file while gff-hab is down.
	if st, _ := os.ReadFile(filepath.Join(e.work, "state")); string(st) != e.state {
		t.Fatalf("the pane's GFF_HAB_STATE: %q, want %q", st, e.state)
	}
	c := e.control()
	waitUntil(t, "the pane's agent state", func() bool {
		var s api.StructureResult
		call(t, c, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, &s)
		for _, a := range s.Structure.Agents {
			if a.Pane == made.Pane && a.Harness == "claude" {
				return true
			}
		}
		return false
	})

	out, errs := lbOK(t, pane, "new", "finding", "The cache is stale")
	id := strings.TrimSpace(out)
	if errs != "" {
		t.Fatalf("the pane's write fell back to the file: %s", errs)
	}
	var rec api.LogbookRecordResult
	call(t, c, "logbook.get", api.LogbookGetParams{ID: id}, &rec)
	by := rec.Record.CreatedBy
	if by == nil || by.Kind != "agent" || by.Harness != "claude" || by.Session != made.Session || by.Pane != made.Pane || by.Host != "test-host" {
		t.Fatalf("a pane's write: %+v", by)
	}
	// claim: the calling session.
	task, _ := lbOK(t, pane, "new", "task", "Rebuild the cache")
	task = strings.TrimSpace(task)
	lbOK(t, pane, "claim", task)
	call(t, c, "logbook.get", api.LogbookGetParams{ID: task}, &rec)
	if rec.Record.Assignee != made.Session || rec.Record.Status != "doing" {
		t.Fatalf("claim: %+v", rec.Record)
	}
	// logbook_write and file_record from the pane.
	out = runEnv(t, pane, "act", "logbook_write", "-action", "note", "-id", task, "-text", "Halfway there.")
	if !strings.Contains(out, "Written: "+task) {
		t.Fatalf("logbook_write:\n%s", out)
	}
	out = runEnv(t, pane, "act", "-json", "file_record", "-type", "task", "-title", "Add a cache test", "-quote", "note that we need a test")
	var filed fileRecordResult
	if err := json.Unmarshal([]byte(out), &filed); err != nil || filed.State != "proposed" {
		t.Fatalf("file_record: %s %v", out, err)
	}
	call(t, c, "logbook.get", api.LogbookGetParams{ID: filed.Proposal}, &rec)
	if by := rec.Record.CreatedBy; by == nil || by.Session != made.Session || rec.Record.Source == nil || rec.Record.Source.Session != made.Session {
		t.Fatalf("the proposal: %+v", rec.Record)
	}

	// The limit: this pane made 5 writes; 55 more pass, the 61st is refused, and nothing lands.
	for i := 0; i < 55; i++ {
		var w api.LogbookWritten
		if err := hookCall(string(b), "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "note", ID: task, Text: "step"}}}, &w); err != nil {
			t.Fatalf("write %d: %v", i+6, err)
		}
	}
	_, errs, code := lbRun(t, pane, "note", task, "the 61st")
	if code != 3 || !strings.Contains(errs, "limit") {
		t.Fatalf("the 61st write: exit %d %s", code, errs)
	}
	call(t, c, "logbook.get", api.LogbookGetParams{ID: task}, &rec)
	if n := len(rec.Record.Notes); n != 56 {
		t.Fatalf("notes after the limit: %d", n)
	}
	// A person is not held up.
	var w api.LogbookWritten
	call(t, c, "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "note", ID: task, Text: "from the app"}}}, &w)
}

func hookCall(addr, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hook.Call(ctx, addr, method, params, result)
}

func runEnv(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gff-hab %v: %v %s", args, err, out)
	}
	return string(out)
}

// fakeCapcomPane is a stub Capcom harness that reports idle and writes its GFF_HAB beside it.
const fakeCapcomPane = `#!/bin/sh
stty raw -echo
printf '\033]7501;state=idle:app=claude\033\\'
printf '%s' "$GFF_HAB" > addr.tmp && mv addr.tmp addr
exec cat >> in.log
`

// TestLogbookCapcomPane: from a Capcom session's pane, gff-hab files every write as a proposal
// (capcom.md D3, D14) through the CLI and logbook_write, refuses its resolve, and no pane
// decides a proposal: a set of its status is refused and the proposal stays pending.
func TestLogbookCapcomPane(t *testing.T) {
	e := newHolderEnv(t, 0)
	binDir := filepath.Join(e.work, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(binDir, "claude"), fakeCapcomPane)
	if err := os.Chmod(filepath.Join(binDir, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.env = []string{"PATH=" + binDir + ":/usr/bin:/bin", "CLAUDE_CONFIG_DIR=" + shortDir(t)}
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	op, err := capcomOpenWith(t, c, api.CapcomOpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	addrFile := filepath.Join(op.Dir, "addr")
	waitUntil(t, "the Capcom pane's GFF_HAB", func() bool { b, err := os.ReadFile(addrFile); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addrFile)
	pane := []string{"GFF_HAB=" + string(b), "GFF_HAB_SOCKET=" + filepath.Join(e.work, "none.sock")}

	out, errs := lbOK(t, pane, "new", "task", "Renew the certificate")
	if !strings.HasPrefix(out, "proposed: proposal-") || errs != "" {
		t.Fatalf("a Capcom pane's new: %q %q", out, errs)
	}
	prop := strings.Fields(out)[1]
	var rec api.LogbookRecordResult
	call(t, c, "logbook.get", api.LogbookGetParams{ID: prop}, &rec)
	if rec.Record.Type != "proposal" || rec.Record.Status != "pending" || rec.Record.CreatedBy == nil || rec.Record.CreatedBy.Session != op.Session {
		t.Fatalf("the proposal: %+v", rec.Record)
	}
	out = runEnv(t, pane, "act", "logbook_write", "-action", "create", "-type", "task", "-title", "Another task")
	if !strings.HasPrefix(out, "Proposed:") {
		t.Fatalf("logbook_write from Capcom:\n%s", out)
	}
	var list api.LogbookRecords
	call(t, c, "logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: "task"}}, &list)
	if len(list.Records) != 0 {
		t.Fatalf("a Capcom write landed: %+v", list.Records)
	}
	// No pane decides a proposal, and Capcom does not resolve.
	if _, errs, code := lbRun(t, pane, "mark", prop, "rejected"); code != 3 || !strings.Contains(errs, "only a person decides a proposal") {
		t.Fatalf("mark a proposal from the pane: %d %s", code, errs)
	}
	if _, errs, code := lbRun(t, pane, "resolve", prop, "title", "x"); code != 3 || !strings.Contains(errs, "wait for review") {
		t.Fatalf("resolve from Capcom: %d %s", code, errs)
	}
	call(t, c, "logbook.get", api.LogbookGetParams{ID: prop}, &rec)
	if rec.Record.Status != "pending" {
		t.Fatalf("the proposal after the refusals: %+v", rec.Record)
	}
}
