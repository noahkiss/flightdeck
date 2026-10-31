package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// Budget caps the whole hook command: reading stdin and posting. A dead gff-hab fails at dial,
// at once; a hung one costs at most this. Codex caps its end hooks at one second.
const Budget = 900 * time.Millisecond

// Post sends one envelope to the address in a GFF_HAB value and waits for the answer, within
// ctx.
func Post(ctx context.Context, addr string, env harness.Envelope) error {
	sock, token, err := ParseAddr(addr)
	if err != nil {
		return err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hab/hooks/"+token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("hab answered %s", resp.Status)
	}
	return nil
}

// Invocation is one run of `gff-hab hook <harness> [event]`.
type Invocation struct {
	Harness string
	Event   string // empty: taken from the payload (hook_event_name, or an opencode event's type)
	Stdin   io.Reader
	Getenv  func(string) string
	Now     func() time.Time
	T       int64 // the event's time in ms, stamped by the caller (the opencode plugin); 0 means now
}

// Run reads the payload, wraps it and posts it. It returns why nothing was sent, or the post
// error; the command exits 0 either way, so a hook never fails a harness turn. It sends nothing
// when GFF_HAB is unset, and nothing for a Claude Code run whose entrypoint is set and is not
// the interactive TUI (a nested `claude -p`).
func Run(ctx context.Context, in Invocation) error {
	payload, err := io.ReadAll(io.LimitReader(in.Stdin, MaxBody))
	if err != nil {
		return err
	}
	addr := in.Getenv(Env)
	if addr == "" {
		return ErrNoHab
	}
	if in.Harness == detect.KindClaude {
		if ep := in.Getenv("CLAUDE_CODE_ENTRYPOINT"); ep != "" && ep != "cli" {
			return fmt.Errorf("claude entrypoint %q is not the interactive TUI; not sent", ep)
		}
	}
	payload = bytes.TrimSpace(payload)
	if !json.Valid(payload) {
		return fmt.Errorf("hook payload on stdin is not JSON")
	}
	event := in.Event
	if event == "" {
		var p struct {
			HookEventName string `json:"hook_event_name"`
			Type          string `json:"type"`
		}
		json.Unmarshal(payload, &p)
		event = p.HookEventName
		if event == "" {
			event = p.Type
		}
		if event == "" {
			return fmt.Errorf("no event name: give one after the harness")
		}
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	t := in.T
	if t <= 0 {
		t = now().UnixMilli()
	}
	env := harness.Envelope{Harness: in.Harness, Event: event, T: t, N: 1, Payload: payload}
	return Post(ctx, addr, env)
}
