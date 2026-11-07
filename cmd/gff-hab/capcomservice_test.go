package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/control"
)

// buildCapcomStub builds testdata/capcomstub into dir.
func buildCapcomStub(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "capcomstub")
	if b, err := exec.Command("go", "build", "-o", out, "./testdata/capcomstub").CombinedOutput(); err != nil {
		t.Fatalf("build capcomstub: %v\n%s", err, b)
	}
	return out
}

type serviceEnv struct {
	*habEnv
	stubDir string
	config  string
	kind    string // the engine the config names: claude or codex
}

const plantedToken = "planted-oauth-value-6e1f"

// engineKinds are the engines the A2 and A4 tests run against (18 A6): the stub stands in for
// each, with its capabilities.
var engineKinds = []string{"claude", "codex"}

func newServiceEnv(t *testing.T, extraConfig string) *serviceEnv {
	return newServiceEnvFor(t, "claude", extraConfig)
}

// newServiceEnvFor runs the stub as the engine kind names. The Codex engine gets an API key,
// so the tests see the credential in hello as for Claude; CODEX_HOME is a throwaway folder.
func newServiceEnvFor(t *testing.T, kind, extraConfig string) *serviceEnv {
	t.Helper()
	e := newHolderEnv(t, 0)
	d := shortDir(t)
	stub := buildCapcomStub(t, d)
	cfg := filepath.Join(d, "hab.toml")
	head := "[capcom]\nengine = \"" + kind + "\"\nrestart_delay = \"1s\"\n"
	if kind == "codex" {
		head += "auth = \"api_key\"\n"
	}
	if err := os.WriteFile(cfg, []byte(head+extraConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	stubDir := filepath.Join(d, "stub")
	os.MkdirAll(stubDir, 0o700)
	e.env = []string{"GFF_HAB_CONFIG=" + cfg, "GFF_HAB_CAPCOM_ENGINE=" + stub, "STUB_DIR=" + stubDir, "CLAUDE_CODE_OAUTH_TOKEN=" + plantedToken,
		"OPENAI_API_KEY=" + plantedToken, "CODEX_HOME=" + filepath.Join(d, "codex-home")}
	se := &serviceEnv{habEnv: e, stubDir: stubDir, config: cfg, kind: kind}
	t.Cleanup(se.killEngine)
	return se
}

// helloKind is the credential kind the stub logs from hello.
func (e *serviceEnv) helloKind() string {
	if e.kind == "codex" {
		return "api_key"
	}
	return "oauth_token"
}

// killEngine ends a stub engine left running: it outlives gff-hab by design.
func (e *serviceEnv) killEngine() {
	b, err := os.ReadFile(filepath.Join(e.state, "capcom", capcomengine.PidFile))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (e *serviceEnv) stubLog() string {
	b, _ := os.ReadFile(filepath.Join(e.stubDir, "log"))
	return string(b)
}

func (e *serviceEnv) enginePid() int {
	b, _ := os.ReadFile(filepath.Join(e.state, "capcom", capcomengine.PidFile))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func callJSON[T any](t *testing.T, c *control.Client, method string, params any) (T, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	var res T
	err := c.Call(ctx, method, params, &res)
	return res, err
}

func threadText(t *testing.T, c *control.Client) []api.CapcomItem {
	t.Helper()
	p, err := callJSON[api.CapcomThreadPage](t, c, "capcom.thread.page", api.CapcomThreadPageParams{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Items
}

func countReplies(items []api.CapcomItem, text string) int {
	n := 0
	for _, it := range items {
		if it.Kind == "message" && it.Author.Kind == "capcom" && it.Text == text {
			n++
		}
	}
	return n
}

// The Capcom service end to end with a stub engine (capcom-service.md 18, A2): the service
// session with no pane, a message and its reply, the read mark, the credential through hello
// only, a tool call on the engine route as the Capcom session, a gff-hab restart mid-turn that
// adopts the engine and loses no reply, a crash that restarts with a new token and refuses the
// old one, and interrupt.
func TestCapcomServiceStub(t *testing.T) {
	for _, kind := range engineKinds {
		t.Run(kind, func(t *testing.T) { testCapcomServiceStub(t, kind) })
	}
}

func testCapcomServiceStub(t *testing.T, kind string) {
	e := newServiceEnvFor(t, kind, "")
	e.serve()
	c := e.control()
	if w := welcome(t, e.sock); !strings.Contains(strings.Join(w.Capabilities, " "), "capcom-thread") {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	var st api.CapcomServiceStatus
	waitUntil(t, "the engine connected", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	ss := sessionOf(t, c, st.Session)
	if ss.Name != "capcom" || ss.Pane != "" || ss.Kind != api.SessionKindCapcom {
		t.Fatalf("service session %+v", ss)
	}
	opened := mustOpen(t, c, false)
	if opened.Session != st.Session || opened.Pane != "" {
		t.Fatalf("capcom.open %+v", opened)
	}
	// The credential reaches the engine in hello, never in its environment.
	sum := sha256.Sum256([]byte(plantedToken))
	waitUntil(t, "hello", func() bool {
		return strings.Contains(e.stubLog(), "hello "+e.helloKind()+" "+hex.EncodeToString(sum[:8]))
	})
	if env, _ := os.ReadFile(filepath.Join(e.state, "capcom", capcomengine.EnvFile)); !strings.Contains(string(env), capcomengine.KindVar+"="+kind) {
		t.Fatalf("the job's environment names no engine kind: %q", env)
	}
	if strings.Contains(e.stubLog(), "env holds a credential") {
		t.Fatal("the engine's environment holds the credential")
	}
	// A reserved name is refused (2.4).
	if _, err := callJSON[api.SessionCreated](t, c, "session.create", api.SessionCreateParams{Name: "capcom-2", Argv: []string{"/bin/sh"}}); errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("session.create named capcom-2: %v", err)
	}

	out := run(t, "capcom", "say", "-socket", e.sock, "hello")
	if !strings.Contains(out, "delivered") {
		t.Fatalf("say: %q", out)
	}
	waitUntil(t, "the reply", func() bool { return countReplies(threadText(t, c), "echo: hello") == 1 })
	for _, it := range threadText(t, c) {
		if it.Author.Kind == "user" && it.Text == "hello" && it.Read == "" {
			t.Fatal("the message is not marked read")
		}
	}
	if out := run(t, "capcom", "thread", "-socket", e.sock); !strings.Contains(out, "capcom: echo: hello") || !strings.Contains(out, "you: hello") {
		t.Fatalf("thread:\n%s", out)
	}

	// chat.page on the service session reads the thread (7.4), oldest first, with a cursor.
	page, err := callJSON[api.ChatMessages](t, c, "chat.page", api.ChatPageParams{Session: st.Session})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, m := range page.Messages {
		kinds = append(kinds, string(m.Kind)+":"+m.Text)
	}
	if got := strings.Join(kinds, "|"); !strings.Contains(got, "user:hello|agent:echo: hello") || page.Cursor == "" {
		t.Fatalf("chat.page %q cursor %q", got, page.Cursor)
	}
	run(t, "capcom", "say", "-socket", e.sock, "more")
	waitUntil(t, "the second reply", func() bool { return countReplies(threadText(t, c), "echo: more") == 1 })
	since, err := callJSON[api.ChatMessages](t, c, "chat.page", api.ChatPageParams{Session: st.Session, After: page.Cursor})
	if err != nil || len(since.Messages) != 2 || since.Messages[0].Text != "more" || since.Messages[1].Kind != api.ChatKindAgent {
		t.Fatalf("chat.page after the cursor: %+v %v", since.Messages, err)
	}

	// The service session's agent state rides on the engine route (5.3, 7.6): idle between
	// turns, blocked:permission while an approval card is open, and an answer by pane and request.
	waitUntil(t, "the idle state", func() bool {
		a, ok := agentOf(t, c, capcomRoute)
		return ok && a.State == api.AgentStatusIdle && a.Harness == capcomHarness
	})
	if kind == "codex" {
		// No approval callback: the engine offers no tool that needs a card (4.1).
		if r := e.sayWait(t, c, "approve"); r != "no approvals here" {
			t.Fatalf("approve on codex: %q", r)
		}
	} else {
		testServiceApproval(t, e, c)
	}
	testServiceRest(t, e, c, st, kind)
}

// testServiceApproval: an approval card on the engine route (5.3).
func testServiceApproval(t *testing.T, e *serviceEnv, c *control.Client) {
	run(t, "capcom", "say", "-socket", e.sock, "approve")
	var blocked api.AgentState
	waitUntil(t, "the approval state", func() bool {
		a, _ := agentOf(t, c, capcomRoute)
		blocked = a
		return a.State == api.AgentStatusBlocked
	})
	if blocked.Kind != api.BlockedKindPermission || blocked.Request == nil || blocked.Request.Tool != "WebFetch" ||
		blocked.Request.Title != "Capcom wants to open example.com/x" || blocked.Request.ExpiresAt == "" || blocked.Turn == "" {
		t.Fatalf("blocked state %+v %+v", blocked, blocked.Request)
	}
	if _, err := callJSON[api.Empty](t, c, "approval.answer", api.ApprovalAnswerParams{Pane: capcomRoute, Request: blocked.Request.ID, Choice: api.ApprovalChoiceAllow}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the approved reply", func() bool { return countReplies(threadText(t, c), "approved: true") == 1 })
	waitUntil(t, "idle after the turn", func() bool {
		a, _ := agentOf(t, c, capcomRoute)
		return a.State == api.AgentStatusIdle && a.Request == nil
	})
	if _, err := callJSON[api.Empty](t, c, "approval.answer", api.ApprovalAnswerParams{Pane: capcomRoute, Request: blocked.Request.ID, Choice: api.ApprovalChoiceDeny}); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("a second answer: %v", err)
	}
}

func testServiceRest(t *testing.T, e *serviceEnv, c *control.Client, st api.CapcomServiceStatus, kind string) {

	// A photo for the service session uploads into <state>/capcom/files, and its message
	// carries the stored path as an image (7.6).
	photo := []byte("\x89PNG\r\n\x1a\nnot really a png")
	photoSum := sha256.Sum256(photo)
	ctx := context.Background()
	put, err := api.Client{Caller: c}.AttachmentPut(ctx, api.AttachmentPutParams{Session: st.Session, Name: "flyer.png", Type: "image/png", Size: uint64(len(photo)), SHA256: hex.EncodeToString(photoSum[:])})
	if err != nil {
		t.Fatal(err)
	}
	if w, err := (api.Client{Caller: c}).AttachmentWrite(ctx, api.AttachmentWriteParams{ID: put.ID, Data: photo}); err != nil || !w.Done {
		t.Fatalf("write: %+v %v", w, err)
	}
	if !strings.HasPrefix(evalDir(t, put.Path), evalDir(t, filepath.Join(e.state, capcomFolders, "files"))+"/") {
		t.Fatalf("photo stored at %s", put.Path)
	}
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: st.Session, Text: "what is on this flyer", Attachments: []string{put.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the photo message", func() bool {
		for _, it := range threadText(t, c) {
			if it.Text == "what is on this flyer" && len(it.Images) == 1 && it.Images[0] == put.Path && len(it.Attachments) == 1 && it.Attachments[0] == put.ID {
				return true
			}
		}
		return false
	})

	// "Allow this site" (5.3): the web fetch card offers it, the answer allows the fetch and
	// keeps the site in capcom.status, and capcom.site.remove takes it off.
	run(t, "capcom", "say", "-socket", e.sock, "approve")
	waitUntil(t, "the second approval", func() bool {
		a, _ := agentOf(t, c, capcomRoute)
		blocked = a
		return a.State == api.AgentStatusBlocked && a.Request != nil
	})
	if _, err := callJSON[api.Empty](t, c, "approval.answer", api.ApprovalAnswerParams{Pane: capcomRoute, Request: blocked.Request.ID, Choice: api.ApprovalChoiceAllowSite}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second approved reply", func() bool { return countReplies(threadText(t, c), "approved: true") == 2 })
	sites, err := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
	if err != nil || strings.Join(sites.Sites, ",") != "example.com" {
		t.Fatalf("sites after allow_site: %v %v", sites.Sites, err)
	}
	if sites, err = callJSON[api.CapcomServiceStatus](t, c, "capcom.site.remove", api.CapcomSiteParams{Site: "example.com"}); err != nil || len(sites.Sites) != 0 {
		t.Fatalf("remove: %v %v", sites.Sites, err)
	}
	if _, err := callJSON[api.CapcomServiceStatus](t, c, "capcom.site.remove", api.CapcomSiteParams{Site: "example.com"}); errKind(err) != api.ErrorKindNotFound {
		t.Fatalf("a second remove: %v", err)
	}

	// A tool call on the engine's token reaches the pane route as the Capcom session.
	run(t, "capcom", "say", "-socket", e.sock, "route")
	var route string
	waitUntil(t, "the route reply", func() bool {
		for _, it := range threadText(t, c) {
			if strings.HasPrefix(it.Text, "route: ") {
				route = it.Text
				return true
			}
		}
		return false
	})
	t.Logf("%s", route)
	if !strings.HasPrefix(route, "route: 200 ") || strings.Contains(route, "not a session") {
		t.Fatalf("the engine route is not the Capcom session's: %s", route)
	}

	// A channel message from another session to capcom starts a turn (2.4, 9.1).
	pane := e.spawn(c, "sleep 60")
	var from api.Session
	waitUntil(t, "the sender's session", func() bool {
		var ok bool
		from, ok = sessionByPaneOf(t, c, pane)
		return ok
	})
	run(t, "send", "-socket", e.sock, "-from", from.ID, "capcom", "ping from an agent")
	waitUntil(t, "the agent message's turn", func() bool {
		for _, it := range threadText(t, c) {
			if it.Author.Kind == "capcom" && strings.HasPrefix(it.Text, "echo: ") && strings.Contains(it.Text, "ping from an agent") {
				return true
			}
		}
		return false
	})

	// A Logbook write on the engine route is the Capcom session's: a proposal, nothing landed
	// (3.3, capcom.md 6.2).
	run(t, "capcom", "say", "-socket", e.sock, "lbwrite")
	var lb string
	waitUntil(t, "the logbook reply", func() bool {
		for _, it := range threadText(t, c) {
			if strings.HasPrefix(it.Text, "lb: ") {
				lb = it.Text
				return true
			}
		}
		return false
	})
	if !strings.HasPrefix(lb, "lb: 200 ") || !strings.Contains(lb, `"proposed"`) {
		t.Fatalf("the engine route's Logbook write: %s", lb)
	}
	out := run(t, "lb", "--socket", e.sock, "--json", "proposals")
	prop := out[strings.Index(out, `"id":"`)+6:]
	prop = prop[:strings.Index(prop, `"`)]
	if out := run(t, "lb", "--socket", e.sock, "--json", "show", prop); !strings.Contains(out, "from capcom") || !strings.Contains(out, st.Session) {
		t.Fatalf("proposal %s (want the Capcom session %s as the author):\n%s", prop, st.Session, out)
	}

	// gff-hab restarts mid-turn: the engine keeps running, its reply arrives once. A message
	// sent during the turn reaches an engine with mid-turn input at once; for one without
	// (Codex) gff-hab holds it until the turn ends (4.1).
	pid := e.enginePid()
	run(t, "capcom", "say", "-socket", e.sock, "slow")
	waitUntil(t, "the slow turn", func() bool { return strings.Contains(e.stubLog(), " slow") })
	run(t, "capcom", "say", "-socket", e.sock, "during the slow turn")
	time.Sleep(300 * time.Millisecond)
	if during := strings.Contains(e.stubLog(), "input during a turn during the slow turn"); during != (kind == "claude") {
		t.Fatalf("%s: input during the turn reached the engine: %v", kind, during)
	}
	e.srv.stop(t)
	os.WriteFile(filepath.Join(e.stubDir, "release"), nil, 0o600)
	e.serve()
	c = e.control()
	waitUntil(t, "the slow reply after the restart", func() bool { return countReplies(threadText(t, c), "echo: slow") == 1 })
	waitUntil(t, "the held message's reply", func() bool { return countReplies(threadText(t, c), "echo: during the slow turn") == 1 })
	if e.enginePid() != pid {
		t.Fatalf("the engine was not adopted: pid %d, then %d", pid, e.enginePid())
	}
	time.Sleep(300 * time.Millisecond)
	if n := countReplies(threadText(t, c), "echo: slow"); n != 1 {
		t.Fatalf("%d slow replies", n)
	}

	// Interrupt reaches the engine.
	if _, err := callJSON[api.Empty](t, c, "capcom.interrupt", api.CapcomServiceParams{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "interrupt", func() bool { return strings.Contains(e.stubLog(), "interrupt") })

	// A crash restarts the engine with a new token; the old one is refused.
	envFile := filepath.Join(e.state, "capcom", capcomengine.EnvFile)
	oldEnv, _ := os.ReadFile(envFile)
	oldTok := tokenOf(string(oldEnv))
	run(t, "capcom", "say", "-socket", e.sock, "crash")
	waitUntil(t, "a new engine", func() bool { p := e.enginePid(); return p != 0 && p != pid })
	waitUntil(t, "the new engine connected", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	newEnv, _ := os.ReadFile(envFile)
	if tokenOf(string(newEnv)) == oldTok || oldTok == "" {
		t.Fatal("the restart kept the token")
	}
	hookSock := filepath.Join(filepath.Dir(e.sock), "hook.sock")
	if code := upgradeStatus(t, hookSock, oldTok); code != http.StatusNotFound {
		t.Fatalf("the old token: %d", code)
	}
	// GFF_HAB's token, which the SDK's binary and gff-hab mcp get, never opens the engine route.
	if p := paneTokenOf(string(newEnv)); p == "" || p == tokenOf(string(newEnv)) || upgradeStatus(t, hookSock, p) != http.StatusNotFound {
		t.Fatalf("the pane route token %q opens the engine route", p)
	}
	// The new engine got the crash message again (it was not taken) and runs on.
	run(t, "capcom", "say", "-socket", e.sock, "after")
	waitUntil(t, "a reply from the new engine", func() bool { return countReplies(threadText(t, c), "echo: after") == 1 })
	var notes []string
	for _, it := range threadText(t, c) {
		if it.Kind == "note" {
			notes = append(notes, it.Text)
		}
	}
	if !strings.Contains(strings.Join(notes, "|"), "Capcom restarted") {
		t.Fatalf("notes %q", notes)
	}

	// The planted credential is in no file under the state directory.
	filepath.WalkDir(e.state, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSocket != 0 {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), plantedToken) {
			t.Errorf("%s holds the credential", p)
		}
		return nil
	})
}

// With no credential the engine does not start, and the row says why; the thread still takes
// messages.
func TestCapcomServiceNoToken(t *testing.T) {
	e := newServiceEnv(t, "")
	e.env = e.env[:len(e.env)-1] // no CLAUDE_CODE_OAUTH_TOKEN
	e.env = append(e.env, "CLAUDE_CODE_OAUTH_TOKEN=")
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	var st api.CapcomServiceStatus
	waitUntil(t, "the error", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.State == "error"
	})
	if !strings.Contains(st.Reason, "no Claude token: run claude setup-token") {
		t.Fatal(st.Reason)
	}
	if out := run(t, "capcom", "say", "-socket", e.sock, "anyone?"); !strings.Contains(out, "delivered") {
		t.Fatal(out)
	}
}

// Crashes up to crash_limit in crash_window stop the restarts; the row shows error and the
// thread a note; gff-hab capcom resume clears the count (3.2). A limit key moves the behaviour it
// names (crash_limit 3); max_turns reaches the engine in hello; a value outside its range is
// refused and the default runs (19.2). settings.set refuses capcom.approval_max_age 0 (19.1).
func TestCapcomServiceCrashLimit(t *testing.T) {
	e := newServiceEnv(t, "crash_limit = 3\nmax_turns = 9999\n")
	os.WriteFile(filepath.Join(e.stubDir, "crashstart"), nil, 0o600)
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	var st api.CapcomServiceStatus
	waitUntil(t, "the restarts stopped", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.State == "error" && strings.Contains(st.Reason, "crashed 3 times")
	})
	if n := strings.Count(e.stubLog(), "crash at start"); n != 3 {
		t.Fatalf("%d starts, want 3 (crash_limit)", n)
	}
	time.Sleep(1500 * time.Millisecond) // past restart_delay: no fourth start
	if n := strings.Count(e.stubLog(), "crash at start"); n != 3 {
		t.Fatalf("%d starts after the stop", n)
	}
	var notes []string
	for _, it := range threadText(t, c) {
		if it.Kind == "note" {
			notes = append(notes, it.Text)
		}
	}
	if len(notes) == 0 {
		t.Fatal("no note in the thread")
	}
	t.Logf("reason %q, notes %q", st.Reason, notes)
	if a, _ := agentOf(t, c, capcomRoute); a.State != api.AgentStatusError {
		t.Fatalf("agent state %+v", a)
	}

	os.Remove(filepath.Join(e.stubDir, "crashstart"))
	run(t, "capcom", "resume", "-socket", e.sock)
	waitUntil(t, "connected after resume", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	if !strings.Contains(e.stubLog(), "settings max_turns 50") {
		t.Fatalf("max_turns out of range did not fall back to the default:\n%s", e.stubLog())
	}

	var got api.SettingsResult
	err := c.Call(context.Background(), "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: "capcom.approval_max_age", Value: []byte("0"), Revision: 1}}}, &got)
	if errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("settings.set capcom.approval_max_age 0: %v", err)
	}
}

// sessionByPaneOf finds the session that lives in pane.
func sessionByPaneOf(t *testing.T, c *control.Client, pane string) (api.Session, bool) {
	t.Helper()
	var st api.StructureResult
	call(t, c, "structure.get", api.StructureGetParams{}, &st)
	for _, ss := range st.Sessions {
		if ss.Pane == pane {
			return ss, true
		}
	}
	return api.Session{}, false
}

// tokenOf returns the engine route token in the job's environment file.
func tokenOf(env string) string {
	for _, l := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(l, capcomengine.TokenVar+"="); ok {
			return v
		}
	}
	return ""
}

// paneTokenOf returns the pane route token in the job's environment file (GFF_HAB).
func paneTokenOf(env string) string {
	for _, l := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(l, "GFF_HAB="); ok {
			return v[strings.LastIndex(v, "/hooks/")+len("/hooks/"):]
		}
	}
	return ""
}

func upgradeStatus(t *testing.T, sock, token string) int {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("POST /capcom/" + token + " HTTP/1.1\r\nHost: hab\r\nConnection: Upgrade\r\nUpgrade: gff-capcom/1\r\nContent-Length: 0\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
