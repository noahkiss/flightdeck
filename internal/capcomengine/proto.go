// Package capcomengine is gff-hab's side of the Capcom engine (docs/specs/capcom-service.md 3,
// 4): the wire format of 4.4, the link that hands thread items to the engine and stores what
// it sends back (delivery rules 4.3), approvals with their expiry (5.3), and the supervisor
// that starts, adopts, restarts and drains the engine job (3.2) with its route token (3.3) and
// its credential (10.2). Pure Go.
package capcomengine

import (
	"encoding/json"
	"time"
)

// Upgrade is the protocol name of the engine route's HTTP upgrade (4.4).
const Upgrade = "gff-capcom/1"

// Path is the engine route's prefix on the hook socket: /capcom/<token> (3.3).
const Path = "/capcom/"

// MaxLine bounds one line of the wire format, either way (4.4).
const MaxLine = 4 << 20

// Message types, hab to engine (4.2, 4.4).
const (
	THello          = "hello"
	TInput          = "input"
	TInterrupt      = "interrupt"
	TReseed         = "reseed"
	TPace           = "pace"
	TApprovalAnswer = "approval.answer"
	TShutdown       = "shutdown"
	TContext        = "context"
	TAck            = "ack"
)

// Message types, engine to hab.
const (
	TReady           = "ready"
	TSessionStarted  = "session.started"
	TTurnStarted     = "turn.started"
	TInputTaken      = "input.taken"
	TReply           = "reply"
	TTool            = "tool"
	TApprovalRequest = "approval.request"
	TTurnEnded       = "turn.ended"
	TSummary         = "summary"
	TCompacted       = "compacted"
	TUsage           = "usage"
	TError           = "error"
	TContextRequest  = "context.request"
)

// What a context message is for (6): the material of a summary update, with the seed's parts
// for a re-seed.
const (
	ForReseed  = "reseed"
	ForCompact = "compact"
	ForQuiet   = "quiet"
)

// Re-seed reasons (4.2).
const (
	ReseedNew           = "new"
	ReseedLimit         = "limit"
	ReseedQuiet         = "quiet"
	ReseedRestartFailed = "restart_failed"
	// ReseedSwitch opens a session of another engine than the thread's last one: the switch is a
	// re-seed (18 A6).
	ReseedSwitch = "switch"
)

// Capabilities an engine declares (4.1).
const (
	CapImages           = "images"
	CapInterrupt        = "interrupt"
	CapMidTurnInput     = "mid_turn_input"
	CapApprovalCallback = "approval_callback"
	CapCompactionEvents = "compaction_events"
	CapUsageEvents      = "usage_events"
	CapResumeByID       = "resume_by_id"
)

// Turn results (4.2).
const (
	ResultSuccess     = "success"
	ResultInterrupted = "interrupted"
	ResultLimit       = "limit"
	ResultError       = "error"
)

// Error kinds the engine reports.
const (
	ErrKindAuth  = "auth"
	ErrKindStart = "start"
)

// Credential is the model credential handed in hello (10.2). Its String and GoString never
// show the value, so a log line that formats it by mistake leaks nothing.
type Credential struct {
	Kind  string `json:"kind"` // oauth_token, api_key, or login (no value: the CLI's own login)
	Value string `json:"value"`
}

func (c Credential) String() string   { return "credential(" + c.Kind + ", redacted)" }
func (c Credential) GoString() string { return c.String() }

// Settings are what hello hands the engine for its turns.
type Settings struct {
	Model             string  `json:"model,omitempty"`
	SmallModel        string  `json:"small_model,omitempty"`
	MaxTurns          int     `json:"max_turns"`
	MaxTurnUSD        float64 `json:"max_turn_usd"`
	WakeRetryMS       int64   `json:"wake_retry_ms"`
	RouteBackoffMaxMS int64   `json:"route_backoff_max_ms"`
	// SummaryMaxChars bounds the summary the engine writes (6.1); PrecompactTimeoutMS bounds the
	// summary update before a compaction, and before a re-seed (6.3).
	SummaryMaxChars     int   `json:"summary_max_chars,omitempty"`
	PrecompactTimeoutMS int64 `json:"precompact_timeout_ms,omitempty"`
}

// Recent is one thread item a summary update covers (6.1): a message, a report, a note.
type Recent struct {
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	Author string    `json:"author"`
	Time   time.Time `json:"time"`
	Text   string    `json:"text"`
}

// Seed is the hab's part of a re-seed (6.2): the Logbook part (6.4), the last messages and what
// is still open. The engine puts the header and the summary before them.
type Seed struct {
	Logbook  string   `json:"logbook,omitempty"`
	Messages []Recent `json:"messages,omitempty"`
	Open     []string `json:"open,omitempty"`
}

// ContextBlock is one attached-context block of an input.
type ContextBlock struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// InputItem is one thread item for the model.
type InputItem struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"` // message, agent, note
	Author  string         `json:"author"`
	Time    time.Time      `json:"time"`
	TZ      string         `json:"tz,omitempty"`
	Text    string         `json:"text"`
	Images  []string       `json:"images,omitempty"`
	Context []ContextBlock `json:"context,omitempty"`
}

// TurnRef names a running turn.
type TurnRef struct {
	Turn   string `json:"turn"`
	Origin string `json:"origin"`
}

// Message is one line of the wire format: the envelope and every field any type uses. Item is
// raw because input carries an object and input.taken an id.
type Message struct {
	Seq  uint64 `json:"seq,omitempty"`
	Type string `json:"type"`
	Ack  uint64 `json:"ack,omitempty"`

	// hello
	Credential *Credential `json:"credential,omitempty"`
	Device     string      `json:"device,omitempty"`
	LastItem   string      `json:"last_item,omitempty"`
	SDKSession string      `json:"sdk_session,omitempty"`
	Settings   *Settings   `json:"settings,omitempty"`
	// Reseed asks the engine to open with a re-seed, for this reason, instead of an empty
	// session: the thread's last session was another engine's, or a re-seed's fresh session never
	// got its id. Set only with no SDKSession.
	Reseed string `json:"reseed,omitempty"`

	// input, input.taken
	Item json.RawMessage `json:"item,omitempty"`

	// reseed, pace, approval.answer, context, context.request
	For     string   `json:"for,omitempty"`
	Recent  []Recent `json:"recent,omitempty"`
	Seed    *Seed    `json:"seed,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Pace    string   `json:"pace,omitempty"`
	Until   string   `json:"until,omitempty"`
	Request string   `json:"request,omitempty"`
	Allow   *bool    `json:"allow,omitempty"`
	Message string   `json:"message,omitempty"`

	// ready
	Engine       string   `json:"engine,omitempty"`
	Version      string   `json:"version,omitempty"`
	SDKVersion   string   `json:"sdk_version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`

	// session.started
	Session string `json:"session,omitempty"`
	Why     string `json:"why,omitempty"`

	// turn.*, reply, tool
	Turn         json.RawMessage `json:"turn,omitempty"` // a string, or ready's {turn, origin}
	Origin       string          `json:"origin,omitempty"`
	Items        []string        `json:"items,omitempty"`
	Part         int             `json:"part,omitempty"`
	Text         string          `json:"text,omitempty"`
	Name         string          `json:"name,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Result       string          `json:"result,omitempty"`
	InputTokens  int64           `json:"input_tokens,omitempty"`
	OutputTokens int64           `json:"output_tokens,omitempty"`
	CostUSD      float64         `json:"cost_usd,omitempty"`
	ContextUsed  float64         `json:"context_used,omitempty"`
	Through      string          `json:"through,omitempty"`
	Reading      json.RawMessage `json:"reading,omitempty"`
	Kind         string          `json:"kind,omitempty"`
	Exiting      bool            `json:"exiting,omitempty"`
}

// TurnID reads the turn field as an id. ready's object form gives "".
func (m Message) TurnID() string {
	var s string
	if json.Unmarshal(m.Turn, &s) == nil {
		return s
	}
	return ""
}

// RunningTurn reads ready's turn object, or nil.
func (m Message) RunningTurn() *TurnRef {
	var t TurnRef
	if len(m.Turn) == 0 || m.Turn[0] != '{' || json.Unmarshal(m.Turn, &t) != nil || t.Turn == "" {
		return nil
	}
	return &t
}

// ItemID reads input.taken's item as an id.
func (m Message) ItemID() string {
	var s string
	if json.Unmarshal(m.Item, &s) == nil {
		return s
	}
	return ""
}

// StrTurn makes the turn field from an id.
func StrTurn(id string) json.RawMessage {
	b, _ := json.Marshal(id)
	return b
}
