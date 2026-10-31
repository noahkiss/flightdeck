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
}

type HostInfo struct {
	ID string `json:"id" doc:"The hab's host id, from config."`
}

type Welcome struct {
	Seq
	Revision       int      `json:"revision" doc:"The revision this connection uses."`
	Epoch          string   `json:"epoch" doc:"Random id of this gff-hab process."`
	Build          string   `json:"build" doc:"gff-hab build string."`
	Host           HostInfo `json:"host"`
	Capabilities   []string `json:"capabilities" doc:"Optional features this host offers (spec 3.4)."`
	ClientID       string   `json:"client_id"`
	ReconnectToken string   `json:"reconnect_token"`
	DeviceID       string   `json:"device_id,omitempty" doc:"SHA-256 fingerprint of the hab's own SSH host key, in the standard SSH form. Clients pin it (transport.md 5). Optional, so a newer client accepts a welcome from an older revision 1 hab."`
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
}

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
	Host     HostInfo  `json:"host"`
	Projects []Project `json:"projects"`
	Sessions []Session `json:"sessions"`
	Panes    []Pane    `json:"panes"`
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
	Revision *uint64   `json:"revision,omitempty" doc:"The object's revision after the change. Absent on ephemeral events (pane.owner, pane.activity)."`
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
}

// --- Data connection (spec 5) ---

type DataHello struct {
	Ticket string  `json:"ticket"`
	Offset *uint64 `json:"offset,omitempty" doc:"Holder offset after the last byte the client applied, to resume."`
}

type DataWelcome struct {
	Stream string `json:"stream"`
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
	Bytes uint64 `json:"bytes"`
}
