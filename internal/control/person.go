package control

import (
	"context"
	"strings"
)

// PersonOnly reports whether a method is the user's own decision or a change of trust, which a
// process inside a pane may not call on the control socket (hab-logic-api.md 4.1). The pane
// route takes none of them either. A grant of a Capcom job's action words is checked by its
// params, in the round handlers.
func PersonOnly(method string) bool {
	switch method {
	case "confirm.answer", "trust.answer", "approval.answer", "capcom.card.answer",
		"settings.set", "peers.accept",
		"push.register", "push.register_activity", "push.unregister":
		return true
	}
	return strings.HasPrefix(method, "devices.")
}

// paneCallKey marks the context of a call from a process inside a pane.
type paneCallKey struct{}

// withPaneCall marks ctx as a call on the control socket from a process inside a pane.
func withPaneCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, paneCallKey{}, true)
}

// ByPerson reports whether a call counts as the user's own: not over a peer link and not from a
// process inside a pane (hab-logic-api.md 4.1). Only such a call grants a Capcom job's words or
// makes a round the person owns (capcom-service.md 9.3, 9.8).
func ByPerson(ctx context.Context) bool {
	pane, _ := ctx.Value(paneCallKey{}).(bool)
	return !pane && !PeerCall(ctx)
}
