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
	{"worktree.list", "List a repository's working trees, the main one first (spec 3.10).", WorktreeListParams{}, WorktreeList{}},
	{"worktree.create", "Make a worktree, copy the files .worktreeinclude names, and start its setup script in a session (spec 3.10).", WorktreeCreateParams{}, WorktreeCreated{}},
	{"worktree.setup", "Start a worktree's setup script in a session now: the yes to a pending setup, or a rerun (spec 3.10).", WorktreeSetupParams{}, WorktreeSetupResult{}},
	{"worktree.remove", "Run the teardown script, then remove a linked worktree; its branch stays. Uncommitted changes need force (spec 3.10).", WorktreeRemoveParams{}, WorktreeRemoved{}},
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
	{"usage.updated", "An account's usage reading changed; the id is the account key. Ephemeral.", UsageAccount{}},
	{"command.updated", "A pane's command started or ended; the id is the pane id (spec 2.5). Ephemeral.", Command{}},
	{"peer.updated", "A peer's link state changed; the id is the peer's label (mesh.md 4.3). Host scope only. Ephemeral.", Peer{}},
	{"catalog.updated", "A catalog this hab holds changed: its own or a peer's copy; the id is the device id (mesh.md 5.2). Host scope only. Ephemeral.", CatalogUpdated{}},
	{"notice.raised", "A notice was raised (notice.raise); the id is the notice id. Ephemeral.", Notice{}},
	{"content.updated", "A session showed a file (content.show); the id is the item id. Ephemeral.", ContentItem{}},
	{"channel.message", "A message this hab sent changed state; the id is the message id (mesh.md 6.5). Host scope only. Ephemeral.", ChannelRecord{}},
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
	"BlockedKind":   {"permission", "question", "auth"},
	"StateSource":   {"server", "hook", "osc", "screen", "spawn"},
	"HookHealth":    {"missing"},
	"ErrorKind":     {"not_found", "conflict", "unavailable", "revision_unsupported", "not_owner", "ticket_invalid", "invalid"},
	// Sets added for the clients' calls (spec 3.9).
	"SendOrigin":      {"typed", "dictated", "voice"},
	"ApprovalChoice":  {"allow", "deny", "answer"},
	"AnswerVia":       {"app", "banner", "watch", "voice", "terminal"},
	"ChatKind":        {"user", "agent", "tool", "event", "plan"},
	"ToolStatus":      {"running", "done", "error", "denied"},
	"ChatEvent":       {"compaction", "permission", "interrupt", "command", "notice", "reset", "unknown"},
	"PlanStepStatus":  {"pending", "in_progress", "done"},
	"PushEnvironment": {"development", "production"},
	"PushKind":        {"permission", "question", "auth", "error", "done", "usage"},
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
	"MeshReason":   {"mail"},
	// Sets added for worktrees (spec 3.10).
	"SetupMode":     {"auto", "run", "skip"},
	"SetupState":    {"none", "started", "pending", "skipped", "error"},
	"TeardownState": {"none", "skipped", "ran", "failed"},
	// Sets added for the hab speech engine (voice.md).
	"SpeechState":  {"absent", "downloading", "loading", "ready"},
	"SpeechFormat": {"opus", "pcm16"},
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
// (app.md 4.4).
var Capabilities = []string{"data-streams", "snapshot-vt", "snapshot-ghostty", "agent-state", "transcripts", "devices", "push", "host-stats", "commands", "permissions", "mesh", "idempotency-keys", "detect-explain", "catalog", "usage", "content", "channels", "speech", "attachments"}
