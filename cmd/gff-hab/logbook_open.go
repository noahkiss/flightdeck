package main

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/catalog"
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
// watch for other writers until the returned stop is called. opts, when set, fills in more of
// the service's config: vector search (13).
func startLogbookVerbs(ctx context.Context, st *logbook.Store, opts func(*lbsvc.Config), synced func(), publish func(structure.Event), logf func(string, ...any)) (*lbsvc.Service, func(), error) {
	cfg := lbsvc.Config{Store: st, Logf: logf, Wrote: synced,
		Publish: func(t api.EventType, id string, rev uint64, data any) {
			publish(structure.Event{Type: t, ID: id, Revision: rev, Data: data})
		}}
	if opts != nil {
		opts(&cfg)
	}
	svc, err := lbsvc.New(ctx, cfg)
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
// change of the setting holds from the next write. wrote, when set, hears of each write that
// went through: the recording habits count it (11.1).
//
// The Capcom service's guides are the one exception (capcom-service.md 6.4): a write on the
// engine's route that only creates or edits capcom guides lands at once while logbook.approval
// is off, and eng adds its "Noted" line, landed or proposed.
func logbookHook(svc *lbsvc.Service, store *structure.Store, agents *harness.Store, host string, approval func() bool, wrote func(pane string), eng *capcomEngine) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		pn := lbsvc.Pane{ID: pane, Host: host}
		if s, ok := store.SessionByPane(pane); ok {
			pn.Session, pn.SessionName = s.ID, s.Name
			pn.Propose = s.Kind == api.SessionKindCapcom
			pn.Project = logbookProject(ctx, svc, store, host, s.Project)
		}
		var guide *api.LogbookWriteParams
		if eng != nil && pane == capcomRoute && pn.Propose && method == "logbook.write" {
			var p api.LogbookWriteParams
			if json.Unmarshal(params, &p) == nil && eng.memory.guideWrite(ctx, p.Changes) {
				guide = &p
				pn.Propose = p.Propose
			}
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
		if err == nil && wrote != nil && (method == "logbook.write" || method == "logbook.resolve") {
			wrote(pane)
		}
		if w, ok := res.(api.LogbookWritten); err == nil && ok && guide != nil {
			eng.noted(ctx, *guide, w)
		}
		return res, nil, err
	}
}

// logbookProject is the Logbook project of a session's project (logbook.md 3.3), by its git
// identity, or "" for a session with none. A project Logbook has not filed yet is filed now,
// first sight, under the folder the session works in.
func logbookProject(ctx context.Context, svc *lbsvc.Service, store *structure.Store, host, project string) string {
	pr, ok := store.Project(project)
	if !ok {
		return ""
	}
	identity := lbsvc.Identity(pr.Remote, pr.RootCommit)
	if identity == "" {
		return ""
	}
	if id, ok := svc.Lookup(ctx, identity); ok {
		return id
	}
	dir := ""
	for _, pd := range store.ProjectDirs() {
		if pd.ID == pr.ID && len(pd.Dirs) > 0 {
			dir = pd.Dirs[0]
		}
	}
	id, err := svc.Sight(ctx, lbsvc.Sighting{Dir: dir, Identity: identity, Host: host})
	if err != nil {
		return ""
	}
	return id
}

// logbookSighter files the folders the catalog reads (logbook.md 3.3): each folder a session
// works in, at first sight, and each folder filed before, whose remote may have changed. It
// takes the catalog's folders on the catalog's loop without blocking, and files them one scan
// at a time on its own goroutine: a write may wait out Logbook's start hold. A scan that comes
// while one is filed replaces any that waits.
type logbookSighter struct {
	svc   *lbsvc.Service
	store *structure.Store
	host  string
	logf  func(string, ...any)
	next  chan map[string]catalog.Folder
}

func newLogbookSighter(ctx context.Context, svc *lbsvc.Service, store *structure.Store, host string, logf func(string, ...any)) *logbookSighter {
	ls := &logbookSighter{svc: svc, store: store, host: host, logf: logf, next: make(chan map[string]catalog.Folder, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case fs := <-ls.next:
				ls.sight(ctx, fs)
			}
		}
	}()
	return ls
}

// Scanned is catalog.Config's Scanned.
func (ls *logbookSighter) Scanned(fs map[string]catalog.Folder) {
	for {
		select {
		case ls.next <- fs:
			return
		default:
		}
		select {
		case <-ls.next: // drop the older scan that waits
		default:
		}
	}
}

func (ls *logbookSighter) sight(ctx context.Context, fs map[string]catalog.Folder) {
	session := map[string]bool{}
	for _, pd := range ls.store.ProjectDirs() {
		for _, d := range pd.Dirs {
			session[d] = true
		}
	}
	dirs := slices.Sorted(maps.Keys(fs))
	for _, d := range dirs {
		if ctx.Err() != nil {
			return
		}
		identity := lbsvc.Identity(fs[d].Remote, fs[d].RootCommit)
		if identity == "" || (!session[d] && !ls.svc.Known(ctx, d)) {
			continue
		}
		if _, err := ls.svc.Sight(ctx, lbsvc.Sighting{Dir: d, Identity: identity, Host: ls.host}); err != nil && ctx.Err() == nil {
			ls.logf("logbook: file the project of %s: %v", d, err)
		}
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

// socketLogbook is the Logbook of the control socket. A call from a process inside a pane
// (control.PaneCall, hab-logic-api.md 4.1) writes as an agent, under the pane route's rules
// (logbook.md 10.5): no decision on a proposal, no project marked private, nothing in a private
// project, and a proposal under logbook.approval = agents. The hab does not know which pane the
// caller runs in, so its writes name no pane or session, and all such callers share one write
// limit. A call from outside every pane is the person's, as before.
type socketLogbook struct {
	*lbsvc.Service
	host     string
	approval func() bool
}

var _ control.Logbook = socketLogbook{}

// pane returns the pane a call from inside a pane writes as, or false for the person's call.
func (l socketLogbook) pane(ctx context.Context) (lbsvc.Pane, bool) {
	if !control.PaneCall(ctx) {
		return lbsvc.Pane{}, false
	}
	return lbsvc.Pane{Host: l.host, Propose: l.approval != nil && l.approval()}, true
}

func (l socketLogbook) Write(ctx context.Context, a api.LogbookActor, p api.LogbookWriteParams) (api.LogbookWritten, error) {
	if pn, ok := l.pane(ctx); ok {
		return l.Service.WritePane(ctx, pn, p)
	}
	return l.Service.Write(ctx, a, p)
}

func (l socketLogbook) Resolve(ctx context.Context, a api.LogbookActor, p api.LogbookResolveParams) (api.LogbookWritten, error) {
	pn, ok := l.pane(ctx)
	if !ok {
		return l.Service.Resolve(ctx, a, p)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return api.LogbookWritten{}, err
	}
	res, err := l.Service.Dispatch(ctx, lbsvc.PaneActor(pn), &pn, "logbook.resolve", raw)
	if err != nil {
		return api.LogbookWritten{}, err
	}
	w, _ := res.(api.LogbookWritten)
	return w, nil
}

func (l socketLogbook) Decide(ctx context.Context, a api.LogbookActor, p api.LogbookDecideParams) (api.LogbookWritten, error) {
	if control.PaneCall(ctx) {
		// The control server refuses it first (control.PersonOnly); this holds if that changes.
		return api.LogbookWritten{}, control.Errorf(api.ErrorKindDenied, "only a person decides a proposal (logbook.md 9.2), and this caller runs inside a Flightdeck pane")
	}
	return l.Service.Decide(ctx, a, p)
}
