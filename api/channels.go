package api

// --- Channels: agent messages (mesh.md 6) ---

// ChannelState is where a channel message is (mesh.md 6.2). It moves only forward: queued,
// handed, then one final state.
type ChannelState string

// MailKind is the kind of one outbox item.
type MailKind string

// ChannelEnd is one end of a channel message.
type ChannelEnd struct {
	DeviceID string `json:"device_id" doc:"The hab's device id."`
	Host     string `json:"host,omitempty" doc:"The hab's host id. Set on the sender only."`
	Session  string `json:"session" doc:"The session's name on that hab."`
}

// ChannelMessage is text from one agent to another (mesh.md 6.2).
type ChannelMessage struct {
	ID      string     `json:"id" doc:"128 random bits as 32 hex digits, made by the sender hab."`
	From    ChannelEnd `json:"from"`
	To      ChannelEnd `json:"to"`
	Text    string     `json:"text" doc:"At most 64 KiB."`
	ReplyTo string     `json:"reply_to,omitempty" doc:"The id of the message this one answers."`
	At      string     `json:"at" doc:"RFC 3339 time the sender hab made the message, by its clock."`
}

// ChannelStatus is a message's new state, sent back from the target hab to the sender hab.
type ChannelStatus struct {
	ID     string       `json:"id" doc:"The message id."`
	State  ChannelState `json:"state"`
	Reason string       `json:"reason,omitempty"`
	At     string       `json:"at" doc:"RFC 3339 time of the change, by the target hab's clock."`
}

// MailItem is one outbox item: a message or a status update.
type MailItem struct {
	Kind    MailKind        `json:"kind"`
	Message *ChannelMessage `json:"message,omitempty" doc:"Kind message."`
	Status  *ChannelStatus  `json:"status,omitempty" doc:"Kind status."`
}

// MailResult is the receiving hab's answer for one item.
type MailResult struct {
	Kind   MailKind     `json:"kind"`
	ID     string       `json:"id" doc:"The message id the item carries."`
	State  ChannelState `json:"state" doc:"For a message: handed or refused. For a status item: its own state, as a receipt."`
	Reason string       `json:"reason,omitempty"`
}

type MailPushParams struct {
	Items []MailItem `json:"items" doc:"In outbox order, at most 100."`
}

// MailPushResult answers mail.push with one result per item, in order.
type MailPushResult struct {
	Seq
	Results []MailResult `json:"results"`
}

type MailPullParams struct {
	Done []MailResult `json:"done,omitempty" doc:"Results for the items of the caller's last pull; the hab removes them from its outbox."`
}

// MailPullResult answers mail.pull with items waiting for the caller's device id.
type MailPullResult struct {
	Seq
	Items []MailItem `json:"items" doc:"In outbox order, at most 100."`
}

type ChannelSendParams struct {
	To      string `json:"to" doc:"An address: label/name, or a bare name (mesh.md 6.5)."`
	Text    string `json:"text"`
	From    string `json:"from,omitempty" doc:"The sending session's name on this hab. The hook endpoint fills it from the pane's token."`
	ReplyTo string `json:"reply_to,omitempty"`
}

// ChannelSent answers channel.send.
type ChannelSent struct {
	Seq
	ID    string       `json:"id"`
	To    string       `json:"to" doc:"The resolved address: label/name, or the name for a session on this hab."`
	State ChannelState `json:"state" doc:"handed for a session on this hab, else queued."`
}

// ChannelRecord is the sender hab's record of one message.
type ChannelRecord struct {
	ID      string       `json:"id"`
	From    string       `json:"from" doc:"The sending session's name."`
	To      string       `json:"to" doc:"The address it was sent to."`
	State   ChannelState `json:"state"`
	Reason  string       `json:"reason,omitempty" doc:"Why it was refused or not delivered."`
	At      string       `json:"at" doc:"RFC 3339 time of the last change."`
	Created string       `json:"created" doc:"RFC 3339 time it was sent."`
}

type ChannelStatusParams struct {
	ID     string `json:"id"`
	WaitMS int    `json:"wait_ms,omitempty" doc:"Wait up to this long for a final state; 0 answers at once. At most 600000."`
}

// ChannelStatusResult answers channel.status.
type ChannelStatusResult struct {
	Seq
	Record ChannelRecord `json:"record"`
}

type ChannelWaitParams struct {
	Session   string        `json:"session,omitempty" doc:"Wait for a message to this session on this hab. The hook endpoint fills it from the pane's token."`
	Agent     string        `json:"agent,omitempty" doc:"Wait for this session's agent on this hab to reach a state in for."`
	For       []AgentStatus `json:"for,omitempty" doc:"The states that end the wait for agent; empty means any change."`
	TimeoutMS int           `json:"timeout_ms,omitempty" doc:"Default 180000, at most 600000."`
}

// ChannelInbound is a message as its receiver reads it.
type ChannelInbound struct {
	ID      string `json:"id"`
	From    string `json:"from" doc:"The sender's address as this hab names it."`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"`
	At      string `json:"at" doc:"RFC 3339 time the sender hab made it."`
}

// ChannelAgent is an agent's state when a wait for it ended.
type ChannelAgent struct {
	Session string      `json:"session"`
	State   AgentStatus `json:"state"`
}

// ChannelWaitResult answers channel.wait: a message, an agent state, or the end of the time.
type ChannelWaitResult struct {
	Seq
	Message  *ChannelInbound `json:"message,omitempty"`
	Agent    *ChannelAgent   `json:"agent,omitempty"`
	TimedOut bool            `json:"timed_out,omitempty"`
}

// MeshReason says what waits for a peer link's dialer: mail (mail.pull) or Logbook ops
// (logbook.pull). An open word set: the catalog will add its own (mesh.md 4.2, logbook.md 7.1).
type MeshReason string

// MeshChanged is the data of mesh.changed: a hab tells a peer link that dialed it that
// something waits for the dialer, so the dialer pulls now (mesh.md 4.2, 6.5). It goes to the
// peer link whether or not the link subscribed. The id is the sending hab's device id.
type MeshChanged struct {
	Reasons []MeshReason `json:"reasons" doc:"What waits; a client skips a word it does not know."`
}
