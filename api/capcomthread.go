package api

// --- The Capcom thread (capcom-service.md 7.4) ---

// CapcomItemKind is a thread item's kind (capcom-service.md 7.2). Open: a client shows an
// unknown kind as a quiet line.
type CapcomItemKind string

// CapcomAuthorKind says who wrote a thread item.
type CapcomAuthorKind string

// CapcomAuthor is a thread item's author.
type CapcomAuthor struct {
	Kind    CapcomAuthorKind `json:"kind"`
	Device  string           `json:"device,omitempty" doc:"user: the device id that sent it."`
	Address string           `json:"address,omitempty" doc:"agent: the sender's channel address."`
}

// CapcomToolCall is one call in a turn's folded tools line.
type CapcomToolCall struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// CapcomCard is a card item's content.
type CapcomCard struct {
	Type    string   `json:"type" doc:"approval, confirmation, handoff, calendar, reminder, proposal or memory."`
	Ref     string   `json:"ref,omitempty" doc:"approval: the request id, as approval.answer takes it. memory: the Logbook record, or the proposal while it waits for review."`
	Choices []string `json:"choices"`
}

// CapcomAnswer is a card's answer: the first wins.
type CapcomAnswer struct {
	Choice string `json:"choice" doc:"One of the card's choices, or expired."`
	By     string `json:"by,omitempty" doc:"The device that answered."`
	Time   string `json:"time" doc:"RFC 3339."`
}

// CapcomItem is one thread item, folded.
type CapcomItem struct {
	ID      string              `json:"id" doc:"Time ordered: a later item has a greater id."`
	Kind    CapcomItemKind      `json:"kind"`
	Time    string              `json:"time" doc:"RFC 3339."`
	Author  CapcomAuthor        `json:"author"`
	Text    string              `json:"text,omitempty"`
	Images  []string            `json:"images,omitempty" doc:"Paths on the home hab."`
	Context []CapcomContextItem `json:"context,omitempty" doc:"The attached context that went with a user message."`
	Turn    string              `json:"turn,omitempty"`
	Event   bool                `json:"event,omitempty" doc:"Written in an event turn: the app marks it wrote first."`
	Tools   []CapcomToolCall    `json:"tools,omitempty" doc:"tools: each call of the turn, in order."`
	Card    *CapcomCard         `json:"card,omitempty"`
	Answer  *CapcomAnswer       `json:"answer,omitempty"`
	Summary string              `json:"summary,omitempty" doc:"divider: the summary the fresh SDK session opened with."`
	Read    string              `json:"read,omitempty" doc:"user message: when the engine took it (RFC 3339). Absent: delivered, not read yet."`
}

// CapcomThreadPageParams asks for a page of the thread.
type CapcomThreadPageParams struct {
	Before string `json:"before,omitempty" doc:"An item id: items older than it. Absent: the newest."`
	Limit  int    `json:"limit,omitempty" doc:"At most this many, and never more than 200. 0: 200."`
}

// CapcomThreadPage is a page of the thread, newest first.
type CapcomThreadPage struct {
	Seq
	Items []CapcomItem `json:"items"`
	More  bool         `json:"more" doc:"Older items remain."`
}

// CapcomServiceParams names nothing: the call acts on this home hab's Capcom service.
type CapcomServiceParams struct{}

// CapcomSDKSession is one SDK session the thread used (capcom-service.md 3.4).
type CapcomSDKSession struct {
	ID    string `json:"id"`
	Start string `json:"start" doc:"RFC 3339."`
	Why   string `json:"why" doc:"first, restart, reseed or new."`
	End   string `json:"end,omitempty" doc:"RFC 3339. Absent: the current session."`
}

// CapcomServiceStatus is the Capcom service on this hab: the engine job's state, the engine,
// and the SDK sessions, for the session's details and a plain-terminal resume (11).
type CapcomServiceStatus struct {
	Seq
	State       string             `json:"state" doc:"off, running, paused or error."`
	Reason      string             `json:"reason,omitempty"`
	Session     string             `json:"session,omitempty" doc:"The service session's id."`
	Connected   bool               `json:"connected"`
	Engine      string             `json:"engine,omitempty"`
	Version     string             `json:"version,omitempty"`
	Turn        string             `json:"turn,omitempty" doc:"The running turn. Absent: idle."`
	Folder      string             `json:"folder" doc:"The engine's folder: a plain terminal resumes its SDK sessions from here."`
	SDKSessions []CapcomSDKSession `json:"sdk_sessions"`
	Sites       []string           `json:"sites,omitempty" doc:"The sites the user allowed with Allow this site (5.3): a web fetch on one needs no card. Hosts, lowercase, sorted."`
}

// CapcomSiteParams names one allowed site.
type CapcomSiteParams struct {
	Site string `json:"site" doc:"A host, as capcom.status lists it."`
}

// CapcomCardAnswerParams answers a card of the Capcom thread (capcom-service.md 7.4).
type CapcomCardAnswerParams struct {
	Item   string `json:"item" doc:"The card's item id."`
	Choice string `json:"choice" doc:"One of the card's choices. A memory line's undo deletes the capcom guide it names, or rejects the proposal while it waits for review (capcom-service.md 6.4)."`
}

// CapcomCardAnswered is a card's answer and what it did.
type CapcomCardAnswered struct {
	Seq
	Item CapcomItem `json:"item" doc:"The card with its answer."`
	Note string     `json:"note,omitempty" doc:"What the answer did, for the user to read: an undo of a record edited since it was noted deleted the edited record."`
}

// CapcomTyping is the data of capcom.typing.
type CapcomTyping struct {
	On bool `json:"on"`
}
