// Package api holds the gff-hab API as Go types. These types are the source of truth: the
// generator in internal/apigen turns them into api/schema.json, the Go client stubs and the
// Swift package. See docs/specs/hab-logic-api.md, sections 3 to 5.
//
// Conventions the generator relies on:
//   - A pointer field, or a field tagged omitempty, is optional. Every other field is required.
//   - []byte is base64 in JSON.
//   - A named string type with an entry in Enums is an open set of words.
//   - An embedded struct is flattened into the outer object.
//   - A `doc` struct tag becomes the field's description.
package api

// Revision is the API revision this build speaks. See spec 3.3.
const Revision = 1

// MinRevision is the oldest revision this build translates down to.
const MinRevision = 1

// Seq carries the last queued event seq. Every result embeds it (spec 3.5).
type Seq struct {
	Seq uint64 `json:"seq" doc:"Seq of the last event queued for this connection before the request ran."`
}

// Empty is the result of a call that returns nothing but the seq.
type Empty struct {
	Seq
}

// --- Handshake (spec 3.2) ---

type ClientKind string

type ClientInfo struct {
	Name    string     `json:"name"`
	Version string     `json:"version"`
	Kind    ClientKind `json:"kind"`
}

type HelloParams struct {
	Revision       int        `json:"revision" doc:"Newest API revision the client speaks."`
	MinRevision    int        `json:"min_revision" doc:"Oldest API revision the client accepts."`
	Client         ClientInfo `json:"client"`
	Label          string     `json:"label,omitempty" doc:"Human label, such as phone or desk."`
	ReconnectToken string     `json:"reconnect_token,omitempty" doc:"Token from an earlier welcome (spec 3.7)."`
	Peer           *PeerHello `json:"peer,omitempty" doc:"Set by a hab that dials another hab (client.kind hab): who is calling (mesh.md 3.2)."`
}

// PeerHello is the dialing hab's identity in a peer link's hello. Over the system sshd it is a
// claim, not a proof; the caller already passed SSH as the owner (mesh.md 3.2).
type PeerHello struct {
	DeviceID string `json:"device_id" doc:"The dialing hab's device id."`
	Host     string `json:"host" doc:"The dialing hab's host id."`
}

type HostInfo struct {
	ID string `json:"id" doc:"The hab's host id, from config."`
}

type Welcome struct {
	Seq
	Revision       int      `json:"revision" doc:"The revision this connection uses."`
	Epoch          string   `json:"epoch" doc:"Random id of this gff-hab process."`
	Build          string   `json:"build" doc:"gff-hab build string."`
	Version        string   `json:"version,omitempty" doc:"gff-hab release version from the release tag: x.y.z, or x.y.z-rc.n or x.y.z-dry.n for a candidate or a dry run; dev for any other build. Clients compare release versions to mark an older hab superseded (gff-client.md 9.1). Optional, so a newer client accepts a welcome from an older revision 1 hab."`
	Host           HostInfo `json:"host"`
	Capabilities   []string `json:"capabilities" doc:"Optional features this host offers (spec 3.4)."`
	ClientID       string   `json:"client_id"`
	ReconnectToken string   `json:"reconnect_token"`
	DeviceID       string   `json:"device_id,omitempty" doc:"SHA-256 fingerprint of the hab's own SSH host key, in the standard SSH form. Clients pin it (transport.md 5). Optional, so a newer client accepts a welcome from an older revision 1 hab."`
	Time           string   `json:"time,omitempty" doc:"The hab's clock when it sent the welcome, RFC 3339 in UTC with milliseconds. Clients measure reset times and ages against it, and may compute their clock's offset. Optional, so a newer client accepts a welcome from an older revision 1 hab."`
}

// --- Structure (spec 2.2) ---

type SessionState string

type Project struct {
	ID         string   `json:"id"`
	Revision   uint64   `json:"revision"`
	Name       string   `json:"name"`
	Remote     string   `json:"remote,omitempty" doc:"Git remote that identifies the project."`
	RootCommit string   `json:"root_commit,omitempty" doc:"Root commit, when the project has no remote."`
	Sessions   []string `json:"sessions" doc:"Session ids in order."`
	// The repository fields feed the app's sidebar header (app.md 3.5). Optional, so a newer
	// client reads a project from an older revision 1 hab.
	Branch string `json:"branch,omitempty" doc:"Current branch of the repository in the project's folder. Absent on a detached HEAD or outside a repository."`
	Head   string `json:"head,omitempty" doc:"Full commit id of HEAD. Absent outside a repository."`
	Ahead  *int   `json:"ahead,omitempty" doc:"Commits on the branch that its upstream lacks. Absent with no upstream."`
	Behind *int   `json:"behind,omitempty" doc:"Commits on the upstream that the branch lacks. Absent with no upstream."`
}

type ExitStatus struct {
	Code   *int   `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
}

type Session struct {
	ID       string       `json:"id"`
	Revision uint64       `json:"revision"`
	Name     string       `json:"name" doc:"Unique on the host."`
	Project  string       `json:"project,omitempty" doc:"Project id. Absent means unfiled."`
	Pane     string       `json:"pane,omitempty"`
	State    SessionState `json:"state"`
	Exit     *ExitStatus  `json:"exit,omitempty"`
	// The agent fields feed the session's row (app.md 3.4, 3.5). Optional, so a newer client
	// reads a session from an older revision 1 hab.
	Title      string `json:"title,omitempty" doc:"The agent session's title, from its adapter. Absent when the harness has none."`
	Model      string `json:"model,omitempty" doc:"The model in use, as the harness names it."`
	ContextPct *int   `json:"context_pct,omitempty" doc:"How full the context window is, in whole percent of the current model's window. Absent when unknown; never guessed."`
	// Kind and Dir mark a Capcom session (capcom.md 2.1, 7.4). Optional, so a newer client
	// reads a session from an older revision 1 hab.
	Kind SessionKind `json:"kind,omitempty" doc:"capcom for a Capcom session, which clients label Capcom. Absent for an ordinary session."`
	Dir  string      `json:"dir,omitempty" doc:"The hab-owned folder a Capcom session runs in; a plain terminal resumes the harness from there. Absent for other sessions."`
	// Round marks a run session (rounds.md 7.2). Optional, so a newer client reads a session
	// from an older revision 1 hab.
	Round *SessionRound `json:"round,omitempty" doc:"Set on a session a round's run started: the round and the run. Every client can badge and filter it."`
}

// SessionKind says what a session is for, beyond running a program. Open: a client shows an
// unknown kind as an ordinary session.
type SessionKind string

type Pane struct {
	ID          string `json:"id" doc:"Holder pane id, never reused."`
	Revision    uint64 `json:"revision"`
	HolderBuild string `json:"holder_build"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
	Owner       string `json:"owner,omitempty" doc:"Client id of the size owner (spec 6.1)."`
	Offset      uint64 `json:"offset" doc:"Holder absolute output offset (spec 5.4)."`
	Session     string `json:"session,omitempty" doc:"Absent means unfiled."`
}

type Structure struct {
	Host     HostInfo     `json:"host"`
	Projects []Project    `json:"projects"`
	Sessions []Session    `json:"sessions"`
	Panes    []Pane       `json:"panes"`
	Agents   []AgentState `json:"agents,omitempty" doc:"The current agent state of each pane that has one (harness-adapters.md, State event model). Absent from a hab without the agent-state capability."`
	Settings []Setting    `json:"settings,omitempty" doc:"The hab-side settings (app.md 20.5), in a host scope snapshot only. Absent from a hab without the settings capability."`
	// Confirmations are the open ones (capcom.md 6.1), so a client shows each card after a
	// (re)subscribe with no read.
	Confirmations []Confirmation `json:"confirmations,omitempty" doc:"The open confirmations (capcom.md 6.1), in a host scope snapshot only. Absent when none is open, and from a hab without the confirm capability."`
	Rounds        []Round        `json:"rounds,omitempty" doc:"The rounds (rounds.md 13), in a host scope snapshot only. Absent when the hab has none, and from a hab without the rounds capability."`
}

// --- Agent state (docs/specs/harness-adapters.md, State event model) ---

type AgentStatus string

type BlockedKind string

type StateSource string

type HookHealth string

// PermissionRequest is what a harness asks permission for, copied from its structured payload,
// never summarised.
type PermissionRequest struct {
	ID      string   `json:"id,omitempty" doc:"The harness's id for the request, when it gives one. A permission answer must name it."`
	Tool    string   `json:"tool,omitempty"`
	Command string   `json:"command,omitempty" doc:"The exact command, for a shell tool."`
	Files   []string `json:"files,omitempty" doc:"The exact files, for a file tool."`
	Cwd     string   `json:"cwd,omitempty"`
	Input   RawJSON  `json:"input,omitempty" doc:"The tool input as the harness sent it, when it is at most 64 KiB."`
	// InputTruncated keeps a large tool input (a whole file being written) out of every event
	// and snapshot; command and files are still exact.
	InputTruncated bool `json:"input_truncated,omitempty" doc:"True when the tool input was over 64 KiB and input is left out."`
	// What the approval sheet and the banner show (app.md 7.2, 7.4, 16.2). Optional, so a newer
	// client reads a request from an older revision 1 hab.
	Title       string `json:"title,omitempty" doc:"A short question: the harness's own description of the call, else one built from the tool."`
	Description string `json:"description,omitempty" doc:"A longer description of the call, when the harness gives one."`
	ContextLine string `json:"context_line,omitempty" doc:"The agent's last message before the request, one or two lines, when the transcript has one."`
	ExpiresAt   string `json:"expires_at,omitempty" doc:"RFC 3339 time after which the hab refuses a remote answer with conflict. Absent when no deadline applies."`
}

// TrustPrompt is a harness's folder trust prompt, read from the pane's screen (app.md 7.10).
type TrustPrompt struct {
	Folder string `json:"folder" doc:"The folder the session started in, which the harness asks to trust."`
}

// VersionWarning says the harness reported a version its adapter was not tested on.
type VersionWarning struct {
	Version string `json:"version"`
	Tested  string `json:"tested" doc:"Newest tested version, as a prefix: 2.1 covers every 2.1.x."`
	Reason  string `json:"reason" doc:"newer_than_tested or unreadable."`
}

// AgentState is the data of an agent.state event and one entry of Structure.agents.
type AgentState struct {
	Pane     string             `json:"pane" doc:"Pane id; also the event's id."`
	Revision uint64             `json:"revision" doc:"Per pane, raised on every accepted change. Restarts with the epoch."`
	Harness  string             `json:"harness" doc:"claude, codex, opencode, or another detection manifest id."`
	Session  string             `json:"session,omitempty" doc:"The harness's native session id, once known."`
	State    AgentStatus        `json:"state"`
	Last     AgentStatus        `json:"last,omitempty" doc:"With state exited: the last done or error before the harness ended."`
	Kind     BlockedKind        `json:"kind,omitempty" doc:"Only with state blocked."`
	Request  *PermissionRequest `json:"request,omitempty" doc:"Only with state blocked and kind permission."`
	// Trust is added by the trust card (app.md 7.10). Optional, so a newer client reads an older
	// hab.
	Trust   *TrustPrompt    `json:"trust,omitempty" doc:"Only with state blocked and kind trust: the folder the harness asks to trust."`
	Source  StateSource     `json:"source"`
	Turn    string          `json:"turn,omitempty" doc:"The harness's turn or prompt id, where it gives one."`
	At      string          `json:"at" doc:"RFC 3339 time gff-hab accepted the change."`
	Version string          `json:"version,omitempty" doc:"Harness version, once reported."`
	Warning *VersionWarning `json:"warning,omitempty" doc:"Set while the harness version is outside the adapter's tested range."`
	// Hooks is added by the Codex adapter. Optional, so a newer client reads an older hab.
	Hooks HookHealth `json:"hooks,omitempty" doc:"missing: the harness finished a turn and none of its hooks reached gff-hab. Its hook config is not installed, or (Codex) not trusted. Absent once a hook arrives."`
	// Error is added by rounds stage 4. Optional, so a newer client reads an older hab.
	Error string `json:"error,omitempty" doc:"With state error: the harness's own kind of failure, where it gives one, such as Claude Code's rate_limit or authentication_failed."`
}

type StructureGetParams struct{}

type StructureResult struct {
	Seq
	Structure
}

// --- Events (spec 3.5) ---

type ScopeKind string

type Scope struct {
	Kind ScopeKind `json:"kind"`
	IDs  []string  `json:"ids,omitempty" doc:"Project or session ids, for kind projects or sessions."`
}

type SubscribeParams struct {
	Scope Scope `json:"scope"`
}

type EventType string

// Event is the params of the `event` notification. Data's shape depends on Type; see Events.
type Event struct {
	Seq      uint64    `json:"seq"`
	Type     EventType `json:"type"`
	ID       string    `json:"id" doc:"Id of the object the event is about."`
	Revision *uint64   `json:"revision,omitempty" doc:"The object's revision after the change. Absent on ephemeral events (spec 3.5, 3.9)."`
	Data     RawJSON   `json:"data"`
}

// Deleted is the data of a *.deleted event: the object is gone at the event's revision.
type Deleted struct{}

type OwnerReason string

type PaneOwner struct {
	Owner  string      `json:"owner,omitempty" doc:"New owner client id. Absent means no owner."`
	Reason OwnerReason `json:"reason"`
	Cols   int         `json:"cols"`
	Rows   int         `json:"rows"`
}

type PaneActivity struct {
	Client string `json:"client"`
}

type Heartbeat struct {
	Seq uint64 `json:"seq"`
}

type ClosingReason string

type Closing struct {
	Reason ClosingReason `json:"reason"`
}

// --- Liveness ---

type PingParams struct{}

// --- Input (spec 3.8) ---

type InputParams struct {
	Pane string `json:"pane"`
	Data []byte `json:"data,omitempty" doc:"Bytes to write. Give data or text."`
	Text string `json:"text,omitempty" doc:"UTF-8 text to write. Give data or text."`
}

// --- Data tickets (spec 5.2) ---

type TicketParams struct {
	Pane    string `json:"pane"`
	Observe bool   `json:"observe,omitempty" doc:"Observer stream: never takes ownership (spec 6.4)."`
}

type Ticket struct {
	Seq
	Ticket      string `json:"ticket" doc:"Single use, bound to this client."`
	ExpiresInMS int    `json:"expires_in_ms"`
}

// --- Size ownership (spec 6) ---

type PaneRef struct {
	Pane string `json:"pane"`
}

type OwnerResult struct {
	Seq
	Owner string `json:"owner,omitempty" doc:"Owner after the call. Absent means no owner."`
}

type DesiredSizeParams struct {
	Pane string `json:"pane"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type DesiredSizeResult struct {
	Seq
	Applied bool `json:"applied" doc:"True when the PTY took this size."`
}

type RedrawMode string

type RedrawParams struct {
	Pane string     `json:"pane"`
	Mode RedrawMode `json:"mode,omitempty" doc:"Default snapshot."`
}

// --- Devices (transport.md 5, 6) ---

type DeviceKind string

type DeviceState string

// Device is one device record in the hab's trust store.
type Device struct {
	ID       string      `json:"id" doc:"SHA-256 fingerprint of the device's public key, in the standard SSH form."`
	Label    string      `json:"label"`
	Kind     DeviceKind  `json:"kind"`
	State    DeviceState `json:"state"`
	PairedAt string      `json:"paired_at" doc:"RFC 3339 time."`
	PairedBy string      `json:"paired_by,omitempty" doc:"Device id of the client that confirmed the pairing."`
	LastSeen string      `json:"last_seen,omitempty" doc:"RFC 3339 time."`
}

type DevicesPairParams struct {
	Label string `json:"label,omitempty" doc:"Label for the device that will pair."`
}

// PairingOffer is what a QR code carries (transport.md 6.1).
type PairingOffer struct {
	Seq
	DeviceID    string   `json:"device_id" doc:"The hab's host-key fingerprint, to pin."`
	Endpoints   []string `json:"endpoints" doc:"Embedded SSH server addresses, host:port."`
	Token       string   `json:"token" doc:"One-time pairing token."`
	ExpiresInMS int      `json:"expires_in_ms"`
}

type DevicesListParams struct{}

type DeviceList struct {
	Seq
	Devices []Device `json:"devices" doc:"Paired and revoked devices; revoked ones stay as tombstones."`
}

type DevicesRevokeParams struct {
	ID string `json:"id" doc:"Device id to revoke."`
}

// --- Peers (mesh.md 3, 4.3) ---

// PeerState is where a peer stands, as this hab sees it.
type PeerState string

// Peer is one entry of this hab's peer list with its link state.
type Peer struct {
	Label     string    `json:"label" doc:"This hab's name for the peer, from its peer list."`
	State     PeerState `json:"state"`
	Endpoint  string    `json:"endpoint,omitempty" doc:"The endpoint in use while online, else the one tried last."`
	DeviceID  string    `json:"device_id,omitempty" doc:"The pinned device id. Absent before the first link."`
	OfferedID string    `json:"offered_id,omitempty" doc:"State refused: the different device id the endpoint presented."`
	Host      string    `json:"host,omitempty" doc:"The peer's host id from its last welcome or hello."`
	LastSeen  string    `json:"last_seen,omitempty" doc:"RFC 3339 time the peer was last online. Absent if never."`
	Reason    string    `json:"reason,omitempty" doc:"Why the peer is not online, in a few words."`
	Inbound   bool      `json:"inbound,omitempty" doc:"The peer has a link to this hab open."`
}

// LinkedPeer is a hab with a link to this hab that is not in this hab's peer list.
type LinkedPeer struct {
	DeviceID string `json:"device_id"`
	Host     string `json:"host"`
}

type PeersListParams struct{}

type PeerList struct {
	Seq
	Peers    []Peer       `json:"peers" doc:"The peer list, in file order. This hab's own entry is left out."`
	Unlisted []LinkedPeer `json:"unlisted" doc:"Habs linked to this one that are not in its peer list."`
	Problems []string     `json:"problems" doc:"Entries of the peer file that could not be used, and why."`
}

type PeersAcceptParams struct {
	Label string `json:"label" doc:"The peer whose new device id to accept."`
}

// PeerAccepted is the result of peers.accept: the pin moved from Old to New.
type PeerAccepted struct {
	Seq
	Label string `json:"label"`
	Old   string `json:"old,omitempty" doc:"The device id pinned before."`
	New   string `json:"new" doc:"The device id pinned now."`
}

// --- Discovery ---

type DiscoverParams struct{}

// --- Errors (spec 3.6) ---

// ErrorCode is the JSON-RPC error code for every application error.
const ErrorCode = -32000

type ErrorKind string

type RevisionRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// ErrorData is error.data on every application error.
type ErrorData struct {
	Kind   ErrorKind      `json:"kind"`
	Hint   string         `json:"hint"`
	Seq    uint64         `json:"seq"`
	Server *RevisionRange `json:"server,omitempty" doc:"Server revision range, on revision_unsupported."`
	Client *RevisionRange `json:"client,omitempty" doc:"Client revision range, on revision_unsupported."`
	ID     string         `json:"id,omitempty" doc:"On every settings.set error: the id of the first refused row (app.md 20.5)."`
}

// --- Data connection (spec 5) ---

type DataHello struct {
	Ticket string  `json:"ticket"`
	Offset *uint64 `json:"offset,omitempty" doc:"Holder offset after the last byte the client applied, to resume."`
}

type DataWelcome struct {
	Stream string `json:"stream" doc:"Id of this stream, for logs."`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	Owner  string `json:"owner,omitempty"`
}

type DataReject struct {
	Kind ErrorKind `json:"kind"`
	Hint string    `json:"hint"`
}

type ResizeFrame struct {
	Cols   int         `json:"cols"`
	Rows   int         `json:"rows"`
	Owner  string      `json:"owner,omitempty"`
	Reason OwnerReason `json:"reason"`
}

type ExitedFrame struct {
	ExitStatus
}

type CreditFrame struct {
	Bytes uint64 `json:"bytes" doc:"Bytes the client applied: the screen bytes of snapshot frames and the program output of bytes frames, without their offsets and names."`
}
