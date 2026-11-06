package api

import (
	_ "embed"
	"encoding/json"
)

//go:generate go run ../internal/apigen/cmd -root ..

// RawJSON is a JSON value whose shape the receiver picks, such as an event's data.
type RawJSON = json.RawMessage

// SchemaJSON is the generated schema. gff-hab serves it from rpc.discover.
//
//go:embed schema.json
var SchemaJSON []byte

// Method describes one JSON-RPC method. Params and Result are zero values of the Go types.
type Method struct {
	Name    string
	Summary string
	Params  any
	Result  any
}

// Notification describes a JSON-RPC notification the server sends.
type Notification struct {
	Name    string
	Summary string
	Params  any
}

// EventDef describes one event type: the shape of Event.Data.
type EventDef struct {
	Type    EventType
	Summary string
	Data    any
}

// FrameKind is the first byte of a data connection frame (spec 5.3).
type FrameKind byte

const (
	FrameHello    FrameKind = 1
	FrameWelcome  FrameKind = 2
	FrameReject   FrameKind = 3
	FrameSnapshot FrameKind = 4
	FrameBytes    FrameKind = 5
	FrameResize   FrameKind = 6
	FrameExited   FrameKind = 7
	FrameCredit   FrameKind = 8
)

// FrameDef describes one data frame kind. Payload is nil for a binary payload; Layout says how
// the binary payload is laid out.
type FrameDef struct {
	Name      string
	Kind      FrameKind
	Direction string
	Payload   any
	Layout    string
}

// Methods is every method in this revision, in a stable order.
var Methods = []Method{
	{"hab.hello", "First request on a control connection: agree a revision (spec 3.2).", HelloParams{}, Welcome{}},
	{"hab.ping", "Client liveness; send every 15 s when idle (spec 3.5).", PingParams{}, Empty{}},
	{"rpc.discover", "The schema of this API. Its result is the schema document and carries no seq.", DiscoverParams{}, RawJSON{}},
	{"structure.get", "Read the whole structure of the host.", StructureGetParams{}, StructureResult{}},
	{"events.subscribe", "Snapshot the objects in scope and start events (spec 3.5).", SubscribeParams{}, StructureResult{}},
	{"pane.input", "Write bytes to a pane's program as one write (spec 3.8).", InputParams{}, Empty{}},
	{"data.ticket", "A single-use ticket, valid 30 s, to open a data connection (spec 5.2).", TicketParams{}, Ticket{}},
	{"pane.take", "Become the pane's size owner now (spec 6.3).", PaneRef{}, OwnerResult{}},
	{"pane.request", "Become the size owner only if the pane has none (spec 6.3).", PaneRef{}, OwnerResult{}},
	{"pane.disown", "Give up size ownership; the pane keeps its size (spec 6.3).", PaneRef{}, OwnerResult{}},
	{"pane.set_desired_size", "Store this client's wanted size (spec 6.3).", DesiredSizeParams{}, DesiredSizeResult{}},
	{"pane.redraw", "Send a fresh snapshot, or nudge the program to redraw (spec 6.7).", RedrawParams{}, Empty{}},
	{"devices.pair", "Make a one-time pairing offer for a new device (transport.md 6.1). Needs the devices capability.", DevicesPairParams{}, PairingOffer{}},
	{"devices.list", "List the devices in the hab's trust store (transport.md 5). Needs the devices capability.", DevicesListParams{}, DeviceList{}},
	{"devices.revoke", "Revoke a device and close its connections (transport.md 6.6). Needs the devices capability.", DevicesRevokeParams{}, Empty{}},
	{"session.create", "Start a program in a new pane and file it as a session (spec 3.9).", SessionCreateParams{}, SessionCreated{}},
	{"session.resume", "Start an exited session's harness again in a new pane, with its resume id.", SessionRef{}, SessionCreated{}},
	{"pane.close", "Release a pane: a running program gets a hang-up first (spec 3.9).", PaneRef{}, Empty{}},
	{"session.rename", "Give a session a new name. A name another session holds is refused with conflict.", SessionRenameParams{}, Empty{}},
	{"session.close", "Remove a session record. A live pane keeps running, unfiled; pane.close ends a program.", SessionRef{}, Empty{}},
	{"session.focus", "Report whether the session is in focus on this client: marks done seen, and holds pushes while watched.", SessionFocusParams{}, Empty{}},
	{"session.interrupt", "Stop the agent's current turn through its adapter.", SessionRef{}, Empty{}},
	{"session.send", "Send a message to the session's agent through its adapter, under the input rules.", SessionSendParams{}, Empty{}},
	{"approval.answer", "Answer a pending request by id. A closed or expired id is refused with conflict.", ApprovalAnswerParams{}, Empty{}},
	{"trust.answer", "Answer the pane's folder trust prompt from the dialog on screen; refused with conflict when it is gone. Needs the trust capability.", TrustAnswerParams{}, TrustAnswered{}},
	{"chat.page", "Read a page of the session's chat messages from the transcript reader. Needs the transcripts capability.", ChatPageParams{}, ChatMessages{}},
	{"attachment.put", "Start an upload of a file for a session; the bytes follow in attachment.write runs (app.md 4.4). Needs the attachments capability.", AttachmentPutParams{}, AttachmentStored{}},
	{"attachment.write", "Send the next run of an upload's bytes; the last run checks the hash and puts the file in place (app.md 4.4). Needs the attachments capability.", AttachmentWriteParams{}, AttachmentWritten{}},
	{"attachment.get", "Fetch a run of an attachment's bytes, or its thumbnail (app.md 4.4). Needs the attachments capability.", AttachmentGetParams{}, AttachmentData{}},
	{"content.show", "Store a reference to a file on the hab's host for a session to show (app.md 4.7). Needs the content capability.", ContentShowParams{}, ContentShown{}},
	{"content.list", "The files a session showed or touched: shown files, images from tool calls and files it changed (app.md 4.7). Needs the content capability.", ContentListParams{}, ContentList{}},
	{"content.read", "Read a run of one listed file's bytes, in bounded chunks (app.md 4.7). Needs the content capability.", ContentReadParams{}, ContentData{}},
	{"content.diff", "A listed file's diff in unified format (app.md 4.7). Needs the content capability.", ContentDiffParams{}, ContentDiff{}},
	{"push.register", "Store an APNs device token for this device. Needs the push capability.", PushRegisterParams{}, Empty{}},
	{"push.register_activity", "Store a Live Activity token for the session this device tracks. Needs the push capability.", PushActivityParams{}, Empty{}},
	{"push.unregister", "Drop a push token. Needs the push capability.", PushUnregisterParams{}, Empty{}},
	{"notice.raise", "Raise a Flightdeck notification: a notice.raised event to subscribers, and a push where push is set up.", NoticeRaiseParams{}, NoticeRaised{}},
	{"usage.list", "The latest usage reading of each harness account on this hab.", UsageListParams{}, UsageReadings{}},
	{"hab.stats", "CPU, memory, disk, battery and uptime of the host. Needs the host-stats capability.", HabStatsParams{}, HostStats{}},
	{"command.list", "Read a page of a pane's shell commands, found by OSC 133 marks (spec 2.5). Needs the commands capability.", CommandListParams{}, CommandList{}},
	{"command.output", "Read a run of one command's output from the holder's ring (spec 2.5). Needs the commands capability.", CommandOutputParams{}, CommandOutput{}},
	{"peers.list", "This hab's peers with their link state, and habs linked to it that it does not list (mesh.md 3, 4.3). Needs the mesh capability.", PeersListParams{}, PeerList{}},
	{"peers.accept", "Pin the device id a peer presented in place of the pinned one (transport.md 6.3). Needs the mesh capability.", PeersAcceptParams{}, PeerAccepted{}},
	{"catalog.get", "This hab's own catalog, whole, when its generation differs from since (mesh.md 5.2). Needs the catalog capability.", CatalogGetParams{}, CatalogGetResult{}},
	{"catalog.put", "A peer link hands over its own hab's catalog (mesh.md 4.2). Needs the catalog capability.", CatalogPutParams{}, Empty{}},
	{"catalog.list", "The merged view: this hab's catalog and its direct peers' copies, with projects matched across hosts (mesh.md 5.2). Needs the catalog capability.", CatalogListParams{}, CatalogList{}},
	{"hab.permissions", "Check the permission grants of the holders, from a probe pane, and of gff-hab (macos-identity.md 7.2). Needs the permissions capability.", PermissionsParams{}, Permissions{}},
	{"speech.prepare", "Load the speech engine now, and start the model download if needed; returns the engine state at once (voice.md). Needs the speech capability.", SpeechPrepareParams{}, SpeechStatus{}},
	{"speech.transcribe", "Turn one recorded clip into text (voice.md). One decode at a time; others wait in order. Needs the speech capability.", SpeechTranscribeParams{}, SpeechTranscript{}},
	{"detect.explain", "Say why a pane shows its agent state: the source that won, the process match and each screen rule's region and result (harness-adapters.md, Detection). Needs the detect-explain capability.", DetectExplainParams{}, DetectExplain{}},
	{"channel.send", "Send a message from a session on this hab to an agent by address (mesh.md 6.5). Needs the channels capability.", ChannelSendParams{}, ChannelSent{}},
	{"channel.status", "A sent message's record, optionally after waiting for its final state (mesh.md 6.5). Needs the channels capability.", ChannelStatusParams{}, ChannelStatusResult{}},
	{"channel.wait", "Wait for a message to a session, or for an agent's state, or the end of the time (mesh.md 6.4). Needs the channels capability.", ChannelWaitParams{}, ChannelWaitResult{}},
	{"mail.push", "A peer link hands over outbox items for this hab; one result per item (mesh.md 6.5). Needs the channels capability.", MailPushParams{}, MailPushResult{}},
	{"mail.pull", "A peer link takes the outbox items waiting for its hab, and reports on its last pull (mesh.md 6.5). Needs the channels capability.", MailPullParams{}, MailPullResult{}},
	{"logbook.pull", "A peer link takes one page of the Logbook ops its hab lacks by its version vector (logbook.md 7.1). Peer links only. Needs the logbook capability.", LogbookPullParams{}, LogbookPullResult{}},
	{"logbook.push", "A peer link hands over one page of Logbook ops this hab lacks; the answer is this hab's version vector (logbook.md 7.1). Peer links only. Needs the logbook capability.", LogbookPushParams{}, LogbookPushResult{}},
	{"worktree.list", "List a repository's working trees, the main one first (spec 3.10).", WorktreeListParams{}, WorktreeList{}},
	{"worktree.create", "Make a worktree, copy the files .worktreeinclude names, and start its setup script in a session (spec 3.10).", WorktreeCreateParams{}, WorktreeCreated{}},
	{"worktree.setup", "Start a worktree's setup script in a session now: the yes to a pending setup, or a rerun (spec 3.10).", WorktreeSetupParams{}, WorktreeSetupResult{}},
	{"settings.get", "Read every hab-side setting, or the named ones (app.md 20.5). Needs the settings capability.", SettingsGetParams{}, SettingsResult{}},
	{"settings.set", "Write hab-side settings, all or nothing, each with the revision last seen (app.md 20.5). Needs the settings capability.", SettingsSetParams{}, SettingsResult{}},
	{"capcom.open", "Return the standing Capcom session on this home host, starting or resuming it as needed; new starts a fresh one (capcom.md 7.2). Needs the capcom capability.", CapcomOpenParams{}, CapcomOpened{}},
	{"capcom.attach", "Attach context to a Capcom session's next message: a notification, a session, a file, a console command or a Logbook record. The hab resolves the ref at once and delivers it as a block before the user's text at the next session.send, then drops it; at most 8 items wait, and the same item twice is kept once (capcom.md 8.1). Needs the capcom capability.", CapcomAttachParams{}, CapcomAttached{}},
	{"capcom.detach", "Remove one item from the context waiting for a Capcom session's next message, and return what still waits; an item that does not wait answers not_found (capcom.md 8.4). Needs the capcom capability.", CapcomDetachParams{}, CapcomAttached{}},
	{"capcom.pending", "Read the context waiting for a Capcom session's next message, in delivery order. Read-only: it starts, resumes and changes nothing (capcom.md 8.4). Needs the capcom capability.", CapcomPendingParams{}, CapcomAttached{}},
	{"capcom.assign", "Make a host the Capcom home host, with a counter above every one seen (capcom.md 7.3). Needs the capcom capability.", CapcomAssignParams{}, CapcomAssigned{}},
	{"worktree.remove", "Run the teardown script, then remove a linked worktree; its branch stays. Uncommitted changes need force (spec 3.10).", WorktreeRemoveParams{}, WorktreeRemoved{}},
	{"transcript.list", "Past and current harness sessions from their native transcripts, newest first, on every host or one (capcom.md 4). Needs the read-tools capability.", TranscriptListParams{}, TranscriptSessions{}},
	{"transcript.search", "Search past transcripts by words, newest first, at most 50 hits and 5 s per host; partial and offline hosts marked (capcom.md 4). Needs the read-tools capability.", TranscriptSearchParams{}, TranscriptHits{}},
	{"transcript.read", "Read a page of a past or current session's messages, like chat.page (capcom.md 4). Needs the read-tools capability.", TranscriptReadParams{}, TranscriptPage{}},
	{"notice.get", "Read a notice this hab raised, by id, with the session it is about (capcom.md 4). Needs the read-tools capability.", NoticeGetParams{}, NoticeRecord{}},
	{"file.read", "Read a run of a text file inside the host's read scope (capcom.md 5). Needs the read-tools capability.", FileReadParams{}, FileData{}},
	{"logbook.find", "Search Logbook records by words, best match first; with no words, list them as logbook.list (logbook.md 10.3). Deleted records are left out unless asked for. Needs the logbook capability.", LogbookFindParams{}, LogbookRecords{}},
	{"logbook.get", "Read one Logbook record with its links both ways, notes and flags (logbook.md 10.3). Needs the logbook capability.", LogbookGetParams{}, LogbookRecordResult{}},
	{"logbook.list", "List Logbook records, newest change first, with filters: type, status, project, tag, host, author, session, since (logbook.md 10.3). Needs the logbook capability.", LogbookListParams{}, LogbookRecords{}},
	{"logbook.brief", "The session brief for a project: tasks in doing and blocked, handed over and ready ones, pinned guides, recent findings, open plans, records tagged everywhere, the untriaged and flagged counts, at most 8 KB (logbook.md 11.2, 11.4). On the pane route the pane's project is the default, and a private project outside the pane's answers not_found. Needs the logbook capability.", LogbookBriefParams{}, LogbookBrief{}},
	{"logbook.ready", "Open tasks and issues with no open blocker, newest change first (logbook.md 10.2). Needs the logbook capability.", LogbookReadyParams{}, LogbookRecords{}},
	{"logbook.history", "Every op on one record, losers and ops this build does not fold included, in stamp order (logbook.md 10.2). Needs the logbook capability.", LogbookHistoryParams{}, LogbookHistory{}},
	{"logbook.write", "Apply one batch of changes, all or nothing, or with propose store it as a pending proposal (logbook.md 10.3, 9.2). On the pane route the hab records the pane's harness, session and host, and refuses a pane's writes past 60 a minute with invalid. Needs the logbook capability.", LogbookWriteParams{}, LogbookWritten{}},
	{"logbook.resolve", "Settle a conflict flag: set the field to the given value, or to the value of a named op, as a write that saw every side (logbook.md 5.2). Needs the logbook capability.", LogbookResolveParams{}, LogbookWritten{}},
	{"logbook.decide", "Decide a pending proposal (logbook.md 9.2). approve applies its draft, or the edits given, as the proposing agent's write with approved_by set to this device, in one batch with the decision; each record it makes links to the proposal with from. reject records the decision only. The first decision stands; one made elsewhere later is flagged decided_twice (5.6). Refused on the pane route: only a person decides. Needs the logbook capability.", LogbookDecideParams{}, LogbookWritten{}},
	{"logbook.projects", "The Logbook projects with their identities (logbook.md 3). Needs the logbook capability.", LogbookProjectsParams{}, LogbookProjects{}},
	{"logbook.status", "This replica's size and health: op and record counts, the version vector, waiting and unfolded ops, origins ahead or shared (logbook.md 10.2). Needs the logbook capability.", LogbookStatusParams{}, LogbookStatus{}},
	{"confirm.request", "Ask for a start or set-up: with confirm.start_elsewhere on, open a confirmation for the user and wait up to wait_ms for the answer; off, run it at once and return the record (capcom.md 6.1). Needs the confirm capability.", ConfirmRequestParams{}, ConfirmRequested{}},
	{"confirm.answer", "Answer an open confirmation: confirm, with the user's edits, or cancel. The first answer wins; a later one answers conflict. A confirm returns once the hab ran the action (capcom.md 6.1). Needs the confirm capability.", ConfirmAnswerParams{}, ConfirmationResult{}},
	{"confirm.list", "The open confirmations, and with all the closed ones the hab keeps (capcom.md 6.1). Needs the confirm capability.", ConfirmListParams{}, ConfirmList{}},
	{"confirm.get", "One confirmation by id, open or closed (capcom.md 6.1). Needs the confirm capability.", ConfirmGetParams{}, ConfirmationResult{}},
	{"round.list", "This hab's rounds with their next slot and newest run (rounds.md 13). Needs the rounds capability.", RoundListParams{}, RoundList{}},
	{"round.get", "One round by id or name, with its definition (rounds.md 13). Needs the rounds capability.", RoundRef{}, RoundResult{}},
	{"round.create", "Create a round; checks the trigger, mode and target. run_now also starts one run, and the call then answers after its spawn (rounds.md 3, 13). Needs the rounds capability.", RoundCreateParams{}, RoundCreated{}},
	{"round.update", "Replace a round's definition; a changed trigger sets the next slot from now. A run in flight keeps its values (rounds.md 3). Needs the rounds capability.", RoundUpdateParams{}, RoundResult{}},
	{"round.delete", "Delete a round and its ended runs; a live run goes on to its end (rounds.md 13). Needs the rounds capability.", RoundRef{}, Empty{}},
	{"round.pause", "Pause a round: no new runs; a queued run ends skipped (rounds.md 13). Needs the rounds capability.", RoundRef{}, RoundResult{}},
	{"round.unpause", "Unpause a round and clear its failure count (rounds.md 10.1). Needs the rounds capability.", RoundRef{}, RoundResult{}},
	{"round.run", "Start a run now, outside the trigger. It answers after the run's session is spawned, or once the run is queued behind the host limit or the wake settle. A round with a live run answers conflict (rounds.md 13). Needs the rounds capability.", RoundRef{}, RoundRunResult{}},
	{"round.runs", "A page of a round's runs, newest first (rounds.md 7.7). Needs the rounds capability.", RoundRunsParams{}, RoundRuns{}},
	{"heavy.take", "Ask for the host's heavy-run lock, or take over a ticket: first come, first served, heavy.slots at once (groups.md 8.1). The calling connection holds the ticket: it is released when the connection drops, or when the pane named by pane_token closes. Answers at once, or with wait_ms once the lock is held or the wait ends. Needs the heavy capability.", HeavyTakeParams{}, HeavyTaken{}},
	{"heavy.release", "Give back a heavy-run ticket, holding or waiting. An unknown ticket is not an error (groups.md 8.1). Needs the heavy capability.", HeavyReleaseParams{}, Empty{}},
	{"heavy.status", "Who holds the heavy-run lock, who waits, and the disk watch's state (groups.md 8). Needs the heavy capability.", HeavyStatusParams{}, HeavyStatus{}},
}

// Notifications is every notification the server sends on a control connection.
var Notifications = []Notification{
	{"event", "One event; see the event types (spec 3.5).", Event{}},
	{"heartbeat", "Sent every 15 s when no other event was sent.", Heartbeat{}},
	{"closing", "Sent once before the server closes the connection.", Closing{}},
}

// Events is every event type, the value of Event.Type.
var Events = []EventDef{
	{"project.updated", "A project was created or changed.", Project{}},
	{"project.deleted", "A project was removed.", Deleted{}},
	{"session.updated", "A session was created or changed, including its exit.", Session{}},
	{"session.deleted", "A session record was removed.", Deleted{}},
	{"pane.updated", "A pane appeared or changed.", Pane{}},
	{"pane.deleted", "A pane is gone.", Deleted{}},
	{"pane.owner", "The pane's size owner changed (spec 6.5).", PaneOwner{}},
	{"pane.activity", "A client sent input; at most once per second per client (spec 7.1).", PaneActivity{}},
	{"agent.state", "A pane's agent state changed (harness-adapters.md, State event model). The id is the pane id.", AgentState{}},
	{"pane.bell", "A pane rang the bell; at most once per second per pane. Ephemeral.", PaneBell{}},
	{"pane.output", "A pane printed; at most once per second per pane, for activity marks. Ephemeral.", PaneOutput{}},
	{"chat.message", "A chat message was appended or changed; the id is the message id. Ephemeral.", ChatMessage{}},
	{"session.turn_end", "The agent's turn ended; the id is the session id. Ephemeral.", TurnEnd{}},
	{"agent.compact", "A self-compact moved a step; the id is the compaction id (harness-adapters.md, Self-compact). Goes where its session goes. Ephemeral.", Compaction{}},
	{"usage.updated", "An account's usage reading changed; the id is the account key. Ephemeral.", UsageAccount{}},
	{"command.updated", "A pane's command started or ended; the id is the pane id (spec 2.5). Ephemeral.", Command{}},
	{"peer.updated", "A peer's link state changed; the id is the peer's label (mesh.md 4.3). Host scope only. Ephemeral.", Peer{}},
	{"catalog.updated", "A catalog this hab holds changed: its own or a peer's copy; the id is the device id (mesh.md 5.2). Host scope only. Ephemeral.", CatalogUpdated{}},
	{"notice.raised", "A notice was raised (notice.raise); the id is the notice id. Ephemeral.", Notice{}},
	{"content.updated", "A session showed a file (content.show); the id is the item id. Ephemeral.", ContentItem{}},
	{"settings.updated", "A hab-side setting changed: by a client or by a config read; the id is the setting id (app.md 20.5). Host scope only. Not ephemeral: the settings are in the host snapshot.", SettingUpdated{}},
	{"channel.message", "A message this hab sent changed state; the id is the message id (mesh.md 6.5). Host scope only. Ephemeral.", ChannelRecord{}},
	{"confirm.requested", "A confirmation opened, a start ran at once as a record, or a confirmed one moved to running; the id is the confirmation id (capcom.md 6.1). Host scope only. Ephemeral: the open ones are in the host snapshot.", Confirmation{}},
	{"confirm.closed", "A confirmation closed: done, failed, cancelled, expired or unknown; every card withdraws its buttons; the id is the confirmation id (capcom.md 6.1). Host scope only. Ephemeral.", Confirmation{}},
	{"round.updated", "A round was created, changed, paused, completed or deleted (data.deleted); the id is the round id (rounds.md 13). Host scope only. Not ephemeral: the rounds are in the host snapshot.", Round{}},
	{"round.run_updated", "A run of a round changed state; the id is the run id (rounds.md 13). Host scope only. Ephemeral.", RoundRun{}},
	{"logbook.updated", "A Logbook record changed, by any writer of the file: the API, the pane route, or the CLI while it wrote the file itself; the id is the record id and the revision its count of folded ops (logbook.md 10.3). Host scope only. Not in the snapshot: a client reads the records it shows.", LogbookUpdated{}},
	{"logbook.conflict", "A Logbook record gained or lost a flag; the id is the record id (logbook.md 10.3). Host scope only.", LogbookConflict{}},
	{"mesh.changed", "Sent only to a peer link that dialed this hab, subscribed or not: something waits for the dialer, so it pulls now; the id is this hab's device id (mesh.md 4.2, 6.5). Ephemeral.", MeshChanged{}},
}

// Frames is every data connection frame kind. A frame is the kind byte, a 4-byte big-endian
// payload length, then the payload. On a WebSocket one binary message is one frame.
var Frames = []FrameDef{
	{"hello", FrameHello, "client", DataHello{}, ""},
	{"welcome", FrameWelcome, "server", DataWelcome{}, ""},
	{"reject", FrameReject, "server", DataReject{}, ""},
	{"snapshot", FrameSnapshot, "server", nil, "8-byte big-endian offset it covers, 1-byte encoding name length, encoding name, screen bytes"},
	{"bytes", FrameBytes, "server", nil, "8-byte big-endian offset of the first byte, then program output"},
	{"resize", FrameResize, "server", ResizeFrame{}, ""},
	{"exited", FrameExited, "server", ExitedFrame{}, ""},
	{"credit", FrameCredit, "client", CreditFrame{}, ""},
}

// Enums lists the known words of each word set. A set the server sends is open: a client maps an
// unknown word to a fallback. A set the client sends is closed: the server refuses unknown words.
var Enums = map[string][]string{
	"ClientKind":    {"app", "terminal", "cli", "script", "hab"},
	"SessionState":  {"running", "exited"},
	"ScopeKind":     {"host", "projects", "sessions"},
	"OwnerReason":   {"create", "take", "request", "typed", "disown", "lost", "disconnect", "restore"},
	"ClosingReason": {"event_backlog", "shutdown"},
	"RedrawMode":    {"snapshot", "nudge"},
	"DeviceKind":    {"hab", "phone", "desktop", "gff"},
	"DeviceState":   {"paired", "revoked"},
	"AgentStatus":   {"idle", "working", "blocked", "done", "error", "none", "exited"},
	"BlockedKind":   {"permission", "question", "auth", "trust"},
	"StateSource":   {"server", "hook", "osc", "screen", "spawn"},
	"HookHealth":    {"missing"},
	"ErrorKind":     {"not_found", "conflict", "unavailable", "revision_unsupported", "not_owner", "ticket_invalid", "locked", "denied", "invalid"},
	// Sets added for the clients' calls (spec 3.9).
	"SendOrigin":      {"typed", "dictated", "voice"},
	"ApprovalChoice":  {"allow", "deny", "answer"},
	"TrustChoice":     {"trust", "exit"},
	"AnswerVia":       {"app", "banner", "watch", "voice", "terminal"},
	"ChatKind":        {"user", "agent", "tool", "event", "plan"},
	"ToolStatus":      {"running", "done", "error", "denied"},
	"ChatEvent":       {"compaction", "permission", "interrupt", "command", "notice", "reset", "unknown"},
	"PlanStepStatus":  {"pending", "in_progress", "done"},
	"PushEnvironment": {"development", "production"},
	"PushKind":        {"permission", "question", "auth", "error", "done", "usage", "round", "trust"},
	"NoticeLevel":     {"passive", "active", "time-sensitive"},
	"CommandState":    {"running", "done", "unknown"},
	// Sets added for rich content (app.md 4.7).
	"ContentKind":       {"image", "markdown", "pdf", "html", "text", "diff", "file"},
	"ContentChange":     {"added", "modified", "deleted", "renamed", "untracked"},
	"ContentDiffSource": {"git", "untracked", "transcript", "none"},
	// Sets added for the permission check (macos-identity.md 7).
	"PermissionProcess": {"holder", "hab"},
	"PermissionState":   {"granted", "denied", "unknown", "unanswered", "off", "note", "error"},
	// Sets added for the mesh (mesh.md 4.3).
	"PeerState": {"online", "connecting", "offline", "refused", "update_needed"},
	// Sets added for channels (mesh.md 6.5).
	"ChannelState": {"queued", "handed", "delivered", "not_delivered", "refused", "expired"},
	"MailKind":     {"message", "status"},
	"MeshReason":   {"mail", "logbook"},
	// Sets added for worktrees (spec 3.10).
	"SetupMode":     {"auto", "run", "skip"},
	"SetupState":    {"none", "started", "pending", "skipped", "error"},
	"TeardownState": {"none", "skipped", "ran", "failed"},
	// Sets added for the hab speech engine (voice.md).
	"SpeechState":  {"absent", "downloading", "loading", "ready"},
	"SpeechFormat": {"opus", "pcm16"},
	// Sets added for hab-side settings (app.md 20.5).
	"SettingSource": {"default", "client", "config"},
	// Sets added for the read tools (capcom.md 4).
	"ReadHostState": {"ok", "partial", "offline", "update_needed", "error"},
	// Sets added for confirmations (capcom.md 6.1).
	"ConfirmKind":   {"start", "setup"},
	"ConfirmState":  {"open", "running", "done", "failed", "cancelled", "expired", "unknown"},
	"ConfirmChoice": {"confirm", "cancel"},
	// Sets added for Capcom (capcom.md 2.1).
	"SessionKind": {"capcom"},
	// Sets added for rounds (rounds.md 3, 7.1).
	"RoundMode":        {"fresh", "resume", "fork"},
	"RoundTriggerKind": {"cron", "once", "manual"},
	"RoundCatchUp":     {"once", "skip"},
	"RoundOnFinish":    {"close", "keep"},
	"RoundNotify":      {"failures", "always", "never"},
	"RoundRunState":    {"queued", "waiting", "running", "blocked", "succeeded", "failed", "timed_out", "skipped", "taken_over"},
	"RoundNoticeKind":  {"run_failed", "run_succeeded", "paused", "skips", "target_gone", "state_set_aside"},
	// Sets added for the Capcom entry API (capcom.md 8.1).
	"CapcomContextKind": {"notification", "session", "file", "command", "logbook"},
	// Sets added for Logbook (logbook.md 10.3).
	"LogbookChangeOp":   {"create", "set", "add", "remove", "note", "forget"},
	"LogbookWriteState": {"applied", "proposed", "rejected"},
	// Sets added for Logbook approval (logbook.md 9.2).
	"LogbookDecision": {"approve", "reject"},
	// Sets added for the heavy-run lock (groups.md 8.1).
	"HeavyState": {"holding", "waiting"},
}

// Translator converts messages between revision To and To-1 (spec 3.3). The server keeps one per
// revision step down to MinRevision.
type Translator interface {
	// UpParams rewrites a request's params from a revision To-1 client into revision To.
	UpParams(method string, params RawJSON) (RawJSON, error)
	// DownResult rewrites a revision To result for a revision To-1 client.
	DownResult(method string, result RawJSON) (RawJSON, error)
	// DownEvent rewrites a revision To event for a revision To-1 client. ok false drops it.
	DownEvent(e Event) (out Event, ok bool, err error)
}

// Translators maps revision N to the translator between N and N-1. Revision 1 needs none.
var Translators = map[int]Translator{}

// Capabilities lists the known capability strings (spec 3.4). "devices" means the hab's embedded
// SSH server is on and the devices.* methods work (transport.md 9.4). "push" means the hab has
// working push config (app.md 8.4). "host-stats" means hab.stats works. "commands" means the
// command.* methods and the command.updated event work (spec 2.5). "permissions" means
// hab.permissions works (macos-identity.md 7.2). "mesh" means the hab keeps a peer list and
// serves peer links, the peers.* methods and the peer.updated event (mesh.md 4.2).
// "idempotency-keys" means the hab honours idempotency_key on session.create and session.send
// (spec 3.9); a client sends keys only to a hab that lists it. "detect-explain" means
// detect.explain works (harness-adapters.md, Detection).
// "catalog" means the catalog.* methods and the catalog.updated event work (mesh.md 5). "usage"
// means usage.list and the usage.updated event work (harness-adapters.md, Usage). "content"
// means the content.* methods and the content.updated event work (app.md 4.7). "channels"
// means the channel.* and mail.* methods and the channel.message event work, and that the hab
// sends mesh.changed to a peer link when mail waits for it (mesh.md 6.5).
// "speech" means the hab's speech engine is installed and turned on, so speech.prepare and
// speech.transcribe work, though the model may still need its download (voice.md).
// "attachments" means the attachment.* methods work and session.send takes attachment ids
// (app.md 4.4). "settings" means settings.get, settings.set and the settings.updated event work,
// and the host snapshot carries the settings (app.md 20.5). "read-tools" means transcript.list,
// transcript.search, transcript.read, notice.get and file.read work (capcom.md 4). "logbook" means
// the hab has a Logbook replica, serves logbook.pull and logbook.push to peer links and sends
// them mesh.changed with logbook when ops wait (logbook.md 7), and the logbook.* methods for
// clients work (logbook.md 10.3). "confirm" means
// the confirm.* methods and events work, the host snapshot carries the open confirmations, and
// session.create takes model and seed (capcom.md 6.1). "capcom" means capcom.open, capcom.assign
// and session.resume of a Capcom session work, and the catalog carries the capcom section
// (capcom.md 7). "rounds" means the round.* methods and the round.updated and
// round.run_updated events work, the host snapshot carries the rounds, and a run session
// carries Session.round (rounds.md 13). "heavy" means heavy.take, heavy.release and
// heavy.status work, and the catalog carries the heavy section (groups.md 8).
var Capabilities = []string{"data-streams", "snapshot-vt", "snapshot-ghostty", "agent-state", "transcripts", "devices", "push", "host-stats", "commands", "permissions", "mesh", "idempotency-keys", "detect-explain", "catalog", "usage", "content", "channels", "speech", "attachments", "settings", "read-tools", "logbook", "confirm", "capcom", "rounds", "heavy"}
