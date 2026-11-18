package main

import (
	"context"
	"encoding/json"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// renamer renames a session record under session.rename's rules: the name charset, the Capcom
// names, and no clash on this host or in a peer's catalog copy (mesh.md 6.5).
type renamer interface {
	RenameSession(session, name string) error
}

// agentHook serves the agent methods on the pane route (harness-adapters.md, Self-compact and
// Agent kit): agent.compact through compacts, agent.name through names. A nil one answers
// unavailable.
func agentHook(compacts *compactService, names renamer, store *structure.Store) hook.ChannelHandler {
	var compact hook.ChannelHandler
	if compacts != nil {
		compact = compactHook(compacts)
	}
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		if method != "agent.name" {
			if compact == nil {
				return nil, nil, control.Errorf(api.ErrorKindUnavailable, "%s is not served by this gff-hab", method)
			}
			return compact(ctx, pane, method, params)
		}
		if names == nil || store == nil {
			return nil, nil, control.Errorf(api.ErrorKindUnavailable, "agent.name is not served by this gff-hab")
		}
		var p api.AgentNameParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err)
		}
		ss, ok := store.SessionByPane(pane)
		if !ok {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "this pane is not a session; only a session has a name")
		}
		if p.Name != ss.Name {
			if err := names.RenameSession(ss.ID, p.Name); err != nil {
				return nil, nil, err
			}
			if ss, ok = store.SessionByPane(pane); !ok {
				return nil, nil, control.Errorf(api.ErrorKindNotFound, "the pane's session closed during the rename")
			}
		}
		return ss, nil, nil
	}
}
