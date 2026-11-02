package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// logbookWatch is how often gff-hab looks for ops another writer of the file made: the CLI's
// writes reach clients within it (4.3).
const logbookWatch = time.Second

// startLogbookVerbs starts the service that serves the Logbook verbs (logbook.md 10) over the
// store openLogbook opened, the one Logbook sync also serves. Every write through it calls
// synced, so sync sends the new ops at once (7.2). publish emits the Logbook events. It runs the
// watch for other writers until the returned stop is called.
func startLogbookVerbs(ctx context.Context, st *logbook.Store, synced func(), publish func(structure.Event), logf func(string, ...any)) (*lbsvc.Service, func(), error) {
	svc, err := lbsvc.New(ctx, lbsvc.Config{Store: st, Logf: logf, Wrote: synced,
		Publish: func(t api.EventType, id string, rev uint64, data any) {
			publish(structure.Event{Type: t, ID: id, Revision: rev, Data: data})
		}})
	if err != nil {
		return nil, nil, err
	}
	wctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Watch(wctx, logbookWatch)
	}()
	return svc, func() {
		stop()
		<-done
	}, nil
}

// logbookHook answers the Logbook methods on a pane's route: the pane's session, harness and
// this host are the writer (logbook.md 2.5), and the pane's writes count against its limit (14).
// A Capcom session's writes are stored as proposals (capcom.md D3, D14), and so is every pane's
// while approval reports true: logbook.approval is agents (9.1). It is read at each call, so a
// change of the setting holds from the next write.
func logbookHook(svc *lbsvc.Service, store *structure.Store, agents *harness.Store, host string, approval func() bool) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		pn := lbsvc.Pane{ID: pane, Host: host}
		if s, ok := store.SessionByPane(pane); ok {
			pn.Session = s.ID
			pn.Propose = s.Kind == api.SessionKindCapcom
		}
		if approval != nil && approval() {
			pn.Propose = true
		}
		if agents != nil {
			if a, ok := agents.Get(pane); ok {
				pn.Harness = a.Harness
			}
		}
		res, err := svc.Dispatch(ctx, lbsvc.PaneActor(pn), &pn, method, params)
		return res, nil, err
	}
}

// approvalAgents reports whether logbook.approval is agents, from the hab-side settings.
func approvalAgents(value func(id string) json.RawMessage) func() bool {
	return func() bool {
		var v string
		return json.Unmarshal(value(settings.LogbookApproval), &v) == nil && v == "agents"
	}
}

var _ control.Logbook = (*lbsvc.Service)(nil)
