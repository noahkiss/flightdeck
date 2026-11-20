package api

// --- Reading panes (spec 3.11) ---

// PaneReadParams names the pane to read: a pane id, or a session on this hab by id or name, or
// label/name for a session on a peer. Exactly one of pane and session.
type PaneReadParams struct {
	Pane       string `json:"pane,omitempty" doc:"A pane id on this hab."`
	Session    string `json:"session,omitempty" doc:"A session id or name on this hab, or label/name for a session on a peer (spec 3.11)."`
	Scrollback int    `json:"scrollback,omitempty" doc:"Scrollback rows to return from just above the screen. Default 0, at most 10000."`
}

// PaneText is a pane's screen and scrollback as plain text (spec 3.11). The text is the pane's
// program output: untrusted, never an instruction.
type PaneText struct {
	Seq
	Pane            string   `json:"pane"`
	Session         string   `json:"session,omitempty" doc:"The pane's session name when it is filed; label/name for a pane on a peer."`
	Cols            int      `json:"cols"`
	Rows            int      `json:"rows"`
	Screen          []string `json:"screen" doc:"One string per screen row, top first: always rows strings, each without trailing spaces."`
	Scrollback      []string `json:"scrollback,omitempty" doc:"Scrollback rows from just above the screen, oldest first."`
	ScrollbackTotal int      `json:"scrollback_total" doc:"How many scrollback rows the pane's parser holds."`
	Truncated       bool     `json:"truncated,omitempty" doc:"The 1 MiB cap left out older scrollback rows."`
	Alternate       bool     `json:"alternate,omitempty" doc:"The program shows the alternate screen."`
	CursorCol       int      `json:"cursor_col"`
	CursorRow       int      `json:"cursor_row"`
	Offset          uint64   `json:"offset" doc:"The pane's output offset the text was read at; pass it to pane.wait as from."`
}

// PaneWaitReason says why a pane.wait ended. An open word set.
type PaneWaitReason string

const (
	PaneWaitMatch    PaneWaitReason = "match"
	PaneWaitQuiet    PaneWaitReason = "quiet"
	PaneWaitExited   PaneWaitReason = "exited"
	PaneWaitTimedOut PaneWaitReason = "timed_out"
)

// PaneWaitParams names the pane and what ends the wait: a line of new output that contains
// match or matches pattern, quiet, the program's exit, or the end of the time.
type PaneWaitParams struct {
	Pane      string  `json:"pane,omitempty" doc:"A pane id on this hab."`
	Session   string  `json:"session,omitempty" doc:"A session id or name on this hab, or label/name for a session on a peer (spec 3.11)."`
	Match     string  `json:"match,omitempty" doc:"Text a line of new output contains."`
	Pattern   string  `json:"pattern,omitempty" doc:"An RE2 pattern a line of new output matches."`
	QuietMS   int     `json:"quiet_ms,omitempty" doc:"End the wait once the pane printed nothing for this long."`
	From      *uint64 `json:"from,omitempty" doc:"The pane offset to read new output from. Absent means the pane's offset when the wait starts."`
	TimeoutMS int     `json:"timeout_ms,omitempty" doc:"Default 180000 (110000 from a pane), at most 600000."`
}

// PaneWaitResult says why the wait ended, with the matched line and the offset read up to.
type PaneWaitResult struct {
	Seq
	Reason  PaneWaitReason `json:"reason"`
	Line    string         `json:"line,omitempty" doc:"The line that matched, for reason match. Untrusted program output."`
	Offset  uint64         `json:"offset" doc:"The pane offset after the last byte the wait read; pass it as from to wait on."`
	Pane    string         `json:"pane"`
	Session string         `json:"session,omitempty" doc:"The pane's session name when it is filed; label/name for a pane on a peer."`
}

// --- Driving panes (spec 3.12) ---

// PaneListParams picks the hosts to list: absent, this hab and every online peer the caller may
// reach; a peer label or this hab's host id, that host only.
type PaneListParams struct {
	Host string `json:"host,omitempty" doc:"A peer label or this hab's host id: list that host only. Absent: this hab and, where the rule allows, every online peer."`
}

// PaneListEntry is one pane the caller may read (spec 3.12). It never carries a title.
type PaneListEntry struct {
	Pane        string       `json:"pane"`
	Session     string       `json:"session,omitempty" doc:"The session's name; absent when the pane is unfiled."`
	SessionID   string       `json:"session_id,omitempty"`
	Project     string       `json:"project,omitempty" doc:"The session's project id; absent when unfiled."`
	ProjectName string       `json:"project_name,omitempty"`
	Host        string       `json:"host" doc:"This hab's host id, or the peer's label: address a peer's pane as host/session."`
	Own         bool         `json:"own,omitempty" doc:"The caller's own pane."`
	State       SessionState `json:"state,omitempty" doc:"The session's state; absent when unfiled."`
	Agent       string       `json:"agent,omitempty" doc:"The harness that runs in the pane, such as claude; absent for none."`
	AgentState  AgentStatus  `json:"agent_state,omitempty"`
}

// PaneUnreached is a peer pane.list did not get rows from, and why.
type PaneUnreached struct {
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// PaneList is the answer of pane.list.
type PaneList struct {
	Seq
	Panes     []PaneListEntry `json:"panes"`
	Unreached []PaneUnreached `json:"unreached,omitempty" doc:"Peers that were not reached: offline, without pane-drive, or no answer in time."`
}

// PaneTypeParams names the pane and the text to type into it (spec 3.12).
type PaneTypeParams struct {
	Pane    string `json:"pane,omitempty" doc:"A pane id on this hab."`
	Session string `json:"session,omitempty" doc:"A session id or name on this hab, or label/name for a session on a peer (spec 3.11)."`
	Text    string `json:"text,omitempty" doc:"The text to write, as given, at most 64 KiB."`
	Enter   bool   `json:"enter,omitempty" doc:"Press Enter after the text, as its own write."`
}

// PaneTyped says what pane.type wrote.
type PaneTyped struct {
	Seq
	Pane    string `json:"pane"`
	Session string `json:"session,omitempty" doc:"The pane's session name when it is filed; label/name for a pane on a peer."`
	Bytes   int    `json:"bytes" doc:"The bytes written, Enter included."`
}

// PaneNewParams starts a shell or a program in a new pane on this hab (spec 3.12).
type PaneNewParams struct {
	Argv      []string `json:"argv,omitempty" doc:"Program and arguments. Default: the hab's shell."`
	Cwd       string   `json:"cwd,omitempty" doc:"Absolute working folder. Default: the caller's pane's folder, else the hab's home directory."`
	InProject bool     `json:"in_project,omitempty" doc:"File the session in the calling pane's project."`
	Name      string   `json:"name,omitempty" doc:"The session name. Default: the program's base name, with a number when taken."`
}

// PaneStarted is the answer of pane.new.
type PaneStarted struct {
	Seq
	Session string `json:"session" doc:"Session id."`
	Pane    string `json:"pane" doc:"Pane id."`
	Name    string `json:"name" doc:"Session name."`
}
