package main

import (
	"context"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// TestCapcomSendFromPane: a process inside a pane cannot send the user's message to the Capcom
// service session: it would open a user turn (hab-logic-api.md 4.1).
func TestCapcomSendFromPane(t *testing.T) {
	t.Parallel()
	a := engineAgents{eng: &capcomEngine{session: "capcom-1"}}
	ctx := control.WithPaneCall(context.Background())
	err := a.Send(ctx, "c1", api.SessionSendParams{Session: "capcom-1", Text: "act now"})
	if errKind(err) != api.ErrorKindDenied {
		t.Fatalf("session.send to Capcom from a pane: %v", err)
	}
}
