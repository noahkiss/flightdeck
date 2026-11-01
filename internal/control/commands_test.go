package control

import (
	"context"
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type fakeCommands struct{}

func (fakeCommands) CommandList(p api.CommandListParams) (api.CommandList, error) {
	if p.Pane == "gone" {
		return api.CommandList{}, Errorf(api.ErrorKindNotFound, "no pane")
	}
	if p.Limit != 100 {
		return api.CommandList{}, Errorf(api.ErrorKindInvalid, "limit %d reached the service", p.Limit)
	}
	return api.CommandList{}, nil // a nil slice: the server sends []
}

func (fakeCommands) CommandOutput(_ context.Context, p api.CommandOutputParams) (api.CommandOutput, error) {
	if p.Command == 9 {
		return api.CommandOutput{}, errors.New("holder gone")
	}
	return api.CommandOutput{Command: api.Command{ID: p.Command, State: api.CommandStateDone}, Start: 1, End: 3, Data: []byte("ok")}, nil
}

// TestCommands: command.list and command.output run through Commands and carry the seq; the list
// limit defaults to 100 and is checked; a missing pane is invalid; a plain error answers
// unavailable; with no Commands both answer unavailable.
func TestCommands(t *testing.T) {
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Commands = fakeCommands{} })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	list, err := cl.CommandList(ctx(t), api.CommandListParams{Pane: "p1"})
	if err != nil || list.Commands == nil {
		t.Fatalf("command.list: %+v %v", list, err)
	}
	out, err := cl.CommandOutput(ctx(t), api.CommandOutputParams{Pane: "p1", Command: 4})
	if err != nil || string(out.Data) != "ok" || out.Command.ID != 4 {
		t.Fatalf("command.output: %+v %v", out, err)
	}
	var ce *Error
	for _, p := range []api.CommandListParams{{}, {Pane: "p1", Limit: 1001}, {Pane: "p1", Limit: -1}} {
		if _, err := cl.CommandList(ctx(t), p); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
			t.Errorf("list %+v: %v", p, err)
		}
	}
	if _, err := cl.CommandList(ctx(t), api.CommandListParams{Pane: "gone"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("list of a gone pane: %v", err)
	}
	for _, p := range []api.CommandOutputParams{{}, {Pane: "p1", Limit: -1}} {
		if _, err := cl.CommandOutput(ctx(t), p); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
			t.Errorf("output %+v: %v", p, err)
		}
	}
	if _, err := cl.CommandOutput(ctx(t), api.CommandOutputParams{Pane: "p1", Command: 9}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("a plain error: %v", err)
	}

	h2 := startHab(t, "", socketPath(t), nil)
	c2 := dial(t, h2.path)
	hello(t, c2, "")
	cl2 := api.Client{Caller: c2}
	if _, err := cl2.CommandList(ctx(t), api.CommandListParams{Pane: "p1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no Commands, list: %v", err)
	}
	if _, err := cl2.CommandOutput(ctx(t), api.CommandOutputParams{Pane: "p1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no Commands, output: %v", err)
	}
}

// TestCommandUpdatedScope: a command.updated event is about a pane and carries no revision.
func TestCommandUpdatedScope(t *testing.T) {
	if !structure.Ephemeral(EventCommandUpdated) {
		t.Error("command.updated carries a revision")
	}
	e := structure.Event{Type: EventCommandUpdated, ID: "p1", Data: api.Command{ID: 1}}
	f := newFilter(api.Scope{Kind: api.ScopeKindHost})
	if !f.pass(e) {
		t.Error("scope host dropped a command.updated event")
	}
}
