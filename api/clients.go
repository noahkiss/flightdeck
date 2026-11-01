package api

// The calls and events the clients need (docs/specs/gff-client.md 14, docs/specs/app.md 17).
// docs/specs/hab-logic-api.md 3.9 lists each with its purpose and the stage that builds it.

// --- Sessions and panes ---

// SessionCreateParams starts a program in a new pane and files it as a session.
type SessionCreateParams struct {
	Name       string   `json:"name,omitempty" doc:"Session name, unique on the host. Default: the program's base name, with -2, -3 and so on when taken."`
	Project    string   `json:"project,omitempty" doc:"Project id to file the session in. Give project or project_dir, or neither for an unfiled session."`
	ProjectDir string   `json:"project_dir,omitempty" doc:"Absolute path inside a git repository. The hab finds the project by the repository's remote, else its root commit, and creates the project when it is new."`
	Harness    string   `json:"harness,omitempty" doc:"Detection manifest id of a harness to start, such as claude. Needs that harness's adapter."`
	Argv       []string `json:"argv,omitempty" doc:"Program and arguments. Default: the hab's $SHELL, else /bin/sh."`
	Cwd        string   `json:"cwd,omitempty" doc:"Absolute working directory. Default: project_dir, else the hab's home directory."`
	Env        []string `json:"env,omitempty" doc:"KEY=value entries added to the hab's own environment; an entry replaces the hab's value for its key."`
	Cols       int      `json:"cols,omitempty" doc:"Initial width. Default 80."`
	Rows       int      `json:"rows,omitempty" doc:"Initial height. Default 24."`
}

// SessionCreated names the session and pane a create or resume made.
type SessionCreated struct {
	Seq
	Session string `json:"session" doc:"Session id."`
	Pane    string `json:"pane" doc:"Pane id."`
}

// SessionRef names one session.
type SessionRef struct {
	Session string `json:"session"`
}

// SessionFocusParams reports whether a session is in focus on this client.
type SessionFocusParams struct {
	Session string `json:"session"`
	Active  bool   `json:"active" doc:"True while the session is on screen in an active window."`
}

// SendOrigin says how the owner entered a message. The hab only logs it.
type SendOrigin string

// SessionSendParams sends one message to the session's agent.
type SessionSendParams struct {
	Session     string     `json:"session"`
	Text        string     `json:"text"`
	Attachments []string   `json:"attachments,omitempty" doc:"Attachment ids from attachment.put."`
	Origin      SendOrigin `json:"origin"`
}

// --- Approvals ---

// ApprovalChoice is the answer to a pending request.
type ApprovalChoice string

// AnswerVia says where an answer came from. The hab only logs it.
type AnswerVia string

// ApprovalAnswerParams answers one pending request by id.
type ApprovalAnswerParams struct {
	Pane    string         `json:"pane"`
	Request string         `json:"request" doc:"The request id from the pane's agent state."`
	Choice  ApprovalChoice `json:"choice" doc:"allow or deny a permission; answer a question with text."`
	Text    string         `json:"text,omitempty" doc:"With choice answer: the chosen option or free text."`
	Via     AnswerVia      `json:"via,omitempty"`
}

// --- Chat ---

// ChatKind is the kind of a chat message. Clients show an unknown kind as unsupported.
type ChatKind string

// ChatEvent says what a chat message of kind event marks. Clients show an unknown word as a
// plain event line.
type ChatEvent string

// ToolStatus is a tool call's status.
type ToolStatus string

// PlanStepStatus is one plan step's status.
type PlanStepStatus string

// ChatTool is the tool call a chat message of kind tool shows.
type ChatTool struct {
	Name   string     `json:"name"`
	Target string     `json:"target,omitempty" doc:"A short target: a file, a command."`
	Status ToolStatus `json:"status"`
	Input  RawJSON    `json:"input,omitempty" doc:"The tool input as the harness wrote it, when it is at most 64 KiB."`
	Output string     `json:"output,omitempty" doc:"The first part of the tool output."`
	// OutputTruncated says Output holds only the first part.
	OutputTruncated bool `json:"output_truncated,omitempty" doc:"True when output holds only the first part of a longer output."`
}

// PlanStep is one step of a plan.
type PlanStep struct {
	Text   string         `json:"text"`
	Status PlanStepStatus `json:"status"`
}

// ChatMessage is one message of a session's conversation, read from the harness transcript.
type ChatMessage struct {
	ID          string     `json:"id" doc:"Stable within the session; an update carries the same id."`
	Session     string     `json:"session"`
	Kind        ChatKind   `json:"kind"`
	At          string     `json:"at" doc:"RFC 3339 time from the transcript."`
	Text        string     `json:"text,omitempty" doc:"Markdown for user and agent text; a line for an event."`
	Sender      string     `json:"sender,omitempty" doc:"Another agent that sent this message (channels)."`
	Parent      string     `json:"parent,omitempty" doc:"The message id of the subagent call this message belongs to."`
	Tool        *ChatTool  `json:"tool,omitempty" doc:"With kind tool."`
	Plan        []PlanStep `json:"plan,omitempty" doc:"With kind plan: the steps in order."`
	Attachments []string   `json:"attachments,omitempty" doc:"Attachment ids shown on the message."`
	// Event is added by the transcript reader (Part 4 stage 5). Optional, so a newer client
	// reads a message from an older revision 1 hab.
	Event ChatEvent `json:"event,omitempty" doc:"With kind event: what the line marks. unknown marks a transcript record the reader does not know; text names it."`
}

// ChatPageParams reads one page of a session's conversation.
type ChatPageParams struct {
	Session string `json:"session"`
	Before  string `json:"before,omitempty" doc:"Message id: return the messages before it. Absent means the tail."`
	Limit   int    `json:"limit,omitempty" doc:"Most messages to return. Default 50, at most 200."`
	After   string `json:"after,omitempty" doc:"A cursor from an earlier page: return the messages added or changed since it, oldest first. Not with before."`
}

// ChatMessages is one page of a conversation, oldest first.
type ChatMessages struct {
	Seq
	Messages []ChatMessage `json:"messages"`
	More     bool          `json:"more" doc:"True when older messages exist. With after: true when more changes wait after cursor."`
	Cursor   string        `json:"cursor,omitempty" doc:"Opaque. Pass it as after to read only what was added or changed since this page."`
}

// TurnEnd is the data of a session.turn_end event.
type TurnEnd struct {
	Session      string `json:"session"`
	FinalMessage string `json:"final_message,omitempty" doc:"Id of the turn's last agent message."`
}

// --- Attachments ---

// AttachmentPutParams starts an upload of one file for a session.
type AttachmentPutParams struct {
	Session string `json:"session"`
	Name    string `json:"name" doc:"File name as the owner sees it."`
	Type    string `json:"type" doc:"Media type, such as image/png."`
	Size    uint64 `json:"size" doc:"Bytes."`
	SHA256  string `json:"sha256" doc:"Hex SHA-256 of the bytes."`
}

// AttachmentStored names a stored attachment.
type AttachmentStored struct {
	Seq
	ID   string `json:"id"`
	Path string `json:"path" doc:"Where the hab stored the file, inside the project's attachment folder."`
}

// AttachmentGetParams fetches an attachment or its thumbnail.
type AttachmentGetParams struct {
	ID        string `json:"id"`
	Thumbnail bool   `json:"thumbnail,omitempty" doc:"Return a small preview image instead of the file."`
}

// AttachmentData is an attachment's bytes.
type AttachmentData struct {
	Seq
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type" doc:"Media type of data."`
	Data []byte `json:"data"`
}

// --- Push (app.md 8, 9) ---

// PushEnvironment is the APNs environment a token belongs to.
type PushEnvironment string

// PushKind is a kind of push a device wants.
type PushKind string

// PushRegisterParams stores an APNs device token for a device.
type PushRegisterParams struct {
	DeviceID    string          `json:"device_id" doc:"The device's key fingerprint (transport.md 5)."`
	Token       string          `json:"token" doc:"APNs device token, hex."`
	Environment PushEnvironment `json:"environment"`
	Kinds       []PushKind      `json:"kinds" doc:"The kinds of push this device wants."`
}

// PushActivityParams stores a Live Activity token for the session a device tracks.
type PushActivityParams struct {
	DeviceID string `json:"device_id"`
	Session  string `json:"session"`
	Token    string `json:"token" doc:"Live Activity push token, hex."`
}

// PushUnregisterParams drops a token.
type PushUnregisterParams struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

// --- Pane activity ---

// PaneBell is the data of a pane.bell event.
type PaneBell struct {
	Count int `json:"count" doc:"Bells since the last pane.bell for this pane."`
}

// PaneOutput is the data of a pane.output event.
type PaneOutput struct {
	Offset uint64 `json:"offset" doc:"The pane's holder offset after the output."`
}

// --- Host stats ---

type HabStatsParams struct{}

// HostStats is a reading of the host's resources.
type HostStats struct {
	Seq
	CPUPct     int    `json:"cpu_pct" doc:"CPU in use over the last few seconds, as a percentage of all cores."`
	MemUsed    uint64 `json:"mem_used" doc:"Bytes."`
	MemTotal   uint64 `json:"mem_total" doc:"Bytes."`
	DiskUsed   uint64 `json:"disk_used" doc:"Bytes, on the file system of the hab's state directory."`
	DiskTotal  uint64 `json:"disk_total" doc:"Bytes."`
	BatteryPct *int   `json:"battery_pct,omitempty" doc:"Absent on a host with no battery."`
	UptimeS    uint64 `json:"uptime_s" doc:"Seconds since the host booted."`
	At         string `json:"at" doc:"RFC 3339 time of the reading."`
}

// --- Usage (app.md 14) ---

// UsageWindow is one limit window of an account.
type UsageWindow struct {
	Name     string `json:"name" doc:"5h, 7d, or a model name."`
	UsedPct  int    `json:"used_pct" doc:"Used, in whole percent."`
	ResetsAt string `json:"resets_at,omitempty" doc:"RFC 3339 time the window resets."`
	ReadAt   string `json:"read_at" doc:"RFC 3339 time of the reading."`
}

// UsageAccount is the latest usage reading of one harness account.
type UsageAccount struct {
	Key     string        `json:"key" doc:"Stable, not secret: the adapter's hash of an account id, else the host and harness."`
	Label   string        `json:"label,omitempty" doc:"Plan name, such as Max 5x."`
	Harness string        `json:"harness"`
	Windows []UsageWindow `json:"windows"`
}

type UsageListParams struct{}

// UsageReadings is every account's latest reading on this hab.
type UsageReadings struct {
	Seq
	Accounts []UsageAccount `json:"accounts"`
}
