package lbcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// Which door a call takes (logbook.md 10.5): the file only when the hab's socket does not answer;
// never for a write from a pane; never for an answer the hab gave, such as an HTTP status.

const token = "0123456789abcdef0123"

// shortDir is a folder short enough for a Unix socket path.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// newState makes a state folder with a Logbook file in it, and returns the folder.
func newState(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	st, err := logbook.Open(context.Background(), filepath.Join(state, "logbook.db"), logbook.Options{Origin: "SHA256:hab"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	return state
}

// opsIn counts the ops in the state folder's file.
func opsIn(t *testing.T, state string) int {
	t.Helper()
	st, err := logbook.Open(context.Background(), filepath.Join(state, "logbook.db"), logbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.Ops
}

// paneRoute serves the pane route on a Unix socket with handler h.
func paneRoute(t *testing.T, sock string, h http.HandlerFunc) {
	t.Helper()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

// inPane runs Main with env as the process environment.
func inPane(t *testing.T, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(Env{Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return env[k] }, Name: "gff lb"})
	return out.String(), errb.String(), code
}

func fastRetry(t *testing.T, d time.Duration) {
	t.Helper()
	old, oldEvery := paneRetry, paneRetryEvery
	paneRetry, paneRetryEvery = d, 20*time.Millisecond
	t.Cleanup(func() { paneRetry, paneRetryEvery = old, oldEvery })
}

// An HTTP status from the pane route is the hab's answer: no read or write goes to the file.
func TestPaneStatusIsAnAnswer(t *testing.T) {
	state := newState(t)
	sock := filepath.Join(shortDir(t), "hook.sock")
	paneRoute(t, sock, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unknown token", http.StatusNotFound) })
	env := map[string]string{hook.Env: hook.Addr(sock, token), "GFF_HAB_STATE": state}
	for _, args := range [][]string{{"new", "task", "x"}, {"ls"}} {
		_, errs, code := inPane(t, env, args...)
		if code != 3 || !strings.Contains(errs, "404") || strings.Contains(errs, "directly") {
			t.Fatalf("%v: exit %d %s", args, code, errs)
		}
	}
	if n := opsIn(t, state); n != 0 {
		t.Fatalf("the file gained %d ops", n)
	}
}

// A $GFF_HAB that does not parse is an error, not a reason to use the file.
func TestMalformedPaneAddr(t *testing.T) {
	state := newState(t)
	env := map[string]string{hook.Env: "not-an-address", "GFF_HAB_STATE": state}
	for _, args := range [][]string{{"new", "task", "x"}, {"ls"}} {
		_, errs, code := inPane(t, env, args...)
		if code != 1 || !strings.Contains(errs, "$GFF_HAB") || strings.Contains(errs, "directly") {
			t.Fatalf("%v: exit %d %s", args, code, errs)
		}
	}
	if n := opsIn(t, state); n != 0 {
		t.Fatalf("the file gained %d ops", n)
	}
}

// With gff-hab down, a pane reads the file (found through the GFF_HAB_STATE gff-hab gave the
// pane), and its write waits for gff-hab, then fails, writing nothing.
func TestPaneHabDown(t *testing.T) {
	fastRetry(t, 300*time.Millisecond)
	state := newState(t)
	sock := filepath.Join(shortDir(t), "hook.sock") // nothing listens
	env := map[string]string{hook.Env: hook.Addr(sock, token), "GFF_HAB_STATE": state}
	out, errs, code := inPane(t, env, "ls")
	if code != 0 || !strings.Contains(out, "no records") || !strings.Contains(errs, "used "+filepath.Join(state, "logbook.db")+" directly") {
		t.Fatalf("a read: %d %q %q", code, out, errs)
	}
	start := time.Now()
	_, errs, code = inPane(t, env, "new", "task", "x")
	if code != 1 || !strings.Contains(errs, "never to the file") || time.Since(start) < paneRetry {
		t.Fatalf("a write: %d %s after %s", code, errs, time.Since(start))
	}
	if n := opsIn(t, state); n != 0 {
		t.Fatalf("the file gained %d ops", n)
	}
}

// A pane's write that finds gff-hab down goes through once gff-hab is back within the wait.
func TestPaneWriteWaitsForHab(t *testing.T) {
	fastRetry(t, 10*time.Second)
	state := newState(t)
	sock := filepath.Join(shortDir(t), "hook.sock")
	var calls atomic.Int32
	time.AfterFunc(200*time.Millisecond, func() {
		paneRoute(t, sock, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			b, _ := io.ReadAll(r.Body)
			var req struct {
				Method string `json:"method"`
			}
			json.Unmarshal(b, &req)
			res, _ := json.Marshal(api.LogbookWritten{State: api.LogbookWriteStateApplied, IDs: []string{"task-aaaaaaaa"}, Ops: []string{}})
			json.NewEncoder(w).Encode(map[string]any{"result": json.RawMessage(res)})
		})
	})
	env := map[string]string{hook.Env: hook.Addr(sock, token), "GFF_HAB_STATE": state}
	out, errs, code := inPane(t, env, "new", "task", "x")
	if code != 0 || strings.TrimSpace(out) != "task-aaaaaaaa" || errs != "" || calls.Load() != 1 {
		t.Fatalf("write: %d %q %q calls %d", code, out, errs, calls.Load())
	}
	if n := opsIn(t, state); n != 0 {
		t.Fatalf("the file gained %d ops", n)
	}
}

// In a pane, --socket reaches the control socket for reads only.
func TestPaneSocketFlagReadsOnly(t *testing.T) {
	state := newState(t)
	env := map[string]string{hook.Env: hook.Addr("/nonexistent/hook.sock", token), "GFF_HAB_STATE": state}
	none := filepath.Join(shortDir(t), "none.sock")
	_, errs, code := inPane(t, env, "--socket", none, "new", "task", "x")
	if code != 1 || !strings.Contains(errs, "not --socket") {
		t.Fatalf("a write: %d %s", code, errs)
	}
	if out, errs, code := inPane(t, env, "--socket", none, "ls"); code != 0 || !strings.Contains(out, "no records") {
		t.Fatalf("a read: %d %q %q", code, out, errs)
	}
	if n := opsIn(t, state); n != 0 {
		t.Fatalf("the file gained %d ops", n)
	}
}

// A control socket that takes the connection and drops it is a hab that answered badly, not one
// that is down: no fallback.
func TestControlSocketBrokenNoFallback(t *testing.T) {
	state := newState(t)
	sock := filepath.Join(shortDir(t), "hab.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, errs, code := inPane(t, map[string]string{}, "--socket", sock, "--state", state, "new", "task", "x")
	if code == 0 || strings.Contains(errs, "directly") {
		t.Fatalf("exit %d %s", code, errs)
	}
	if n := opsIn(t, state); n != 0 {
		t.Fatalf("the file gained %d ops", n)
	}
}

func TestFindFlagged(t *testing.T) {
	state := newState(t)
	runCLI(t, state, "", "new", "task", "plain")
	if out, _, code := runCLI(t, state, "", "find", "plain", "--flagged"); code != 0 || !strings.Contains(out, "no records") {
		t.Fatalf("find --flagged: %d\n%s", code, out)
	}
	if out, _, _ := runCLI(t, state, "", "find", "plain"); !strings.Contains(out, "plain") {
		t.Fatalf("find:\n%s", out)
	}
}
