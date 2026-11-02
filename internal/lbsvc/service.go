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

// Config is what New needs. Store is required.
type Config struct {
	Store *logbook.Store
	// Publish emits one event: logbook.updated or logbook.conflict. Nil emits nothing (the CLI).
	Publish func(t api.EventType, id string, revision uint64, data any)
	// PaneWrites is the pane write limit per minute; 0 is PaneWrites.
	PaneWrites int
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// Service serves the verbs over one store.
type Service struct {
	cfg Config
	st  *logbook.Store

	mu     sync.Mutex // orders Changed: one cursor, events in order
	cursor int64
	flags  map[string][]string // records that carry a flag, by id: their flag kinds

	limMu  sync.Mutex
	writes map[string][]time.Time // per pane: the times of its writes in the last minute
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
	s := &Service{cfg: cfg, st: cfg.Store, flags: map[string][]string{},
		writes: map[string][]time.Time{}, warned: map[string]time.Time{}}
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
	case errors.Is(err, logbook.ErrSharedOrigin):
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
	return s.search(ctx, p.Words, api.LogbookFilter{Type: p.Type, Status: p.Status, Project: p.Project, Tag: p.Tag,
		Host: p.Host, Author: p.Author, Session: p.Session, Since: p.Since, General: p.General, Deleted: p.Deleted, Limit: p.Limit}, false)
}

// List lists records, newest change first.
func (s *Service) List(ctx context.Context, p api.LogbookListParams) (api.LogbookRecords, error) {
	return s.search(ctx, "", p.LogbookFilter, false)
}

// Ready lists open tasks and issues with no open blocker.
func (s *Service) Ready(ctx context.Context, p api.LogbookReadyParams) (api.LogbookRecords, error) {
	return s.search(ctx, "", api.LogbookFilter{Project: p.Project, Limit: p.Limit}, true)
}

func (s *Service) search(ctx context.Context, words string, f api.LogbookFilter, ready bool) (api.LogbookRecords, error) {
	q := logbook.Query{Words: words, Type: f.Type, Status: f.Status, Project: f.Project, Tag: f.Tag, Host: f.Host,
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
	if p.ID == "" {
		return api.LogbookRecordResult{}, invalid("logbook.get needs an id")
	}
	r, err := s.st.Get(ctx, p.ID)
	if err != nil {
		return api.LogbookRecordResult{}, apiErr(err)
	}
	upd, err := s.st.Updated(ctx, r)
	if err != nil {
		return api.LogbookRecordResult{}, err
	}
	return api.LogbookRecordResult{Record: Record(r, upd)}, nil
}

// History lists every op on a record.
func (s *Service) History(ctx context.Context, p api.LogbookHistoryParams) (api.LogbookHistory, error) {
	if p.ID == "" {
		return api.LogbookHistory{}, invalid("logbook.history needs an id")
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
	items, _, err := s.st.Search(ctx, logbook.Query{Type: logbook.TypeProject, Limit: logbook.MaxLimit})
	if err != nil {
		return api.LogbookProjects{}, apiErr(err)
	}
	out := api.LogbookProjects{Projects: []api.LogbookProject{}}
	for _, it := range items {
		r, err := s.st.Get(ctx, it.ID)
		if err != nil {
			return out, apiErr(err)
		}
		out.Projects = append(out.Projects, api.LogbookProject{ID: r.ID, Name: r.Title, Identities: r.Identities, Revision: r.Revision})
	}
	slices.SortFunc(out.Projects, func(a, b api.LogbookProject) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
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
}

// WritePane is a write through a pane's token: the hab records the pane's harness, session and
// host, sets the source's session to the pane's, and refuses past the pane write limit.
func (s *Service) WritePane(ctx context.Context, pn Pane, p api.LogbookWriteParams) (api.LogbookWritten, error) {
	if err := s.allow(pn.ID); err != nil {
		return api.LogbookWritten{}, err
	}
	if p.Source != nil || pn.Session != "" {
		src := api.LogbookSource{}
		if p.Source != nil {
			src = *p.Source
		}
		src.Session = pn.Session
		p.Source = &src
	}
	return s.Write(ctx, PaneActor(pn), p)
}

// PaneActor is the actor of a write through a pane.
func PaneActor(pn Pane) api.LogbookActor {
	return api.LogbookActor{Kind: "agent", Harness: pn.Harness, Session: pn.Session, Pane: pn.ID, Host: pn.Host}
}

// allow counts one write from a pane, or refuses it past the limit.
func (s *Service) allow(pane string) error {
	now := s.cfg.Now()
	s.limMu.Lock()
	defer s.limMu.Unlock()
	w := s.writes[pane]
	cut := 0
	for cut < len(w) && now.Sub(w[cut]) >= time.Minute {
		cut++
	}
	w = w[cut:]
	if len(w) >= s.cfg.PaneWrites {
		s.writes[pane] = w
		if now.Sub(s.warned[pane]) >= time.Minute {
			s.warned[pane] = now
			s.cfg.Logf("logbook: pane %s wrote %d times in a minute; its writes are refused until the minute passes", pane, len(w))
		}
		wait := time.Minute - now.Sub(w[0])
		return invalid("this pane wrote %d times in the last minute, the limit: nothing was written. Wait %s, and write less often; a loop that writes on every step should batch its changes",
			len(w), wait.Round(time.Second))
	}
	s.writes[pane] = append(w, now)
	if len(s.writes) > 4096 { // panes come and go: drop the quiet ones
		for k, v := range s.writes {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute {
				delete(s.writes, k)
				delete(s.warned, k)
			}
		}
	}
	return nil
}

// Write applies one batch, or with propose stores it as a pending proposal (9.2).
func (s *Service) Write(ctx context.Context, a api.LogbookActor, p api.LogbookWriteParams) (api.LogbookWritten, error) {
	if len(p.Changes) == 0 {
		return api.LogbookWritten{}, invalid("logbook.write needs at least one change")
	}
	if len(p.IdempotencyKey) > logbook.MaxKey {
		return api.LogbookWritten{}, invalid("an idempotency key is at most %d bytes", logbook.MaxKey)
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
		b.Key, b.KeyHash = p.IdempotencyKey, hashOf("logbook.write", p)
	}
	res, err := s.st.Write(ctx, b)
	if err != nil {
		return api.LogbookWritten{}, apiErr(err)
	}
	s.Changed(ctx)
	out := written(res, state)
	if p.Propose && len(res.Records) > 0 {
		out.Proposal = res.Records[0]
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
	s.Changed(ctx)
	return written(res, api.LogbookWriteStateApplied), nil
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
			case (c.Tag == "") == (c.Link == nil):
				return nil, bad("takes a tag or a link, one of them")
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
	return &logbook.Actor{Kind: a.Kind, Harness: a.Harness, Session: a.Session, Pane: a.Pane, Host: a.Host, Device: a.Device}
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
	return &api.LogbookActor{Kind: a.Kind, Harness: a.Harness, Session: a.Session, Pane: a.Pane, Host: a.Host, Device: a.Device}
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
	_ = json.Unmarshal(r.Fields["priority"].Value, &out.Priority)
	_ = json.Unmarshal(r.Fields["pinned"].Value, &out.Pinned)
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
