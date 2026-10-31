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
	"ClientKind":    {"app", "terminal", "cli", "script"},
	"SessionState":  {"running", "exited"},
	"ScopeKind":     {"host", "projects", "sessions"},
	"OwnerReason":   {"create", "take", "request", "typed", "disown", "lost", "disconnect", "restore"},
	"ClosingReason": {"event_backlog", "shutdown"},
	"RedrawMode":    {"snapshot", "nudge"},
	"ErrorKind":     {"not_found", "conflict", "unavailable", "revision_unsupported", "not_owner", "ticket_invalid", "invalid"},
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

// Capabilities lists the known capability strings (spec 3.4).
var Capabilities = []string{"data-streams", "snapshot-vt", "snapshot-ghostty", "agent-state", "transcripts"}
