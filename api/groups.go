package api

// --- Agent groups: the group record, its plan and its confirmation (groups.md 3, 5, 15.1) ---

// GroupState is where a group is (groups.md 5.2). The server sends it, so the set is open: a
// client shows an unknown word as given.
type GroupState string

// GroupItemState is where one plan item is (groups.md 5.3). Open, as GroupState.
type GroupItemState string

// GroupMemberState is where one member's start is: starting, running, exited, failed. Open.
type GroupMemberState string

// GroupRole is a member's role (groups.md 2.1). A closed set: the plan sends it.
type GroupRole string

// GroupPlanItem is one separable piece of the task (groups.md 3.1).
type GroupPlanItem struct {
	ID      string   `json:"id" doc:"1 to 64 lowercase letters, digits and '-', unique in the plan."`
	Title   string   `json:"title"`
	Brief   string   `json:"brief,omitempty" doc:"The item in full: its lead's seed carries it after the group's brief."`
	Touches []string `json:"touches,omitempty" doc:"Paths or item ids it overlaps."`
	After   string   `json:"after,omitempty" doc:"An item whose branch this one stacks on (groups.md 7.2). It starts once that item reaches review."`
	Review  string   `json:"review,omitempty" doc:"on (the default) or off (groups.md 6.1)."`
}

// GroupPlanMember is one member the plan starts or may start (groups.md 3.1).
type GroupPlanMember struct {
	Role    GroupRole `json:"role" doc:"lead, worker or reviewer."`
	Item    string    `json:"item" doc:"The item it works on."`
	Host    string    `json:"host,omitempty" doc:"A peer label from the home hab's peer list, or its host id. Absent: the home hab."`
	Harness string    `json:"harness" doc:"claude, codex or opencode, or an alias from the home hab's [aliases] table."`
	Model   string    `json:"model,omitempty"`
	Name    string    `json:"name,omitempty" doc:"Its session name, free on every host. Absent: harness, six hex digits of the group id, role and item position, such as codex-3fa9c2-review-2."`
}

// GroupLane is where the group's items land (groups.md 3.1, 7).
type GroupLane struct {
	Target     string `json:"target,omitempty" doc:"The target branch. Absent: the project's default branch."`
	FixInPlace bool   `json:"fix_in_place,omitempty" doc:"A reviewer may fix in place on its review branch (groups.md 6.4)."`
}

// GroupPlan is what the coordinator proposes and the confirmation covers (groups.md 3.1).
type GroupPlan struct {
	Title   string            `json:"title" doc:"The task, one line."`
	Brief   string            `json:"brief,omitempty" doc:"The task in full: every member's seed carries it."`
	Project string            `json:"project" doc:"The project: its name or remote in the catalog. A group works on one repository."`
	Items   []GroupPlanItem   `json:"items"`
	Members []GroupPlanMember `json:"members"`
	Lane    GroupLane         `json:"lane,omitempty"`
}

// GroupEnvelope is what a confirmed plan allows (groups.md 3.3). The home hab copies it into
// every member's session.create, so the member's hab decides a later start alone.
type GroupEnvelope struct {
	Group        string   `json:"group"`
	Home         string   `json:"home" doc:"The home hab's device id."`
	Confirmation string   `json:"confirmation" doc:"The plan's confirmation id, or none when it started with no confirmation."`
	Project      string   `json:"project" doc:"The project's identity: its normalized remote, else root:<commit>."`
	Hosts        []string `json:"hosts" doc:"The device ids of the plan's hosts."`
	Harnesses    []string `json:"harnesses"`
	MaxWorkers   int      `json:"max_workers" doc:"Workers per lead (group.max_workers when the plan was confirmed)."`
}

// SessionGroup marks a group member's session (groups.md 2.1).
type SessionGroup struct {
	ID   string    `json:"id"`
	Role GroupRole `json:"role"`
	Item string    `json:"item,omitempty"`
}

// GroupCoordinator is the session that made the plan and reads the results.
type GroupCoordinator struct {
	Session string `json:"session" doc:"Its session id on the home hab."`
	Name    string `json:"name"`
	Address string `json:"address" doc:"Its channel address: label/name as another hab of the mesh sees it, by the home hab's host id."`
}

// GroupConfirmationRef is the plan's confirmation and its outcome.
type GroupConfirmationRef struct {
	ID      string       `json:"id" doc:"The confirmation id, or none when confirm.group_plan was off."`
	Outcome ConfirmState `json:"outcome,omitempty"`
}

// GroupItem is one item in the record (groups.md 5.1, 5.3).
type GroupItem struct {
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Task   string         `json:"task,omitempty" doc:"Its Logbook task id."`
	Lead   string         `json:"lead,omitempty" doc:"Its lead's session name."`
	Branch string         `json:"branch,omitempty" doc:"group/<group>/<item>."`
	After  string         `json:"after,omitempty"`
	Review string         `json:"review,omitempty"`
	State  GroupItemState `json:"state" doc:"queued, stalled, working, review, fix, ready, landing, landed or dropped."`
	Reason string         `json:"reason,omitempty" doc:"Why it is stalled or waits."`
}

// GroupMember is one member in the record (groups.md 5.1).
type GroupMember struct {
	Session    string           `json:"session,omitempty" doc:"Its session id on its host, once started."`
	Name       string           `json:"name"`
	Host       string           `json:"host" doc:"Its host, as the home hab labels it."`
	Device     string           `json:"device,omitempty" doc:"Its hab's device id."`
	Harness    string           `json:"harness"`
	Model      string           `json:"model,omitempty"`
	Role       GroupRole        `json:"role"`
	Item       string           `json:"item"`
	Folder     string           `json:"folder,omitempty" doc:"Its worktree on its host."`
	State      GroupMemberState `json:"state" doc:"planned, starting, running, exited or failed."`
	Reason     string           `json:"reason,omitempty" doc:"Why it failed."`
	Agent      AgentStatus      `json:"agent,omitempty" doc:"Its agent state, from its hab or the catalog copy."`
	AgentAge   string           `json:"agent_age,omitempty" doc:"RFC 3339 time the home hab last confirmed that state with the member's hab."`
	ContextPct *int             `json:"context_pct,omitempty"`
	Compacts   int              `json:"compacts,omitempty"`
	Replaced   string           `json:"replaced,omitempty" doc:"The session it replaced (groups.md 9.3)."`
	Attempts   int              `json:"attempts,omitempty" doc:"Starts tried again by group.retry; each has its own idempotency key."`
}

// GroupReport is one report that waits for a coordinator that is not running (groups.md 5.5).
type GroupReport struct {
	At   string `json:"at" doc:"RFC 3339 time the home hab took it."`
	From string `json:"from,omitempty" doc:"Who sent it: a member's name, or Flightdeck."`
	Text string `json:"text"`
}

// GroupPause holds the two pause reasons apart (groups.md 5.6).
type GroupPause struct {
	User  string `json:"user,omitempty" doc:"RFC 3339 time the user paused the group; absent when not."`
	Usage string `json:"usage,omitempty" doc:"RFC 3339 time usage pacing paused it; absent when not."`
	Until string `json:"until,omitempty" doc:"With usage: when the governor expects room."`
}

// GroupLaneState is the lane's queue (groups.md 7.3). Empty until the lane is built.
type GroupLaneState struct {
	Queue   []string `json:"queue"`
	Landing string   `json:"landing,omitempty"`
}

// Group is the group record (groups.md 5.1), kept by the home hab.
type Group struct {
	ID           string               `json:"id" doc:"64 random bits as 16 hex digits."`
	Revision     uint64               `json:"revision"`
	Title        string               `json:"title"`
	Project      string               `json:"project" doc:"The project's name in the home hab's catalog."`
	PlanRecord   string               `json:"plan_record,omitempty" doc:"The Logbook plan record id."`
	Home         string               `json:"home" doc:"The home hab's device id."`
	Generation   uint64               `json:"generation" doc:"1 at creation; one more at each move of the home (groups.md 5.7)."`
	Coordinator  GroupCoordinator     `json:"coordinator"`
	Inbox        []GroupReport        `json:"inbox"`
	State        GroupState           `json:"state" doc:"confirming, running, paused, ending, ended or dropped."`
	Confirmation GroupConfirmationRef `json:"confirmation"`
	Items        []GroupItem          `json:"items"`
	Members      []GroupMember        `json:"members"`
	Lane         GroupLaneState       `json:"lane"`
	Pause        GroupPause           `json:"pause"`
	Waits        []string             `json:"waits" doc:"What holds the group now, one line each."`
	Leftovers    []string             `json:"leftovers"`
	Plan         GroupPlan            `json:"plan" doc:"The plan as confirmed, with each amend merged in."`
	Envelope     *GroupEnvelope       `json:"envelope,omitempty" doc:"What the confirmed plan allows (groups.md 3.3). Absent while confirming."`
	Created      string               `json:"created"`
	Updated      string               `json:"updated"`
	Ended        string               `json:"ended,omitempty"`
}

// ConfirmGroup is the plan a kind group confirmation would start, or the change an amend adds.
type ConfirmGroup struct {
	Group    string    `json:"group" doc:"The group id."`
	Amend    bool      `json:"amend,omitempty" doc:"The plan is a change to a running group (group.amend): its new items and members."`
	Plan     GroupPlan `json:"plan"`
	Trust    string    `json:"trust" doc:"The trust rule the confirmation answers, in one line: which new worktrees, on which hosts, for which harnesses (groups.md 4.2)."`
	Warnings []string  `json:"warnings,omitempty" doc:"Lines the card shows above the buttons, such as one item: a single session costs less (groups.md 11)."`
}

type GroupCreateParams struct {
	Plan        GroupPlan `json:"plan"`
	Coordinator string    `json:"coordinator,omitempty" doc:"The coordinator's session name on this hab. From a pane the pane's session, whatever this says."`
	WaitMS      int       `json:"wait_ms,omitempty" doc:"Wait up to this long for the confirmation's answer; 0 answers at once. At most 120000."`
}

// GroupCreated answers group.create and group.amend.
type GroupCreated struct {
	Seq
	Group        Group         `json:"group"`
	Confirmation *Confirmation `json:"confirmation,omitempty" doc:"The plan's confirmation, open or closed. Absent when none was asked."`
}

type GroupAmendParams struct {
	Group  string    `json:"group"`
	Change GroupPlan `json:"change" doc:"The items and members to add. Title, brief and project are not read."`
	WaitMS int       `json:"wait_ms,omitempty"`
}

type GroupRef struct {
	Group string `json:"group"`
}

type GroupListParams struct {
	State GroupState `json:"state,omitempty" doc:"Only groups in this state."`
}

// GroupResult answers group.get, group.pause, group.resume, group.close and group.retry.
type GroupResult struct {
	Seq
	Group Group `json:"group"`
}

// GroupList answers group.list: the short forms, newest first.
type GroupList struct {
	Seq
	Groups []CatalogGroup `json:"groups"`
}

type GroupReportParams struct {
	Group string `json:"group"`
	Text  string `json:"text" doc:"One line or a short text, at most 4000 characters."`
	From  string `json:"from,omitempty" doc:"Who sends it: a member's session name. From a pane the pane's session."`
}

// GroupReported answers group.report.
type GroupReported struct {
	Seq
	State string `json:"state" doc:"sent: a channel message to the running coordinator; waiting: it waits in the record's inbox."`
}

type GroupNoteParams struct {
	Group string `json:"group"`
	Item  string `json:"item"`
	Text  string `json:"text" doc:"At most 4000 characters."`
	From  string `json:"from,omitempty" doc:"From a pane the pane's session."`
}

type GroupRetryParams struct {
	Group  string `json:"group"`
	Member string `json:"member" doc:"A failed member's name."`
}
