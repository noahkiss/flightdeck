package api

// --- Diagnostics: friction notes (diagnostics.md 9) ---

// FrictionParams is a friction note: the user's own words about a moment where Flightdeck fell
// short. The hab stores it with a tail of its merged logs; the text never enters a log line.
type FrictionParams struct {
	Text    string `json:"text" doc:"The user's own words, 1 to 2,000 characters."`
	Session string `json:"session,omitempty" doc:"The session the user was in, when there is one."`
	Pane    string `json:"pane,omitempty" doc:"The pane the user was in, when there is one."`
	Blocker bool   `json:"blocker,omitempty" doc:"The work could not be done in Flightdeck, so the user went back to another tool. Absent or false: a preference."`
}

// FrictionNoted answers diag.friction: the stored note's trace, which the client shows.
type FrictionNoted struct {
	Seq
	Trace string `json:"trace" doc:"The note's trace, 12 Crockford base32 characters; gff friction show takes it."`
}

// --- Diagnostics: debug lines (diagnostics.md 4.5) ---

// DebugParams turns gff-hab's debug lines on until a time, or off. The window survives a
// restart and a crash loop, and ends by itself.
type DebugParams struct {
	Until string `json:"until,omitempty" doc:"RFC 3339 time the debug lines stop, at most 24 hours ahead. Absent, or a time already past: debug off."`
}

// DebugState answers diag.debug: the debug window now.
type DebugState struct {
	Seq
	Until string `json:"until,omitempty" doc:"RFC 3339 time the debug lines stop. Absent: no window is open."`
	Env   bool   `json:"env,omitempty" doc:"GFF_HAB_DEBUG is set in gff-hab's environment, so the debug lines stay on with no time limit."`
}
