package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
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

// PostContext sends one envelope of a context event (ContextEvent) and returns the text the hab
// adds for the agent, or "" for none (logbook.md 11.3). The hab answers 204 for none, else 200
// with {"text": ...}.
func PostContext(ctx context.Context, addr string, env harness.Envelope) (string, error) {
	b, err := postQuery(ctx, addr, env, "?context=1")
	if err != nil || b == nil {
		return "", err
	}
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return "", fmt.Errorf("hab answer: %w", err)
	}
	return a.Text, nil
}

func post(ctx context.Context, addr string, env harness.Envelope, wait bool) ([]byte, error) {
	q := ""
	if wait {
		q = "?wait=1"
	}
	return postQuery(ctx, addr, env, q)
}

func postQuery(ctx context.Context, addr string, env harness.Envelope, query string) ([]byte, error) {
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
	url := "http://hab/hooks/" + token + query
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
	case resp.StatusCode == http.StatusOK && query != "":
		// The hab keeps an answer within MaxHookAnswer (harness-adapters.md, Question answers);
		// a longer body is an error, not a cut one.
		b, err := io.ReadAll(io.LimitReader(resp.Body, harness.MaxHookAnswer+1))
		if err == nil && len(b) > harness.MaxHookAnswer {
			err = fmt.Errorf("hab answer longer than %d bytes", harness.MaxHookAnswer)
		}
		return b, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil, &StatusError{Status: resp.Status, Code: resp.StatusCode}
}

// ErrSkipped means the hook sent nothing by design: a nested Claude Code run.
var ErrSkipped = errors.New("not sent")

// ErrPayload means the payload on stdin could not be sent: not JSON, or no event name.
var ErrPayload = errors.New("bad payload")

// The failure kinds a failed hook run writes to hook.log (diagnostics.md 4.4).
const (
	FailNoHab   = "no_hab"  // gff-hab does not answer on its hook socket
	FailTimeout = "timeout" // the run reached its budget
	FailStatus  = "status"  // gff-hab answered with an error status
	FailPayload = "payload" // the payload could not be sent
	FailOther   = "other"
)

// FailureKind is the kind of a hook run's error for hook.log, or "" for a run that is no
// failure: it sent its envelope, or it runs outside any pane gff-hab owns (ErrNoHab), or it sent
// nothing by design (ErrSkipped).
func FailureKind(err error) string {
	var se *StatusError
	switch {
	case err == nil, errors.Is(err, ErrNoHab), errors.Is(err, ErrSkipped):
		return ""
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return FailTimeout
	case Unreachable(err):
		return FailNoHab
	case errors.As(err, &se):
		return FailStatus
	case errors.Is(err, ErrPayload):
		return FailPayload
	}
	return FailOther
}

// Invocation is one run of `gff-hab hook <harness> [event]`.
type Invocation struct {
	Harness string
	Event   string // empty: taken from the payload's hook_event_name
	Stdin   io.Reader
	Getenv  func(string) string
	Now     func() time.Time
	T       int64 // the event's time in ms, stamped by the caller (-t); 0 means now
	// Wait asks the hab to hold a permission request until the app answers (PostWait). Only
	// the PermissionRequest of Claude Code and Codex waits; any other event posts as usual.
	Wait bool
	// Stdout receives the harness's hook output: a wait's decision, or the context the hab adds
	// at a context event (ContextEvent). Nil prints nothing, and context events post as others.
	Stdout io.Writer
	// FileBrief builds the brief from the hab's file when gff-hab does not answer at the brief
	// event (logbook.md 11.3), for the working folder cwd (the payload's, else ""). Nil: none.
	FileBrief func(ctx context.Context, cwd string) (string, error)
	// OnEvent, if set, is called with the event once Run knows it: Event, else the payload's
	// hook_event_name. Run calls it on its own goroutine.
	OnEvent func(event string)
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
			return fmt.Errorf("%w: claude entrypoint %q is not the interactive TUI", ErrSkipped, ep)
		}
	}
	payload = bytes.TrimSpace(payload)
	if !json.Valid(payload) {
		return fmt.Errorf("%w: hook payload on stdin is not JSON", ErrPayload)
	}
	event := in.Event
	if event == "" {
		var p struct {
			HookEventName string `json:"hook_event_name"`
		}
		json.Unmarshal(payload, &p)
		event = p.HookEventName
		if event == "" {
			return fmt.Errorf("%w: no event name: give one after the harness", ErrPayload)
		}
	}
	if in.OnEvent != nil {
		in.OnEvent(event)
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	t := in.T
	if t <= 0 {
		t = now().UnixMilli()
	}
	if in.Harness == detect.KindClaude && event == "SessionStart" {
		exportSession(in.Getenv(ClaudeEnvFile), payload)
	}
	env := harness.Envelope{Harness: in.Harness, Event: event, T: t, N: 1, Payload: payload}
	if habit, ok := ContextEvent(in.Harness, event); ok && in.Stdout != nil {
		text, err := PostContext(ctx, addr, env)
		if err != nil && habit == "brief" && in.FileBrief != nil && Unreachable(err) {
			var p struct {
				Cwd string `json:"cwd"`
			}
			json.Unmarshal(payload, &p)
			text, err = in.FileBrief(ctx, p.Cwd)
		}
		if err != nil || text == "" {
			return err
		}
		_, err = in.Stdout.Write(ContextOutput(in.Harness, event, text))
		return err
	}
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
// Claude Code and Codex, which share one shape. An allow keeps its updatedInput. It returns nil for an answer with no decision:
// the hook then prints nothing, and the harness shows its own dialog.
func PermissionDecision(answer []byte) ([]byte, error) {
	var a struct {
		Behavior     string          `json:"behavior"`
		Message      string          `json:"message,omitempty"`
		UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	}
	if err := json.Unmarshal(answer, &a); err != nil {
		return nil, fmt.Errorf("hab answer: %w", err)
	}
	if a.Behavior != "allow" && a.Behavior != "deny" {
		return nil, nil
	}
	type decision struct {
		Behavior     string          `json:"behavior"`
		Message      string          `json:"message,omitempty"`
		UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
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
	} else if len(a.UpdatedInput) > 0 && string(a.UpdatedInput) != "null" {
		// A Claude Code question's input with the owner's answers (harness-adapters.md,
		// Question answers).
		out.HookSpecificOutput.Decision.UpdatedInput = a.UpdatedInput
	}
	b, err := harness.MarshalPlain(out)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ContextEvent reports whether a harness's event asks gff-hab for context, and names the
// Logbook habit it carries (logbook.md 11.3): the brief at a session's start (Claude Code and
// Codex SessionStart, which also fires after a compaction and on resume), the reminder at a prompt
// and the closeout at a stop. Claude Code's PostToolUse carries no habit ("") but asks too: the
// context nudge (harness-adapters.md) rides on it and on UserPromptSubmit. opencode has no hooks:
// gff-hab adds its brief through the pane's server (logbook.md 11.4).
func ContextEvent(harness, event string) (habit string, ok bool) {
	switch harness {
	case detect.KindClaude, detect.KindCodex:
		switch event {
		case "SessionStart":
			return "brief", true
		case "UserPromptSubmit":
			return "reminder", true
		case "Stop":
			return "closeout", true
		case "PostToolUse":
			return "", harness == detect.KindClaude
		}
	}
	return "", false
}

// ContextOutput is the hook output that hands text to the agent in harness's shape (logbook.md
// 11.3). Claude Code and Codex share it: SessionStart, UserPromptSubmit and Claude Code's
// PostToolUse add context with hookSpecificOutput.additionalContext; Stop blocks once with the text as the reason, so the
// agent takes one more turn.
func ContextOutput(harness, event, text string) []byte {
	var v any
	switch {
	case event == "Stop":
		v = struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}{"block", text}
	default:
		type specific struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		}
		v = struct {
			HookSpecificOutput specific `json:"hookSpecificOutput"`
		}{specific{event, text}}
	}
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

// Unreachable reports whether err means nothing answers at the hab's socket: nothing listens on
// it, or there is no socket file. Only then is gff-hab down.
func Unreachable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// ClaudeEnvFile names the file Claude Code gives its SessionStart hook: each `export` line the
// hook appends there reaches the session's later tool commands.
const ClaudeEnvFile = "CLAUDE_ENV_FILE"

// exportSession appends AGENT_KIND and AGENT_SESSION_ID for the session in a Claude Code
// SessionStart payload to the file Claude Code named (harness-adapters.md, Environment contract).
// A later start in the same process (/clear, a resume, a compaction) appends again, and the last
// line wins. An id outside letters, digits, '-' and '_' is not written, so no value needs shell
// quoting. It fails silently: a hook never fails a turn.
func exportSession(file string, payload []byte) {
	if file == "" {
		return
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(payload, &p) != nil || !safeID(p.SessionID) {
		return
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "export AGENT_KIND=%s\nexport AGENT_SESSION_ID=%s\n", detect.KindClaude, p.SessionID)
	f.Close()
}

// safeID reports whether id is 1 to 128 of letters, digits, '-' and '_'.
func safeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
