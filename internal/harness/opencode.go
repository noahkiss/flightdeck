package harness

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// OpencodeStateEvents lists the opencode 2.x server events that carry agent state
// (harness-adapters.md, opencode).
var OpencodeStateEvents = []string{
	"session.created", "session.viewed", "session.deleted",
	"session.execution.started", "session.execution.succeeded", "session.execution.failed",
	"session.execution.interrupted", "session.retry.scheduled",
	"permission.asked", "permission.replied",
}

// OpencodeSession is what the adapter knows about the session an opencode server event names.
type OpencodeSession struct {
	// Child: the session has a parent session. It is a subagent's (opencode's task tool): it
	// never binds the pane, and only its permission requests show.
	Child bool
	// Running: the session's agent loop runs now, as far as the adapter saw.
	Running bool
	// Bound: the pane is bound to this session now, with a state.
	Bound bool
}

// ServerEvent is one opencode server event: its type, its data, and the folder it names.
type ServerEvent struct {
	Type string
	Data json.RawMessage
	Dir  string
}

// NormaliseOpencodeServer maps one event from a pane's opencode server to an Update, applied
// with source server. The server is private to the pane (gff-hab starts it beside the TUI), so
// every top-level session it runs is the pane's: a session created, viewed or started on it
// binds the pane. Events of a child session give only its permission requests, as a subagent's
// with no session, so binding never drops them.
func NormaliseOpencodeServer(e ServerEvent, s OpencodeSession) (Update, error) {
	var d struct {
		SessionID string   `json:"sessionID"`
		Version   string   `json:"version"`
		ID        string   `json:"id"`
		RequestID string   `json:"requestID"`
		Action    string   `json:"action"`
		Resources []string `json:"resources"`
	}
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return Update{}, fmt.Errorf("opencode %s: %w", e.Type, err)
		}
	}
	u := Update{Session: d.SessionID}
	if s.Child {
		switch e.Type {
		case "permission.asked", "permission.replied":
			u.Session, u.Subagent = "", true
		default:
			return Update{}, fmt.Errorf("%w opencode %s of a child session", ErrUnknownEvent, e.Type)
		}
	}
	switch e.Type {
	case "session.created":
		u.Bind, u.State, u.Version = true, api.AgentStatusIdle, d.Version
	case "session.viewed":
		if s.Bound {
			// The TUI marks the session it shows as viewed, also right after each turn. That
			// is not the owner looking: the bound session keeps its state (done stays done).
			return Update{}, fmt.Errorf("%w opencode session.viewed of the bound session", ErrUnknownEvent)
		}
		u.Bind, u.State = true, api.AgentStatusIdle
		if s.Running {
			u.State = api.AgentStatusWorking
		}
	case "session.deleted":
		u.Unbind = true
	case "session.execution.started":
		u.Bind, u.State = true, api.AgentStatusWorking
	case "session.retry.scheduled":
		u.State = api.AgentStatusWorking
	case "session.execution.succeeded":
		u.State = api.AgentStatusDone
	case "session.execution.failed":
		u.State = api.AgentStatusError
	case "session.execution.interrupted":
		u.State = api.AgentStatusIdle
	case "permission.asked":
		u.State, u.Kind = api.AgentStatusBlocked, api.BlockedKindPermission
		u.Request = OpencodeRequest(e.Data, d.ID, d.Action, d.Resources, e.Dir)
	case "permission.replied":
		u.Resolves, u.State = d.RequestID, api.AgentStatusWorking
	default:
		return Update{}, fmt.Errorf("%w opencode %s", ErrUnknownEvent, e.Type)
	}
	return u, nil
}

// OpencodeBound reports whether the pane is bound to an opencode session, with a state other
// than none or exited.
func (s *Store) OpencodeBound(paneID, session string) bool {
	st, ok := s.Get(paneID)
	return ok && session != "" && st.Harness == detect.KindOpencode && st.Session == session &&
		st.State != api.AgentStatusNone && st.State != api.AgentStatusExited
}

// OpencodeRequest copies a permission request from a permission.asked event. The action names
// the tool. A shell action's resources are its exact commands; a file action's are its files.
// The event's data is the request's input.
func OpencodeRequest(raw json.RawMessage, id, action string, resources []string, dir string) *api.PermissionRequest {
	r := &api.PermissionRequest{ID: id, Tool: action, Cwd: dir}
	setInput(r, raw)
	switch action {
	case "shell", "bash":
		r.Command = strings.Join(resources, "\n")
	case "edit", "write", "read", "patch", "apply_patch", "external_directory":
		r.Files = append([]string(nil), resources...)
	}
	return r
}
