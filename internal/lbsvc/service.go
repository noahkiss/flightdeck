// Package lbsvc serves Logbook's verbs (docs/specs/logbook.md 10) over one internal/logbook
// store: the logbook.* methods of the control connection and the pane route in gff-hab, and the
// same verbs in the CLI when it opens the file itself because gff-hab is down (4.3). It turns API
// params into store batches and records into API shapes, fills in who wrote (2.5), applies the
// pane write limit (14), and emits logbook.updated and logbook.conflict for every writer of the
// file.
package lbsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// PaneWrites is the default pane write limit: writes from one pane in a minute (14, D20).
const PaneWrites = 60

// PaneChanges is the default pane change limit: changes from one pane in a minute, counted over
// all its writes (10.5). A write of many changes counts each of them.
const PaneChanges = 600

// MaxChanges bounds the changes in one logbook.write, from any writer (10.5).
const MaxChanges = 200

// Config is what New needs. Store is required.
type Config struct {
	Store *logbook.Store
	// Publish emits one event: logbook.updated or logbook.conflict. Nil emits nothing (the CLI).
	Publish func(t api.EventType, id string, revision uint64, data any)
	// Wrote is called after each write through the service lands in the store: gff-hab tells
	// Logbook sync, which sends the new ops at once (7.2). Nil calls nothing (the CLI).
	Wrote func()
	// PaneWrites is the pane write limit per minute; 0 is PaneWrites.
	PaneWrites int
	// PaneChanges is the pane change limit per minute; 0 is PaneChanges.
	PaneChanges int
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// Service serves the verbs over one store.
type Service struct {
	cfg Config
	st  *logbook.Store

	sightMu sync.Mutex // one Sight at a time: two first sights of one identity file one project

	mu     sync.Mutex // orders Changed: one cursor, events in order
	cursor int64
	flags  map[string][]string // records that carry a flag, by id: their flag kinds

	limMu  sync.Mutex
	writes map[string][]paneWrite // per pane: its writes in the last minute
	warned map[string]time.Time   // per pane: when the limit was last logged
}

// New returns a service. Events start from the file's current end: what is already in the file
// is in every read.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.PaneWrites <= 0 {
		cfg.PaneWrites = PaneWrites
	}
	if cfg.PaneChanges <= 0 {
		cfg.PaneChanges = PaneChanges
	}
	s := &Service{cfg: cfg, st: cfg.Store, flags: map[string][]string{},
		writes: map[string][]paneWrite{}, warned: map[string]time.Time{}}
	cur, err := s.st.Cursor(ctx)
	if err != nil {
		return nil, err
	}
	s.cursor = cur
	return s, nil
}

// Store returns the service's store.
func (s *Service) Store() *logbook.Store { return s.st }

// --- errors ---

// apiErr maps a store error to the API's kinds (hab-logic-api.md 3.6).
func apiErr(err error) error {
	var ie *logbook.InvalidError
	var ce *control.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.As(err, &ie):
		return control.Errorf(api.ErrorKindInvalid, "%s", strings.TrimPrefix(ie.Error(), "logbook: "))
	case errors.Is(err, logbook.ErrNotFound):
		return control.Errorf(api.ErrorKindNotFound, "no such record or op on this replica")
	case errors.Is(err, logbook.ErrKeyReused):
		return control.Errorf(api.ErrorKindConflict, "%s", strings.TrimPrefix(err.Error(), "logbook: "))
	case errors.Is(err, logbook.ErrSharedOrigin), errors.Is(err, logbook.ErrRestoring):
		return control.Errorf(api.ErrorKindUnavailable, "%s", strings.TrimPrefix(err.Error(), "logbook: "))
	}
	return err
}

func invalid(format string, args ...any) error {
	return control.Errorf(api.ErrorKindInvalid, format, args...)
}

// --- reads ---

// Find searches by words; with none it lists, as List.
func (s *Service) Find(ctx context.Context, p api.LogbookFindParams) (api.LogbookRecords, error) {
	return s.find(ctx, p, nil)
}

func (s *Service) find(ctx context.Context, p api.LogbookFindParams, hide []string) (api.LogbookRecords, error) {
	return s.search(ctx, hide, p.Words, api.LogbookFilter{Type: p.Type, Status: p.Status, Project: p.Project, Tag: p.Tag,
		Host: p.Host, Author: p.Author, Session: p.Session, Since: p.Since, General: p.General, Deleted: p.Deleted, Flagged: p.Flagged, Limit: p.Limit}, false)
}

// List lists records, newest change first.
func (s *Service) List(ctx context.Context, p api.LogbookListParams) (api.LogbookRecords, error) {
	return s.search(ctx, nil, "", p.LogbookFilter, false)
}

// Ready lists open tasks and issues with no open blocker.
func (s *Service) Ready(ctx context.Context, p api.LogbookReadyParams) (api.LogbookRecords, error) {
	return s.ready(ctx, p, nil)
}

func (s *Service) ready(ctx context.Context, p api.LogbookReadyParams, hide []string) (api.LogbookRecords, error) {
	return s.search(ctx, hide, "", api.LogbookFilter{Project: p.Project, Limit: p.Limit}, true)
}

func (s *Service) search(ctx context.Context, hide []string, words string, f api.LogbookFilter, ready bool) (api.LogbookRecords, error) {
	q := logbook.Query{Hide: hide, Words: words, Type: f.Type, Status: f.Status, Project: f.Project, Tag: f.Tag, Host: f.Host,
		Author: f.Author, Session: f.Session, General: f.General, Flagged: f.Flagged, Ready: ready, Deleted: f.Deleted, Limit: f.Limit}
	if f.Limit < 0 {
		return api.LogbookRecords{}, invalid("limit is a positive number")
	}
	if f.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, f.Since)
		if err != nil {
			return api.LogbookRecords{}, invalid("since is an RFC 3339 time: %v", err)
		}
		q.SinceMS = t.UnixMilli()
	}
	items, more, err := s.st.Search(ctx, q)
	if err != nil {
		return api.LogbookRecords{}, apiErr(err)
	}
	out := api.LogbookRecords{Records: []api.LogbookRecord{}, More: more}
	for _, it := range items {
		out.Records = append(out.Records, api.LogbookRecord{ID: it.ID, Type: it.Type, Status: it.Status, Title: it.Title,
			Project: it.Project, Updated: stamp(it.UpdatedMS), Revision: it.Revision, Deleted: it.Deleted})
	}
	return out, nil
}

// Get reads one record whole.
func (s *Service) Get(ctx context.Context, p api.LogbookGetParams) (api.LogbookRecordResult, error) {
	return s.get(ctx, p, nil)
}

func (s *Service) get(ctx context.Context, p api.LogbookGetParams, hide []string) (api.LogbookRecordResult, error) {
	if p.ID == "" {
		return api.LogbookRecordResult{}, invalid("logbook.get needs an id")
	}
	r, err := s.st.Get(ctx, p.ID)
	if err == nil {
		err = s.hidden(ctx, r, hide)
	}
	if err != nil {
		return api.LogbookRecordResult{}, apiErr(err)
	}
	upd, err := s.st.Updated(ctx, r)
	if err != nil {
		return api.LogbookRecordResult{}, err
	}
	rec, err := s.record(ctx, r, upd, nil)
	if err != nil {
		return api.LogbookRecordResult{}, err
	}
	return api.LogbookRecordResult{Record: rec}, nil
}

// History lists every op on a record.
func (s *Service) History(ctx context.Context, p api.LogbookHistoryParams) (api.LogbookHistory, error) {
	return s.history(ctx, p, nil)
}

func (s *Service) history(ctx context.Context, p api.LogbookHistoryParams, hide []string) (api.LogbookHistory, error) {
	if p.ID == "" {
		return api.LogbookHistory{}, invalid("logbook.history needs an id")
	}
	if len(hide) > 0 {
		r, err := s.st.Get(ctx, p.ID)
		if err == nil {
			err = s.hidden(ctx, r, hide)
		}
		if err != nil {
			return api.LogbookHistory{}, apiErr(err)
		}
	}
	ops, err := s.st.History(ctx, p.ID)
	if err != nil {
		return api.LogbookHistory{}, apiErr(err)
	}
	out := api.LogbookHistory{Ops: []api.LogbookOp{}}
	for _, o := range ops {
		lo := api.LogbookOp{ID: o.ID().String(), At: stamp(o.Wall), Kind: string(o.Kind), Record: o.Record, Field: o.Field,
			Value: api.RawJSON(o.Value), By: actor(o.Actor), Folded: o.Known()}
		for _, p := range o.Prev {
			lo.Prev = append(lo.Prev, p.String())
		}
		if o.Target != nil {
			lo.Target = o.Target.String()
		}
		out.Ops = append(out.Ops, lo)
	}
	return out, nil
}

// Projects lists the projects.
func (s *Service) Projects(ctx context.Context) (api.LogbookProjects, error) {
	return s.projects(ctx, nil)
}

func (s *Service) projects(ctx context.Context, hide []string) (api.LogbookProjects, error) {
	items, _, err := s.st.Search(ctx, logbook.Query{Type: logbook.TypeProject, Hide: hide, Limit: logbook.MaxLimit})
	if err != nil {
		return api.LogbookProjects{}, apiErr(err)
	}
	private, err := s.st.PrivateProjects(ctx)
	if err != nil {
		return api.LogbookProjects{}, err
	}
	out := api.LogbookProjects{Projects: []api.LogbookProject{}}
	for _, it := range items {
		r, err := s.st.Get(ctx, it.ID)
		if err != nil {
			return out, apiErr(err)
		}
		rec, err := s.record(ctx, r, 0, private)
		if err != nil {
			return out, err
		}
		out.Projects = append(out.Projects, api.LogbookProject{ID: r.ID, Name: r.Title, Identities: r.Identities, Revision: r.Revision,
			Private: rec.Private, Marked: rec.Marked, JoinedInto: rec.JoinedInto})
	}
	slices.SortFunc(out.Projects, func(a, b api.LogbookProject) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
	return out, nil
}

// record is Record with what only the store knows filled in: a project's privacy and the
// project it is joined into (3.3). private is PrivateProjects' answer, or nil to read it.
func (s *Service) record(ctx context.Context, r *logbook.Record, updated int64, private []string) (api.LogbookRecord, error) {
	out := Record(r, updated)
	if r.Type != logbook.TypeProject {
		return out, nil
	}
	c, err := s.st.Canonical(ctx, r.ID)
	if err != nil {
		return out, err
	}
	if c != r.ID {
		out.JoinedInto = c
	}
	if private == nil {
		if private, err = s.st.PrivateProjects(ctx); err != nil {
			return out, err
		}
	}
	out.Private = slices.Contains(private, c)
	return out, nil
}

// Status reads the replica's size and health.
func (s *Service) Status(ctx context.Context) (api.LogbookStatus, error) {
	st, err := s.st.Status(ctx)
	if err != nil {
		return api.LogbookStatus{}, err
	}
	out := api.LogbookStatus{Origin: s.st.Origin(), Ops: st.Ops, Records: st.Records, Unfolded: st.Unfolded, Waiting: st.Waiting,
		Ahead: st.Ahead, Shared: st.Shared, Vector: []api.LogbookOrigin{}}
	for o, n := range st.Vector {
		out.Vector = append(out.Vector, api.LogbookOrigin{Origin: o, Seq: n})
	}
	slices.SortFunc(out.Vector, func(a, b api.LogbookOrigin) int { return strings.Compare(a.Origin, b.Origin) })
	return out, nil
}

// --- writes ---

// Pane names the pane a write came from, as gff-hab resolves its token (2.5).
type Pane struct {
	ID      string
	Session string // the Flightdeck session the pane belongs to, or ""
	Harness string
	Host    string
	// Project is the project the pane's session works in, by any of its ids, or "" for none
	// (3.3). The pane's reads leave out every private project but this one (12.1).
	Project string
	// Propose makes every write from the pane a pending proposal: a Capcom session's records
	// wait for review (capcom.md D3, D14), and under logbook.approval = agents every agent's do
	// (9.1).
	Propose bool
}

// WritePane is a write through a pane's token: the hab records the pane's harness, session and
// host, sets the source's session to the pane's, and refuses past the pane write limit. An agent
// never decides a proposal (9.2): a pane's write refuses a create of type proposal and a set of a
// proposal's status or decided_by. It forgets only ops its own session wrote (10.5). A write
// filed as a proposal is checked strictly: what the rules cannot check now is refused (paneChecks).
func (s *Service) WritePane(ctx context.Context, pn Pane, p api.LogbookWriteParams) (api.LogbookWritten, error) {
	hash := hashOf("logbook.write", p) // the request as sent, before the pane's fields go in
	if err := s.paneChecks(ctx, pn.Session, p.Changes, p.Propose || pn.Propose); err != nil {
		return api.LogbookWritten{}, err
	}
	hide, err := s.Hide(ctx, pn.Project)
	if err != nil {
		return api.LogbookWritten{}, err
	}
	if err := s.writesHidden(ctx, p.Changes, hide); err != nil {
		return api.LogbookWritten{}, err
	}
	if err := s.allow(pn.ID, len(p.Changes)); err != nil {
		return api.LogbookWritten{}, err
	}
	p.Propose = p.Propose || pn.Propose
	if p.Source != nil || pn.Session != "" {
		src := api.LogbookSource{}
		if p.Source != nil {
			src = *p.Source
		}
		src.Session = pn.Session
		p.Source = &src
	}
	return s.write(ctx, PaneActor(pn), p, hash)
}

// PaneActor is the actor of a write through a pane.
func PaneActor(pn Pane) api.LogbookActor {
	return api.LogbookActor{Kind: "agent", Harness: pn.Harness, Session: pn.Session, Pane: pn.ID, Host: pn.Host}
}

// paneChecks refuses what an agent of session may not write (10.5): a decision on a proposal,
// and a forget of an op its own session did not write. With strict, for a write that lands
// later (a proposal's draft, 9.2), it also refuses what it cannot check against this replica: a
// forget of an op the replica does not hold, and a set of the status of a record it does not
// hold. Else the store would see them only at the approve, where an op id guessed now (the
// person's next write) may name another writer's op.
func (s *Service) paneChecks(ctx context.Context, session string, changes []api.LogbookChange, strict bool) error {
	created := map[string]bool{}
	for i, c := range changes {
		switch c.Op {
		case api.LogbookChangeOpCreate:
			if c.Type == logbook.TypeProposal {
				return invalid("change %d: an agent files a proposal with propose: true, never as a create of type proposal", i+1)
			}
			if c.Type == logbook.TypeProject {
				return invalid("change %d: %s", i+1, projectsHint)
			}
		case api.LogbookChangeOpAdd, api.LogbookChangeOpRemove:
			if c.Identity != "" {
				return invalid("change %d: a project's identities are the user's to change: %s", i+1, projectsHint)
			}
			if c.ID != "" {
				created[c.ID] = true
			}
		case api.LogbookChangeOpSet:
			if err := s.paneSet(ctx, c.ID, c.Field, created[c.ID], strict); err != nil {
				return invalid("change %d: %v", i+1, err)
			}
		case api.LogbookChangeOpForget:
			id, err := logbook.ParseOpID(c.OpID)
			if err != nil {
				continue // Steps refuses it with its reason
			}
			o, err := s.st.Op(ctx, id)
			if errors.Is(err, logbook.ErrNotFound) {
				if strict {
					return invalid("change %d: this hab holds no op %s, so a proposal cannot forget it: a proposal forgets only ops this hab holds that the pane's own session wrote", i+1, c.OpID)
				}
				continue // the store refuses it with its reason
			}
			if err != nil {
				return err
			}
			if session == "" || o.Actor == nil || o.Actor.Session != session {
				return invalid("change %d: a pane forgets only ops its own session wrote, and %s is not one of them. A person can forget it: gff lb forget %s, run outside a pane", i+1, c.OpID, c.OpID)
			}
		}
	}
	return nil
}

// paneSet refuses a pane's set of a proposal's decision fields: only a person decides (9.2).
// With strict, a set of the status of a record this replica does not hold is refused too: it
// may be a proposal that arrives before the set lands.
func (s *Service) paneSet(ctx context.Context, id, field string, created, strict bool) error {
	if field == "decided_by" {
		return errors.New("only a person decides a proposal: decided_by is not an agent's to set")
	}
	if field == "private" {
		return errors.New("only the user marks a project private: gff lb project private, run outside a pane")
	}
	if field != "status" || created || id == "" {
		return nil
	}
	r, err := s.st.Get(ctx, id)
	if errors.Is(err, logbook.ErrNotFound) && strict {
		return fmt.Errorf("this hab holds no record %s, so a proposal cannot set its status", id)
	}
	if err != nil {
		return nil // the store refuses a missing record with its reason
	}
	if r.Type == logbook.TypeProposal {
		return fmt.Errorf("only a person decides a proposal: %s's status is not an agent's to set", id)
	}
	return nil
}

// paneWrite is one write a pane made: when, and how many changes it carried.
type paneWrite struct {
	at      time.Time
	changes int
}

// allow counts one write of n changes from a pane, or refuses it past either limit: writes in
// the last minute, or changes in the last minute.
func (s *Service) allow(pane string, n int) error {
	now := s.cfg.Now()
	s.limMu.Lock()
	defer s.limMu.Unlock()
	w := s.writes[pane]
	cut := 0
	for cut < len(w) && now.Sub(w[cut].at) >= time.Minute {
		cut++
	}
	w = w[cut:]
	sum := 0
	for _, x := range w {
		sum += x.changes
	}
	if len(w) >= s.cfg.PaneWrites || (len(w) > 0 && sum+n > s.cfg.PaneChanges) {
		s.writes[pane] = w
		if now.Sub(s.warned[pane]) >= time.Minute {
			s.warned[pane] = now
			s.cfg.Logf("logbook: pane %s wrote %d times (%d changes) in a minute; its writes are refused until the minute passes", pane, len(w), sum)
		}
		wait := time.Minute - now.Sub(w[0].at)
		if len(w) >= s.cfg.PaneWrites {
			return invalid("this pane wrote %d times in the last minute, the limit: nothing was written. Wait %s, and write less often; a loop that writes on every step should batch its changes",
				len(w), wait.Round(time.Second))
		}
		return invalid("this pane wrote %d changes in the last minute, and %d more pass the limit of %d: nothing was written. Wait %s, and write less",
			sum, n, s.cfg.PaneChanges, wait.Round(time.Second))
	}
	s.writes[pane] = append(w, paneWrite{at: now, changes: n})
	if len(s.writes) > 4096 { // panes come and go: drop the quiet ones
		for k, v := range s.writes {
			if len(v) == 0 || now.Sub(v[len(v)-1].at) >= time.Minute {
				delete(s.writes, k)
				delete(s.warned, k)
			}
		}
	}
	return nil
}

// Write applies one batch, or with propose stores it as a pending proposal (9.2).
func (s *Service) Write(ctx context.Context, a api.LogbookActor, p api.LogbookWriteParams) (api.LogbookWritten, error) {
	return s.write(ctx, a, p, hashOf("logbook.write", p))
}

// write is Write with the key's hash: the hash of the request as its writer sent it, so a key
// means the same request whichever door it came through (4.5).
func (s *Service) write(ctx context.Context, a api.LogbookActor, p api.LogbookWriteParams, hash string) (api.LogbookWritten, error) {
	if len(p.Changes) == 0 {
		return api.LogbookWritten{}, invalid("logbook.write needs at least one change")
	}
	if len(p.Changes) > MaxChanges {
		return api.LogbookWritten{}, invalid("logbook.write takes at most %d changes, and this one has %d: split it into several writes", MaxChanges, len(p.Changes))
	}
	if len(p.IdempotencyKey) > logbook.MaxKey {
		return api.LogbookWritten{}, invalid("an idempotency key is at most %d bytes", logbook.MaxKey)
	}
	changes, err := selfAssignee(p.Changes, a)
	if err != nil {
		return api.LogbookWritten{}, err
	}
	p.Changes = changes
	if err := s.noDecision(ctx, p.Changes); err != nil {
		return api.LogbookWritten{}, err
	}
	steps, err := Steps(p.Changes, p.Source)
	if err != nil {
		return api.LogbookWritten{}, err
	}
	state := api.LogbookWriteStateApplied
	if p.Propose {
		st, err := proposal(p)
		if err != nil {
			return api.LogbookWritten{}, err
		}
		steps = []logbook.Step{st}
		state = api.LogbookWriteStateProposed
	}
	b := logbook.Batch{Actor: storeActor(a), Steps: steps}
	if p.IdempotencyKey != "" {
		b.Key, b.KeyHash = p.IdempotencyKey, hash
	}
	res, err := s.st.Write(ctx, b)
	if err != nil {
		return api.LogbookWritten{}, apiErr(err)
	}
	s.wrote(ctx)
	out := written(res, state)
	if p.Propose && len(res.Records) > 0 {
		out.Proposal = res.Records[0]
		out.Note = ProposedNote(out.Proposal)
	}
	return out, nil
}

// decideHint names the doors that decide a proposal.
const decideHint = "decide it with gff lb approve or gff lb reject, or logbook.decide"

// noDecision refuses a set of a proposal's status or decided_by in a plain write, from anyone
// (9.4): only Decide decides, so every decision carries its approve's draft and approved_by. A
// peer's ops from other builds still fold as they come; this guards local writes.
func (s *Service) noDecision(ctx context.Context, changes []api.LogbookChange) error {
	proposals := map[string]bool{}
	for i, c := range changes {
		switch c.Op {
		case api.LogbookChangeOpCreate:
			if c.ID != "" && c.Type == logbook.TypeProposal {
				proposals[c.ID] = true
			}
		case api.LogbookChangeOpSet:
			if c.Field != "status" && c.Field != "decided_by" {
				continue
			}
			isProp := proposals[c.ID]
			if !isProp {
				r, err := s.st.Get(ctx, c.ID)
				isProp = err == nil && r.Type == logbook.TypeProposal
			}
			if isProp {
				return invalid("change %d: a proposal's %s changes only by a decision: %s", i+1, c.Field, decideHint)
			}
		}
	}
	return nil
}

// ProposedNote is what a writer whose change became a proposal reads (9.1): it waits for the
// user, who sees it with the other approvals, so the agent should not ask again.
func ProposedNote(id string) string {
	return "Nothing landed yet: the change waits for the user's approval as proposal " + id +
		". The user sees it with their other approvals, so do not ask them about it again; go on with your work."
}

// Decide decides a pending proposal (9.2). approve applies the draft, or the edits given, as the
// proposing agent's write with approved_by set to the deciding device, in one batch with the
// decision; each record it makes links to the proposal with from. reject sets the decision only.
// The store refuses a second decision it can see (5.6). Only a person decides: the pane route
// never reaches it, and Dispatch refuses it with a pane.
func (s *Service) Decide(ctx context.Context, a api.LogbookActor, p api.LogbookDecideParams) (api.LogbookWritten, error) {
	if p.ID == "" {
		return api.LogbookWritten{}, invalid("logbook.decide needs a proposal id")
	}
	if p.Decision != api.LogbookDecisionApprove && p.Decision != api.LogbookDecisionReject {
		return api.LogbookWritten{}, invalid("decision %q is not one of %s", p.Decision, strings.Join(api.Enums["LogbookDecision"], ", "))
	}
	if p.Decision == api.LogbookDecisionReject && len(p.Edits) > 0 {
		return api.LogbookWritten{}, invalid("a reject takes no edits")
	}
	if len(p.Edits) > MaxChanges {
		return api.LogbookWritten{}, invalid("edits take at most %d changes, and these have %d", MaxChanges, len(p.Edits))
	}
	if len(p.IdempotencyKey) > logbook.MaxKey {
		return api.LogbookWritten{}, invalid("an idempotency key is at most %d bytes", logbook.MaxKey)
	}
	if a.Kind == "agent" {
		return api.LogbookWritten{}, invalid("only a person decides a proposal")
	}
	device := a.Device
	if device == "" {
		return api.LogbookWritten{}, invalid("logbook.decide needs the deciding device: this connection has no label")
	}
	r, err := s.st.Get(ctx, p.ID)
	if err != nil {
		return api.LogbookWritten{}, apiErr(err)
	}
	if r.Type != logbook.TypeProposal {
		return api.LogbookWritten{}, invalid("%s is a %s, not a proposal", r.ID, r.Type)
	}
	if r.Deleted {
		return api.LogbookWritten{}, invalid("%s is deleted: restore it first", r.ID)
	}
	if r.Status == "pending" {
		if exp := str(r.Fields["expires"].Value); exp != "" {
			if t, err := time.Parse(time.RFC3339Nano, exp); err == nil && !s.cfg.Now().Before(t) {
				return api.LogbookWritten{}, invalid("%s expired at %s and can no longer be decided", r.ID, exp)
			}
		}
	}
	status := "rejected"
	who := storeActor(a)
	var steps []logbook.Step
	if p.Decision == api.LogbookDecisionApprove {
		status = "approved"
		if len(p.Edits) > 0 {
			status = "edited"
		}
		if target := str(r.Fields["target"].Value); target == "" || target == "record" {
			steps, who, err = s.draftSteps(ctx, r, p.Edits, device)
			if err != nil {
				return api.LogbookWritten{}, err
			}
		}
		// A memory or flight rules proposal records the decision only: Part 8 writes the file
		// (instructions-mcp-flight-rules.md 12).
	}
	steps = append(steps,
		logbook.Step{Kind: logbook.KindSet, Record: r.ID, Field: "status", Value: status},
		logbook.Step{Kind: logbook.KindSet, Record: r.ID, Field: "decided_by", Value: device})
	b := logbook.Batch{Actor: who, Steps: steps}
	if p.IdempotencyKey != "" {
		b.Key, b.KeyHash = p.IdempotencyKey, hashOf("logbook.decide", p)
	}
	res, err := s.st.Write(ctx, b)
	if err != nil {
		return api.LogbookWritten{}, apiErr(err)
	}
	s.wrote(ctx)
	state := api.LogbookWriteStateApplied
	if status == "rejected" {
		state = api.LogbookWriteStateRejected
	}
	out := written(res, state)
	out.Proposal = r.ID
	return out, nil
}

// draftSteps turns a proposal's draft, or the person's edits in its place, into the approve
// batch's steps, and returns the batch's actor: the proposing agent, with approved_by the
// deciding device (9.2). Each record the batch makes links to the proposal with from. The
// changes never decide a proposal (noDecision), and when an agent filed the proposal they pass
// the pane rules as its session, against this replica now: an approve never applies what the
// agent could not write itself, whatever was filed or edited. A person who wants a forget
// writes it themselves.
func (s *Service) draftSteps(ctx context.Context, r *logbook.Record, edits []api.LogbookChange, device string) ([]logbook.Step, *logbook.Actor, error) {
	var draft struct {
		Changes []api.LogbookChange `json:"changes"`
		Source  *api.LogbookSource  `json:"source,omitempty"`
	}
	if err := json.Unmarshal(r.Fields["draft"].Value, &draft); err != nil {
		return nil, nil, invalid("%s's draft does not read as changes: %v", r.ID, err)
	}
	who := &logbook.Actor{}
	if r.CreatedBy != nil {
		*who = *r.CreatedBy
	}
	who.ApprovedBy = device
	changes := draft.Changes
	if len(edits) > 0 {
		var err error
		if changes, err = selfAssignee(edits, *actor(who)); err != nil {
			return nil, nil, err
		}
	}
	if len(changes) == 0 {
		return nil, nil, invalid("%s's draft holds no changes: reject it, or approve it with edits", r.ID)
	}
	if err := s.noDecision(ctx, changes); err != nil {
		return nil, nil, err
	}
	if who.Kind == "agent" {
		if err := s.paneChecks(ctx, who.Session, changes, true); err != nil {
			var ce *control.Error
			msg := err.Error()
			if errors.As(err, &ce) {
				msg = ce.Message
			}
			return nil, nil, invalid("%s cannot be approved as it stands: the agent that filed it may not write this (%s). Reject it, or approve it with edits that leave this out; a forget you want, write yourself with gff lb forget", r.ID, msg)
		}
	}
	steps, err := Steps(changes, draft.Source)
	if err != nil {
		return nil, nil, err
	}
	var out []logbook.Step
	for _, st := range steps {
		out = append(out, st)
		if st.Kind == logbook.KindCreate {
			out = append(out, logbook.Step{Kind: logbook.KindAdd, Record: st.Record, Field: "links",
				Value: logbook.Link{Kind: logbook.LinkFrom, To: r.ID}})
		}
	}
	return out, who, nil
}

// count is how many of bs are true.
func count(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func str(v json.RawMessage) string {
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// Self is the assignee word for the writer itself (10.2 claim): the pane's session, or the
// person's device.
const Self = "self"

// selfAssignee replaces the assignee self with the writer: its session, else its device. A
// writer with neither, such as a pane outside a session, is refused: self names no one then.
func selfAssignee(changes []api.LogbookChange, a api.LogbookActor) ([]api.LogbookChange, error) {
	me := a.Session
	if me == "" {
		me = a.Device
	}
	quoted, _ := json.Marshal(me)
	out := slices.Clone(changes)
	for i, c := range out {
		var v string
		isSelf := (c.Op == api.LogbookChangeOpCreate && c.Assignee == Self) ||
			(c.Op == api.LogbookChangeOpSet && c.Field == "assignee" && json.Unmarshal(c.Value, &v) == nil && v == Self)
		if !isSelf {
			continue
		}
		if me == "" {
			return nil, invalid("change %d: the assignee %s names the writer's session, and this writer has none: name the assignee", i+1, Self)
		}
		if c.Op == api.LogbookChangeOpCreate {
			out[i].Assignee = me
		} else {
			out[i].Value = api.RawJSON(quoted)
		}
	}
	return out, nil
}

// Resolve settles a conflict flag: a set that names every head of the field (5.2).
func (s *Service) Resolve(ctx context.Context, a api.LogbookActor, p api.LogbookResolveParams) (api.LogbookWritten, error) {
	if p.ID == "" || p.Field == "" {
		return api.LogbookWritten{}, invalid("logbook.resolve needs an id and a field")
	}
	if (len(p.Value) == 0) == (p.Op == "") {
		return api.LogbookWritten{}, invalid("logbook.resolve takes a value or an op, one of them")
	}
	if p.Field == "status" || p.Field == "decided_by" {
		if r, err := s.st.Get(ctx, p.ID); err == nil && r.Type == logbook.TypeProposal {
			return api.LogbookWritten{}, invalid("a proposal's %s changes only by a decision: %s", p.Field, decideHint)
		}
	}
	val := json.RawMessage(p.Value)
	if p.Op != "" {
		want, err := logbook.ParseOpID(p.Op)
		if err != nil {
			return api.LogbookWritten{}, invalid("%v", err)
		}
		ops, err := s.st.History(ctx, p.ID)
		if err != nil {
			return api.LogbookWritten{}, apiErr(err)
		}
		val = nil
		for _, o := range ops {
			if o.ID() == want && o.Kind == logbook.KindSet && o.Field == p.Field {
				val = o.Value
			}
		}
		if val == nil {
			return api.LogbookWritten{}, control.Errorf(api.ErrorKindNotFound, "%s holds no set of %s on %s", p.Op, p.Field, p.ID)
		}
	}
	b := logbook.Batch{Actor: storeActor(a), Steps: []logbook.Step{{Kind: logbook.KindSet, Record: p.ID, Field: p.Field, Value: val}}}
	if p.IdempotencyKey != "" {
		b.Key, b.KeyHash = p.IdempotencyKey, hashOf("logbook.resolve", p)
	}
	res, err := s.st.Write(ctx, b)
	if err != nil {
		return api.LogbookWritten{}, apiErr(err)
	}
	s.wrote(ctx)
	return written(res, api.LogbookWriteStateApplied), nil
}

// wrote follows a write that landed: its events, then Logbook sync's nudge.
func (s *Service) wrote(ctx context.Context) {
	s.Changed(ctx)
	if s.cfg.Wrote != nil {
		s.cfg.Wrote()
	}
}

func written(res logbook.WriteResult, state api.LogbookWriteState) api.LogbookWritten {
	out := api.LogbookWritten{State: state, IDs: append([]string{}, res.Records...), Ops: []string{}, Repeated: res.Repeated}
	for _, o := range res.Ops {
		out.Ops = append(out.Ops, o.String())
	}
	return out
}

// hashOf hashes a request without its key, so the same key with another request is told apart
// (hab-logic-api.md 3.9).
func hashOf(method string, p any) string {
	switch v := p.(type) {
	case api.LogbookWriteParams:
		v.IdempotencyKey = ""
		p = v
	case api.LogbookResolveParams:
		v.IdempotencyKey = ""
		p = v
	case api.LogbookDecideParams:
		v.IdempotencyKey = ""
		p = v
	}
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(append([]byte(method+"\x00"), b...))
	return hex.EncodeToString(sum[:])
}

// Steps turns a write's changes into store steps. It checks the shape of each change; the store
// checks the rest (2.2, 4.5 Limits) when the batch is written.
func Steps(changes []api.LogbookChange, src *api.LogbookSource) ([]logbook.Step, error) {
	var steps []logbook.Step
	for i, c := range changes {
		bad := func(format string, args ...any) error {
			return invalid("change %d (%s): %s", i+1, c.Op, fmt.Sprintf(format, args...))
		}
		switch c.Op {
		case api.LogbookChangeOpCreate:
			if c.Type == "" {
				return nil, bad("needs a type")
			}
			id := c.ID
			if id == "" {
				id = logbook.NewID(c.Type)
			}
			fields := map[string]any{"title": c.Title}
			put := func(name string, set bool, v any) {
				if set {
					fields[name] = v
				}
			}
			put("project", c.Project != "", c.Project)
			put("body", c.Body != "", c.Body)
			put("status", c.Status != "", c.Status)
			put("assignee", c.Assignee != "", c.Assignee)
			put("priority", c.Priority != 0, c.Priority)
			put("triage", c.Triage != "", c.Triage)
			put("component", c.Component != "", c.Component)
			put("pinned", c.Pinned, true)
			st := logbook.Step{Kind: logbook.KindCreate, Record: id, Type: c.Type, Fields: fields}
			if src != nil && (src.Session != "" || src.Message != "" || src.Quote != "") {
				st.Source = &logbook.Source{Session: src.Session, Message: src.Message, Quote: src.Quote}
			}
			steps = append(steps, st)
			for _, t := range c.Tags {
				steps = append(steps, logbook.Step{Kind: logbook.KindAdd, Record: id, Field: "tags", Value: t})
			}
			for _, l := range c.Links {
				steps = append(steps, logbook.Step{Kind: logbook.KindAdd, Record: id, Field: "links", Value: logbook.Link{Kind: l.Kind, To: l.To, Pos: l.Pos}})
			}
		case api.LogbookChangeOpSet:
			if c.ID == "" || c.Field == "" || len(c.Value) == 0 {
				return nil, bad("needs an id, a field and a value")
			}
			if !json.Valid(c.Value) {
				return nil, bad("the value is not JSON")
			}
			steps = append(steps, logbook.Step{Kind: logbook.KindSet, Record: c.ID, Field: c.Field, Value: json.RawMessage(c.Value)})
		case api.LogbookChangeOpAdd, api.LogbookChangeOpRemove:
			kind := logbook.KindAdd
			if c.Op == api.LogbookChangeOpRemove {
				kind = logbook.KindRemove
			}
			switch {
			case c.ID == "":
				return nil, bad("needs an id")
			case count(c.Tag != "", c.Link != nil, c.Identity != "") != 1:
				return nil, bad("takes a tag, a link or an identity, one of them")
			case c.Identity != "":
				steps = append(steps, logbook.Step{Kind: kind, Record: c.ID, Field: "identities", Value: c.Identity})
			case c.Tag != "":
				steps = append(steps, logbook.Step{Kind: kind, Record: c.ID, Field: "tags", Value: c.Tag})
			default:
				steps = append(steps, logbook.Step{Kind: kind, Record: c.ID, Field: "links", Value: logbook.Link{Kind: c.Link.Kind, To: c.Link.To, Pos: c.Link.Pos}})
			}
		case api.LogbookChangeOpNote:
			if c.ID == "" || strings.TrimSpace(c.Text) == "" {
				return nil, bad("needs an id and text")
			}
			steps = append(steps, logbook.Step{Kind: logbook.KindAppend, Record: c.ID, Field: "notes", Value: c.Text})
		case api.LogbookChangeOpForget:
			t, err := logbook.ParseOpID(c.OpID)
			if err != nil {
				return nil, bad("needs op_id: %v", err)
			}
			steps = append(steps, logbook.Step{Kind: logbook.KindForget, Target: t})
		default:
			return nil, invalid("change %d: op %q is not one of %s", i+1, c.Op, strings.Join(api.Enums["LogbookChangeOp"], ", "))
		}
	}
	return steps, nil
}

// proposal is the create of a pending proposal that holds the write as its draft (9.2): target
// record, the changes as the draft, the source, and a title from the first change.
func proposal(p api.LogbookWriteParams) (logbook.Step, error) {
	draft, err := json.Marshal(struct {
		Changes []api.LogbookChange `json:"changes"`
		Source  *api.LogbookSource  `json:"source,omitempty"`
	}{p.Changes, p.Source})
	if err != nil {
		return logbook.Step{}, err
	}
	if len(draft) > logbook.MaxBody {
		return logbook.Step{}, invalid("the proposal's draft is over 256 KiB")
	}
	c := p.Changes[0]
	title := c.Title
	switch {
	case c.Op == api.LogbookChangeOpCreate && title != "":
		title = "New " + c.Type + ": " + title
	case c.ID != "":
		title = "Change " + c.ID
	default:
		title = "Proposed change"
	}
	if len(p.Changes) > 1 {
		title += fmt.Sprintf(" and %d more", len(p.Changes)-1)
	}
	if utf8.RuneCountInString(title) > logbook.MaxTitle {
		title = string([]rune(title)[:logbook.MaxTitle-1]) + "…"
	}
	fields := map[string]any{"title": title, "target": "record", "draft": json.RawMessage(draft)}
	for _, ch := range p.Changes {
		if ch.Op == api.LogbookChangeOpCreate && ch.Project != "" {
			fields["project"] = ch.Project
			break
		}
	}
	st := logbook.Step{Kind: logbook.KindCreate, Record: logbook.NewID(logbook.TypeProposal), Type: logbook.TypeProposal, Fields: fields}
	if src := p.Source; src != nil {
		st.Source = &logbook.Source{Session: src.Session, Message: src.Message, Quote: src.Quote}
	}
	return st, nil
}

func storeActor(a api.LogbookActor) *logbook.Actor {
	if a == (api.LogbookActor{}) {
		return nil
	}
	return &logbook.Actor{Kind: a.Kind, Harness: a.Harness, Session: a.Session, Pane: a.Pane, Host: a.Host, Device: a.Device, ApprovedBy: a.ApprovedBy}
}

// --- events ---

// Changed reads the records whose ops arrived since the last call, by any writer of the file,
// and emits logbook.updated for each, and logbook.conflict for each whose flags changed. gff-hab
// calls it after its own writes and on a timer, which catches the CLI's writes (4.3).
func (s *Service) Changed(ctx context.Context) {
	if s.cfg.Publish == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, next, err := s.st.Changes(ctx, s.cursor)
	if err != nil {
		s.cfg.Logf("logbook: changes: %v", err)
		return
	}
	s.cursor = next
	for _, id := range ids {
		r, err := s.st.Get(ctx, id)
		if errors.Is(err, logbook.ErrNotFound) {
			continue
		}
		if err != nil {
			s.cfg.Logf("logbook: read %s: %v", id, err)
			continue
		}
		rev := uint64(r.Revision)
		s.cfg.Publish(api.EventLogbookUpdated, id, rev, api.LogbookUpdated{ID: id, Revision: r.Revision, Type: r.Type, Project: r.Project, Deleted: r.Deleted})
		var kinds []string
		for _, f := range r.Flags {
			if !slices.Contains(kinds, f.Kind) {
				kinds = append(kinds, f.Kind)
			}
		}
		slices.Sort(kinds)
		if !slices.Equal(kinds, s.flags[id]) {
			if len(kinds) == 0 {
				delete(s.flags, id)
			} else {
				s.flags[id] = kinds
			}
			s.cfg.Publish(api.EventLogbookConflict, id, rev, api.LogbookConflict{ID: id, Revision: r.Revision, Flags: append([]string{}, kinds...)})
		}
	}
}

// Watch calls Changed every interval until ctx ends: the CLI's writes to the file while gff-hab
// runs reach clients within one interval.
func (s *Service) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Changed(ctx)
		}
	}
}

// --- shapes ---

// stamp writes a wall time in ms as the API's RFC 3339 form.
func stamp(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func actor(a *logbook.Actor) *api.LogbookActor {
	if a == nil {
		return nil
	}
	return &api.LogbookActor{Kind: a.Kind, Harness: a.Harness, Session: a.Session, Pane: a.Pane, Host: a.Host, Device: a.Device, ApprovedBy: a.ApprovedBy}
}

// Record is the API shape of a folded record. updated is its last change in ms.
func Record(r *logbook.Record, updated int64) api.LogbookRecord {
	out := api.LogbookRecord{ID: r.ID, Type: r.Type, Status: r.Status, Title: r.Title, Project: r.Project, Body: r.Body,
		Updated: stamp(updated), Revision: r.Revision, Deleted: r.Deleted, Tags: r.Tags, Created: stamp(r.Created.Wall),
		CreatedBy: actor(r.CreatedBy), SupersededBy: r.Successors, Identities: r.Identities}
	str := func(name string) string {
		var v string
		_ = json.Unmarshal(r.Fields[name].Value, &v)
		return v
	}
	out.Assignee, out.Triage, out.Component, out.Target, out.DecidedBy = str("assignee"), str("triage"), str("component"), str("target"), str("decided_by")
	out.Expires = str("expires")
	_ = json.Unmarshal(r.Fields["priority"].Value, &out.Priority)
	_ = json.Unmarshal(r.Fields["pinned"].Value, &out.Pinned)
	_ = json.Unmarshal(r.Fields["private"].Value, &out.Marked)
	out.Private = out.Marked // Service.record also counts the projects joined with it
	if d := r.Fields["draft"].Value; len(d) > 0 {
		out.Draft = api.RawJSON(d)
	}
	if len(r.Source) > 0 {
		var src api.LogbookSource
		if json.Unmarshal(r.Source, &src) == nil {
			out.Source = &src
		} else {
			// A forgotten source is a marker string, not an object.
			var marker string
			_ = json.Unmarshal(r.Source, &marker)
			out.Source = &api.LogbookSource{Quote: marker}
		}
	}
	for _, l := range r.Links {
		out.Links = append(out.Links, api.LogbookLink{Kind: l.Kind, To: l.To, Pos: l.Pos, Here: l.Here})
	}
	for _, l := range r.Backlinks {
		out.Backlinks = append(out.Backlinks, api.LogbookLink{Kind: l.Kind, To: l.To, Pos: l.Pos, Here: l.Here})
	}
	for _, n := range r.Notes {
		out.Notes = append(out.Notes, api.LogbookNote{Op: n.Op.String(), At: stamp(n.At.Wall), Text: n.Text, By: actor(n.Actor)})
	}
	for _, f := range r.Flags {
		lf := api.LogbookFlag{Kind: f.Kind, Field: f.Field}
		for _, v := range f.Values {
			lf.Values = append(lf.Values, api.LogbookFlagValue{Op: v.Op.String(), At: stamp(v.At.Wall), Value: api.RawJSON(v.Value), By: actor(v.Actor), Origin: v.Origin})
		}
		out.Flags = append(out.Flags, lf)
	}
	return out
}
