package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Event triggers in gff-hab (rounds.md 4.3, 4.5): the matcher hears every event this hab
// publishes, tests each event round's filter, and hands what matched to the scheduler's
// pending batches. Events come from this hab only.

// roundEventQueue bounds the events waiting for the matcher; past it an event is dropped and
// logged, so a publisher never waits.
const roundEventQueue = 4096

// roundLogbook is the Logbook the matcher reads: an op's actor and change, a record's title and
// tags, and the private projects.
type roundLogbook interface {
	History(ctx context.Context, p api.LogbookHistoryParams) (api.LogbookHistory, error)
	Get(ctx context.Context, p api.LogbookGetParams) (api.LogbookRecordResult, error)
	Store() *logbook.Store
}

type agentSeen struct {
	state api.AgentStatus
	turn  string
}

// roundMatcher turns hab events into round events. Its maps belong to its loop.
type roundMatcher struct {
	svc  *roundsService
	lb   roundLogbook
	ch   chan structure.Event
	done chan struct{}

	projects map[string]api.Project
	agents   map[string]agentSeen
	commands map[string]bool
	// folds is, per record, the highest fold number handled; start is the replica's number when
	// the matcher started: only ops folded after it count (rounds.md 4.3, folded since).
	folds map[string]int64
	start int64
	// mu guards stopped and the close of ch against observe.
	mu      sync.Mutex
	stopped bool
}

func newRoundMatcher(svc *roundsService, lb roundLogbook) *roundMatcher {
	m := &roundMatcher{svc: svc, lb: lb, ch: make(chan structure.Event, roundEventQueue), done: make(chan struct{}),
		projects: map[string]api.Project{}, agents: map[string]agentSeen{}, commands: map[string]bool{}, folds: map[string]int64{}}
	for _, p := range svc.cfg.store.Snapshot(api.HostInfo{}).Projects {
		m.projects[p.ID] = p
	}
	if lb != nil {
		if n, err := lb.Store().LastFold(context.Background()); err == nil {
			m.start = n
		} else {
			svc.cfg.logf("rounds: logbook fold number: %v", err)
		}
	}
	return m
}

// observe takes one published event. It never blocks: a full queue drops the event.
func (m *roundMatcher) observe(e structure.Event) {
	switch string(e.Type) {
	case rounds.EventProjectUpdated, rounds.EventAgentState, rounds.EventCommandUpdated, rounds.EventHabWoke,
		rounds.EventNoticeRaised, rounds.EventLogbookUpdated:
	default:
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	select {
	case m.ch <- e:
	default:
		m.svc.cfg.logf("rounds: event queue full; dropped a %s event", e.Type)
	}
}

func (m *roundMatcher) run() {
	defer close(m.done)
	for e := range m.ch {
		m.handle(e)
	}
}

// stop ends the loop once the queued events are handled. Later events are dropped.
func (m *roundMatcher) stop() {
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.ch)
	}
	m.mu.Unlock()
	<-m.done
}

// listening returns the event rounds that wait for kind and can take events now.
func (m *roundMatcher) listening(kind string) []rounds.Round {
	var out []rounds.Round
	for _, e := range m.svc.store.List() {
		r := e.Round
		if r.Trigger.Kind == rounds.TriggerEvent && r.Trigger.Event == kind && !r.Paused && !e.State.Completed {
			out = append(out, r)
		}
	}
	return out
}

// ownSessions returns the sessions whose events never trigger round r (rounds.md 4.3, no
// loops): its run sessions, and in capcom and group mode its destination's actor.
func (m *roundMatcher) ownSessions(r rounds.Round) map[string]bool {
	own := map[string]bool{}
	if runs, _, err := m.svc.store.Runs().Page(r.ID, 0, rounds.MaxKeepRuns); err == nil {
		for _, run := range runs {
			if run.SessionID != "" {
				own[run.SessionID] = true
			}
		}
	}
	for _, id := range m.svc.destinationActors(r) {
		own[id] = true
	}
	return own
}

func (m *roundMatcher) handle(e structure.Event) {
	kind := string(e.Type)
	now := time.Now().UTC()
	switch kind {
	case rounds.EventProjectUpdated:
		p, ok := e.Data.(api.Project)
		if !ok {
			return
		}
		prev, seen := m.projects[p.ID]
		m.projects[p.ID] = p
		if !seen {
			return // first sight: nothing to compare
		}
		var changed []string
		if prev.Head != p.Head {
			changed = append(changed, "head")
		}
		if prev.Branch != p.Branch {
			changed = append(changed, "branch")
		}
		if !sameInt(prev.Behind, p.Behind) {
			changed = append(changed, "behind")
		}
		if len(changed) == 0 {
			return
		}
		data := map[string]any{"project": p.ID, "name": p.Name, "changed": changed, "head": p.Head, "branch": p.Branch}
		if p.Behind != nil {
			data["behind"] = *p.Behind
		}
		title := fmt.Sprintf("project %s: %s changed (head %s on %s)", p.Name, strings.Join(changed, ", "), short(p.Head), p.Branch)
		for _, r := range m.listening(kind) {
			f := filterOf(r)
			if (f.Project != "" && f.Project != p.ID) || (len(f.Field) > 0 && !overlaps(f.Field, changed)) {
				continue
			}
			m.add(r, newEvent(now, kind, title, "", data))
		}
	case rounds.EventAgentState:
		st, ok := e.Data.(api.AgentState)
		if !ok {
			return
		}
		prev := m.agents[st.Pane]
		m.agents[st.Pane] = agentSeen{state: st.State, turn: st.Turn}
		if st.State != api.AgentStatusDone && st.State != api.AgentStatusError {
			return
		}
		if prev.state == st.State && prev.turn == st.Turn {
			return // the same end published again
		}
		ss, ok := m.svc.cfg.store.SessionByPane(st.Pane)
		if !ok {
			return
		}
		title := fmt.Sprintf("%s: agent %s", ss.Name, st.State)
		if st.Error != "" {
			title += " (" + st.Error + ")"
		}
		data := map[string]any{"session": ss.ID, "name": ss.Name, "project": ss.Project, "harness": st.Harness, "state": st.State}
		if st.Error != "" {
			data["error"] = st.Error
		}
		for _, r := range m.listening(kind) {
			f := filterOf(r)
			if (f.Session != "" && f.Session != ss.ID) || (f.Project != "" && f.Project != ss.Project) ||
				(len(f.State) > 0 && !slices.Contains(f.State, string(st.State))) || m.ownSessions(r)[ss.ID] {
				continue
			}
			m.add(r, newEvent(now, kind, title, "", data))
		}
	case rounds.EventCommandUpdated:
		c, ok := e.Data.(api.Command)
		if !ok || c.End == nil || c.Exit == nil || *c.Exit == 0 {
			return
		}
		key := fmt.Sprintf("%s/%d", e.ID, c.ID)
		if m.commands[key] {
			return
		}
		if len(m.commands) >= 10000 {
			m.commands = map[string]bool{}
		}
		m.commands[key] = true
		ss, ok := m.svc.cfg.store.SessionByPane(e.ID)
		if !ok {
			return
		}
		title := fmt.Sprintf("%s: command failed with exit %d: %s", ss.Name, *c.Exit, cut(c.Text, 200))
		data := map[string]any{"session": ss.ID, "name": ss.Name, "exit": *c.Exit, "command": c.Text, "cwd": c.Cwd}
		for _, r := range m.listening(kind) {
			f := filterOf(r)
			if (f.Session != "" && f.Session != ss.ID) || m.ownSessions(r)[ss.ID] {
				continue
			}
			m.add(r, newEvent(now, kind, title, "", data))
		}
	case rounds.EventHabWoke:
		w, ok := e.Data.(api.HabWoke)
		if !ok {
			return
		}
		slept := time.Duration(w.SleptS) * time.Second
		title := fmt.Sprintf("the host woke after %s asleep", slept.Round(time.Second))
		for _, r := range m.listening(kind) {
			m.add(r, newEvent(now, kind, title, "", map[string]any{"slept_s": w.SleptS}))
		}
	case rounds.EventNoticeRaised:
		n, ok := e.Data.(api.Notice)
		if !ok {
			return
		}
		project := ""
		if n.Session != "" {
			if ss, ok := m.svc.cfg.store.Session(n.Session); ok {
				project = ss.Project
			}
		}
		data := map[string]any{"id": n.ID, "title": n.Title, "body": n.Body, "level": n.Level, "session": n.Session}
		text := strings.ToLower(n.Title + "\n" + n.Body)
		for _, r := range m.listening(kind) {
			f := filterOf(r)
			switch {
			case n.Round != nil && n.Round.Round == r.ID: // a notice about the round itself
			case f.Level != "" && rounds.LevelRank(string(n.Level)) < rounds.LevelRank(f.Level):
			case f.Session != "" && f.Session != n.Session:
			case f.Project != "" && f.Project != project:
			case f.Text != "" && !strings.Contains(text, strings.ToLower(f.Text)):
			case n.Session != "" && m.ownSessions(r)[n.Session]:
			default:
				m.add(r, newEvent(now, kind, n.Title, string(n.Level), data))
			}
		}
	case rounds.EventLogbookUpdated:
		u, ok := e.Data.(api.LogbookUpdated)
		if !ok || m.lb == nil {
			return
		}
		m.logbook(now, u)
	}
}

// logbook matches a logbook.updated event: first its type, project and deleted mark, then the
// ops this replica folded for the record since the last one handled (rounds.md 4.3).
func (m *roundMatcher) logbook(now time.Time, u api.LogbookUpdated) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listening := m.listening(rounds.EventLogbookUpdated)
	if len(listening) == 0 {
		// No round reads the ops: the record's mark moves on, so a round made later sees only
		// the ops folded after it.
		if n, err := m.lb.Store().LastFold(ctx); err == nil {
			m.folds[u.ID] = n
		}
		return
	}
	after, ok := m.folds[u.ID]
	if !ok {
		after = m.start
	}
	var ops []api.LogbookOp
	read := false
	readOps := func() []api.LogbookOp {
		if read {
			return ops
		}
		read = true
		h, err := m.lb.History(ctx, api.LogbookHistoryParams{ID: u.ID, FoldedAfter: &after})
		if err != nil {
			m.svc.cfg.logf("rounds: logbook %s: %v", u.ID, err)
			return nil
		}
		ops = h.Ops
		if h.LastFold > after {
			m.folds[u.ID] = h.LastFold
		}
		return ops
	}
	defer readOps() // every event moves the record's mark, matched or not
	canon := func(id string) string {
		if c, err := m.lb.Store().Canonical(ctx, id); err == nil {
			return c
		}
		return id
	}
	var rec *api.LogbookRecord
	record := func() *api.LogbookRecord {
		if rec == nil {
			r, err := m.lb.Get(ctx, api.LogbookGetParams{ID: u.ID})
			if err != nil {
				rec = &api.LogbookRecord{ID: u.ID, Type: u.Type, Project: u.Project}
			} else {
				rec = &r.Record
			}
		}
		return rec
	}
	recProject := canon(u.Project)
	for _, r := range listening {
		f := filterOf(r)
		change := f.Change
		if change == "" {
			change = rounds.ChangeCreated
		}
		if f.Type != "" && f.Type != u.Type {
			continue
		}
		if f.Project != "" && m.logbookProject(ctx, f.Project) != recProject {
			continue
		}
		if u.Deleted && change != rounds.ChangeDeleted && change != rounds.ChangeAny {
			continue
		}
		if change == rounds.ChangeDeleted && !u.Deleted {
			continue
		}
		own := m.ownSessions(r)
		var hit []api.LogbookOp
		for _, o := range readOps() {
			if o.By != nil && o.By.Session != "" && own[o.By.Session] {
				continue
			}
			if f.By != "" && f.By != rounds.ByAny && byKind(o.By) != f.By {
				continue
			}
			if opChanges(o, change, f.Status) {
				hit = append(hit, o)
			}
		}
		if len(hit) == 0 {
			continue
		}
		if len(f.Tags) > 0 && !overlaps(f.Tags, record().Tags) {
			continue
		}
		if m.private(ctx, recProject) && m.roundLogbookProject(ctx, r) != recProject {
			m.add(r, newEvent(now, rounds.EventLogbookUpdated, "1 record in a private project", "", nil))
			continue
		}
		rc := record()
		what := describeOps(hit)
		title := fmt.Sprintf("%s %s: %s", rc.Type, cut(rc.Title, 120), what)
		data := map[string]any{"id": u.ID, "type": rc.Type, "title": rc.Title, "status": rc.Status, "project": rc.Project,
			"change": what, "revision": u.Revision}
		if o := hit[len(hit)-1]; o.By != nil {
			data["by"] = byKind(o.By)
		}
		m.add(r, newEvent(now, rounds.EventLogbookUpdated, title, "", data))
	}
}

// opChanges reports whether a Logbook op is the change a filter asks for.
func opChanges(o api.LogbookOp, change string, statuses []string) bool {
	switch change {
	case rounds.ChangeAny:
		return true
	case rounds.ChangeCreated:
		return o.Kind == "create"
	case rounds.ChangeStatus:
		if o.Kind != "set" || o.Field != "status" {
			return false
		}
		var v string
		_ = json.Unmarshal(o.Value, &v)
		return len(statuses) == 0 || slices.Contains(statuses, v)
	case rounds.ChangeDeleted:
		if o.Kind != "set" || o.Field != "deleted" {
			return false
		}
		var v bool
		_ = json.Unmarshal(o.Value, &v)
		return v
	}
	return false
}

func describeOps(ops []api.LogbookOp) string {
	o := ops[len(ops)-1]
	switch {
	case o.Kind == "create":
		return "created"
	case o.Kind == "set" && o.Field == "status":
		var v string
		_ = json.Unmarshal(o.Value, &v)
		return "status " + v
	case o.Kind == "set" && o.Field == "deleted":
		return "deleted"
	case o.Field != "":
		return o.Kind + " " + o.Field
	}
	return o.Kind
}

// byKind is who wrote an op: person, agent, or the hab itself.
func byKind(a *api.LogbookActor) string {
	if a != nil && (a.Kind == rounds.ByPerson || a.Kind == rounds.ByAgent) {
		return a.Kind
	}
	return rounds.ByHab
}

// logbookProject maps a filter's project, a structure project id, to its Logbook project's
// canonical id by its identity; any other value is taken as a Logbook project id.
func (m *roundMatcher) logbookProject(ctx context.Context, project string) string {
	if p, ok := m.svc.cfg.store.Project(project); ok {
		for _, ident := range []string{p.Remote, p.RootCommit} {
			if id, ok, err := m.lb.Store().ProjectByIdentity(ctx, ident); err == nil && ok {
				return id
			}
		}
		return ""
	}
	if c, err := m.lb.Store().Canonical(ctx, project); err == nil {
		return c
	}
	return project
}

// roundLogbookProject is the Logbook project a round's run session belongs to: a fresh round's
// project, a resume or fork round's target's; "" for capcom (the service has no project) and
// group (rounds.md 4.3, Private projects).
func (m *roundMatcher) roundLogbookProject(ctx context.Context, r rounds.Round) string {
	project := r.Project
	if r.Mode == rounds.ModeResume || r.Mode == rounds.ModeFork {
		if ss, ok := m.svc.cfg.store.Session(r.Target); ok {
			project = ss.Project
		}
	}
	if project == "" || r.Mode.Messages() {
		return ""
	}
	return m.logbookProject(ctx, project)
}

func (m *roundMatcher) private(ctx context.Context, project string) bool {
	if project == "" {
		return false
	}
	ids, err := m.lb.Store().PrivateProjects(ctx)
	return err == nil && slices.Contains(ids, project)
}

// add hands one event to the round's pending batch.
func (m *roundMatcher) add(r rounds.Round, ev rounds.Event) {
	if err := m.svc.sched.AddEvents(r.ID, []rounds.Event{ev}); err != nil {
		m.svc.cfg.logf("rounds: round %s: event: %v", r.Name, err)
	}
}

func newEvent(at time.Time, kind, title, level string, data any) rounds.Event {
	ev := rounds.Event{At: at, Kind: kind, Title: title, Level: level}
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			ev.Data = b
		}
	}
	return ev
}

func filterOf(r rounds.Round) rounds.EventFilter {
	if r.Trigger.Filter == nil {
		return rounds.EventFilter{}
	}
	return *r.Trigger.Filter
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func short(head string) string {
	if len(head) > 7 {
		return head[:7]
	}
	return head
}

// cut shortens s to at most n bytes on a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// --- What a run sees of its batch (rounds.md 4.5) ---

// runBatch reads a run's batch; a run with none, or whose file is gone, has an empty batch.
func (s *roundsService) runBatch(run rounds.Run) rounds.Batch {
	if run.Events == 0 {
		return rounds.Batch{}
	}
	b, err := s.store.ReadEvents(run.RoundID, run.ID)
	if err != nil {
		s.cfg.logf("rounds: run %s: its events: %v", run.ID, err)
		return rounds.Batch{Dropped: run.Events}
	}
	return b
}

// firstMessage is a fresh or fork harness run's first message: the event block, then the
// prompt (rounds.md 4.5).
func (s *roundsService) firstMessage(r rounds.Round, run rounds.Run, prompt string) string {
	if run.Events == 0 {
		return prompt
	}
	return s.eventBlockOf(run) + "\n\n" + prompt
}

// eventBlockOf is a run's event block (channels.EventBlock) at the hab's block bounds; the
// file in $GFF_ROUND_EVENTS holds the events the block leaves out.
func (s *roundsService) eventBlockOf(run rounds.Run) string {
	b := s.runBatch(run)
	block, _ := channels.EventBlock(run.ID, apiEvents(b.Events), b.Len(), s.cfg.settings.BlockMaxEvents,
		s.cfg.settings.BlockMaxBytes, s.cfg.settings.BlockDataMax)
	return block
}

// Woke raises hab.woke (rounds.WakeSink): a wake the scheduler detected, after its settle.
func (s *roundsService) Woke(slept time.Duration) {
	s.cfg.publish(structure.Event{Type: api.EventType(rounds.EventHabWoke),
		Data: api.HabWoke{SleptS: int64(slept / time.Second), At: time.Now().UTC().Format(time.RFC3339)}})
}
