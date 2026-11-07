package api

// --- Catalog (mesh.md 5) ---

// Catalog is what one hab publishes about its host (mesh.md 5.1). Each hab is the only source
// of its own catalog; other habs keep copies.
type Catalog struct {
	DeviceID     string              `json:"device_id" doc:"The publishing hab's device id."`
	Host         string              `json:"host" doc:"The publishing hab's host id."`
	Generation   uint64              `json:"generation" doc:"Raised on every change of this catalog; kept across restarts."`
	At           string              `json:"at" doc:"RFC 3339 time of the last change, by the publishing hab's clock."`
	System       CatalogSystem       `json:"system"`
	Toolchains   []CatalogTool       `json:"toolchains" doc:"Tools found on PATH, with their versions."`
	Harnesses    []CatalogTool       `json:"harnesses" doc:"Coding-agent harnesses found on PATH, with their versions."`
	Capabilities []CatalogCapability `json:"capabilities" doc:"Detected and declared capability words, sorted."`
	Services     []CatalogService    `json:"services" doc:"Services the user declared for this host."`
	Actions      []CatalogAction     `json:"actions" doc:"Quick actions the user declared for this host."`
	Projects     []CatalogProject    `json:"projects" doc:"Git projects on this host, one per identity."`
	Agents       []CatalogAgent      `json:"agents" doc:"Sessions with a running agent."`
	AlwaysOn     bool                `json:"always_on,omitempty" doc:"The user marked this host always on (mesh.md 8)."`
	Peers        []string            `json:"peers" doc:"The device ids in this hab's peer list."`
	Watches      []string            `json:"watches" doc:"The peers marked always on that this hab watches: each is in its peer list and its copy carries the mark (mesh.md 8)."`
	Capcom       *CatalogCapcom      `json:"capcom,omitempty" doc:"The Capcom home host as this hab knows it, and the Capcom sessions on this host (capcom.md 7.3). Absent from a hab without the capcom capability, and while no host was ever assigned and this host has no Capcom session."`
	// Rounds was added within revision 1, a new field set of each catalog message (rounds.md 9).
	Rounds []CatalogRound `json:"rounds,omitempty" doc:"The rounds this hab runs, by name. Absent from a hab without the rounds capability or with no round."`
	// Heavy was added within revision 1, a new field of each catalog message (groups.md 8.2).
	Heavy *CatalogHeavy `json:"heavy,omitempty" doc:"The heavy-run lock and the free space of the state and worktree volumes. Absent from a hab without the heavy capability."`
	// Groups was added by rounds stage 9 as the read side of groups.md 5.1; Part 15 fills it.
	Groups []CatalogGroup `json:"groups,omitempty" doc:"The groups this hab is the home of: the short form (groups.md 5.1). Absent with none."`
	// Governor was added within revision 1 by the usage governor (capcom-service.md 10.1).
	Governor *CatalogGovernor `json:"governor,omitempty" doc:"The usage governor's answer for each consumer this hab paces. Absent with none."`
}

// CatalogGovernor is the usage governor's state on the publishing hab (capcom-service.md 10.1,
// groups.md 10).
type CatalogGovernor struct {
	Consumers []GovernorConsumer `json:"consumers" doc:"By id."`
}

// GovernorConsumer is one consumer's pace.
type GovernorConsumer struct {
	ID           string `json:"id" doc:"capcom, or group:<id>."`
	Basis        string `json:"basis" doc:"plan: the plan's weekly window; usd: today's result cost against the daily dollar cap; none: unpaced, with no reading, its tokens counted. An open set."`
	Pace         string `json:"pace" doc:"The next user turn's pace: ok, small (the small model) or wait. An open set."`
	EventPace    string `json:"event_pace" doc:"The next event turn's pace: ok or wait. An open set."`
	Until        string `json:"until,omitempty" doc:"RFC 3339 UTC time a wait of user turns ends."`
	EventUntil   string `json:"event_until,omitempty" doc:"RFC 3339 UTC time a wait of event turns ends."`
	UsedPct      int    `json:"used_pct,omitempty" doc:"plan: the weekly window's used percentage, the whole account's."`
	SharePct     int    `json:"share_pct,omitempty" doc:"plan: the consumer's share of the window so far, in whole percent: the daily share times the days begun, at most 100."`
	SpentCents   int64  `json:"spent_cents,omitempty" doc:"usd: today's reported cost, in US cents."`
	CapCents     int64  `json:"cap_cents,omitempty" doc:"usd: the daily dollar cap, in US cents."`
	InputTokens  int64  `json:"input_tokens,omitempty" doc:"Today's input tokens, by the hab's local day."`
	OutputTokens int64  `json:"output_tokens,omitempty" doc:"Today's output tokens."`
}

// CatalogGroup is one group's short form in its home hab's catalog (groups.md 5.1, 12.5).
type CatalogGroup struct {
	ID          string         `json:"id"`
	Title       string         `json:"title,omitempty"`
	State       string         `json:"state" doc:"confirming, running, paused, ending, ended or dropped. An open set."`
	Coordinator *CatalogMember `json:"coordinator,omitempty" doc:"The coordinator, when the group has one."`
	Generation  uint64         `json:"generation,omitempty"`
	// Home, Items, Landed, Members and Pause come with the group record (groups.md 5.1, stage
	// B3). Optional, so a reader of an older copy sees none.
	Home    string      `json:"home,omitempty" doc:"The home hab's device id."`
	Items   int         `json:"items,omitempty" doc:"Plan items."`
	Landed  int         `json:"landed,omitempty" doc:"Items landed."`
	Members int         `json:"members,omitempty" doc:"Members started."`
	Pause   *GroupPause `json:"pause,omitempty" doc:"The pause reasons, while paused: each member's hab holds messages into that group's members (groups.md 5.6)."`
	// Revision came with the pause push (groups.md 5.6): a member's hab compares it with the
	// revision of the last group.sync it took. Optional, so an older copy reads 0.
	Revision uint64 `json:"revision,omitempty" doc:"The group record's revision when this copy was made."`
}

// CatalogMember names a session on a hab of the mesh.
type CatalogMember struct {
	Device  string `json:"device" doc:"The device id of the hab the session runs on."`
	Session string `json:"session,omitempty" doc:"The session id on that hab."`
	Name    string `json:"name" doc:"The session's name on that hab: its channel address there."`
	Capcom  bool   `json:"capcom,omitempty" doc:"The coordinator is the Capcom service: a round message goes to it (groups.md 12.1)."`
}

// CatalogRound is one round on the publishing host, as peers see it (rounds.md 9, 10.3).
type CatalogRound struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Mode      RoundMode     `json:"mode"`
	Trigger   string        `json:"trigger" doc:"The trigger in plain words, such as every 1h, cron 0 9 * * 1-5 in Europe/Berlin, once at <time>, or by hand."`
	Project   string        `json:"project,omitempty" doc:"The project's name on the publishing host. Absent for resume and fork rounds."`
	Remote    string        `json:"remote,omitempty" doc:"The project's normalized remote, when it has one: its identity across hosts."`
	NextSlot  string        `json:"next_slot,omitempty" doc:"RFC 3339 UTC time of the next slot. Absent when there is none."`
	LastRun   RoundRunState `json:"last_run,omitempty" doc:"The newest run's state. Absent before the first run."`
	LastRunAt string        `json:"last_run_at,omitempty" doc:"RFC 3339 time the newest run ended, else started, else was made."`
	Paused    bool          `json:"paused,omitempty"`
	Completed bool          `json:"completed,omitempty"`
}

// CatalogCapcom is one hab's view of the Capcom assignment, and its own Capcom sessions. Every
// hab publishes the highest assignment it has seen; the home host's own catalog carries the
// mark (home is its own device id). Across catalogs the highest assignment wins, and on a tie
// the lower home device id.
type CatalogCapcom struct {
	Assignment uint64 `json:"assignment" doc:"The highest assignment counter this hab has seen; 0 for none."`
	Home       string `json:"home,omitempty" doc:"The device id of the home host at that assignment."`
	// Service was added by rounds stage 9 as the read side of capcom-service.md 9.5.
	Service  string                 `json:"service,omitempty" doc:"On the home host, while it runs the Capcom service: the service's actor id, the session id its notices and Logbook ops carry."`
	Sessions []CatalogCapcomSession `json:"sessions" doc:"The Capcom sessions on this host, the standing one marked."`
}

// CatalogCapcomSession is one Capcom session on the publishing host.
type CatalogCapcomSession struct {
	Session  string `json:"session" doc:"The session id on its host."`
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	Standing bool   `json:"standing,omitempty" doc:"The standing conversation, which capcom.open returns."`
}

// CatalogSystem is the host's detected system facts. A fact that could not be read is absent.
type CatalogSystem struct {
	OS          string `json:"os" doc:"linux, darwin, or another Go GOOS word."`
	OSVersion   string `json:"os_version,omitempty"`
	Arch        string `json:"arch" doc:"Go GOARCH word, such as amd64 or arm64."`
	Cores       int    `json:"cores"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
}

// CatalogTool is a program found on PATH.
type CatalogTool struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty" doc:"The first version-like word of its version output. Absent when none was found."`
}

// CatalogCapability is a capability word and where it came from.
type CatalogCapability struct {
	Name     string `json:"name"`
	Detected bool   `json:"detected,omitempty"`
	Declared bool   `json:"declared,omitempty" doc:"Named in the hab's config."`
}

// CatalogService is a service the user declared for the host.
type CatalogService struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Health string `json:"health,omitempty" doc:"A health URL, as the user wrote it."`
}

// CatalogAction is a quick action the user declared for the host.
type CatalogAction struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// CatalogProject is one project identity on the host, with its folders.
type CatalogProject struct {
	Name       string          `json:"name"`
	Remote     string          `json:"remote,omitempty" doc:"The normalized git remote that identifies the project."`
	RootCommit string          `json:"root_commit,omitempty" doc:"The root commit, when the project has no remote."`
	Summary    string          `json:"summary,omitempty" doc:"The first line of AGENTS.md, else README, in its first folder."`
	Folders    []CatalogFolder `json:"folders"`
}

// CatalogFolder is one working folder of a project.
type CatalogFolder struct {
	Path       string `json:"path"`
	Branch     string `json:"branch,omitempty" doc:"Absent on a detached HEAD."`
	Head       string `json:"head,omitempty" doc:"Full commit id of HEAD. Absent before the first commit."`
	Dirty      *bool  `json:"dirty,omitempty" doc:"Changes or untracked files. Absent when not read yet."`
	LastCommit string `json:"last_commit,omitempty" doc:"RFC 3339 commit time of HEAD."`
}

// CatalogAgent is a session with a running agent.
type CatalogAgent struct {
	Session string      `json:"session" doc:"The session's name on its host."`
	Harness string      `json:"harness"`
	Project string      `json:"project,omitempty" doc:"The project's name. Absent for an unfiled session."`
	State   AgentStatus `json:"state"`
	Since   string      `json:"since" doc:"RFC 3339 time the state began."`
	// Group and ContextPct come with stage B3 of agent groups (groups.md 2.2): the home hab
	// reads its members' states from the copies. Optional, so a reader of an older copy sees
	// none.
	Group      *SessionGroup `json:"group,omitempty" doc:"The session is a member of this group."`
	ContextPct *int          `json:"context_pct,omitempty" doc:"How full its context window is, in whole percent; absent when unknown."`
	ID         string        `json:"id,omitempty" doc:"The session's id on its host."`
	// Compacts and Replaced come with context supervision (groups.md 9, stage B6).
	Compacts int    `json:"compacts,omitempty" doc:"A member's compactions its hab counted."`
	Replaced string `json:"replaced,omitempty" doc:"The session this fresh member session replaced."`
}

type CatalogGetParams struct {
	Since uint64 `json:"since,omitempty" doc:"The generation the caller holds; 0 for none."`
}

// CatalogGetResult answers catalog.get with this hab's own catalog.
type CatalogGetResult struct {
	Seq
	Generation uint64   `json:"generation" doc:"This hab's catalog generation now."`
	Catalog    *Catalog `json:"catalog,omitempty" doc:"The whole catalog. Absent when generation equals since."`
}

type CatalogPutParams struct {
	Catalog Catalog `json:"catalog" doc:"The caller's own catalog. Its device_id must be the peer link's."`
}

type CatalogListParams struct{}

// CatalogList is the merged view: this hab's catalog and the copies it holds of its direct
// peers' catalogs (mesh.md 5.2).
type CatalogList struct {
	Seq
	Hosts    []CatalogEntry        `json:"hosts" doc:"This hab first, then its peers' copies in peer list order."`
	Projects []CatalogProjectGroup `json:"projects" doc:"Projects matched by identity across hosts."`
}

// CatalogEntry is one host's catalog as this hab holds it.
type CatalogEntry struct {
	Label   string  `json:"label,omitempty" doc:"This hab's label for the peer. Absent for this hab and for a hab not in the peer list."`
	Own     bool    `json:"own,omitempty" doc:"This hab's own catalog."`
	Stale   bool    `json:"stale,omitempty" doc:"The peer is offline; the copy is its last."`
	SeenAt  string  `json:"seen_at" doc:"RFC 3339 time this hab last confirmed the copy with the peer."`
	Catalog Catalog `json:"catalog"`
}

// CatalogProjectGroup is one project identity with every host it is on.
type CatalogProjectGroup struct {
	Key        string            `json:"key" doc:"remote:<remote>, root:<commit>, else host:<device id>/<folder>."`
	Name       string            `json:"name"`
	Remote     string            `json:"remote,omitempty"`
	RootCommit string            `json:"root_commit,omitempty"`
	Locations  []CatalogLocation `json:"locations" doc:"One per host, in host order."`
}

// CatalogLocation is a project on one host.
type CatalogLocation struct {
	DeviceID string          `json:"device_id"`
	Label    string          `json:"label,omitempty" doc:"As in the host's entry."`
	Stale    bool            `json:"stale,omitempty"`
	Folders  []CatalogFolder `json:"folders"`
}

// CatalogUpdated is the data of catalog.updated: a catalog this hab holds changed.
type CatalogUpdated struct {
	DeviceID   string `json:"device_id"`
	Generation uint64 `json:"generation"`
}

// --- Capcom (capcom.md 7, 8.3) ---

type CapcomOpenParams struct {
	New     bool               `json:"new,omitempty" doc:"Start a fresh Capcom session in a new folder; the standing one stays resumable."`
	Context []CapcomContextRef `json:"context,omitempty" doc:"Context to attach to the returned session's next message, in order, as capcom.attach takes it (capcom.md 8.1). A refused item fails the call; items checked before the open fail it before any start. A file names an attachment of the returned session, or a path under its folder."`
}

// CapcomContextKind is what an attached context item names (capcom.md 8.1). Closed: the hab
// refuses an unknown word.
type CapcomContextKind string

// CapcomContextRef names one thing to attach to a Capcom message. The hab resolves it itself.
type CapcomContextRef struct {
	Kind CapcomContextKind `json:"kind"`
	Ref  string            `json:"ref" doc:"notification: a notice id. session: a session id on this hab. file: an attachment id from attachment.put for the Capcom session, or an absolute path under its folder. command: <pane id>:<command id>, from command.list. logbook: a record id."`
	Host string            `json:"host,omitempty" doc:"notification only: a peer label or device id, or this hab's host id or device id, as notice.get takes it; a push's capcom_context gives a device id. Absent: this hab. The other kinds name this hab's own things."`
}

// CapcomContextItem is one item attached to a Capcom session's next message, for the client's
// chip.
type CapcomContextItem struct {
	Kind  CapcomContextKind `json:"kind"`
	Ref   string            `json:"ref" doc:"As attached."`
	Host  string            `json:"host" doc:"The label of the host that resolved it."`
	Title string            `json:"title" doc:"A short label: the notice title, the session name, the file name or the command line. At most 120 characters."`
}

type CapcomAttachParams struct {
	Session string            `json:"session" doc:"A Capcom session on this hab."`
	Kind    CapcomContextKind `json:"kind"`
	Ref     string            `json:"ref" doc:"As in CapcomContextRef."`
	Host    string            `json:"host,omitempty" doc:"As in CapcomContextRef."`
}

// CapcomPendingParams names a Capcom session whose waiting context to read.
type CapcomPendingParams struct {
	Session string `json:"session" doc:"A Capcom session on this hab."`
}

// CapcomDetachParams names one pending item to remove, as it was attached.
type CapcomDetachParams struct {
	Session string            `json:"session" doc:"A Capcom session on this hab."`
	Kind    CapcomContextKind `json:"kind"`
	Ref     string            `json:"ref" doc:"As attached."`
	Host    string            `json:"host,omitempty" doc:"As attached, or the item's host label. Absent: this hab."`
}

// CapcomAttached answers capcom.attach with everything now pending for the session's next
// message, in delivery order.
type CapcomAttached struct {
	Seq
	Session string              `json:"session"`
	Pending []CapcomContextItem `json:"pending"`
}

// CapcomOpened answers capcom.open with the standing Capcom session.
type CapcomOpened struct {
	Seq
	Session string `json:"session"`
	Pane    string `json:"pane"`
	Started bool   `json:"started,omitempty" doc:"This call started the session, or resumed it after an exit."`
	Dir     string `json:"dir" doc:"The session's folder on the home host."`
	// Pending is field set 2: the context waiting for the session's next message (capcom.md 8.1).
	Pending []CapcomContextItem `json:"pending,omitempty" doc:"Context attached to the session's next message, in delivery order. Absent when none waits."`
	// Proposals is field set 3: the review list a new conversation opens (capcom-service.md 6.4).
	Proposals int `json:"proposals,omitempty" doc:"The Capcom service's Logbook proposals that wait for review. With new set, the app opens the review list when it is above 0."`
}

type CapcomAssignParams struct {
	Host string `json:"host" doc:"self, a peer label from this hab's peer list, or a device id."`
	// Assignment is set by a hab that forwards an assignment to the chosen peer.
	Assignment uint64 `json:"assignment,omitempty" doc:"The lowest counter to use. A hab that forwards an assignment to the chosen peer sends one above every counter it has seen."`
}

// CapcomAssigned answers capcom.assign with the assignment now in force.
type CapcomAssigned struct {
	Seq
	Home       string `json:"home" doc:"The home host's device id."`
	Assignment uint64 `json:"assignment"`
}
