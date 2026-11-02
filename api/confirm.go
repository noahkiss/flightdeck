package api

// --- Confirmations: starts and set-ups an agent asks for (capcom.md 6.1, mesh.md 7) ---

// ConfirmKind is what a confirmation would do. The client sends it in confirm.request, so the
// set is closed there; the server sends it in a confirmation, where a client maps an unknown
// word to "an action".
type ConfirmKind string

// ConfirmState is where a confirmation is. The server sends it, so the set is open: a client
// shows an unknown word as given, and treats it as closed.
type ConfirmState string

// ConfirmChoice is the user's answer to a confirmation. A closed set.
type ConfirmChoice string

// ConfirmStart is the session start a confirmation would run (capcom.md 3.3, 6.3).
type ConfirmStart struct {
	Host    string `json:"host" doc:"The target hab: a peer label from the peer list, or this hab's host id. In confirm.request an empty host means this hab."`
	Folder  string `json:"folder" doc:"The absolute project folder on the target host. The session starts there, in its project."`
	Project string `json:"project,omitempty" doc:"The project's name, from the catalog. Filled by the hab."`
	Branch  string `json:"branch,omitempty" doc:"The folder's branch, from the catalog. Filled by the hab."`
	Harness string `json:"harness" doc:"The harness to start: claude, codex or opencode."`
	Model   string `json:"model,omitempty" doc:"A model for the harness, as the harness names it."`
	Name    string `json:"name,omitempty" doc:"The new session's name. Absent: the target hab picks one."`
	Seed    string `json:"seed,omitempty" doc:"The first message of the new session, at most 8000 characters. The target hab delivers it once the agent can take it, marked as from the requesting session on the owner's behalf."`
}

// ConfirmSetup is the project set-up a confirmation would run (mesh.md 7, set up elsewhere).
type ConfirmSetup struct {
	Host       string `json:"host" doc:"The target hab: a peer label, or this hab's host id."`
	Repository string `json:"repository" doc:"The repository to clone or fetch."`
	Branch     string `json:"branch,omitempty" doc:"The ref to check out."`
	Bootstrap  string `json:"bootstrap,omitempty" doc:"The bootstrap command the project declares. Filled by the hab."`
}

// ConfirmResult is what a finished start made.
type ConfirmResult struct {
	Host        string `json:"host" doc:"The target hab, as this hab labels it."`
	Session     string `json:"session" doc:"The new session's id on the target hab."`
	Name        string `json:"name,omitempty" doc:"The new session's name."`
	Pane        string `json:"pane" doc:"The new session's pane id."`
	SeedProblem string `json:"seed_problem,omitempty" doc:"Why the seed could not be queued; the session runs without it."`
}

// Confirmation is one start or set-up an agent asked for: an open question to the user, or,
// with confirm.start_elsewhere off, a record of what ran (capcom.md 6.1).
type Confirmation struct {
	ID         string         `json:"id" doc:"128 random bits as 32 hex digits. The idempotency key of the start's session.create on the target hab."`
	Kind       ConfirmKind    `json:"kind"`
	State      ConfirmState   `json:"state" doc:"open: waits for the user. running: confirmed, the hab runs it. done, failed, cancelled, expired, unknown: closed. unknown: the hab lost the answer, such as a peer link that dropped mid-start."`
	Record     bool           `json:"record,omitempty" doc:"The action ran at once, with no question: confirm.start_elsewhere is off, or the start is on the agent's own host. The card is a record with no buttons."`
	From       string         `json:"from,omitempty" doc:"The requesting session's name on this hab. Absent for a request from outside a pane."`
	Start      *ConfirmStart  `json:"start,omitempty" doc:"Kind start: the start as it will run, or ran."`
	Setup      *ConfirmSetup  `json:"setup,omitempty" doc:"Kind setup: the set-up as it will run."`
	Created    string         `json:"created" doc:"RFC 3339 time the hab took the request."`
	ExpiresAt  string         `json:"expires_at,omitempty" doc:"RFC 3339 time an open confirmation expires, 60 minutes after it opened. An expired one never runs."`
	Closed     string         `json:"closed,omitempty" doc:"RFC 3339 time it closed."`
	AnsweredBy string         `json:"answered_by,omitempty" doc:"The client id that answered it."`
	Edited     bool           `json:"edited,omitempty" doc:"The user changed the action before confirming it."`
	Result     *ConfirmResult `json:"result,omitempty" doc:"State done: what the start made."`
	Reason     string         `json:"reason,omitempty" doc:"States failed and unknown: why, with the target hab's error. State expired or cancelled: a short note."`
}

type ConfirmRequestParams struct {
	Kind   ConfirmKind   `json:"kind"`
	Start  *ConfirmStart `json:"start,omitempty" doc:"Kind start."`
	Setup  *ConfirmSetup `json:"setup,omitempty" doc:"Kind setup."`
	From   string        `json:"from,omitempty" doc:"The requesting session's name on this hab. The hook endpoint fills it from the pane's token."`
	WaitMS int           `json:"wait_ms,omitempty" doc:"Wait up to this long for the answer; 0 answers at once. At most 120000. With no answer by then, the answer reaches the requesting session later as a message from Flightdeck."`
}

// ConfirmRequested answers confirm.request: the confirmation as it stands.
type ConfirmRequested struct {
	Seq
	Confirmation Confirmation `json:"confirmation"`
}

// ConfirmEdits are the user's changes to a start before confirming it. An empty field keeps
// the value.
type ConfirmEdits struct {
	Host    string `json:"host,omitempty" doc:"Another target hab. Give folder with it."`
	Folder  string `json:"folder,omitempty"`
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	Seed    string `json:"seed,omitempty" doc:"The edited seed, at most 8000 characters."`
}

type ConfirmAnswerParams struct {
	ID     string        `json:"id"`
	Choice ConfirmChoice `json:"choice"`
	Edits  *ConfirmEdits `json:"edits,omitempty" doc:"With choice confirm: the user's changes."`
}

// ConfirmationResult answers confirm.answer and confirm.get.
type ConfirmationResult struct {
	Seq
	Confirmation Confirmation `json:"confirmation"`
}

type ConfirmListParams struct {
	All bool `json:"all,omitempty" doc:"Also list closed confirmations the hab still keeps, newest first after the open ones."`
}

// ConfirmList answers confirm.list.
type ConfirmList struct {
	Seq
	Confirmations []Confirmation `json:"confirmations"`
}

type ConfirmGetParams struct {
	ID string `json:"id"`
}
