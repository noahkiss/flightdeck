package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/cmdmarks"
	"github.com/noahkiss/flightdeck/internal/control"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// commandService serves command.list and command.output from the holder link's command lists
// (hab-logic-api.md 2.5).
type commandService struct{ lk *link.Link }

func (s commandService) CommandList(p api.CommandListParams) (api.CommandList, error) {
	u, ok := parseUUID(p.Pane)
	if !ok {
		return api.CommandList{}, control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", p.Pane)
	}
	cs, more, err := s.lk.Commands(u, p.Before, p.Limit)
	if err != nil {
		return api.CommandList{}, commandErr(p.Pane, err)
	}
	out := api.CommandList{Commands: make([]api.Command, 0, len(cs)), More: more}
	for _, c := range cs {
		out.Commands = append(out.Commands, apiCommand(c))
	}
	return out, nil
}

func (s commandService) CommandOutput(ctx context.Context, p api.CommandOutputParams) (api.CommandOutput, error) {
	u, ok := parseUUID(p.Pane)
	if !ok {
		return api.CommandOutput{}, control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", p.Pane)
	}
	b, err := s.lk.CommandOutput(ctx, u, p.Command, p.From, p.Limit)
	if err != nil {
		return api.CommandOutput{}, commandErr(p.Pane, err)
	}
	return api.CommandOutput{Command: apiCommand(b.Command), Start: b.Start, End: b.End, Data: b.Data,
		Gap: b.Gap, More: b.More}, nil
}

// commandErr gives a link error its kind.
func commandErr(pane string, err error) error {
	switch {
	case errors.Is(err, link.ErrNoPane), errors.Is(err, link.ErrGone):
		return control.Errorf(api.ErrorKindNotFound, "pane %s: %v", pane, err)
	case errors.Is(err, link.ErrNoCommand):
		return control.Errorf(api.ErrorKindNotFound, "pane %s: %v", pane, err)
	}
	return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, err)
}

func apiCommand(c cmdmarks.Command) api.Command {
	a := api.Command{ID: c.ID, State: api.CommandState(c.State), Prompt: c.Prompt, Input: c.Input,
		Output: c.Output, End: c.End, Exit: c.Exit, Text: c.Text, TextTruncated: c.TextTruncated}
	if !c.Started.IsZero() {
		a.StartedAt = c.Started.UTC().Format(time.RFC3339Nano)
	}
	if !c.Ended.IsZero() {
		a.EndedAt = c.Ended.UTC().Format(time.RFC3339Nano)
	}
	return a
}

// commandFlush is the shortest time between two flushes of command.updated events.
const commandFlush = 200 * time.Millisecond

// maxCommandEvents is the most command.updated events one flush sends for one pane.
const maxCommandEvents = 32

// commandEvents sends command.updated events. The link says when a pane's commands changed;
// the changes are read and sent at most every commandFlush, at most maxCommandEvents per pane
// per flush (the newest), so a program that prints marks in a loop cannot flood the clients.
// When a flush leaves changes out, its first event for the pane says how many in dropped.
type commandEvents struct {
	changes func(hp.UUID) []cmdmarks.Command // a pane's changes since the last call
	publish func(structure.Event)

	mu    sync.Mutex
	dirty map[string]bool // pane ids with changes not yet read
	wake  chan struct{}
	stop  chan struct{}
	done  chan struct{}
}

func newCommandEvents(changes func(hp.UUID) []cmdmarks.Command, publish func(structure.Event)) *commandEvents {
	e := &commandEvents{changes: changes, publish: publish, dirty: map[string]bool{}, wake: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{})}
	go e.run()
	return e
}

// notice takes a link notice; it runs on the link's goroutines.
func (e *commandEvents) notice(n link.Notice) {
	if n.Kind != link.PaneCommands {
		return
	}
	e.mu.Lock()
	e.dirty[n.Pane.UUID.String()] = true
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *commandEvents) run() {
	defer close(e.done)
	for {
		select {
		case <-e.stop:
			return
		case <-e.wake:
		}
		e.flush()
		select {
		case <-e.stop:
			return
		case <-time.After(commandFlush):
		}
	}
}

func (e *commandEvents) flush() {
	e.mu.Lock()
	panes := e.dirty
	e.dirty = map[string]bool{}
	e.mu.Unlock()
	for pane := range panes {
		u, ok := parseUUID(pane)
		if !ok {
			continue
		}
		cs := e.changes(u)
		dropped := max(len(cs)-maxCommandEvents, 0)
		for i, c := range cs[dropped:] {
			a := apiCommand(c)
			if i == 0 {
				a.Dropped = dropped
			}
			e.publish(structure.Event{Type: control.EventCommandUpdated, ID: pane, Data: api.NonNil(a)})
		}
	}
}

func (e *commandEvents) Close() {
	close(e.stop)
	<-e.done
}
