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
}

// CatalogCapcom is one hab's view of the Capcom assignment, and its own Capcom sessions. Every
// hab publishes the highest assignment it has seen; the home host's own catalog carries the
// mark (home is its own device id). Across catalogs the highest assignment wins, and on a tie
// the lower home device id.
type CatalogCapcom struct {
	Assignment uint64                 `json:"assignment" doc:"The highest assignment counter this hab has seen; 0 for none."`
	Home       string                 `json:"home,omitempty" doc:"The device id of the home host at that assignment."`
	Sessions   []CatalogCapcomSession `json:"sessions" doc:"The Capcom sessions on this host, the standing one marked."`
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
	New bool `json:"new,omitempty" doc:"Start a fresh Capcom session in a new folder; the standing one stays resumable."`
}

// CapcomOpened answers capcom.open with the standing Capcom session.
type CapcomOpened struct {
	Seq
	Session string `json:"session"`
	Pane    string `json:"pane"`
	Started bool   `json:"started,omitempty" doc:"This call started the session, or resumed it after an exit."`
	Dir     string `json:"dir" doc:"The session's folder on the home host."`
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
