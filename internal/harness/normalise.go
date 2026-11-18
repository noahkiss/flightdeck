// Package harness turns harness hook events into agent state (docs/specs/harness-adapters.md,
// Hook endpoint and State event model). Normalise maps one native event of Claude Code, Codex
// or opencode to an Update. Store applies updates per pane under the spec's precedence and
// ordering rules, keeps the last state of each pane for snapshots, and emits an agent.state
// event for every accepted change.
package harness

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// Envelope is what the hook command posts: the native payload, untouched, wrapped with the
// harness, the native event name, the client clock and a per-process counter.
type Envelope struct {
	Harness string          `json:"harness"`
	Event   string          `json:"event"`
	T       int64           `json:"t"`                 // client clock, ms since the Unix epoch
	N       uint64          `json:"n"`                 // per-process counter of the hook command
	Version string          `json:"version,omitempty"` // harness version, when the caller knows it
	Payload json.RawMessage `json:"payload"`
}

// Update is what one native event means for its pane. A zero State changes no state.
type Update struct {
	Session  string // the native session id in the payload
	Bind     bool   // a session start: bind the pane to Session
	Unbind   bool   // a session end: the session no longer lives
	Subagent bool   // from a subagent: never binds or rebinds the pane
	NewTurn  bool   // a prompt submit: resets source precedence
	Turn     string // the native turn or prompt id, where the harness gives one

	State   api.AgentStatus
	Kind    api.BlockedKind
	Request *api.PermissionRequest

	Resolves string // the permission request id this event resolves
	Version  string // the harness version, where the payload carries one
	// Failure is the harness's own kind of failure, with state error, where it gives one
	// (Claude Code's StopFailure error: rate_limit, authentication_failed and the like).
	Failure string
}

var (
	// ErrUnknownHarness: the envelope names a harness with no adapter. Logged and dropped.
	ErrUnknownHarness = errors.New("unknown harness")
	// ErrUnknownEvent: the adapter does not use this native event. Logged at debug and dropped.
	ErrUnknownEvent = errors.New("unknown event")
)

// Harnesses lists the harnesses with an adapter.
var Harnesses = []string{detect.KindClaude, detect.KindCodex, detect.KindOpencode}

// Normalise maps one envelope to an Update.
func Normalise(env Envelope) (Update, error) {
	var u Update
	var err error
	switch env.Harness {
	case detect.KindClaude, detect.KindCodex:
		u, err = normaliseHookJSON(env)
	case detect.KindOpencode:
		// opencode reports through the server gff-hab follows (NormaliseOpencodeServer), never
		// through hooks. A post from an old plugin is dropped.
		return Update{}, fmt.Errorf("%w opencode %s: opencode state comes from its server", ErrUnknownEvent, env.Event)
	default:
		return Update{}, fmt.Errorf("%w %q", ErrUnknownHarness, env.Harness)
	}
	if err != nil {
		return Update{}, err
	}
	if env.Version != "" {
		u.Version = env.Version
	}
	return u, nil
}

// hookPayload holds the fields Claude Code and Codex hook payloads share, plus each one's own.
// Unknown fields are ignored.
type hookPayload struct {
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	AgentID          string          `json:"agent_id"`          // Claude: set inside a subagent
	PromptID         string          `json:"prompt_id"`         // Claude
	TurnID           string          `json:"turn_id"`           // Codex
	ToolName         string          `json:"tool_name"`         // tool events
	ToolInput        json.RawMessage `json:"tool_input"`        // tool events
	ToolUseID        string          `json:"tool_use_id"`       // tool events
	NotificationType string          `json:"notification_type"` // Claude Notification
	Error            string          `json:"error"`             // Claude StopFailure: the failure kind
	Version          string          `json:"version"`
	CliVersion       string          `json:"cli_version"`
}

// normaliseHookJSON handles Claude Code and Codex. Their hook events share names and payload
// shapes; Claude adds Notification and StopFailure, Codex adds Interrupt.
func normaliseHookJSON(env Envelope) (Update, error) {
	var p hookPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return Update{}, fmt.Errorf("payload: %w", err)
	}
	u := Update{Session: p.SessionID, Subagent: p.AgentID != "", Version: firstNonEmpty(p.Version, p.CliVersion)}
	u.Turn = firstNonEmpty(p.PromptID, p.TurnID)
	claude := env.Harness == detect.KindClaude
	switch env.Event {
	case "SessionStart":
		u.Bind, u.State = !u.Subagent, api.AgentStatusIdle
	case "SessionEnd":
		u.Unbind, u.State = !u.Subagent, api.AgentStatusExited
	case "UserPromptSubmit":
		u.NewTurn, u.State = true, api.AgentStatusWorking
	case "PreToolUse":
		u.State = api.AgentStatusWorking
	case "PostToolUse":
		u.State, u.Resolves = api.AgentStatusWorking, p.ToolUseID
	case "PermissionRequest":
		u.State, u.Kind = api.AgentStatusBlocked, api.BlockedKindPermission
		u.Request = request(p.ToolUseID, p.ToolName, p.ToolInput, p.Cwd)
	case "Stop":
		u.State = api.AgentStatusDone
	case "Notification":
		if !claude {
			return Update{}, fmt.Errorf("%w %s %s", ErrUnknownEvent, env.Harness, env.Event)
		}
		switch p.NotificationType {
		case "permission_prompt":
			u.State, u.Kind = api.AgentStatusBlocked, api.BlockedKindPermission
		case "elicitation_dialog":
			u.State, u.Kind = api.AgentStatusBlocked, api.BlockedKindQuestion
		case "idle_prompt":
			u.State = api.AgentStatusIdle
		default:
			return Update{}, fmt.Errorf("%w %s Notification %q", ErrUnknownEvent, env.Harness, p.NotificationType)
		}
	case "StopFailure":
		if !claude {
			return Update{}, fmt.Errorf("%w %s %s", ErrUnknownEvent, env.Harness, env.Event)
		}
		u.State, u.Failure = api.AgentStatusError, p.Error
	case "PreCompact":
		// Compaction starts (harness-adapters.md, Self-compact): no state change; gff-hab's
		// self-compact and context nudge watch for it.
		if !claude {
			return Update{}, fmt.Errorf("%w %s %s", ErrUnknownEvent, env.Harness, env.Event)
		}
	case "Interrupt":
		if claude {
			return Update{}, fmt.Errorf("%w %s %s", ErrUnknownEvent, env.Harness, env.Event)
		}
		u.State = api.AgentStatusIdle
	default:
		return Update{}, fmt.Errorf("%w %s %s", ErrUnknownEvent, env.Harness, env.Event)
	}
	return u, nil
}

// request copies a permission request from a tool payload. Command is set only when the input
// carries the command as one string; Input always carries the input exactly.
func request(id, tool string, input json.RawMessage, cwd string) *api.PermissionRequest {
	r := &api.PermissionRequest{ID: id, Tool: tool, Cwd: cwd}
	if len(input) > 0 && string(input) != "null" {
		setInput(r, input)
		var in struct {
			Command      json.RawMessage `json:"command"`
			FilePath     string          `json:"file_path"`
			Path         string          `json:"path"`
			NotebookPath string          `json:"notebook_path"`
		}
		if json.Unmarshal(input, &in) == nil {
			var cmd string
			if json.Unmarshal(in.Command, &cmd) == nil {
				r.Command = cmd
			}
			for _, f := range []string{in.FilePath, in.Path, in.NotebookPath} {
				if f != "" {
					r.Files = append(r.Files, f)
				}
			}
		}
	}
	return r
}

// MaxInput bounds the tool input copied into a permission request. A larger one is left out
// and flagged, so one large write cannot fill a client's event backlog.
const MaxInput = 64 << 10

func setInput(r *api.PermissionRequest, input []byte) {
	if len(input) > MaxInput {
		r.Input, r.InputTruncated = nil, true
		return
	}
	r.Input = append(api.RawJSON(nil), input...)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// NetworkFailure reports whether a failure kind (AgentState.Error) is one the harness may get
// over once the network or a sign-in is back: a run that failed so right after a wake is tried
// once more (rounds.md 8.3). Claude Code: a sign-in that failed or lapsed, cloud credentials it
// could not load, a server error, and an unknown error, which is how a request that never
// reached the API ends. Other harnesses give no failure kind yet.
func NetworkFailure(harness, kind string) bool {
	if harness != detect.KindClaude {
		return false
	}
	switch kind {
	case "authentication_failed", "cloud_credential_error", "server_error", "unknown":
		return true
	}
	return false
}
