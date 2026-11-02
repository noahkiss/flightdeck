# Rounds: behaviour spec

Status: accepted for build. Part 13. Written 2026-11-01; questions settled and build stages
planned 2026-11-01 (section 12, section 14).

A round is a scheduled agent job. Each run is an ordinary visible session: you can watch it,
type into it and take it over. This spec covers the job definition, triggers, session modes,
the lock that keeps a run out of a session you are using, catch-up after sleep, where rounds run,
failure and notification behaviour, and the build stages. It
describes behaviour in our own words and copies no code from the reference tools.

Rules this spec works within (owner decisions in the master plan):

- Rounds are a product feature (Part 13), built after core.
- Each run is a visible session. Nothing runs headless and out of sight.
- A laptop that sleeps is normal. A round catches up once at wake, never once per missed slot.
- No hub and no leader election. One machine alone runs its own rounds (`mesh.md`).
- Notifications go through Flightdeck's own path (`app.md` 8). No outside service.
- The bar is Paseo, herdr and Rex (`landscape.md` 2.14). We match Paseo's two kinds of
  schedule and add only cheap guards.
- A confirmation that adds friction gets a setting to turn it off.

Sections that settle a question end with a **Decision** line and its reason. The three
user-visible questions were settled with defaults under the owner's power-user rule; they are in
[section 12](#12-settled-questions).

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [The round definition](#3-the-round-definition)
4. [Triggers](#4-triggers)
5. [Session modes](#5-session-modes)
6. [The lock: never type into a session in use](#6-the-lock-never-type-into-a-session-in-use)
7. [A run from start to end](#7-a-run-from-start-to-end)
8. [Sleep, wake and catch-up](#8-sleep-wake-and-catch-up)
9. [Where rounds run](#9-where-rounds-run)
10. [Failure and notifications](#10-failure-and-notifications)
11. [Edge cases](#11-edge-cases)
12. [Settled questions](#12-settled-questions)
13. [API and CLI additions](#13-api-and-cli-additions)
14. [Build stages](#14-build-stages)
15. [Sources](#15-sources)

---

## 1. References

### 1.1 Paseo schedules (the bar)

Read from its public docs and its schedule service (repository HEAD, 2026-11-01).

- **Two kinds on one engine.** A *schedule* starts a fresh agent each run. A *heartbeat* sends a
  recurring prompt into one existing agent, so the same conversation continues.
- **Cadence.** A 5-field cron expression with an optional IANA time zone; UTC by default. Presets
  such as `5m` compile to cron; there is no rolling interval anchored to creation time. The next
  run is stored as a UTC instant, so local times hold across daylight saving changes.
- **Limits.** `max-runs`, an expiry time, and `run-now` for one run at creation.
- **Missed slots.** A 1 s tick runs a schedule whose next run is due. After the run it moves the
  next run past now, so a long sleep gives one run, not one per slot. After a daemon restart it
  skips overdue slots and marks a run that was in flight as failed.
- **Overlap.** A schedule never runs twice at once.
- **Busy target.** A heartbeat whose agent already has a turn running fails that run. A run that
  stops on a permission request counts as failed.
- **Target gone.** A deleted agent or a missing folder completes the schedule instead of retrying.
- **Clean-up.** "Archive on finish" is on by default: a schedule's run workspace and agent are
  archived when the run ends. The app can turn it off.
- **Surfaces.** An app view, chat (the agent creates it through MCP), a CLI, and MCP. A heartbeat
  has a deliberately small surface: create, delete, and change the cadence only, "so a heartbeat
  cannot silently become a different job".

What we take: both kinds, cron with a zone, presets that compile to cron, run limits, run once
on wake, no overlap, target gone ends the round, clean-up on finish. What we change: a hab restart
does not skip a due slot (our hab restarts on every update, 8.4); a busy target waits a short
while before it skips (6.3); a run blocked on a permission stays a live, answerable session
(7.4).

### 1.2 herdr and Rex

- herdr has no scheduler.
- Rex has Lua timers (`after`, `every`) inside its scripts. They run in the server process and
  have no catch-up rules we could find.

### 1.3 System schedulers

| Tool | Missed while asleep or off | Notes |
|---|---|---|
| cron | Lost | anacron runs a daily or weekly job once after boot when its period passed |
| launchd `StartCalendarInterval` | Asleep: runs once at wake, missed slots coalesced. Off: lost | A model for our wake rule |
| systemd timers | `Persistent=true` runs a missed calendar slot once at the next start | `RandomizedDelaySec` spreads load |
| GitHub schedules | Lost; runs may be late or dropped under load | UTC only, 5-minute floor, off after 60 days without repo activity |

The common shape: missed slots are coalesced into one run, or dropped. Nothing replays each slot.

### 1.4 An earlier private scheduler

The plan also studied a personal scheduler: one timer tick per machine, a cursor per job, and
catch-up once. We take the three ideas, not the tool. Nothing in Flightdeck reads its files or
runs beside it.

---

## 2. Model

| Thing | What it is | Identity |
|---|---|---|
| round | A job definition: what to run, where, when, with which limits | Random id; a name unique on its hab |
| runner | The one hab that owns a round and starts its runs | The hab's device id (`mesh.md` 2) |
| slot | One time the trigger asks for a run | A UTC instant |
| run | One attempt to serve a slot, an event or a manual request | Random id, plus a run number per round |
| run session | The session a run starts or uses | An ordinary session id (`hab-logic-api.md` 2.2) |
| target | For `resume` and `fork`: the existing session a round works on | Session id |

Words. One word per concept:

- **round**: the definition. Never "schedule" or "job" in the product.
- **run**: one execution of a round.
- **mode**: `fresh`, `resume` or `fork` (5).
- **trigger**: what asks for a run: `cron`, `once`, `event` or `manual` (4).
- **take over**: the user types into a run session; the round stops treating it as automatic
  (7.5).

Decision: landscape idea 4.6 ("heartbeats") is `resume` mode, not a new word. Reason: "heartbeat"
already means the API's keepalive notification (`hab-logic-api.md` 3.5). Two meanings for one
word would break the one-word rule.

---

## 3. The round definition

| Field | Required | Meaning |
|---|---|---|
| `name` | Yes | Unique on the runner. Letters, digits, `-` and `_` |
| `mode` | Yes | `fresh`, `resume` or `fork` (5) |
| `trigger` | Yes | One trigger (4) |
| `prompt` | Yes, unless `command` | Text the agent receives each run |
| `harness` | `fresh` with a prompt | `claude`, `codex`, `opencode`, or a later adapter |
| `model`, `harness_args` | No | Passed to the harness at start, as `session.create` does. Permission modes go here; the hab adds none of its own |
| `command` | No | `fresh` only: a plain program (`argv`) instead of a harness and a prompt. Success is exit status 0 |
| `project` | `fresh` | The project, by id. The folder is the project's most recent folder on the runner (`hab-logic-api.md` 3.9, project folders) |
| `cwd` | No | A folder inside the project, instead of its most recent one |
| `target` | `resume`, `fork` | The session to resume or fork |
| `max_runs` | No | The round completes after this many finished runs |
| `expires_at` | No | The round completes at this time |
| `timeout` | No | Longest run, default 60 minutes (7.3) |
| `wait` | No | `resume` only: how long a run may wait for the lock (6.3), default 10 minutes |
| `catch_up` | No | `once` (default) or `skip` (8.2) |
| `on_finish` | No | `close` (default) or `keep`, for `fresh` and `fork` (7.6, Q2) |
| `notify` | No | `failures` (default), `always` or `never` (10.2) |
| `paused` | No | A paused round keeps its definition and starts no runs |
| `created_by` | Set by the hab | The session that created the round, when an agent did; absent for a user. Read only |

Rules:

- **Where rounds are stored.** In the runner hab's state directory, saved on every change, so a
  restart loses nothing. They are edited through the API: the app, `gff round`, and the agent
  kit's tools all call the same methods (13).
- **A file for version control.** `gff round export` prints a hab's rounds as TOML.
  `gff round apply <file>` makes the hab's rounds match the file, after it prints the diff and
  the user confirms (`-yes` skips the prompt). The file names projects by their remote, so it
  works on any host that has them.
- **Secrets.** A round holds no secrets. The run inherits the runner hab's environment, as any
  session does. A secret the job needs comes from the harness's own login or the host's
  environment.
- **Edits.** An edit takes effect from the next run. A run already in flight keeps the values it
  started with. Changing the trigger recomputes the next slot from now. A changed trigger also
  re-arms a completed round (a spent `once` given a new instant, a round past `max_runs` or
  `expires_at`): it clears the completed mark, and the round runs at its new next slot. An edit
  that keeps the trigger leaves a completed round completed.

- **Field rules (stage 1).** A name is at most 64 characters. `harness`, `project` and `cwd`
  are for `fresh` rounds only: a `resume` or `fork` run uses its target's harness and folder.
  `model` and `harness_args` apply in every mode that starts a harness. A `command` round takes
  no prompt, harness, model or harness arguments. An `expires_at` in the past is refused, as a
  past `once` instant is. Both checks apply at creation and when the value changes, so an edit
  to another field never fails on a time that has passed since.

Decision: rounds live in the runner's state and are edited by the API, with export and apply for
a file. Reason: the app and agents must create and edit rounds, as in Paseo, and a store the hab
writes has no merge problem. Export and apply keep the "rounds in a config file" option open for
users who want it in git.

---

## 4. Triggers

| Trigger | Meaning |
|---|---|
| `cron` | A 5-field cron expression and an IANA time zone |
| `once` | One instant. The round completes after that run |
| `event` | A hab event that matches a filter (4.3). Later stage |
| `manual` | Only `gff round run`, the app's Run now, or the API. Any round can also be run by hand |

### 4.1 Cron

- **Expression.** Standard 5 fields: minute, hour, day of month, month, day of week. Lists,
  ranges, steps and the names `MON` and `JAN` are accepted, in any case. Day of week 7 is Sunday,
  as 0 is. When both day fields are restricted, a day matches when either matches; when one
  starts with `*`, both must match (classic cron). Seconds are refused, and so is every `@` form
  (`@reboot`, `@daily`): a preset covers the short forms. `L`, `W`, `#` and `?` are refused.
- **Presets.** `--every 5m`, `1h`, `1d` and the like compile to cron, as in Paseo: `5m` is
  `*/5 * * * *`. A preset that cron cannot express (`7m` does not divide the hour) is refused
  with the nearest presets that work. There is no rolling interval anchored to creation time.
  The presets are `1m`, `2m`, `5m`, `10m`, `15m`, `20m`, `30m`, `1h`, `2h`, `3h`, `4h`, `6h`,
  `8h`, `12h` and `1d` (midnight in the round's zone). An equal span in another unit works
  (`60m` is `1h`); any other span is refused, naming the presets on either side (`7m`: `5m`,
  `10m`).
- **Time zone.** Stored with the round. The default is the runner host's zone at creation time,
  written out as an IANA name, so a laptop that travels keeps the time the user meant. The UI
  shows the next run in the viewer's zone and the round's zone.
- **Daylight saving.** The next slot is computed in the round's zone and stored as a UTC instant.
  A local time that does not exist (the hour skipped in spring) runs at the first valid minute
  after it. A local time that happens twice (the hour repeated in autumn) runs once, at its first
  occurrence. The rule holds for every round: an hourly or `*/5` round does not run again in the
  repeated hour, and several slots in a skipped hour make one run at its end. A zone that skips
  a whole day follows the same rule.
- **Floor.** One minute, the cron granularity. Paseo allows the same.
- **Run on create.** `--run-now` starts one run at creation, as Paseo's flag does. It does not
  move the next slot.

Decision: the default zone is the runner's zone, stored by name. Reason: users write "9 am" in
their own time; Paseo's UTC default surprises everyone who is not in UTC. Storing the name keeps a
travelling laptop's rounds on home time until the user changes them.

### 4.2 Once

- One instant, in the round's zone. Past instants are refused at creation.
- An instant missed while the runner slept runs at wake (8.2), unless `catch_up = skip`.

### 4.3 Events (later stage)

Events come from the runner's own hab only. No hab triggers a round on another hab.

| Event | Filter | Example |
|---|---|---|
| `project.updated` | Project; which field changed: `head`, `branch`, `behind` | Review each new commit pulled into a branch |
| `agent.state` | Project or session; the new state: `done`, `error` | Summarise when any agent in a project finishes |
| `command.updated` | Session; command ended with a non-zero status | Explain a failed build in the console |
| `hab.woke` | None | A morning sweep after the laptop opens |

- **Debounce.** Events that match within 30 s make one run.
- **Cooldown.** At least 5 minutes between event runs of one round, by default. Events in the
  cooldown are coalesced into one run at its end.
- **No loops.** An event caused by the round's own run session never triggers that round.
- **No webhooks.** The hab listens on no TCP port (`hab-logic-api.md` 4). An outside system
  triggers a round with `gff-hab api call round.run` over SSH.

Decision: events in a later stage, with this fixed list. Reason: Paseo's event triggers live in its
paid Hub; cron covers the bar. The list uses events the hab already sends, so each is cheap.

---

## 5. Session modes

| Mode | What a run does | Native call |
|---|---|---|
| `fresh` | Starts a new session in the project with the harness, the model and the prompt. Or runs `command` | `session.create` |
| `resume` | Sends the prompt into the target session. A live target gets it under the lock (6). An exited target is resumed first, then gets it | `session.send`; `session.resume` when exited |
| `fork` | Starts a new session that branches from the target's conversation, then sends the prompt. The target is untouched | Claude Code `--resume <id> --fork-session`; Codex `codex fork <id>`; opencode *(unverified)* |

- **`fresh`** suits daily triage, reports and sweeps: each run starts clean (Paseo's schedule).
- **`resume`** suits "check the deploy every hour in this conversation": context carries over
  (Paseo's heartbeat, landscape idea 4.6).
- **`fork`** suits "each morning, start from the planning conversation and do the next step,
  without growing that conversation".
- **Adapters say what they can do.** Fork needs a harness that forks natively. A round whose mode
  the target's adapter cannot serve is refused at creation, with the reason.
- **One owner per session.** No mode starts a second process on a live session's id
  (`harness-adapters.md`, rule 1). `fork` makes a new session id; `resume` of an exited session
  starts the harness again in a new pane, as `session.resume` does.
- **The prompt is marked.** In `resume` and `fork`, the prompt arrives in a short envelope: it
  says the text is from round `<name>`, run `<n>`, for slot `<time>`. Part 4 owns the exact text,
  as for agent messages. The chat view marks it as from Flightdeck.

### 5.1 What a run's agent can read

The run session's environment carries these variables, beside the `GFF_HAB` contract
(`harness-adapters.md`, environment contract):

| Variable | Value |
|---|---|
| `GFF_ROUND` | The round id |
| `GFF_ROUND_RUN` | The run id |
| `GFF_ROUND_SLOT` | The slot this run serves, RFC 3339; absent for a manual or event run |
| `GFF_ROUND_LAST_OK` | When the last successful run started, RFC 3339; absent before the first |

`GFF_ROUND_LAST_OK` is the round's cursor. A prompt can say "triage what changed since
`$GFF_ROUND_LAST_OK`", so a run after a long sleep covers the whole gap and a repeated run does
no harm. In `resume` mode the variables are in the envelope, because a live session's environment
cannot change.

Decision: give the agent the last success time, not a stored cursor of its own. Reason: a time
covers most sweeps, needs no new storage, and makes catch-up once safe: one run sees every missed
change.

---

## 6. The lock: never type into a session in use

The lock matters in `resume` mode, the only mode that writes into an existing live session.

### 6.1 When a session is in use

A live target is in use when any of these hold:

| Signal | Source |
|---|---|
| The agent is `working` or `blocked`, or has an open permission request | Agent state (`harness-adapters.md`, input delivery) |
| A client typed into the pane in the last 2 minutes | `pane.input` and `session.send` from any client |
| A client reported the session in focus in an active window in the last 60 s | `session.focus` (`app.md` 8.3) |
| Another round's prompt is still starting its turn | The one-send-at-a-time rule (`harness-adapters.md`, built choices) |

### 6.2 What the lock does

- **A run never writes while the target is in use.** It waits.
- **It writes only through `session.send`.** So every input delivery rule applies too: paste,
  then Enter; never into a live dialog.
- **It never answers a dialog.** A `blocked` target stays blocked.

### 6.3 Waiting, then skipping

- A run waits up to `wait` (default 10 minutes) for a moment when the target is not in use.
- If the moment never comes, the run ends as `skipped` with the reason ("in use", "working",
  "blocked"). A skip is not a failure (10.1).
- At most one run per round waits. A slot that comes due while a run waits is coalesced into it.
- The next slot is unaffected.

Decision: wait 10 minutes, then skip. Reason: Paseo fails a heartbeat on a busy agent at once; a
short wait catches the common case of a turn that is about to end, and skipping is the safe end
state. The owner's rule wins over the bar here because typing into a session in use is the one
thing this feature must never do.

---

## 7. A run from start to end

### 7.1 States

| State | Meaning |
|---|---|
| `queued` | Due, waiting for a run slot on the host (7.2) or the wake settle (8.3) |
| `waiting` | `resume` only: waiting for the lock (6.3) |
| `running` | The session is live and the agent works on the run's turn |
| `blocked` | The agent waits for the user: a permission, a question or a sign-in |
| `succeeded` | The turn ended `done`, or `command` exited 0 |
| `failed` | Spawn failed, the turn ended `error`, the harness exited before the turn ended, or `command` exited non-zero |
| `timed_out` | The run passed `timeout` |
| `skipped` | The lock never cleared, or the project folder is missing for one run |
| `taken_over` | The user typed into the run session (7.5) |

### 7.2 Start

- **One run per round at a time.** A slot that comes due while a run is live is coalesced into
  the next run after it ends, at most one.
- **Host limit.** At most 2 runs start at once per host (`rounds.max_running` in the hab's
  config). Others wait as `queued`, oldest slot first; equal slots go by when the run was made.
  Built rule: every live run that is not `queued` holds a place, `waiting` and `blocked`
  included. Reason: a blocked run still holds a session and an agent.
- **The scheduler owns `queued`.** Only the scheduler starts a queued run. A run update from
  outside (the run session, stage 3) cannot move a run out of `queued`, or into it.
- **Start returns at the spawn.** The scheduler saves the run as `running`, then asks the run
  starter for the session. The starter returns once the session exists, with its id, or with
  the error that fails the run. It never waits for `idle` or the turn: readiness, `waiting`,
  `blocked` and the end come back later as run updates.
- **Session name.** `fresh` and `fork` sessions are named after the round, with the hab's usual
  `-2`, `-3` suffix rule. The session carries a `round` field (round id and run id) so every client
  can badge and filter it.
- **Readiness.** The prompt goes in as the first message once the adapter reports `idle`, under
  the input delivery rules. A harness that never reaches `idle` within 2 minutes fails the run.

### 7.3 End

- **A harness run ends** at the first `session.turn_end` after the run's prompt was delivered and
  read back. `done` is a success; `error` is a failure.
- **A command run ends** when the program exits.
- **Timeout.** At `timeout` the hab interrupts the turn (`session.interrupt`) and marks the run
  `timed_out`. A command run gets a hang-up, as `pane.close` does.
- **Result.** The run keeps the final agent message (first 4 KiB), the session id and the
  times. The full conversation stays in the harness's own transcript.

### 7.4 Blocked runs

- A run that stops on a permission, question or sign-in becomes `blocked`. It is a normal session
  with a normal request: the usual approval card and push go out (`app.md` 7, 8). The round adds
  no second notification.
- The timeout keeps counting. A run still blocked at its timeout ends as `timed_out`; the request
  stays open in the session for the user to answer later.
- Rounds never answer permissions. A round that should run without prompts says so in
  `harness_args`, with the harness's own permission mode, chosen by the user.

Decision: a blocked run stays live and answerable. Reason: Paseo fails such a run, but our runs
are visible sessions; an approval from the phone lets the job finish, which is the point of
running it where the user can see it.

### 7.5 Take over

- When any client types into a run session, or sends it a message, the run ends as `taken_over`.
- From then on the session is the user's: the round never closes it, never times it out and never
  sends it another prompt from this run.
- A taken-over run counts as finished for `max_runs`. It is neither a success nor a failure.
- In `resume` mode the target is already the user's session; typing there after the prompt went in
  marks the run `taken_over` too, and nothing else changes.
- Built (stage 3): a client's `pane.input` or `session.send` takes the run over only once the
  input reached the pane or the adapter delivered the message; refused input or a refused send
  takes nothing over. A client's `pane.close` takes the run over before the close, so the
  hang-up's exit does not end the run as failed. The hab's own closes (a timeout,
  `on_finish close`) take nothing over.

### 7.6 After the run

- **`on_finish = close`.** After a `succeeded` run, the hab closes the run session's pane
  (`pane.close`). The session record stays as exited, so the conversation can be resumed from the
  app or a plain terminal, and the transcript stays where the harness wrote it.
- **Never closed:** a run that failed, timed out, is blocked, or was taken over. The user should
  see what happened.
- **`on_finish = keep`.** The session stays open at the harness's prompt until the user closes it.
- **Retention.** Each round keeps its last 200 runs or 90 days, whichever is more, up to a hard
  cap of 2,000 ended runs per round; past the cap the oldest ended runs go first. Reason for the
  cap: the rounds store saves its whole file on every change (7.7), and a 1-minute round would
  otherwise keep about 130,000 runs inside 90 days, a file of tens of megabytes rewritten
  several times a minute. Exited run sessions older than that are removed from the structure;
  their transcripts are not touched.
  A run's age counts from when it was made (`created`). A run that has not ended is never
  pruned. Deleting a round drops its ended runs; a live run stays until it ends, and the next
  prune drops it.
- `resume` mode never closes its target.

### 7.7 Where round and run state lives, and the Logbook seam

The plan has rounds use Logbook (Part 12) for job state and results. Logbook is built after
rounds, so rounds keep their own state now.

- **Own store.** `<state>/rounds/state.json` holds every round, its next slot, its last served
  slot, its failure and skip counts, and its run records. It is saved whole through a synced
  temporary file and one rename, as the channels state is (`mesh.md` 6.5). A file that does not
  parse, or that names no version (`null` included), is set aside as
  `state.json.bad-<unix seconds>` (with `-2`, `-3` when that name is taken; an earlier
  set-aside file is never overwritten), and the hab starts with no rounds and raises one
  notice.
- **A stored trigger that no longer compiles.** A round whose stored trigger fails to compile
  at load (a zone this host's zone database lacks) still loads and can be read, edited or
  deleted. The store reports it per round, and the scheduler leaves it unscheduled and shows
  the error on the round, never silently.
- **Two parts, one seam.** The scheduler owns the definitions and slots. Run records go through
  a small run log interface: append a run, update its state, page a round's runs, list the
  runs that have not ended, prune by the retention rule (7.6). The first implementation writes to the rounds store.
- **What Logbook takes over later.** When Logbook exists, a second run log implementation files
  each finished run as a Logbook record (the later ideas in section 14), and `round.runs` reads from it.
  The scheduler, the definitions and `GFF_ROUND_LAST_OK` stay in the rounds store: a slot
  decision must never wait on a store that syncs between hosts.
- **The run record.** Run id, run number, round id, slot, trigger kind, `missed`, state, reason,
  session id, when it was made (`created`), start and end times, and the first 4 KiB of the
  final agent message. Logbook gets the same fields.
- **One write per new run.** The scheduler saves a new queued run and its round's state (the
  run number, the last served slot, the next slot) in one write. A failed write leaves neither,
  so the slot stays due and the next tick tries again. A slot is never marked served without
  its run.
- **Run numbers.** The scheduler gives each run the next number from the round's state in the
  rounds store; the run log stores a run as given and refuses an id it already holds, under
  any round. A run that has ended never changes its state, number or `created` again. Numbers so stay in
  the store that owns the slots, whichever run log holds the records.
- **A newer file.** A state file a newer build wrote is not set aside: gff-hab refuses to start
  with it, so a downgrade never loses rounds.

Decision: rounds keep their own state behind a run log interface. Reason: Logbook comes after
rounds; a narrow seam lets it take over the run history with no change to the scheduler or the
API.

---

## 8. Sleep, wake and catch-up

### 8.1 The tick

- The scheduler in `gff-hab` ticks every 30 s and compares **wall-clock** time with each round's
  next slot.
- **Wake detection.** At each tick it compares the wall-clock gap since the last tick with the
  monotonic gap. A wall-clock gap more than 60 s longer means the host slept. This needs no cgo,
  so it keeps the `check-pure` rule (`AGENTS.md`).
- **Go's monotonic clock stops during sleep** on both systems. Checked in the Go 1.27 runtime
  source. Linux: `runtime.nanotime1` reads `CLOCK_MONOTONIC` (`src/runtime/sys_linux_amd64.s`,
  `sys_linux_arm64.s`), which does not count suspend; `CLOCK_BOOTTIME` would. macOS:
  `nanotime1` (`src/runtime/sys_darwin.go`) calls `mach_absolute_time`
  (`sys_darwin_amd64.s`, `sys_darwin_arm64.s`), which does not advance while the Mac sleeps;
  `mach_continuous_time` would. `time.Now` takes its monotonic reading from `nanotime`.
- **Every entry point checks for a wake.** A tick, a run update, a pause and an unpause each
  read the clock under the scheduler's lock and compare it with the reading before. So a run
  that ends at wake, before the first tick after it, still sees the wake: missed slots stay
  missed, due runs wait the settle, and a network failure gets its retry.
- **The first reading after start counts as a wake.** A restart is then treated like a short sleep
  (8.4), with the settle.
- **The clock is injected.** The scheduler reads the wall clock and the monotonic reading through
  one interface, so tests move them apart on a fake clock.
- A timer set for "one hour from now" would fire an hour of awake time later on a sleeping
  laptop. Wall-clock comparison avoids that.

### 8.2 Catch-up once

- When a tick finds a round's next slot in the past, it starts **one** run for the latest missed
  slot, then sets the next slot to the first slot strictly after now. A missed round never runs
  once per missed slot, whatever the gap.
- The run records how many slots it covered (`missed`), so the history shows "ran once for 9
  slots".
- `catch_up = skip` drops the missed slots instead: the next slot moves past now and no run
  starts. Use it for jobs that only make sense on time.
- A `once` round missed while asleep runs at wake, or completes unrun with `skip`. A `once`
  round completes when its run ends, unless a retry after wake is planned; then it completes
  when the retry ends.
- **A `once` round never hangs.** A `once` round with no next slot, no live run and no retry
  planned completes at the next tick. It has two reasons. "once trigger served": its slot was
  served, but its run ended without completing it (a pause ended the run, or a restart dropped
  its retry). "once trigger passed while paused": its instant passed while the round was
  paused, so it never ran.
- **On time or missed.** A slot is on time when it came due since the tick before, with no wake
  between. Every slot found at a wake tick is a missed one. `skip` drops missed slots only.
- **Coalescing.** A slot due while the round's run is `queued` or `waiting` joins that run: the
  run takes the later slot and adds to `missed`. A slot due while the run is `running` or
  `blocked` stays due, and runs once when that run ends. An on-time slot held this way runs even
  with `skip`; the hab keeps that mark in memory only, so after a restart `skip` drops it.
- **Counting bound.** `missed` counts up to 10,000 slots. A 1-minute round asleep for a year
  has 525,600; the run still serves the latest slot. Reason: one tick never walks a year of
  slots.

### 8.3 Settle after wake

- After a detected wake, due runs wait 30 s (`rounds.wake_settle`) before they start. A network
  that returns a few seconds after wake is then up.
- Runs due at wake start oldest slot first, within the host limit (7.2).
- A run that fails in the first 2 minutes after a wake, with a harness that reports a network or
  sign-in error, is tried once more 2 minutes later. Other failures are not retried (10.1).
  The retry serves the same slot, and the failure still counts toward the pause. A new slot due
  before the retry replaces it. The plan lives in memory: a restart drops it.
- The settle ends at the first tick on or after it, so a run due at wake starts 30 s later.

### 8.4 Restarts

- **gff-hab restarts** (an update, a crash). Rounds, next slots and run records are on disk. A
  live run's session survives in the holder; the new gff-hab finds the run by its session id and
  goes on watching it. A slot that came due during the restart runs once, as after a sleep.
- **Every live run loads at start**, a deleted round's included. Its end can still be
  reported, and it holds its place under the host limit.
- **A lost start.** A run saved as started with no session id lost its start: gff-hab stopped
  between saving the run and the spawn. At start it fails, "start lost", and counts as a
  failure. Stage 3 must also fail a running run whose session the holder no longer has.
- **Host reboot.** Run sessions end with the holder. A run that was live is marked `failed`, "host
  restarted". Due slots run once after start, under the settle rule.
- Built (stage 3): at start gff-hab looks at each live run that has a session. A pane the link
  has is watched again, with the timeout counted from `started_at`. A pane that exited while
  gff-hab was down ends the run by its exit status. A session record that is gone fails the run,
  "run session gone". A pane no holder has, once the link has reached every registered holder,
  fails the run, "host restarted"; until then the run waits, looked at again when a pane comes
  back and every 2 s. A run's values in flight are not stored: after a restart the stored
  round's timeout and `on_finish` apply. While gff-hab runs, a run whose holder connection ends
  waits the same way: a holder that lives comes back and the run goes on; a holder that died
  leaves no pane once the link reaches every holder, and the run fails "host restarted".
- Built (stage 3), a start cut short: the scheduler saves the session id after the spawn. When
  gff-hab stops in between, the program runs on and its session record names the run
  (`Session.round`). Before the scheduler loads, gff-hab looks for that session and writes its
  id into the run, which is then adopted as above; only a run with no such session fails
  "start lost". Reason for a pass before the scheduler, not a lookup hook in it: the run log
  stays the one source the scheduler reads, and the scheduler needs no knowledge of sessions.
- Built (stage 3): gff-hab adopts the live runs before the control and hook servers take
  calls, so a client's input to an adopted run always takes it over.

Decision: a hab restart catches up instead of skipping. Reason: Paseo skips overdue slots after a
daemon restart, but our gff-hab restarts on every update and on purpose any time; skipping would
lose runs for reasons the user never sees. A restart is treated like a short sleep.

### 8.5 Clock changes

- **The clock jumps forward** (manual change, a long NTP fix): treated as a sleep. One catch-up
  run at most.
- **The clock jumps back.** The next slot stays. A slot at or before the last served slot never
  runs again, so no slot runs twice. A next slot at or before the last served one (an edit while
  the clock is back) moves to the first slot after the last served one, with no run.
- **The time zone database changes.** Next slots are recomputed at start.

---

## 9. Where rounds run

- **Each round has one runner.** By default, the hab where it was created. The runner must have
  the project checked out (`fresh`) or hold the target session (`resume`, `fork`).
- **Choose a runner.** At creation the user may pick any peer that has the project
  (`gff round add --host <label>`). The round is created on that hab through its API. To get a
  project onto another host first, use project set-up (task-j600w23a).
- **Move a round.** `gff round move <name> --host <label>` creates the round on the new runner
  and deletes it on the old one, after the user confirms. A `resume` or `fork` round cannot move,
  because its target lives on one host. A completed round cannot move either: its counts stay
  on the old host, so it would run again on the new one. The user edits it or deletes it.
- **No failover.** While the runner is offline, its rounds do not run anywhere. No other hab picks
  them up. When the runner wakes, catch-up applies.
- **The always-on host.** A hab marked `always_on` (`mesh.md` 8) is the natural runner for rounds
  that must not miss a slot. When the user creates a round on a laptop, the app suggests an
  always-on peer that has the project, in one line. It never moves a round on its own.
- **Peers see every round.** Each hab publishes its rounds in its catalog (a `rounds` section:
  name, mode, trigger, project, next slot, last run state). Clients and agents read the merged
  view, so one list shows every round on every host, with "host asleep since <time>" for an
  offline runner.

Decision: one runner per round, chosen by the user, with no failover. Reason: the mesh has no
election and no shared state (`mesh.md`); two habs running one round would need both. Paseo runs a
schedule only on its own daemon too. The always-on suggestion covers the need without a
coordinator.

---

## 10. Failure and notifications

### 10.1 Failure rules

- **No automatic retry,** apart from the one network retry after a wake (8.3). A failed run waits
  for the next slot. Paseo does the same.
- **Pause after repeated failures.** After 5 failed or timed-out runs in a row, the round pauses
  itself and says why. One notification goes out. `gff round unpause` clears the count.
- **Skips are not failures.** But 5 skips in a row raise one notification ("round `<name>` has
  not run since <time>: the session is in use").
- **Target gone.** A `resume` or `fork` round whose target session was closed (its record
  removed) completes, as in Paseo, with one notification. A `fresh` round whose project folder is
  missing skips that run; after 5 such skips it pauses.
- **Limits reached.** A round that hits `max_runs` or `expires_at` completes quietly.
- **What counts.** A success clears both counts. A failure or timeout adds a failure and clears
  the skips. A take over clears the skips. A skip adds a skip. A run the scheduler ends
  itself counts for nothing: a `queued` or `waiting` run ended because its round paused,
  expired, completed or was deleted. The scheduler marks such an end itself; a skip reported
  from outside always counts, whatever its reason text.
  `max_runs` counts succeeded, failed, timed-out and taken-over runs; a skipped run never ran.
- **Pause by the user.** A queued or waiting run ends `skipped`, "paused". Expiry and completion
  end such a run the same way. Slots that pass while a round is paused never run; unpause sets
  the next slot from now and clears both counts. A pause also drops a planned retry and an
  owed slot (8.2, 8.3). Unpause on a round that is not paused does nothing.
- **`notify = never`** is applied by the scheduler: it asks for no notice but the self-pause.

Decision: pause after 5 failures in a row. Reason: a broken unattended job costs tokens each slot;
this is a cheap guard. Paseo has none, but the owner allows cheap guards.

### 10.2 Notifications

Rounds use the Part 6 notification path (`app.md` 8): push from the runner hab to the user's
devices, and the app's in-app notices. A new push kind `round` is added to `push.register`.

| Event | Banner | Level |
|---|---|---|
| Run `failed` or `timed_out` | Yes, with the round name, the reason and "Open" | Active |
| Run `blocked` | No extra banner: the session's own approval push covers it | — |
| Run `succeeded` | Only with `notify = always`; shows the first line of the final message | Passive |
| Round paused after failures | Yes | Active |
| 5 skips in a row | Yes | Passive |
| Round completed because its target is gone | Yes | Passive |

- `notify = never` silences every row except the self-pause.
- Each round's notifications collapse by round id, so a failing hourly round shows one banner,
  updated.
- "Ask an agent" on a round notification works as for any notification (`capcom.md` 8.2): the
  event, with the run's result, is attached to the Capcom conversation.

### 10.3 The status board

- A **Rounds** list per host: name, mode, trigger, next run in the viewer's zone, last run state,
  paused or completed.
- Run sessions carry a round badge on their cards. A filter hides or shows them.
- A round's page shows its run history: slot, state, `missed`, duration, the first line of the
  result, and a link to the session.
- Actions: Run now, Pause or Unpause, Edit, Delete. Delete asks first.

---

## 11. Edge cases

| Case | Behaviour |
|---|---|
| The laptop sleeps through 9 slots of an hourly round | One run at wake after the 30 s settle; history shows `missed = 9` (8.2) |
| The laptop sleeps through a daily 09:00 round and wakes at 22:00 | One run at 22:00, unless `catch_up = skip` |
| A run is live when the laptop sleeps | The session freezes with the host. At wake the run goes on; `timeout` counts wall-clock time, so a long sleep may time it out |
| Two rounds target the same session | They take turns: one send at a time per pane; the second waits under the lock |
| The user deletes a round during a run | The run goes on to its end; its session follows the normal rules; no further runs |
| The user pauses a round while a run waits for the lock | The waiting run ends as `skipped`, "paused" |
| A `fresh` run in a folder where the user's own agent works | Allowed: two sessions, one folder, as Paseo's default "local" isolation. Worktree isolation for a run is a later stage (14), on top of `worktree.create` (`hab-logic-api.md` 3.10) |
| The harness needs a sign-in | The run is `blocked` with kind `auth`; the usual push goes out |
| The harness's usage window is full | The run fails with the harness's message. A usage-aware skip is a later stage (14) |
| A round's harness is not installed on the runner | Refused at creation; if it disappears later, each run fails with "harness not found", then the round pauses |
| The runner hab is offline when a client edits a round | The edit is refused with "host offline"; edits never queue for a sleeping host |
| A peer shows a round from a stale catalog | Marked stale with the catalog's age (`mesh.md` 5) |
| A run session's pane is closed by the user | The run ends as `taken_over` |
| The prompt is longer than 64 KiB | Refused at creation, the channel message limit (`mesh.md` 6.3) |
| An agent creates a round that triggers itself every minute | Allowed within the 1-minute floor, with no card (Q1); the agent can stop it with `gff round rm "$GFF_ROUND"`, and the failure pause (10.1) caps a broken one |
| A cron expression that never matches (`0 0 31 2 *`) | Refused at creation: "never runs" |

---

## 12. Settled questions

The study left three user-visible questions open. The owner asked for them to be settled with
defaults under the power-user rule (master plan, 2026-11-01): default to no extra confirmation,
drop a confirmation when the harness already asks, and add a setting only where people really
differ. These are the coordinator's calls on that rule, not owner decisions.

| Question | Decision | Reason |
|---|---|---|
| Q1. Does an agent-created round need a confirmation card? | No card, for any mode. A round an agent creates on another host goes through `confirm.start_elsewhere`, as every start on another host does (`capcom.md` 6.1). The hab records `created_by` | The harness already asks before the agent runs the round command; a card would be double permission. The failure pause and the round badge cap the risk |
| Q2. Does a successful fresh or fork run close its pane? | Yes: `on_finish = close` by default. Failed, timed-out, blocked and taken-over runs stay open | Matches Paseo; daily rounds would leave idle harnesses. The session record and transcript stay, so the chat view shows the run and resume is one tap |
| Q3. Does a successful run notify? | No: `notify = failures` by default, `always` per round | Success shows on the status board and in the history; the `done` banner is off by default too (`app.md` 8.2) |

The study's options and recommendations follow, for the record.

### Q1. Does a round that an agent creates need the user's confirmation?

| Option | For | Against |
|---|---|---|
| A. No confirmation. The harness's own tool permission covers the command | Matches Paseo; chat-created rounds just work | An agent with broad permissions can set up recurring, token-spending jobs unseen |
| B. A confirmation card for every agent-created round, with a setting to turn it off | The user sees every recurring job before it costs anything | One tap per round |
| C. A card for `fresh` and `fork` rounds; none for a `resume` round on the agent's own session ("wake yourself in 20 minutes") | Keeps the low-risk self-check frictionless; gates new sessions | Two rules to learn |

Recommendation: **C**, with one setting that turns the card off. It matches the owner's answer for
work on another host (a card, with a setting to turn it off), and leaves Paseo's "wake yourself"
flow as easy as in Paseo. The card shows mode, trigger in plain words, next three run times,
project, harness, model and the prompt (`capcom.md` 6.3).

### Q2. What happens to a fresh run's session after a successful run?

| Option | For | Against |
|---|---|---|
| A. Close the pane by default (`on_finish = close`); keep it on failure, block or take over | Matches Paseo's "archive on finish" default; hosts do not fill with idle harnesses | To read a finished run, the user opens its transcript or resumes it |
| B. Keep it open by default | The result sits in a live session | Daily rounds leave a pile of idle panes |

Recommendation: **A**. The session record and transcript stay, so the chat view still shows the
run, and resume is one tap.

### Q3. Notify on success?

Recommendation: off by default (`notify = failures`), on per round. A successful run is visible on
the status board and in the round's history; the bar's `done` banner is off by default too
(`app.md` 8.2).

---

## 13. API and CLI additions

All are additive under `hab-logic-api.md` 3.3. Each answers `unavailable` until its stage builds
it.

| Item | Kind | Purpose |
|---|---|---|
| `rounds` | Capability | The hab runs rounds |
| `round.list` | Method | This hab's rounds, with next slot and last run state |
| `round.get` | Method | One round with its definition |
| `round.create` | Method | Create a round; validates the trigger, mode and target; returns the round. Takes `run_now` |
| `round.update` | Method | Change fields; recomputes the next slot when the trigger changes |
| `round.delete` | Method | Delete a round; a live run goes on to its end |
| `round.pause`, `round.unpause` | Method | Pause or unpause; unpause clears the failure count |
| `round.run` | Method | Start a run now, outside the trigger; `conflict` while a run is live |
| `round.runs` | Method | A page of a round's runs, newest first |
| `round.updated` | Event | A round was created, changed, paused, completed or deleted. Stored, with a revision |
| `round.run_updated` | Event | A run changed state. Ephemeral |
| `Session.round` | Optional field | Round id and run id for a run session |
| catalog `rounds` | Catalog section | Name, mode, trigger, project, next slot, last run state (`mesh.md` 5) |
| push kind `round` | Word | Round notifications (10.2) |

CLI (`gff round`, also the agent kit's MCP tools with the same verbs). Until the agent kit
(Part 4) gives agents the `gff` verbs, the same commands ship as `gff-hab round`, beside
`gff-hab send` and `gff-hab show`, with the same doors: `-socket` or `-via` for the control
connection, and from a pane the hook endpoint named by `$GFF_HAB`, which names the calling
session for `--target self` and `created_by`.

Decision: build the CLI as `gff-hab round` first. Reason: the agent-facing commands that exist
today live in gff-hab and reach the hab from a pane through `$GFF_HAB`; the agent kit renames
them all at once (`mesh.md` 6.5 did the same for channels).

| Command | What it does |
|---|---|
| `gff round ls [--host <label>\|--all]` | List rounds on one host or every connected host |
| `gff round show <name>` | The definition, next three slots, last runs |
| `gff round add <name> --mode fresh\|resume\|fork (--cron <expr> [--tz <zone>] \| --every <preset> \| --at <time> \| --manual) [--project <dir>] [--target <session>] [--harness h] [--model m] [--host <label>] [--run-now] [limits] <prompt>` | Create a round. Inside a session, `--target self` names the calling session |
| `gff round edit <name> [fields]` | Change fields |
| `gff round rm <name>` | Delete, after a prompt (`-yes` skips it) |
| `gff round pause\|unpause <name>` | Pause or unpause |
| `gff round run <name>` | Run now |
| `gff round runs <name>` | Run history |
| `gff round move <name> --host <label>` | Move a `fresh` round to another runner (9) |
| `gff round export`, `gff round apply <file>` | Print as TOML, or make the hab match a file (3) |

Built (stage 3, task-ams092is). Where this spec left a choice open:

- **Shapes.** `round.create` and `round.update` take a `RoundSpec`: the definition fields, with
  durations as `timeout_ms` and `wait_ms`, times as RFC 3339, and a trigger of `kind`, `cron`,
  `zone`, `at`, and `every` (a preset, stored as the cron it compiles to). A project is named by
  `project` (id), `project_dir` (found or made as `session.create` does) or `project_remote`;
  a round read back carries both `project` and, when the project has one, `project_remote`.
  `round.get`, `pause`, `unpause`, `run`, `delete` and `runs` name a round by `id` or `name`.
  `round.update` replaces the whole definition, keeps the pause mark (pause and unpause change
  it), and with `revision` refuses a stale edit with `conflict`.
- **Revisions.** A round's revision starts at 1 when gff-hab starts and rises by one on each
  `round.updated`, compared within an epoch as the settings are. A delete is a `round.updated`
  with `deleted: true` at the next revision; there is no `round.deleted`. Both round events go
  to `host` scopes only; the snapshot holds the rounds with their newest run.
- **Runs by hand.** `round.run` and `run_now` make a run with no slot and trigger `manual`. It
  waits under the host limit and the wake settle as any queued run. A paused or completed round
  refuses a run by hand with `conflict`, as a live run does. `round.run` (and `round.create`
  with `run_now`) answers after the run's session is spawned, or once the run is queued: the
  start runs on the call's own goroutine, off the connection's read loop.
- **Command runs.** The run's `session.create` goes through the hab's own idempotency table with
  the key `round-run-<run id>`, is named after the round (`-2`, `-3` when taken), and carries
  `Session.round`, saved with the session record. Exit 0 succeeds; another status fails with
  "exit status N" or "ended by SIGNAL"; a pane gone with no exit fails. At the timeout the run
  ends `timed_out` at once and the pane gets `pane.close`.
- **Take over.** A client's `pane.input` or `session.send` that succeeds on a run session ends
  the run `taken_over`; a client's `pane.close` ends it before the close (7.5). `approval.answer`
  is not a take over. The hab's own closes (a timeout, `on_finish close`) are not either.
- **The pane route is narrower.** From a pane (`$GFF_HAB`), a round's `target` may only be the
  calling session (`self` or its own id), and `round.update`, `round.delete`, `round.pause`,
  `round.unpause` and `round.run` act only on rounds the calling session created; anything
  else is refused with `denied`. `round.list`, `round.get` and `round.runs` read every round.
  The control socket, the user's own clients, keeps full access. Reason: an agent, possibly in
  a harness sandbox, must not gain an unsandboxed scheduled command over someone else's round,
  nor aim a round at another session. The owner may loosen this later.
- **Not built yet.** Before stage 5 a round in `resume` or `fork` mode could be created, read
  and edited (so `--target self` works), but its run failed. (Before stage 4 a `fresh` round
  with a prompt failed the same way.) Before stage 6 the scheduler's notices were only logged,
  and a set-aside state file raised one plain notice.
- **Retention.** The prune of 7.6 runs when a run ends and at start; it removes the session
  records of the runs it drops when they have exited.
- **Export and apply.** The file is `[[rounds]]` tables with the definition fields, the trigger
  as `cron`, `every`, `at` or `manual = true`, `zone`, durations as Go text (`timeout = "1h0m0s"`),
  the project as `project_remote` (or `project` when it has no remote), and `paused`. Apply fills
  the defaults before it compares, so a file that leaves them out matches; a round with no
  `zone` keeps the hab's. It creates rounds only in the file, updates the ones that differ (a
  changed `paused` pauses or unpauses), deletes the ones only on the hab, prints the plan, and
  asks unless `-yes`. After the yes it reads the rounds again and plans again: the same plan
  applies with the revisions read now; a different one (a run ended, another client edited a
  round) is shown and asked again.
- **The CLI.** `round add` with no `-project` takes the current folder, from a pane or on the
  local socket; with `-via` it is refused, since that folder is on the client's machine, and
  `-project` must be absolute. `round edit -paused` is refused: `round pause` and
  `round unpause` change the mark.
- **Config.** `[rounds]` in the hab's config file: `max_running` (from 1) and `wake_settle` (a
  duration; `"0s"` for none). gff-hab runs rounds only with the holder link.

Built (stage 4, task-kt363fw0): `fresh` runs with a harness and a prompt
(`cmd/gff-hab/roundharness.go`). Where this spec left a choice open:

- **Start.** The run's `session.create` names the harness, the model and `argv` = the harness's
  name plus `harness_args`, so Codex gets `--no-daemon` and opencode its wrapper as any start
  does. Claude Code also gets `--session-id <uuid>` that gff-hab picks, unless `harness_args`
  already pick a session (`--session-id`, `--resume`, `--continue`, `--fork-session`): the
  final message and a later `claude --resume` then find the transcript even where no hook
  reaches gff-hab.
- **Harness checks.** `round.create`, and `round.update` when the harness or mode changes,
  refuse a harness with no adapter (`invalid`) and one whose program is not on gff-hab's
  `PATH` (`unavailable`). A harness that goes away later fails each run "start failed:
  harness not found: ...", which counts toward the failure pause.
- **Folder.** A round made with `project_dir` records that folder as the project's most
  recent one, so its runs start there (a stage 3 gap: a new project had no folder, and runs
  started in the home folder).
- **Readiness.** The prompt goes in at the agent's first `idle` (or `done`, or `error`) after
  the spawn. The 2-minute limit stops while the run is `blocked` (a sign-in at start) and
  starts again, whole, when it goes on. It also stops while the prompt is being sent: the send
  has its own 30 s limit.
- **Trust dialog.** In a folder it has not trusted yet, Claude Code asks "Quick safety check
  ... Yes, I trust this folder" before it reports any state. While a Claude Code run waits
  for its first `idle`, gff-hab reads the pane's screen every 0.5 s with its own screen rule
  (the detection manifest format; the vendored Claude manifest has no rule for the dialog).
  While the dialog shows, the run is `blocked` with a reason that names it, and the readiness
  limit waits. The user answers it in the pane: that input is not a take over, as an
  `approval.answer` is not, and neither is input up to 2 s after the dialog went. Rounds never
  answer it: trusting a folder is the user's call. Unanswered at the timeout, the run ends
  `timed_out` with that reason. The default option, "No, exit", ends Claude: the run fails,
  the harness exited.
- **Delivery.** The prompt goes through `session.send`'s path (paste, then Enter, under the
  input rules) as the hab, so it is not a take over. A refusal for a busy agent waits for its
  next `idle`: the agent's current state is applied again after 0.5 s, and the readiness
  limit starts again, whole. A prompt that may have reached the pane in part, or any other
  refusal, fails the run, "the prompt was not delivered". A run that ends while its prompt is
  being sent (the timeout, a take over) cancels the send, so it never pastes the prompt
  later.
- **Turn end.** No `session.turn_end` event is sent yet, so the agent state is the turn end:
  the first `done` or `error` after the send began. Read back is `working` or `blocked` after
  the send. Before the read back, only a change of state or of turn ends the turn: a
  republish of a `done` the agent already showed when the send began changes other fields
  only. An `idle` in the turn ends nothing: the owner's interrupt key shows `idle` before
  the input that carried it takes the run over, and that run must end `taken_over`. Once the
  turn's end is seen, it wins: a pane exit, the timeout or a take over that comes while the
  final message is read leaves the run as the turn ended it (a client typing right after
  `done` does not take the run over). Agent states are applied with their revision check in
  one critical section, so a stale state never applies after a newer one.
- **Result.** The last agent message after the last user message (the prompt), from the
  Claude Code transcript by the session id gff-hab chose, else through the chat view's
  reader. It is read before the run ends, so before `on_finish close` closes the pane
  (opencode's conversation lives in its server). The read tries for up to 5 s, since a
  transcript may trail the turn's end.
- **Failure.** `error` fails the run, "the turn ended with an error", with the harness's
  failure kind after a colon where it gives one: the new optional `AgentState.error`, from
  Claude Code's `StopFailure`. The kinds `harness.NetworkFailure` lists (`authentication_failed`,
  `cloud_credential_error`, `server_error`, `unknown`) set the network flag for the retry
  after a wake (8.3). The harness's program exiting before the turn end, whatever its status,
  fails the run, "the harness exited before its turn ended (exit status N)". An `exited`
  agent state waits 2 s for the pane's exit, which carries the status, and ends the run
  without it only when the pane does not exit.
- **Blocked.** Any `blocked` kind, or the trust dialog, shows the run `blocked`; the agent
  going on shows it `running` again. A live run update now keeps its reason (the trust
  dialog's), and the next one replaces or clears it. The round adds no notice.
- **Timeout.** The run ends `timed_out` at once. A turn that is `working` then gets Esc,
  Claude Code's and Codex's interrupt key, through the adapter (the `session.interrupt`
  method itself is still not served). A `blocked` run gets nothing: the reason names the
  request kind, which stays open. opencode has no interrupt yet.
- **Restarts.** Whether a run's prompt went in is not stored; an adopted harness run reads
  its conversation once. Claude Code's session id comes back from the program's arguments
  (`--session-id`). No user message: the prompt never went in, so the run waits for its first
  `idle` again, under the readiness limit. An agent message after the user message: the turn
  may have ended while gff-hab was down, so the first `idle` (or `done`) ends the run
  `succeeded` with that message. Otherwise, or when the conversation cannot be read (a Codex
  run before its first hook), the run is taken to be in its turn and ends at the next turn
  end, the exit, a take over or the timeout.
- **Close.** gff-hab starts no background work after its rounds close: no prompt is sent and
  no run ends. Runs stay live on disk for the next gff-hab.
- **Tests.** `$GFF_TEST_ROUND_READY` (testseam builds only) shortens the readiness limit.

Built (stage 5, task-21rifvi9): `resume` and `fork` runs (`cmd/gff-hab/roundlock.go`). Where
this spec left a choice open:

- **In use (6.1).** gff-hab looks at a waiting run's target every 250 ms. The agent: `working`
  ("working"), `blocked` or an open request ("blocked"), or no agent with an adapter in the
  pane ("in use: no agent this hab knows runs in it"). Clients: a `pane.input` or a client's
  `session.send` to the pane, counted when it is asked for and when it ends, refused or not,
  in the last 2 minutes; a `session.focus` with `active` true in the last 60 s (clients send
  it every 30 s while on screen), cleared at once by `active` false. A send of any client
  still starting its turn, and another round's run that holds the pane, count too. The
  reasons start "in use:" for the client and send signals. The client signals live in memory
  only, so for 2 minutes after gff-hab starts every pane counts as in use ("in use: gff-hab
  just started"), for adopted runs too: a client may have typed or watched while it was down.
- **The lock and the send.** A free target is taken: the run holds the pane (two rounds on one
  target take turns), reports `running`, then sends through `session.send`'s path as the hab.
  The client signals are checked again under the send's claim, after the agent's own check
  and before any byte is written, so no other send comes between the check and the paste. A
  send refused for a busy agent or a client puts the run back to `waiting`. Input or a client
  send that reaches the pane before the prompt went in does the same; it is not a take over
  (7.5 takes over only after the prompt). Closing the pane is still one. A client typing
  in the instant between that check and the paste is not covered; nothing is atomic with a
  person's keys. The prompt is pasted after anything the user left unsent in the prompt; the
  lock never clears input.
- **Waiting.** The run reports `waiting` with the reason, and again when the reason changes.
  Past `wait`, counted from the run's start, it ends `skipped` with the last reason. The
  scheduler already coalesces a due slot into a waiting run and ends it `skipped`, "paused" on
  a pause; the move from `waiting` to `running` is reported before the send, so a pause that
  came first leaves nothing to send. A waiting run that reaches its `timeout` ends `skipped`
  with the reason, not `timed_out`: it never ran.
- **The envelope.** `[Flightdeck: the prompt of round '<name>', run <n>, slot <time>` (or `run by
  hand`) `, sent by Flightdeck on the owner's schedule. Its values: GFF_ROUND=... GFF_ROUND_RUN=...
  [GFF_ROUND_SLOT=...] [GFF_ROUND_LAST_OK=...]. ...]`, then `[begin round message <run id>]`,
  the prompt, `[end round message <run id>]`, cleaned and with markers neutralised as the
  channels envelope is (`internal/channels`). Fork runs get it too, and the values in their
  environment as well.
- **Turn and end.** After the send a resume run is a stage 4 harness run: read back, the turn's
  `done` or `error`, the final message, take over. The target is never closed, and retention
  removes only a session whose `Session.round` names the run, so a target stays. On `on_finish
  = close` the same check applies, and a round that is gone (deleted mid-run) keeps the pane:
  a run never closes a session it did not make. A timeout
  sends Esc only to a turn this gff-hab started with the prompt.
- **Exited target.** `session.resume` serves Capcom sessions only, so gff-hab starts the
  target's harness again itself, as Capcom's resume does: the exited pane is released, then
  `claude <harness_args> --resume <id>` or `codex resume <id> <harness_args>` runs in a new pane
  of the same session, in the target's folder. The prompt goes in at the first `idle`, under
  the readiness limit, with the same check under the claim. opencode's resume is not built:
  the run fails. A target whose pane's holder is not reached yet waits ("in use: its holder is
  not connected"). Before the start, gff-hab looks for a live agent in another pane on the same
  conversation (the user or Capcom resumed it there): then the run waits ("in use: its
  conversation runs in another pane"), and no second program starts on one conversation. Two
  runs on one exited target: the store refuses the second start, and that run waits for the
  lock and takes its turn. A run that ends while its target starts (its timeout, a pause, a
  delete), or a gff-hab that closes meanwhile, leaves the started target alone.
- **What the target runs.** No session record holds a harness or a native session id, so the
  round remembers them (`target` in its state, with the session they were seen in): after a
  create or an edit commits, and at each look of a waiting run that sees the target's agent.
  The memo is written and read only while its session is the round's target, so a refused edit
  or a run still waiting on a replaced target never names the wrong conversation.
- **Fork.** `claude <harness_args> --resume <id> --fork-session` or `codex fork <id>
  <harness_args>` (with `--no-daemon`), in the target's folder and project, named after the
  round with `Session.round`; then a fresh run with the envelope. gff-hab picks no Claude
  Code session id for a fork; the fork's id comes from its hooks. The adapter's fork support is
  `forks()`: Claude Code and Codex. A fork round whose target runs opencode is refused at
  create (`invalid`); a target whose harness is not seen yet passes, and its run fails with the
  reason. The target's transcript is never written.
- **Target gone (10.1).** A target whose record is gone at the start, or while the run waits,
  ends the run `skipped`, "the target session is gone", which counts for nothing, and
  completes the round ("target session gone") with one `target_gone` notice (none with
  `notify = never`). Stage 6 sends it. A run on a target an edit already replaced completes
  nothing: the round goes on with its new target.
- **Restarts.** A `waiting` resume run waits for the lock again. A `running` one reads its
  target's conversation: a user message that holds its envelope's begin line means the prompt
  went in (in its turn, or ended); none means it waits for the lock again. A fork run is adopted
  as a fresh harness run, the begin line telling its prompt from the parent's messages.
- **CLI.** `gff-hab round add|edit -wait <duration>`.
- **Not yet.** The real fork on a Mac; opencode fork and exited-opencode resume; a user
  draft left in the prompt is not detected.
- **Known limits.**
  - The resume and fork folder is the session's recorded folder, not the agent's working
    folder from its hooks. Claude Code finds `--resume <id>` by project folder, so a session
    whose `claude` started in a subfolder fails the run, safely.
  - The chat view does not mark the round envelope yet (5); only Capcom's context chips parse
    markers. That is an app-stage item. The marker neutraliser misses look-alikes such as
    `[begin-round-message` and full-width brackets; channels has the same gap.
  - A restart between Enter and the harness's write of the user message to its transcript
    finds no envelope at adoption: the run waits for the lock again and can send its prompt a
    second time. The window is small.

Built (stage 6, task-00cqxbrf): notifications, the catalog `rounds` section, and rounds on
other hosts (`cmd/gff-hab/roundhosts.go`, `internal/push/round.go`). Where this spec left a
choice open:

- **The rows (10.2).** Each row raises one notice through the path `notice.raise` uses: a kept
  notice for `notice.get` and Capcom, a `notice.raised` event, a push. The notice carries the
  new optional `round` field (`kind`, `round`, `name`, `run`); `kind` is `run_failed`,
  `run_succeeded`, `paused`, `skips`, `target_gone` or `state_set_aside`, an open set.

  | Row | Title | Body | Level | Action |
  |---|---|---|---|---|
  | `run_failed` | "Round failed: <name>" ("Round timed out: <name>") | The reason | Active | Open the run session |
  | `run_succeeded` | "Round done: <name>" | The first line of the final message | Passive | Open the run session |
  | `paused` | "Round paused: <name>" | "5 failed runs in a row. Unpause it to run again." | Active | Open the last run's session |
  | `skips` | "Round not running: <name>" | "Not run since <time in the round's zone>: <reason>" | Passive | Open the target (`resume`, `fork`); a `fresh` round has none: the skipped run's session, when it has one |
  | `target_gone` | "Round completed: <name>" | "Its target session is gone." | Passive | None |
  | `state_set_aside` | "Rounds state set aside" | What gff-hab did | Active | None |

- **The push.** Kind `round` goes only to a device that names it in `push.register`, as `done`
  does: the app opts in (stage 7). The collapse id is `round-<round id>` (`round-state` for the
  state file), so a failing round's failures, then its pause, update one banner. The push's
  metadata carries `round` and `run` beside `notice`. The presence tiers apply as for any
  notice: a client watching the run session holds the push.
- **notify.** The scheduler applies `notify`; gff-hab applies it again before it raises, so no
  path past the scheduler sends a row the setting leaves out. `never` leaves only the pause.
- **Ask an agent.** The kept notice's body is the run number, its state and reason, then the
  run's final message, so Capcom gets the result with the event. The final message is kept only
  when the read scope allows the run session's folder; an unknown folder keeps it out. The
  banner never holds it, except the first line of a success.
- **The catalog section.** `rounds`: per round its id, name, mode, the trigger in plain words
  (`every 1h`, `every 1d in Europe/Berlin` for a preset of an hour or more, `cron <expr> in
  <zone>`, `once at <time>`, `by hand`), the project's name and remote, the next slot in UTC,
  the last run's state and time, paused, completed. Every round and run change pokes the
  catalog, so the generation rises with a run's state. The age of a stale copy is the entry's
  `seen_at` (`mesh.md` 5.2); there is no field per round.
- **-host.** `gff-hab round <verb> -host <label>` acts on that peer's rounds over a control
  connection the CLI opens itself, through the endpoint the peer's link uses from this hab's
  peer list. The peer must be online there, else "host offline: <label> is offline, last seen
  ..."; edits never queue (11). The peer must be pinned: a row with no device id (the link
  could not save the pin) is refused "host not pinned" before any dial, since any hab at that
  endpoint would pass. The hab that answers must present the pinned device id. Every verb
  takes it. Decision: the CLI dials, not the hab over its peer link. Reason: the user's
  edit then reaches the runner with the user's own SSH access, as `-via` does, and no hab
  forwards `round.*` for another; a hab-to-hab path for rounds belongs with the agent kit's
  confirmation (stage 10).
- **-host is refused from a pane, and with -via.** From a pane it is an agent's round on
  another host, which waits for `confirm.start_elsewhere` in the agent kit (Q1, stage 10); a
  direct dial would also skip the pane route's narrowing. With `-via` the peer list's endpoints
  are the remote hab's, not this machine's.
- **The project on the peer.** `add -host` with `-project` takes a folder on the peer, as `-via`
  does; with no `-project` it names the project by the remote of this folder's project in this
  hab's catalog (`-remote` gives one by hand). `round.create` with `project_remote` takes a
  project the peer's structure has, else a checkout its catalog found under a project root,
  which becomes a project as `project_dir` would.
- **ls -all.** `catalog.list`: this hab's rounds, then each peer's, with "host asleep since
  <seen_at>" on a stale host's rows. `-json` prints the rows with their host.
- **move.** It asks first (`-yes` skips it), then pauses the round here, so no slot runs on both
  hosts, creates it on the new runner with the same definition and pause mark, its project named
  by remote, and deletes it here. The next slot is the trigger's next one from now on both, so
  it does not change. A create the new runner refused unpauses the round here; when the
  refusal is a name it already holds (an earlier move whose delete failed, or another round),
  the error says so and what to do. A create whose answer was lost (the connection dropped, a
  timeout) may have taken effect, so the CLI asks the new runner with `round.get` by name: the
  same round there finishes the move; no round there unpauses it here; no answer, or another
  round by that name, leaves it paused here and prints the commands that settle it. A delete
  that fails leaves it paused here, and says so. The steps after the question get a context of
  their own, and the rollback one from `context.Background`, so a slow answer or a slow create
  never skips the unpause. Refused: `resume` and `fork` rounds, a completed round (its counts
  stay behind, so on the new runner it would run again: edit its trigger, `max_runs` or
  `expires_at`, or delete it), a round with a `cwd` (a folder on this host), a project with no
  remote. The run history and the counts stay behind
  and go with the delete; `created_by` is not carried. A live run here goes on to its end.
- **Not yet.** The always-on suggestion at creation (9) and the status board's list (10.3) are
  app and client stages (7, 8).

---

## 14. Build stages

The study proposed eight build stages. The plan splits the scheduler core in three, so each
stage is one outcome a single helper can build and test. Each stage is a task in the Part 13
plan; the task holds the work and the acceptance checks. Core stages (2, 3 and 5) get a
fresh-eyes reviewer before they close.

| # | Stage | Builds | Task |
|---|---|---|---|
| 1 | Definition and store | Round and run types and checks; cron, presets, zones and the DST rules; the rounds store and the run log seam (7.7); retention | task-e22g2ajs |
| 2 | Scheduler | The wall-clock tick and wake detection; catch-up once and skip; restarts and clock jumps; one run per round; the host limit; the wake settle; failure and skip counts, pause and completion. Fake-clock tests | task-tt1ysbq2 |
| 3 | API, CLI and command runs | `round.*` methods and events, `Session.round`, the `rounds` capability; `gff-hab round`, export and apply; `fresh` runs with `command` in a real pane; re-adopting a live run after a gff-hab restart | task-ams092is |
| 4 | Harness fresh runs | `fresh` with a harness and a prompt; end on `turn_end`; timeout by interrupt; blocked runs; take over; `on_finish` | task-kt363fw0 |
| 5 | Resume and fork under the lock | The four in-use signals; wait then skip; the envelope; exited targets; `fork` for Claude Code and Codex; target gone | task-21rifvi9 |
| 6 | Notifications, catalog, other hosts | Push kind `round` and the rows in 10.2; the catalog `rounds` section; `--host`, `move` and `ls --all` | task-00cqxbrf |
| 7 | App | Rounds list, round page, history, badges, actions, round notifications (Swift; the app lead) | task-xhpsgzkr |
| 8 | gff client | Badges, the filter and the board's Rounds list | task-cpazgcsj |
| 9 | Event triggers | The four events in 4.3, debounce, cooldown, no self-loops | task-awkgh5h1 |
| 10 | Agent kit | `gff round` verbs and MCP tools; a round on another host under `confirm.start_elsewhere` | task-ybb4c3ed |
| — | Later ideas | Worktree isolation for `fresh` runs (on `worktree.create`, `hab-logic-api.md` 3.10); skip when a harness usage window is above a threshold; a Logbook run log (7.7) that files each run as a Logbook record (Part 12) | Planned when Logbook is built |

Stages 1 to 3 run in order. Stages 4 and 6 can run side by side after 3; 5 follows 4. Stages 7
and 8 need 3 and 6. Stage 9 waits for 5 and 6; stage 10 waits for the agent kit and Capcom's
confirmation cards.

---

## 15. Sources

- Paseo, github.com/getpaseo/paseo, read 2026-11-01: `public-docs/schedules.md`,
  `public-docs/schedules-cli.md`, `public-docs/schedules-chat.md`,
  `packages/protocol/src/schedule/types.ts`, `packages/server/src/server/schedule/service.ts`
  (read to confirm the tick, missed-slot, restart, busy-target and target-gone rules).
- herdr and Rex: `landscape.md` 2.14.
- launchd: `launchd.plist(5)`, `StartCalendarInterval` (missed intervals coalesced at wake).
  systemd: `systemd.timer(5)`, `Persistent=`, `RandomizedDelaySec=`. cron and anacron man pages.
  GitHub Actions docs, the `schedule` event.
- Claude Code `--resume` and `--fork-session`, and Codex `resume` and `fork`: each CLI's `--help`,
  read 2026-11-01. opencode fork: not checked *(unverified)*.
- Ours: `AGENTS.md`; `landscape.md` 2.14 and idea 4.6; `mesh.md` 2, 5, 6.3, 8;
  `hab-logic-api.md` 2.2, 3.3, 3.5, 3.9, 4; `harness-adapters.md` (rules, environment contract,
  input delivery); `capcom.md` 6 and 8.2; `app.md` 7 and 8.
