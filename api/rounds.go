package api

// --- Rounds: scheduled jobs (rounds.md 3, 7, 13) ---

// RoundMode says how a run reaches its session (rounds.md 5). The client sends it, so the set is
// closed there; a client reading a round maps an unknown word to "a round".
type RoundMode string

// RoundTriggerKind says what asks for a run (rounds.md 4). Closed when the client sends it.
type RoundTriggerKind string

// RoundCatchUp says what a round does with slots missed while the host slept (rounds.md 8.2).
type RoundCatchUp string

// RoundOnFinish says what happens to a run's session after a successful run (rounds.md 7.6).
type RoundOnFinish string

// RoundNotify says which run ends raise a notification (rounds.md 10.2).
type RoundNotify string

// RoundRunState is a run's state (rounds.md 7.1). The server sends it, so the set is open: a
// client shows an unknown word as given.
type RoundRunState string

// RoundTrigger is a round's one trigger.
type RoundTrigger struct {
	Kind  RoundTriggerKind `json:"kind" doc:"cron, once or manual. event triggers come in a later stage and are refused."`
	Cron  string           `json:"cron,omitempty" doc:"Kind cron: a 5-field cron expression."`
	Every string           `json:"every,omitempty" doc:"Kind cron, in a create or update only: a preset such as 5m, 1h or 1d, instead of cron. The hab stores the cron expression it compiles to, so a round read back has cron and no every."`
	Zone  string           `json:"zone,omitempty" doc:"Kind cron or once: the IANA time zone. Absent in a create: the hab's zone, stored by name."`
	At    string           `json:"at,omitempty" doc:"Kind once: the RFC 3339 instant. A past instant is refused."`
}

// RoundSpec is what a user or an agent writes of a round: create and update send it, and a
// round read back holds it with the defaults filled.
type RoundSpec struct {
	Name          string        `json:"name" doc:"Unique on the hab: 1 to 64 letters, digits, - and _."`
	Mode          RoundMode     `json:"mode" doc:"fresh, resume or fork. This hab runs fresh rounds with a command; the others are refused until their stage."`
	Trigger       RoundTrigger  `json:"trigger"`
	Prompt        string        `json:"prompt,omitempty" doc:"The text the agent gets each run, at most 64 KiB. Required unless command is set."`
	Harness       string        `json:"harness,omitempty" doc:"Fresh with a prompt: claude, codex or opencode."`
	Model         string        `json:"model,omitempty"`
	HarnessArgs   []string      `json:"harness_args,omitempty" doc:"Passed to the harness at start, as session.create does."`
	Command       []string      `json:"command,omitempty" doc:"Fresh only: a plain program, as an argv, instead of a harness and a prompt. Exit status 0 is a success."`
	Project       string        `json:"project,omitempty" doc:"Fresh: the project id. The run starts in the project's most recently used folder."`
	ProjectDir    string        `json:"project_dir,omitempty" doc:"In a create or update only, instead of project: an absolute folder; the hab finds or creates its project, as session.create does, and stores the project id."`
	ProjectRemote string        `json:"project_remote,omitempty" doc:"In a create or update, instead of project: a git remote; the hab takes its project with that remote. In a round read back: the project's remote, when it has one, so an exported round names its project on any host."`
	Cwd           string        `json:"cwd,omitempty" doc:"Fresh: an absolute folder to start in, instead of the project's most recent one."`
	Target        string        `json:"target,omitempty" doc:"Resume and fork: the target session id."`
	MaxRuns       int           `json:"max_runs,omitempty" doc:"The round completes after this many finished runs. Absent: no limit."`
	ExpiresAt     string        `json:"expires_at,omitempty" doc:"RFC 3339 time the round completes. A past time is refused."`
	TimeoutMS     int64         `json:"timeout_ms,omitempty" doc:"The longest a run may take. Absent: 60 minutes."`
	WaitMS        int64         `json:"wait_ms,omitempty" doc:"Resume only: how long a run waits for the session to be free. Absent: 10 minutes."`
	CatchUp       RoundCatchUp  `json:"catch_up,omitempty" doc:"once (the default) or skip."`
	OnFinish      RoundOnFinish `json:"on_finish,omitempty" doc:"Fresh and fork: close (the default) closes the run's pane after a success; keep leaves it."`
	Notify        RoundNotify   `json:"notify,omitempty" doc:"failures (the default), always or never."`
	Paused        bool          `json:"paused,omitempty" doc:"A paused round starts no runs. An update keeps the stored mark: round.pause and round.unpause change it."`
}

// Round is one round as the hab holds it: its definition and where its schedule stands.
type Round struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision" doc:"Raised by one on each change; starts at 1 when gff-hab starts. Compared only within an epoch (spec 3.5)."`
	RoundSpec
	CreatedBy string `json:"created_by,omitempty" doc:"The session that created the round, when an agent did from its pane. Absent for a user. Read only."`
	CreatedAt string `json:"created_at" doc:"RFC 3339."`
	UpdatedAt string `json:"updated_at" doc:"RFC 3339."`
	NextSlot  string `json:"next_slot,omitempty" doc:"RFC 3339 time of the next slot. Absent when there is none: manual, paused past it, completed."`
	LastSlot  string `json:"last_slot,omitempty" doc:"RFC 3339 time of the last slot served."`
	LastOK    string `json:"last_ok,omitempty" doc:"RFC 3339 time the last successful run started: the run's GFF_ROUND_LAST_OK."`
	// The counts of rounds.md 10.1.
	Failures        int       `json:"failures,omitempty" doc:"Failed or timed-out runs in a row."`
	Skips           int       `json:"skips,omitempty" doc:"Skipped runs in a row."`
	PausedReason    string    `json:"paused_reason,omitempty" doc:"Why the round paused itself. Absent for a pause by the user."`
	Completed       bool      `json:"completed,omitempty" doc:"The round will not run again until its trigger changes."`
	CompletedReason string    `json:"completed_reason,omitempty"`
	TriggerError    string    `json:"trigger_error,omitempty" doc:"The stored trigger no longer compiles on this host, such as a zone its zone database lacks. The round is not scheduled until an edit fixes it."`
	LastRun         *RoundRun `json:"last_run,omitempty" doc:"The newest run, in round.list and round.get."`
	Deleted         bool      `json:"deleted,omitempty" doc:"In a round.updated event only: the round was deleted. Its live run goes on to its end."`
}

// SessionRound marks a run session (rounds.md 7.2): Session.round.
type SessionRound struct {
	Round string `json:"round" doc:"The round id."`
	Run   string `json:"run" doc:"The run id."`
}

// RoundRun is one run's record (rounds.md 7.7).
type RoundRun struct {
	ID        string           `json:"id"`
	Number    int              `json:"number" doc:"The run number within its round, from 1."`
	Round     string           `json:"round" doc:"The round id."`
	Slot      string           `json:"slot,omitempty" doc:"RFC 3339 time of the slot the run serves. Absent for a run by hand."`
	Trigger   RoundTriggerKind `json:"trigger" doc:"What asked for the run: the round's trigger kind, or manual for a run by hand."`
	Missed    int              `json:"missed,omitempty" doc:"How many slots the run covers: 1 on time, more after a catch-up."`
	State     RoundRunState    `json:"state"`
	Reason    string           `json:"reason,omitempty"`
	Session   string           `json:"session,omitempty" doc:"The run session's id, once it exists."`
	Created   string           `json:"created" doc:"RFC 3339 time the run was made."`
	StartedAt string           `json:"started_at,omitempty" doc:"RFC 3339."`
	EndedAt   string           `json:"ended_at,omitempty" doc:"RFC 3339."`
	Result    string           `json:"result,omitempty" doc:"The first 4 KiB of the final agent message."`
}

// RoundRef names one round by id or by name; give one.
type RoundRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type RoundListParams struct{}

// RoundList answers round.list: every round on this hab, by name.
type RoundList struct {
	Seq
	Rounds []Round `json:"rounds"`
}

// RoundResult answers round.get, round.update, round.pause and round.unpause.
type RoundResult struct {
	Seq
	Round Round `json:"round"`
}

type RoundCreateParams struct {
	Round  RoundSpec `json:"round"`
	RunNow bool      `json:"run_now,omitempty" doc:"Also start one run now. It does not move the next slot."`
}

// RoundCreated answers round.create.
type RoundCreated struct {
	Seq
	Round Round     `json:"round"`
	Run   *RoundRun `json:"run,omitempty" doc:"With run_now: the run it started."`
}

type RoundUpdateParams struct {
	ID       string    `json:"id"`
	Round    RoundSpec `json:"round" doc:"The whole new definition. A field left out takes its default; round.get gives the stored one to edit."`
	Revision uint64    `json:"revision,omitempty" doc:"The round's revision the client last saw. Another revision refuses the call with conflict. Absent: no check."`
}

type RoundRunsParams struct {
	RoundRef
	Before int `json:"before,omitempty" doc:"Return runs with a number below this. Absent: from the newest."`
	Limit  int `json:"limit,omitempty" doc:"At most this many runs; default 50, at most 200."`
}

// RoundRuns answers round.runs: a page of a round's runs, newest first.
type RoundRuns struct {
	Seq
	Runs []RoundRun `json:"runs"`
	More bool       `json:"more,omitempty" doc:"Older runs remain."`
}

// RoundRunResult answers round.run.
type RoundRunResult struct {
	Seq
	Run RoundRun `json:"run"`
}
