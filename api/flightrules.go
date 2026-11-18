package api

// --- Flight rules: the per-session context ledger (instructions-mcp-flight-rules.md 11) ---

// FlightRuleKind says what reached the agent. The server sends it, so the set is open: a
// client shows an unknown kind as given.
type FlightRuleKind string

const (
	FlightRuleInstruction FlightRuleKind = "instruction"
	FlightRuleMemory      FlightRuleKind = "memory"
	FlightRuleHook        FlightRuleKind = "hook"
	FlightRuleSkills      FlightRuleKind = "skills"
	FlightRuleSkill       FlightRuleKind = "skill"
	FlightRuleMCP         FlightRuleKind = "mcp"
	FlightRuleSystem      FlightRuleKind = "system"
)

// FlightRuleScope is the harness's own level for an entry. Open, like FlightRuleKind.
type FlightRuleScope string

const (
	FlightScopeManaged FlightRuleScope = "managed"
	FlightScopeUser    FlightRuleScope = "user"
	FlightScopeProject FlightRuleScope = "project"
	FlightScopeLocal   FlightRuleScope = "local"
	FlightScopeMemory  FlightRuleScope = "memory"
	FlightScopeSession FlightRuleScope = "session"
	FlightScopeSkill   FlightRuleScope = "skill"
	FlightScopeMCP     FlightRuleScope = "mcp"
)

// FlightRuleAttribution says how Flightdeck knows where an entry came from. Open, like
// FlightRuleKind.
type FlightRuleAttribution string

const (
	// FlightRecorded: the harness named the source in what it recorded.
	FlightRecorded FlightRuleAttribution = "recorded"
	// FlightMatched: Flightdeck found the file's text inside a recorded block.
	FlightMatched FlightRuleAttribution = "matched"
	// FlightComputed: Flightdeck ran the harness's discovery itself.
	FlightComputed FlightRuleAttribution = "computed"
	// FlightUnknown: what the harness recorded does not prove the source.
	FlightUnknown FlightRuleAttribution = "unknown"
)

type FlightRulesParams struct {
	Session string `json:"session"`
}

// FlightRule is one ledger entry: one text that reached the agent, from one source. A changed
// text from the same source is a new entry, never an overwrite.
type FlightRule struct {
	ID                string                `json:"id" doc:"Stable for the session: made from kind, source and digest. flightrules.text takes it."`
	Kind              FlightRuleKind        `json:"kind" doc:"instruction, memory, hook, skills, skill, mcp or system."`
	Scope             FlightRuleScope       `json:"scope,omitempty" doc:"The harness's level: managed, user, project, local, memory, session, skill or mcp. Absent when the record does not say."`
	Source            string                `json:"source" doc:"A file path, a hook name, a skill name, an MCP server name, or what the record is when nothing names a source."`
	Event             string                `json:"event,omitempty" doc:"A hook entry: the hook event, such as SessionStart or PostToolUse."`
	Path              string                `json:"path,omitempty" doc:"The file behind a source that is not itself a path, such as an invoked skill's file."`
	ConfiguredSources []string              `json:"configured_sources,omitempty" doc:"The user's source files from Flightdeck's config, when this is a file Flightdeck rendered and its digest header matches the render record."`
	Digest            string                `json:"digest,omitempty" doc:"Hex SHA-256 of the text that reached the agent. Absent for a placeholder."`
	Bytes             int64                 `json:"bytes" doc:"Size of that text in bytes."`
	FirstSeen         string                `json:"first_seen" doc:"RFC 3339 time of the record that first carried this text."`
	LastSeen          string                `json:"last_seen" doc:"RFC 3339 time of the latest record that carried it again, after a compaction or resume."`
	Count             int                   `json:"count,omitempty" doc:"How many records carried this entry. For a source whose text changes on every load (a hook that prints a clock), one entry stands for every load: digest and bytes are the latest text's. Absent from an older hab."`
	Gone              string                `json:"gone,omitempty" doc:"RFC 3339 time the harness recorded that this source left the context. Absent while it is in."`
	Reason            string                `json:"reason,omitempty" doc:"Why the harness loaded it, in the harness's words, such as session_start or compaction."`
	Attribution       FlightRuleAttribution `json:"attribution" doc:"recorded, matched, computed or unknown."`
	Note              string                `json:"note,omitempty" doc:"Why the attribution is unknown, or what else the record says, such as that only the start of a file was sent."`
	Stale             bool                  `json:"stale,omitempty" doc:"A file Flightdeck rendered, still in the context, whose loaded copy differs from the last render of the same target: the agent works from an older render. Absent from an older hab."`
}

// FlightRules answers flightrules.list: a session's ledger, oldest entry first.
type FlightRules struct {
	Seq
	Session  string       `json:"session"`
	Harness  string       `json:"harness,omitempty" doc:"The harness whose records built the ledger. Absent when the session has no agent session yet."`
	Entries  []FlightRule `json:"entries"`
	Stale    bool         `json:"stale,omitempty" doc:"An entry is stale: the session loaded a rendered file that a later render replaced. Absent from an older hab."`
	Warnings []string     `json:"warnings,omitempty" doc:"Records the reader could not read, or read as placeholders."`
}

type FlightRuleTextParams struct {
	Session string `json:"session"`
	ID      string `json:"id" doc:"An entry id from flightrules.list for this session."`
}

// FlightRuleText is the text of one ledger entry, read again from where the harness recorded it.
type FlightRuleText struct {
	Seq
	ID        string `json:"id"`
	Text      string `json:"text"`
	Bytes     int64  `json:"bytes" doc:"Size of the whole text in bytes."`
	Truncated bool   `json:"truncated,omitempty" doc:"The text is cut at 1 MiB, on a character boundary."`
}
