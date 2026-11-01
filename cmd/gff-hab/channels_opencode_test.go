package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/opencode"
)

// TestChannelsOpencode: channel mail to an opencode pane gff-hab started goes through the
// pane's server (the fake opencode): the server gets the enveloped text once, as one prompt,
// and the sender sees delivered.
func TestChannelsOpencode(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	fake := buildFakeOpencode(t)
	addr := startSender(t, e)
	c := e.control()
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	var ss api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "bob", Harness: "opencode", Argv: []string{fake}, Cwd: "/",
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "DIR=" + e.work}}, &ss); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(e.work, name)); return string(b) }
	emit := func(line string) {
		f, err := os.OpenFile(filepath.Join(e.work, "emit"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(line + "\n")
		f.Close()
	}
	waitUntil(t, "a bound session", func() bool {
		emit(`{"type":"session.created","data":{"sessionID":"ses_1","version":"2.2.5"}}`)
		time.Sleep(100 * time.Millisecond)
		st := agentStates(t, e.sock)[ss.Pane]
		return st.Session == "ses_1" && st.State == api.AgentStatusIdle
	})

	text := "look at\nthis"
	id, _ := sendFromPane(t, addr, "bob", text)
	if r := channelStatus(t, e.sock, id, 20000); r.State != api.ChannelStateDelivered {
		t.Fatalf("record %+v", r)
	}
	want := "ses_1: " + channels.Envelope(id, "alice", text) + "\n"
	if got := read("prompts"); got != want {
		t.Fatalf("prompts %q, want %q", got, want)
	}
	// Later delivery rounds must not send it again.
	time.Sleep(1500 * time.Millisecond)
	if got := read("prompts"); got != want {
		t.Fatalf("prompts after a while %q, want it once", got)
	}
	if out, err := os.ReadFile(filepath.Join(e.work, "tui.args")); err != nil || len(out) == 0 {
		t.Fatalf("the TUI never ran: %v", err)
	}
}

// fakeOpencodeTUI reports opencode over OSC 7501 with no server gff-hab follows: an opencode
// started by hand, or by another tool, in a plain pane.
const fakeOpencodeTUI = `
stty raw -echo
printf '\033]7501;state=idle:app=opencode\033\\'
while [ ! -e "$DIR/end" ]; do sleep 0.05; done
`

// TestChannelsOpencodeNoServer: an opencode pane whose server gff-hab does not follow cannot
// take channel mail: the message is refused, not held, and nothing reaches the pane.
func TestChannelsOpencodeNoServer(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	addr := startSender(t, e)
	ss := createSession(t, e.sock, api.SessionCreateParams{
		Name: "bob", Argv: []string{"/bin/sh", "-c", fakeOpencodeTUI}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}})
	waitState(t, e.sock, ss.Pane, "bob reports opencode idle", func(s api.AgentState) bool {
		return s.Harness == "opencode" && s.State == api.AgentStatusIdle
	})
	id, _ := sendFromPane(t, addr, "bob", "anyone there")
	r := channelStatus(t, e.sock, id, 20000)
	if r.State != api.ChannelStateRefused || !strings.Contains(r.Reason, "follows no opencode server") {
		t.Fatalf("record %+v", r)
	}
}

// TestChannelsOpencodePromptFailure: a prompt the server never got holds the channel message
// for a later round; a prompt the server may have queued (no answer, a dropped connection, a
// 5xx) is not_delivered and never sent again, as a paste without its Enter is.
func TestChannelsOpencodePromptFailure(t *testing.T) {
	saved := opencodeTimeout
	opencodeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { opencodeTimeout = saved })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch {
		case strings.Contains(r.URL.Path, "ses_400"):
			w.WriteHeader(http.StatusBadRequest)
		case strings.Contains(r.URL.Path, "ses_500"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "ses_hang"):
			<-release
		case strings.Contains(r.URL.Path, "ses_drop"):
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	defer srv.Close()
	defer close(release)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()

	a := &agentService{}
	for _, c := range []struct {
		what, base, session string
		want                error
	}{
		{"no server listening", closed, "ses_1", channels.ErrBusy},
		{"refused with 400", srv.URL, "ses_400", channels.ErrBusy},
		{"failed with 500", srv.URL, "ses_500", channels.ErrNotDelivered},
		{"no answer in time", srv.URL, "ses_hang", channels.ErrNotDelivered},
		{"connection dropped", srv.URL, "ses_drop", channels.ErrNotDelivered},
	} {
		oc, err := opencode.New(c.base, "pw")
		if err != nil {
			t.Fatal(err)
		}
		got := deliverErr(a.promptOpencode(context.Background(), oc, api.AgentState{Session: c.session}, "hi"))
		if !errors.Is(got, c.want) {
			t.Errorf("%s: %v, want %v", c.what, got, c.want)
		}
	}
}
