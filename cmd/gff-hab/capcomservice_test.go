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
}

const plantedToken = "planted-oauth-value-6e1f"

func newServiceEnv(t *testing.T, extraConfig string) *serviceEnv {
	t.Helper()
	e := newHolderEnv(t, 0)
	d := shortDir(t)
	stub := buildCapcomStub(t, d)
	cfg := filepath.Join(d, "hab.toml")
	if err := os.WriteFile(cfg, []byte("[capcom]\nengine = \"claude\"\nrestart_delay = \"1s\"\n"+extraConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	stubDir := filepath.Join(d, "stub")
	os.MkdirAll(stubDir, 0o700)
	e.env = []string{"GFF_HAB_CONFIG=" + cfg, "GFF_HAB_CAPCOM_ENGINE=" + stub, "STUB_DIR=" + stubDir, "CLAUDE_CODE_OAUTH_TOKEN=" + plantedToken}
	se := &serviceEnv{habEnv: e, stubDir: stubDir, config: cfg}
	t.Cleanup(se.killEngine)
	return se
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
	e := newServiceEnv(t, "")
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
	waitUntil(t, "hello", func() bool { return strings.Contains(e.stubLog(), "hello oauth_token "+hex.EncodeToString(sum[:8])) })
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

	// gff-hab restarts mid-turn: the engine keeps running, its reply arrives once.
	pid := e.enginePid()
	run(t, "capcom", "say", "-socket", e.sock, "slow")
	waitUntil(t, "the slow turn", func() bool { return strings.Contains(e.stubLog(), " slow") })
	e.srv.stop(t)
	os.WriteFile(filepath.Join(e.stubDir, "release"), nil, 0o600)
	e.serve()
	c = e.control()
	waitUntil(t, "the slow reply after the restart", func() bool { return countReplies(threadText(t, c), "echo: slow") == 1 })
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

func tokenOf(env string) string {
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
