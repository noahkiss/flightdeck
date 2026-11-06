package api

// --- Logbook (logbook.md 10.3). Needs the logbook capability. Times are RFC 3339 in UTC with
// milliseconds, from the writing hab's clock. Record types, statuses and link kinds are the words
// of logbook.md 2.2 and 2.4; a client shows an unknown one as given. ---

// LogbookFilter narrows a list or a search. Empty fields match everything.
type LogbookFilter struct {
	Type    string `json:"type,omitempty" doc:"A record type: finding, task, plan, guide, issue, proposal or project."`
	Status  string `json:"status,omitempty"`
	Project string `json:"project,omitempty" doc:"A project id. A project joined into another matches both (logbook.md 5.6)."`
	Tag     string `json:"tag,omitempty"`
	Host    string `json:"host,omitempty" doc:"Records created on this host: the writer's host id, or the creating hab's device id."`
	Author  string `json:"author,omitempty" doc:"Records created by this session id, client device id or harness."`
	Session string `json:"session,omitempty" doc:"Records that came from this session: written by it, or with it as their source. Capcom lists one conversation's proposals with it."`
	Since   string `json:"since,omitempty" doc:"RFC 3339 time: only records changed at or after it."`
	General bool   `json:"general,omitempty" doc:"Only records that belong to no project (logbook.md 3.2)."`
	Flagged bool   `json:"flagged,omitempty" doc:"Only records with a flag: a conflict, decided twice and the like (logbook.md 10.2 conflicts)."`
	Deleted bool   `json:"deleted,omitempty" doc:"Include deleted records."`
	Limit   int    `json:"limit,omitempty" doc:"At most this many; default 50, at most 500."`
	// Added by stage 6 (logbook.md 11.4).
	All bool `json:"all,omitempty" doc:"Every project. Without it, a list on the pane route with no project, general or all covers the pane's project, the default view (logbook.md 12.1)."`
}

type LogbookFindParams struct {
	Words   string `json:"words,omitempty" doc:"Words to search for in title, body, notes and tags; every word must match. Absent: list records, as logbook.list."`
	Type    string `json:"type,omitempty"`
	Status  string `json:"status,omitempty"`
	Project string `json:"project,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Host    string `json:"host,omitempty"`
	Author  string `json:"author,omitempty"`
	Since   string `json:"since,omitempty" doc:"RFC 3339 time."`
	Limit   int    `json:"limit,omitempty"`
	Session string `json:"session,omitempty" doc:"As in logbook.list."`
	General bool   `json:"general,omitempty" doc:"As in logbook.list."`
	Deleted bool   `json:"deleted,omitempty" doc:"As in logbook.list."`
	Flagged bool   `json:"flagged,omitempty" doc:"As in logbook.list."`
	// Added by stage 9 (logbook.md 13).
	Semantic bool `json:"semantic,omitempty" doc:"Rank by meaning through vector search, on this hab when it has the logbook-vectors capability, else on an online peer that has it; with none, unavailable: no always-on host with vector search. Needs words."`
}

type LogbookListParams struct {
	LogbookFilter
}

type LogbookReadyParams struct {
	Project string `json:"project,omitempty" doc:"A project id. Absent: every project, except on the pane route, where it is the pane's project unless all is set (logbook.md 12.1)."`
	Limit   int    `json:"limit,omitempty"`
	// Added by stage 6 (logbook.md 11.4).
	All bool `json:"all,omitempty" doc:"Every project, also on the pane route."`
}

// LogbookBriefParams asks for a session brief (logbook.md 11.2).
type LogbookBriefParams struct {
	Project string `json:"project,omitempty" doc:"A project id. Absent: on the pane route, the pane's project; else no project, and the brief holds only the records tagged everywhere."`
}

// LogbookBrief is a session brief: what an agent should know about its project now, one line per
// record, at most 8 KB (logbook.md 11.2, 11.4).
type LogbookBrief struct {
	Seq
	Project string `json:"project,omitempty" doc:"The project the brief covers, or absent for none."`
	Name    string `json:"name,omitempty" doc:"The project's name."`
	Text    string `json:"text" doc:"The brief as the agent gets it."`
	Records int    `json:"records" doc:"The record lines in text."`
	Dropped int    `json:"dropped,omitempty" doc:"Record lines left out at the 8 KB cap, lowest rank first; text then ends with a more line."`
}

// LogbookRecord is one Logbook record as a read returns it. A search or list fills the summary
// fields only: id, type, status, title, project, updated, revision, deleted.
type LogbookRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Status  string `json:"status,omitempty"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Body    string `json:"body,omitempty" doc:"Absent in a search result."`
	Updated string `json:"updated,omitempty" doc:"RFC 3339 time of the last change."`
	// Added by stage 4 (logbook.md 10.3).
	Revision     int            `json:"revision,omitempty" doc:"The record's count of folded ops: it never goes down. logbook.updated carries the same number."`
	Deleted      bool           `json:"deleted,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
	Created      string         `json:"created,omitempty" doc:"RFC 3339 time of the create."`
	CreatedBy    *LogbookActor  `json:"created_by,omitempty"`
	Source       *LogbookSource `json:"source,omitempty"`
	Assignee     string         `json:"assignee,omitempty" doc:"A task or issue: a session or a person."`
	Priority     int            `json:"priority,omitempty" doc:"A task or issue: 1 to 3."`
	Triage       string         `json:"triage,omitempty" doc:"A task or issue: new until a person looked at it."`
	Component    string         `json:"component,omitempty" doc:"An issue: the tool, file or service that misbehaves."`
	Pinned       bool           `json:"pinned,omitempty" doc:"A guide: in every brief for its project."`
	Target       string         `json:"target,omitempty" doc:"A proposal: record, memory or flight_rules."`
	Draft        RawJSON        `json:"draft,omitempty" doc:"A proposal: the full content it would apply, as logbook.write's changes."`
	DecidedBy    string         `json:"decided_by,omitempty" doc:"A decided proposal: the deciding device."`
	Expires      string         `json:"expires,omitempty" doc:"A memory proposal: RFC 3339 time after which it can no longer be decided (logbook.md 9.2)."`
	Identities   []string       `json:"identities,omitempty" doc:"A project: its identities (logbook.md 3.1)."`
	Links        []LogbookLink  `json:"links,omitempty" doc:"This record's links."`
	Backlinks    []LogbookLink  `json:"backlinks,omitempty" doc:"Other records' links to this one; to names the linking record."`
	Notes        []LogbookNote  `json:"notes,omitempty"`
	Flags        []LogbookFlag  `json:"flags,omitempty" doc:"What the user should look at: conflict, decided_twice, edited_after_delete, id_collision, superseded_twice, approval_overruled."`
	SupersededBy []string       `json:"superseded_by,omitempty" doc:"A finding: the findings that supersede it."`
	// Added by stage 5 (logbook.md 3.3).
	Private    bool   `json:"private,omitempty" doc:"A project hidden from agents' reads outside it (logbook.md 12.1): it, or a project joined with it, is marked private."`
	Marked     bool   `json:"marked,omitempty" doc:"A project that is itself marked private."`
	JoinedInto string `json:"joined_into,omitempty" doc:"A project joined with an older one: the project it shows as (logbook.md 5.6)."`
}

// LogbookActor is who asked for a change (logbook.md 2.5): an agent through its pane, or a
// person through a client device.
type LogbookActor struct {
	Kind    string `json:"kind,omitempty" doc:"agent or person."`
	Harness string `json:"harness,omitempty"`
	Session string `json:"session,omitempty"`
	// SessionName is added after stage 7: an older op has none, so a client shows the session id.
	SessionName string `json:"session_name,omitempty" doc:"The session's name when the write was made, for display; session stays the id."`
	Pane        string `json:"pane,omitempty"`
	Host        string `json:"host,omitempty"`
	Device      string `json:"device,omitempty"`
	// Added by stage 7 (logbook.md 9.2).
	ApprovedBy string `json:"approved_by,omitempty" doc:"A change an approved proposal made: the device that approved it. The actor stays the proposing agent."`
}

// LogbookSource is where a record came from: a session, a message and a quote of the user's
// words (logbook.md 2.5).
type LogbookSource struct {
	Session string `json:"session,omitempty" doc:"Filled in by the hab from the pane's token on the pane route."`
	Message string `json:"message,omitempty"`
	Quote   string `json:"quote,omitempty"`
}

// LogbookLink is a typed link (logbook.md 2.4).
type LogbookLink struct {
	Kind string `json:"kind" doc:"supersedes, stage, part_of, blocks, relates, from or duplicates."`
	To   string `json:"to" doc:"A record id."`
	Pos  int    `json:"pos,omitempty" doc:"A stage link's position."`
	Here bool   `json:"here,omitempty" doc:"In a read: the target is on this replica. False shows as not here yet."`
}

// LogbookNote is one entry of a record's notes.
type LogbookNote struct {
	Op   string        `json:"op"`
	At   string        `json:"at"`
	Text string        `json:"text"`
	By   *LogbookActor `json:"by,omitempty"`
}

// LogbookFlag marks something the user should look at, with each side.
type LogbookFlag struct {
	Kind   string             `json:"kind"`
	Field  string             `json:"field,omitempty"`
	Values []LogbookFlagValue `json:"values,omitempty"`
}

// LogbookFlagValue is one side of a flag: the op, its value, who made it and where.
type LogbookFlagValue struct {
	Op     string        `json:"op"`
	At     string        `json:"at"`
	Value  RawJSON       `json:"value,omitempty"`
	By     *LogbookActor `json:"by,omitempty"`
	Origin string        `json:"origin"`
}

// LogbookRecords answers logbook.find, logbook.list and logbook.ready.
type LogbookRecords struct {
	Seq
	Records []LogbookRecord `json:"records"`
	More    bool            `json:"more,omitempty"`
}

type LogbookGetParams struct {
	ID string `json:"id"`
}

// LogbookRecordResult answers logbook.get.
type LogbookRecordResult struct {
	Seq
	Record LogbookRecord `json:"record"`
}

type LogbookHistoryParams struct {
	ID string `json:"id"`
	// FoldedAfter was added by rounds stage 9 (rounds.md 4.3).
	FoldedAfter *int64 `json:"folded_after,omitempty" doc:"Only the ops this replica folded after this local fold number, in fold order. The numbers are this replica's own and never synced; logbook.history's folded gives the highest."`
}

// LogbookOp is one op as logbook.history shows it.
type LogbookOp struct {
	ID     string        `json:"id" doc:"seq@origin."`
	At     string        `json:"at"`
	Kind   string        `json:"kind" doc:"create, set, add, remove, append or forget; another word from a newer build."`
	Record string        `json:"record"`
	Field  string        `json:"field,omitempty"`
	Value  RawJSON       `json:"value,omitempty"`
	Prev   []string      `json:"prev,omitempty" doc:"A set: the ops whose value the writer saw. A remove: the adds it saw."`
	Target string        `json:"target,omitempty" doc:"A forget: the op it redacts."`
	By     *LogbookActor `json:"by,omitempty"`
	Folded bool          `json:"folded,omitempty" doc:"This build folds the op. False: kept and forwarded for a newer build (logbook.md 4.4)."`
	Fold   int64         `json:"fold,omitempty" doc:"With folded_after: the local fold number this replica gave the op."`
}

// LogbookHistory answers logbook.history: every op on the record, losers included, in stamp
// order.
type LogbookHistory struct {
	Seq
	Ops []LogbookOp `json:"ops"`
	// LastFold was added by rounds stage 9.
	LastFold int64 `json:"last_fold,omitempty" doc:"With folded_after: the highest fold number this replica has given, to pass as folded_after next."`
}

// LogbookChangeOp is one kind of change in logbook.write. A closed set.
type LogbookChangeOp string

// LogbookChange is one change of a write. create takes the record fields, and tags and links
// added in the same batch; set takes field and value; add and remove take tag, link or identity;
// note takes text; forget takes op_id.
type LogbookChange struct {
	Op        LogbookChangeOp `json:"op"`
	ID        string          `json:"id,omitempty" doc:"The record. On create, an id to claim; absent mints one."`
	Type      string          `json:"type,omitempty" doc:"create: the record type."`
	Project   string          `json:"project,omitempty"`
	Title     string          `json:"title,omitempty"`
	Body      string          `json:"body,omitempty"`
	Status    string          `json:"status,omitempty"`
	Assignee  string          `json:"assignee,omitempty"`
	Priority  int             `json:"priority,omitempty"`
	Triage    string          `json:"triage,omitempty"`
	Component string          `json:"component,omitempty"`
	Pinned    bool            `json:"pinned,omitempty"`
	Tags      []string        `json:"tags,omitempty" doc:"create: tags to add."`
	Links     []LogbookLink   `json:"links,omitempty" doc:"create: links to add."`
	Field     string          `json:"field,omitempty" doc:"set: the field."`
	Value     RawJSON         `json:"value,omitempty" doc:"set: the new value."`
	Tag       string          `json:"tag,omitempty" doc:"add, remove: a tag."`
	Link      *LogbookLink    `json:"link,omitempty" doc:"add, remove: a link."`
	Text      string          `json:"text,omitempty" doc:"note: the note."`
	OpID      string          `json:"op_id,omitempty" doc:"forget: the op whose value is redacted everywhere (logbook.md 5.7)."`
	// Added by stage 5 (logbook.md 3.3).
	Identity string `json:"identity,omitempty" doc:"add, remove: a project's identity (logbook.md 3.1). Only a person changes one."`
}

// LogbookWriteParams is one batch of changes: it lands all or nothing.
type LogbookWriteParams struct {
	IdempotencyKey string          `json:"idempotency_key,omitempty" doc:"At most 128 bytes. A key the hab holds applies nothing again and answers the first write's result; with another request it answers conflict. Kept 90 days."`
	Propose        bool            `json:"propose,omitempty" doc:"Store the batch as a pending proposal, whatever logbook.approval says (logbook.md 9.1)."`
	Changes        []LogbookChange `json:"changes"`
	Source         *LogbookSource  `json:"source,omitempty" doc:"Where the records came from; set on each create, and on a proposal."`
}

// LogbookWriteState says what a write did. The server sends it, so the set is open.
type LogbookWriteState string

// LogbookWritten answers logbook.write, logbook.resolve and logbook.decide.
type LogbookWritten struct {
	Seq
	State    LogbookWriteState `json:"state" doc:"applied: the changes landed. proposed: a pending proposal holds them. rejected: logbook.decide rejected the proposal."`
	IDs      []string          `json:"ids" doc:"The records the batch touched, in change order; for a proposal, the proposal."`
	Ops      []string          `json:"ops" doc:"The op ids the batch made."`
	Proposal string            `json:"proposal,omitempty" doc:"With state proposed: the proposal's id."`
	Repeated bool              `json:"repeated,omitempty" doc:"The idempotency key was used before: nothing was applied again; ids and ops are the first write's."`
	// Added by stage 7 (logbook.md 9.1).
	Note string `json:"note,omitempty" doc:"Words for the writer to read. With state proposed: the change waits for the user's approval, and the writer should not ask again."`
}

type LogbookResolveParams struct {
	ID             string  `json:"id"`
	Field          string  `json:"field"`
	Value          RawJSON `json:"value,omitempty" doc:"The value to keep. Or name op."`
	Op             string  `json:"op,omitempty" doc:"The op id whose value to keep, from the conflict flag."`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

// LogbookDecision is how logbook.decide decides a proposal. A closed set.
type LogbookDecision string

// LogbookDecideParams decides one pending proposal (logbook.md 9.2). Only a person decides: the
// pane route refuses it.
type LogbookDecideParams struct {
	ID             string          `json:"id" doc:"The proposal."`
	Decision       LogbookDecision `json:"decision"`
	Edits          []LogbookChange `json:"edits,omitempty" doc:"approve: the changes to apply in place of the draft's. The proposal is then edited, not approved."`
	IdempotencyKey string          `json:"idempotency_key,omitempty" doc:"As in logbook.write."`
}

type LogbookProjectsParams struct{}

// LogbookProject is one project (logbook.md 3).
type LogbookProject struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Identities []string `json:"identities,omitempty"`
	Revision   int      `json:"revision"`
	// Added by stage 5 (logbook.md 3.3).
	Private    bool   `json:"private,omitempty" doc:"Hidden from agents' reads outside the project (logbook.md 12.1): this project, or one joined with it, is marked private."`
	Marked     bool   `json:"marked,omitempty" doc:"This project itself is marked private."`
	JoinedInto string `json:"joined_into,omitempty" doc:"Joined with an older project: the project it shows as (logbook.md 5.6)."`
}

// LogbookProjects answers logbook.projects.
type LogbookProjects struct {
	Seq
	Projects []LogbookProject `json:"projects"`
}

type LogbookStatusParams struct{}

// LogbookOrigin is one origin's place in the version vector.
type LogbookOrigin struct {
	Origin string `json:"origin"`
	Seq    uint64 `json:"seq" doc:"The highest seq held with no gap below it."`
}

// LogbookStatus answers logbook.status: the replica's size and health.
type LogbookStatus struct {
	Seq
	Origin   string          `json:"origin" doc:"The device id this replica writes as."`
	Ops      int             `json:"ops"`
	Records  int             `json:"records"`
	Unfolded int             `json:"unfolded,omitempty" doc:"Ops this build keeps but does not fold."`
	Waiting  int             `json:"waiting,omitempty" doc:"Ops held behind a gap or an incomplete batch."`
	Ahead    []string        `json:"ahead,omitempty" doc:"Origins whose clock ran more than an hour ahead."`
	Shared   []string        `json:"shared,omitempty" doc:"Origins two hosts share; their further ops are refused."`
	Vector   []LogbookOrigin `json:"vector"`
}

// LogbookUpdated is logbook.updated's data: a record changed, by any writer of the file.
type LogbookUpdated struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Type     string `json:"type"`
	Project  string `json:"project,omitempty"`
	Deleted  bool   `json:"deleted,omitempty"`
}

// LogbookConflict is logbook.conflict's data: a record's flags changed. An empty list means the
// last one went.
type LogbookConflict struct {
	ID       string   `json:"id"`
	Revision int      `json:"revision"`
	Flags    []string `json:"flags" doc:"The flag kinds the record now has; empty when it has none."`
}
