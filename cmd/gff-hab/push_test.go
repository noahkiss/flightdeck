package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/progstatus"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// sentLog records the messages a notifier sends instead of reaching APNs.
type sentLog struct {
	mu   sync.Mutex
	msgs []push.Message
}

func (s *sentLog) Send(_ context.Context, m push.Message) error {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	return nil
}

func (s *sentLog) alerts() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, m := range s.msgs {
		if m.Type != push.TypeAlert {
			continue
		}
		var v map[string]any
		json.Unmarshal(m.Payload, &v)
		v["collapse"] = m.CollapseID
		out = append(out, v)
	}
	return out
}

func title(v map[string]any) any {
	return v["aps"].(map[string]any)["alert"].(map[string]any)["title"]
}

// writePushConfig writes a push config with a fresh test key: no real team, key or app. The
// key command is /bin/cat of a key file in state, so a test can see the key bytes it printed.
func writePushConfig(t *testing.T, state string) (pemKey []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	os.MkdirAll(state, 0o700)
	keyPath := filepath.Join(state, "AuthKey_TEST.p8")
	pemKey = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := push.Config{KeyCommand: []string{"/bin/cat", keyPath}, KeyID: "KEY0000001", TeamID: "TEAM000001", Topic: "dev.example.flightdeck", Environment: push.Development}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(state, push.ConfigFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return pemKey
}

// pemBody is the base64 body of a PEM key, the part that must never reach a log.
func pemBody(pemKey []byte) string {
	lines := strings.Split(strings.TrimSpace(string(pemKey)), "\n")
	return strings.Join(lines[1:len(lines)-1], "")
}

// lockedLog collects log lines from several goroutines.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) logf(f string, a ...any) {
	l.mu.Lock()
	fmt.Fprintf(&l.b, f+"\n", a...)
	l.mu.Unlock()
}

func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// TestPushKeyCommand: push turns on only once the key command printed a key. A command that
// fails leaves push off with one log line that names its exit status and never its output;
// gff-hab starts without waiting for it. A command that works turns push on in the background.
// No log line holds the key.
func TestPushKeyCommand(t *testing.T) {
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	setup := func(state string, logs *lockedLog) pushSetup {
		return pushSetup{state: state, hab: "laptop", getenv: func(string) string { return "" }, logf: logs.logf,
			connected: func(string) bool { return false }, label: func(string) string { return "" }, publish: func(structure.Event) {}}
	}

	// A command that prints the key on stderr and fails.
	state := t.TempDir()
	pemKey := writePushConfig(t, state)
	keyPath := filepath.Join(state, "AuthKey_TEST.p8")
	script := filepath.Join(state, "key.sh")
	os.WriteFile(script, []byte("cat '"+keyPath+"' >&2\nexit 7\n"), 0o700)
	b, _ := json.Marshal(push.Config{KeyCommand: []string{"/bin/sh", script}, KeyID: "KEY0000001", TeamID: "TEAM000001", Topic: "dev.example.flightdeck"})
	os.WriteFile(filepath.Join(state, push.ConfigFile), b, 0o600)
	logs := &lockedLog{}
	start := time.Now()
	s := newPushService(setup(state, logs), st, nil)
	if time.Since(start) > time.Second {
		t.Errorf("newPushService waited %v for the key command", time.Since(start))
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "exit status 7") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Enabled() || s.live() != nil {
		t.Error("push on with a failing key command")
	}
	s.Close()
	out := logs.String()
	if !strings.Contains(out, "push is off") || strings.Count(out, "exit status 7") != 1 || strings.Contains(out, "push is on") {
		t.Errorf("failing command log: %q", out)
	}
	if strings.Contains(out, pemBody(pemKey)[:20]) || strings.Contains(out, "PRIVATE KEY") {
		t.Fatalf("the key reached the log: %q", out)
	}

	// A command that works.
	state = t.TempDir()
	pemKey = writePushConfig(t, state)
	logs = &lockedLog{}
	s = newPushService(setup(state, logs), st, nil)
	defer s.Close()
	deadline = time.Now().Add(10 * time.Second)
	for !s.Enabled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.Enabled() || s.live() == nil {
		t.Fatalf("push did not turn on: %q", logs.String())
	}
	if out := logs.String(); !strings.Contains(out, "push is on") || strings.Contains(out, pemBody(pemKey)[:20]) {
		t.Fatalf("log: %q", out)
	}
}

// TestAnswerWithdrawsBanner: a permission request pushes a banner; an approval.answer from the
// phone resolves it, and the notifier replaces the banner by its collapse id with "Answered on
// phone". The state change that ends the request is kept from the notifier, so the withdrawal
// can only come from the answer itself (Answered). A second answer to the same request id is
// refused with conflict and sends no keys.
func TestAnswerWithdrawsBanner(t *testing.T) {
	state := t.TempDir()
	writePushConfig(t, state)
	st, err := structure.Open(structure.Config{Dir: filepath.Join(state, "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sent := &sentLog{}
	var pushes *pushService
	var forward atomic.Bool
	forward.Store(true)
	agents := harness.NewStore(harness.Config{Publish: func(e structure.Event) {
		if s, ok := e.Data.(api.AgentState); ok && pushes != nil && forward.Load() {
			pushes.notifier.State(s)
		}
	}})
	pushes = newPushService(pushSetup{
		state: state, hab: "laptop", getenv: func(string) string { return "" }, logf: t.Logf, sender: sent,
		connected: func(string) bool { return false },
		label:     func(c string) string { return map[string]string{"c-phone": "phone"}[c] },
		publish:   func(structure.Event) {},
	}, st, agents)
	defer pushes.Close()
	if !pushes.Enabled() {
		t.Fatal("push did not load")
	}
	token := strings.Repeat("ab", 32)
	if err := pushes.Register("c-phone", api.PushRegisterParams{DeviceID: "dev1", Token: token, Environment: "development", Kinds: []api.PushKind{"permission"}}); err != nil {
		t.Fatal(err)
	}
	keys := &keyLog{}
	a := &agentService{agents: agents, gate: harness.NewGate(time.Minute), input: keys, logf: t.Logf, answering: pushes.answering}
	agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	_, request := agents.Hook("p", claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`, 2))
	pushes.notifier.Sync()
	al := sent.alerts()
	if len(al) != 1 || title(al[0]) != "Run make test?" || al[0]["collapse"] != "r-"+request {
		t.Fatalf("banner: %v", al)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- a.Answer(ctx, "c-phone", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceAllow, Via: api.AnswerViaBanner})
	}()
	waitUntil(t, "the answer's keys", func() bool { keys.mu.Lock(); defer keys.mu.Unlock(); return len(keys.writes) == 1 })
	forward.Store(false)
	agents.Report("p", progstatus.Report{State: "working", App: "claude"}, time.Now().UnixMilli(), true)
	if err := <-done; err != nil {
		t.Fatalf("answer: %v", err)
	}
	pushes.notifier.Sync()
	al = sent.alerts()
	if len(al) != 2 || title(al[1]) != "Answered on phone" || al[1]["collapse"] != "r-"+request {
		t.Fatalf("withdrawal: %v", al)
	}
	err = a.Answer(ctx, "c-mac", api.ApprovalAnswerParams{Pane: "p", Request: request, Choice: api.ApprovalChoiceDeny})
	if errKind(err) != api.ErrorKindConflict {
		t.Fatalf("second answer: %v", err)
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if len(keys.writes) != 1 {
		t.Fatalf("keys %q", keys.writes)
	}
}

// TestPushAskContext: while Capcom runs on another host, a notice.raise banner carries the
// notice id as capcom_context with this hab's device id as the host, and the ask category; a
// permission banner carries the id of the notice gff-hab kept it as, with approval-ask. Both
// notices read back from the saved store, also after it is opened again (a restart). Keeping
// raises no notice.raised event. A request in a folder the read scope denies is kept without
// its command. A device that registered no Ask feature gets the old categories.
func TestPushAskContext(t *testing.T) {
	state := t.TempDir()
	writePushConfig(t, state)
	st, err := structure.Open(structure.Config{Dir: filepath.Join(state, "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sent := &sentLog{}
	var pushes *pushService
	agents := harness.NewStore(harness.Config{Publish: func(e structure.Event) {
		if s, ok := e.Data.(api.AgentState); ok && pushes != nil {
			pushes.notifier.State(s)
		}
	}})
	var raisedEvents atomic.Int32
	pushes = newPushService(pushSetup{
		state: state, hab: "laptop", self: "dev-self", capcom: func() string { return "dev-home" },
		getenv: func(string) string { return "" }, logf: t.Logf, sender: sent,
		connected: func(string) bool { return false }, label: func(string) string { return "" },
		publish: func(e structure.Event) {
			if e.Type == control.EventNoticeRaised {
				raisedEvents.Add(1)
			}
		},
		notices: openNoticeLog(state, nil, t.Logf),
		denied:  func(folder string) bool { return folder == "/secret/proj" },
	}, st, agents)
	defer pushes.Close()
	if err := pushes.Register("c-phone", api.PushRegisterParams{DeviceID: "dev1", Token: strings.Repeat("ab", 32), Environment: "development",
		Kinds: []api.PushKind{"permission"}, Features: []string{push.FeatureAsk}}); err != nil {
		t.Fatal(err)
	}
	ask := func(v map[string]any) (category any, ctx map[string]any, home any) {
		fd := v["flightdeck"].(map[string]any)
		c, _ := fd["capcom_context"].(map[string]any)
		return v["aps"].(map[string]any)["category"], c, fd["capcom_home"]
	}

	raised, err := pushes.Raise("c-script", api.NoticeRaiseParams{Title: "Deploy failed", Body: "exit 2"})
	if err != nil {
		t.Fatal(err)
	}
	pushes.notifier.Sync()
	al := sent.alerts()
	if len(al) != 1 {
		t.Fatalf("alerts %v", al)
	}
	cat, c, home := ask(al[0])
	if cat != push.CategoryAsk || home != "dev-home" || c["kind"] != "notification" || c["ref"] != raised.ID || c["host"] != "dev-self" {
		t.Fatalf("notice banner: category %v, context %v, home %v", cat, c, home)
	}

	agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
	agents.Hook("p", claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"make test"}}`, 2))
	pushes.notifier.Sync()
	al = sent.alerts()
	if len(al) != 2 || title(al[1]) != "Run make test?" {
		t.Fatalf("permission banner: %v", al)
	}
	cat, c, _ = ask(al[1])
	kept, _ := c["ref"].(string)
	if cat != push.CategoryApprovalAsk || c["kind"] != "notification" || kept == "" || kept == raised.ID || c["host"] != "dev-self" {
		t.Fatalf("permission banner: category %v, context %v", cat, c)
	}
	for _, l := range []*noticeLog{pushes.notices, openNoticeLog(state, nil, t.Logf)} {
		if n, ok := l.get(raised.ID); !ok || n.Title != "Deploy failed" || n.Body != "exit 2" {
			t.Errorf("raised notice: %v %+v", ok, n)
		}
		if n, ok := l.get(kept); !ok || n.Title != "Run make test?" || n.Body != "make test" || n.Level != "time-sensitive" {
			t.Errorf("kept notice: %v %+v", ok, n)
		}
	}
	if n := raisedEvents.Load(); n != 1 {
		t.Errorf("%d notice.raised events, want 1: a kept banner raises none", n)
	}

	// A request in a denied folder: the banner still shows the command to this device, but the
	// kept notice holds only what a hidden banner shows.
	agents.Hook("p", claudeEnv("PermissionRequest", `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","cwd":"/secret/proj","tool_input":{"command":"curl -H token"}}`, 3))
	pushes.notifier.Sync()
	al = sent.alerts()
	_, c, _ = ask(al[len(al)-1])
	ref, _ := c["ref"].(string)
	if n, ok := pushes.notices.get(ref); !ok || n.Title != "Approval needed" || strings.Contains(n.Title+n.Body, "curl") {
		t.Errorf("kept notice in a denied folder: %v %+v", ok, n)
	}

	// An app that registered no Ask categories keeps the old ones; the context rides along.
	if err := pushes.Register("c-phone", api.PushRegisterParams{DeviceID: "dev1", Token: strings.Repeat("ab", 32), Environment: "development", Kinds: []api.PushKind{"permission"}}); err != nil {
		t.Fatal(err)
	}
	pushes.Raise("c-script", api.NoticeRaiseParams{Title: "Backup done"})
	pushes.notifier.Sync()
	al = sent.alerts()
	if cat, c, _ := ask(al[len(al)-1]); cat != nil || c["ref"] == nil {
		t.Errorf("old app notice: category %v, context %v", cat, c)
	}
}

// TestServePush: with no push config the welcome has no push capability, push.register
// answers unavailable, and `gff-hab notify` still raises a notice for live clients. With a
// valid config the capability is on and tokens register. Nothing contacts Apple: no push is
// sent in either case.
func TestServePush(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	state := filepath.Join(d, "state")
	off := []string{push.Env.KeyCommand + "=", push.Env.KeyFile + "=", push.Env.KeyID + "=", push.Env.TeamID + "=", push.Env.Topic + "="}
	s := startServeWith(t, sock, state, off)
	if slices.Contains(welcome(t, sock).Capabilities, "push") {
		t.Fatal("push capability with no config")
	}
	cmd := exec.Command(bin, "api", "call", "-socket", sock, "push.register", `{"device_id":"d","token":"`+strings.Repeat("ab", 32)+`","environment":"production","kinds":["permission"]}`)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "unavailable") {
		t.Fatalf("push.register with no config: %v %q", err, stderr.String())
	}
	var res api.NoticeRaised
	if err := json.Unmarshal([]byte(run(t, "notify", "-socket", sock, "-level", "passive", "Backup finished", "All snapshots verified.")), &res); err != nil {
		t.Fatal(err)
	}
	if res.ID == "" || res.Pushed != 0 {
		t.Fatalf("notify: %+v", res)
	}
	s.stop(t)

	writePushConfig(t, state)
	startServeWith(t, sock, state, off)
	// The key command runs in the background; the capability is in each welcome once it ends.
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(welcome(t, sock).Capabilities, "push") {
		if time.Now().After(deadline) {
			t.Fatal("no push capability with a valid config")
		}
		time.Sleep(50 * time.Millisecond)
	}
	run(t, "api", "call", "-socket", sock, "push.register", `{"device_id":"d","token":"`+strings.Repeat("ab", 32)+`","environment":"production","kinds":["permission"]}`)
	fi, err := os.Stat(filepath.Join(state, "push", push.TokensFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
}

// TestFocusGoneSession: session.focus with active false for a session that no longer exists
// is accepted, so the client can clear its focus after a delete; active true still answers
// not_found.
func TestFocusGoneSession(t *testing.T) {
	state := t.TempDir()
	st, err := structure.Open(structure.Config{Dir: filepath.Join(state, "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newPushService(pushSetup{state: state, getenv: func(string) string { return "" }, logf: t.Logf, publish: func(structure.Event) {}}, st, nil)
	defer s.Close()
	if err := s.Focus("c1", api.SessionFocusParams{Session: "gone", Active: false}); err != nil {
		t.Fatalf("active false for a gone session: %v", err)
	}
	if err := s.Focus("c1", api.SessionFocusParams{Session: "gone", Active: true}); errKind(err) != api.ErrorKindNotFound {
		t.Fatalf("active true for a gone session: %v", err)
	}
}
