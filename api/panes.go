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
