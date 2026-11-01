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

// WaitBudget caps a hook run with -wait: the hab's longest wait plus a margin. The header of
// the answer must still come within Budget.
const WaitBudget = harness.MaxPermissionWait + 5*time.Second

// Post sends one envelope to the address in a GFF_HAB value and waits for the answer, within
// ctx.
func Post(ctx context.Context, addr string, env harness.Envelope) error {
	_, err := post(ctx, addr, env, false)
	return err
}

// PostWait sends one envelope and asks the hab to hold the answer until the app decides
// (harness-adapters.md, blocking PermissionRequest hook). The hab answers 204 at once when it
// does not hold this hook; then the answer is nil. Else it answers 200 at once and sends the body
// when it has a decision or gives up; the body is the decision as a JSON object, or {} for none.
func PostWait(ctx context.Context, addr string, env harness.Envelope) ([]byte, error) {
	return post(ctx, addr, env, true)
}

func post(ctx context.Context, addr string, env harness.Envelope, wait bool) ([]byte, error) {
	sock, token, err := ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: Budget,
	}
	defer tr.CloseIdleConnections()
	url := "http://hab/hooks/" + token
	if wait {
		url += "?wait=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil
	case resp.StatusCode == http.StatusOK && wait:
		return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil, fmt.Errorf("hab answered %s", resp.Status)
}

// Invocation is one run of `gff-hab hook <harness> [event]`.
type Invocation struct {
	Harness string
	Event   string // empty: taken from the payload (hook_event_name, or an opencode event's type)
	Stdin   io.Reader
	Getenv  func(string) string
	Now     func() time.Time
	T       int64 // the event's time in ms, stamped by the caller (the opencode plugin); 0 means now
	// Wait asks the hab to hold a permission request until the app answers (PostWait). Only
	// the PermissionRequest of Claude Code and Codex waits; any other event posts as usual.
	Wait bool
	// Stdout receives the harness's hook output when a wait ends with a decision.
	Stdout io.Writer
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
	if !in.Wait || !Waits(in.Harness) || event != "PermissionRequest" || in.Stdout == nil {
		return Post(ctx, addr, env)
	}
	answer, err := PostWait(ctx, addr, env)
	if err != nil || answer == nil {
		return err
	}
	out, err := PermissionDecision(answer)
	if err != nil || out == nil {
		return err
	}
	_, err = in.Stdout.Write(out)
	return err
}

// Waits reports whether harness's PermissionRequest hook can wait for the app's answer: Claude
// Code and Codex share the hook output shape.
func Waits(harness string) bool {
	return harness == detect.KindClaude || harness == detect.KindCodex
}

// PermissionDecision turns the hab's answer body into the PermissionRequest hook output of
// Claude Code and Codex, which share one shape. It returns nil for an answer with no decision:
// the hook then prints nothing, and the harness shows its own dialog.
func PermissionDecision(answer []byte) ([]byte, error) {
	var a struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
	}
	if err := json.Unmarshal(answer, &a); err != nil {
		return nil, fmt.Errorf("hab answer: %w", err)
	}
	if a.Behavior != "allow" && a.Behavior != "deny" {
		return nil, nil
	}
	type decision struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
	}
	out := struct {
		HookSpecificOutput struct {
			HookEventName string   `json:"hookEventName"`
			Decision      decision `json:"decision"`
		} `json:"hookSpecificOutput"`
	}{}
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	out.HookSpecificOutput.Decision = decision{Behavior: a.Behavior}
	if a.Behavior == "deny" {
		out.HookSpecificOutput.Decision.Message = a.Message
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
