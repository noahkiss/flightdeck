# Rounds: behaviour spec

Status: accepted for build. Part 13. Written 2026-11-01; questions settled and build stages
planned 2026-11-01 (section 12, section 14). Watchers added 2026-11-06 from the swarm study
(`swarm-study.md` 5, 8): the `watch` trigger, the `capcom` and `group` modes, and the
`notice.raised`, `logbook.updated` and `hab.woke` events (4.3 to 4.6, 5.2, 14.1). The owner
answered the watcher questions on 2026-11-06 (12): Capcom's second answer, module watchers
(stage 12) and, for Capcom jobs, the `actions` field (3).

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
16. [Open questions: watchers](#16-open-questions-watchers)

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
| watcher | For `watch`: the long-lived program whose lines are events (4.4) | The round id; one per round |
| event batch | The events one event run serves (4.5) | Part of the run |
| destination | For `capcom` and `group`: where the run's message goes (5.2) | Capcom's service, or a group's coordinator |

Words. One word per concept:

- **round**: the definition. Never "schedule" or "job" in the product.
- **run**: one execution of a round.
- **mode**: `fresh`, `resume`, `fork`, `capcom` or `group` (5).
- **trigger**: what asks for a run: `cron`, `once`, `event`, `watch` or `manual` (4).
- **watcher**: the program of a `watch` trigger. Never "daemon" or "listener".
- **event**: one hab event (4.3) or one line from a watcher (4.4).
- **event run**: a run a batch of events made, from an `event` or a `watch` trigger. "Event
  round" means a round with either trigger.
- **take over**: the user types into a run session; the round stops treating it as automatic
  (7.5).

Decision: landscape idea 4.6 ("heartbeats") is `resume` mode, not a new word. Reason: "heartbeat"
already means the API's keepalive notification (`hab-logic-api.md` 3.5). Two meanings for one
word would break the one-word rule.

Decision: the swarm study's `capcom` and `group` "targets" are modes here. Reason: in this spec
"target" is the session a `resume` or `fork` round works on, and a mode is what a run does. A run
that hands a message to Capcom or to a coordinator is a new thing a run does.

---

## 3. The round definition

| Field | Required | Meaning |
|---|---|---|
| `name` | Yes | Unique on the runner. Letters, digits, `-` and `_` |
| `mode` | Yes | `fresh`, `resume`, `fork`, `capcom` or `group` (5) |
| `trigger` | Yes | One trigger (4). An `event` trigger holds the event and its filter (4.3); a `watch` trigger holds `watch` (4.4) |
| `prompt` | Yes, unless `command` | Text the agent receives each run. In `capcom` and `group` mode, the instruction that goes with the events (5.2) |
| `group` | `group` | The group, by id (5.2) |
| `actions` | No | `capcom` only: the action words Capcom may use in the event turn that takes this round's message (`capcom-service.md` 9.6). Default none. Stage A9 |
| `debounce`, `cooldown` | No | `event` and `watch` only: default 30 s and 5 minutes (4.5) |
| `secrets` | No | `watch` only: the name of a secret set in the hab's config (4.4) |
| `harness` | `fresh` with a prompt | `claude`, `codex`, `opencode`, or a later adapter |
| `model`, `harness_args` | No | Passed to the harness at start, as `session.create` does. Permission modes go here; the hab adds none of its own |
| `command` | No | `fresh` only: a plain program (`argv`) instead of a harness and a prompt. Success is exit status 0 |
| `project` | `fresh` | The project, by id. The folder is the project's most recent folder on the runner (`hab-logic-api.md` 3.9, project folders) |
| `cwd` | No | A folder inside the project, instead of its most recent one |
| `target` | `resume`, `fork` | The session to resume or fork |
| `max_runs` | No | The round completes after this many finished runs |
| `expires_at` | No | The round completes at this time |
| `timeout` | No | Longest run, default 60 minutes (7.3) |
| `wait` | No | `resume` and `group`: how long a run may wait for the lock (6.3) or its delivery (5.2), default 10 minutes |
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
  environment, or, for a watcher, a secret set in the hab's config (4.4).
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
- **Field rules (watchers).** A `capcom` or `group` round takes a prompt and no `harness`,
  `model`, `harness_args`, `command`, `project`, `cwd`, `target` or `on_finish`. `group` is for
  `group` rounds only. `debounce` and `cooldown` are for `event` and `watch` triggers only,
  `secrets` for `watch` only. Any mode takes any trigger: a `cron` round in `capcom` mode is a
  morning brief from Capcom.
- **Field rules (Capcom jobs).** `actions` is for `capcom` rounds only, from the closed set of
  `capcom-service.md` 9.6. A create, update or apply from any agent route (a pane's, or
  Capcom's own) that sets a non-empty `actions` or adds a word is refused `denied`; only a
  person grants Capcom an action. Removing words is allowed from anywhere. While `actions` is
  not empty, an agent route may only remove words: any other edit, a pause or a delete from it
  is `denied`, and a person's grant clears `created_by`.

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
| `watch` | A line from the round's watcher, a program gff-hab keeps running (4.4). Later stage |
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
| `notice.raised` | Lowest `level`; session or project; `text`, a case-insensitive part of the title or body | Ask Capcom about each active notice a script raises |
| `logbook.updated` | Record `type`; project; `change`: `created` (the default), `status`, `deleted` or `any`; for `status`, the new statuses; `tags` (any of); `by`: `person`, `agent`, `hab` or `any` (the default) | Triage each task a person files; tell a group's coordinator when a task in its project is `done` |

- **`hab.woke`** is a new event. The scheduler raises it once for each wake it detects from the
  clock gap (8.1), after the wake settle (8.3). It carries how long the host slept. The first
  reading after gff-hab starts is not a wake for this event: a restart is not a sleep the user
  saw.
- **`notice.raised`** is the event of `notice.raise` (`hab-logic-api.md` 3.9):
  notices from scripts, `gff-hab notify`, rounds and gff-hab itself. The banners of agent states
  raise no such event (`capcom.md` 8.5), so a round cannot match a permission request this way.
  A notice about the round itself (its `round` field names this round) never triggers it, nor
  one whose `session` is the round's run session (the no-loop rule below).
- **`logbook.updated`** carries only the record's id, revision, type, project and `deleted`
  (`logbook.md` 10.5). The matcher tests `type`, project and `deleted` on the event. Only then
  does it read the ops this replica folded for the record since the last one it handled, and it
  matches `change`, the statuses, `tags` and `by` on those ops. `by` is the op's actor kind
  (`logbook.md` 3.3: a person, an agent, or the hab itself).
- **Folded since, not revision.** The revision counts folded ops, but a peer's op can fold into
  the middle of a record's history by its HLC, so "the last N ops" is not "the new ops". Stage 9
  adds an optional `folded_after` to `logbook.history`: a local fold sequence number the replica
  gives each op as it folds it, never synced. The matcher keeps the highest number it handled, in
  memory. At gff-hab start it begins at the replica's current number: only ops folded after the
  start count, a peer's op that arrives later included, whenever it was written.
- **Logbook changes from peers count.** An op that sync brings from a peer folds into this hab's
  replica and raises the event here, so it matches. Reason: the replica is this hab's own state;
  the rule above is about events, not about who wrote the record.
- **Private projects.** A record in a private project (`logbook.md` 3.3) goes into a run's event
  batch only when the run's session belongs to that project. Elsewhere the batch says "1 record
  in a private project" with no id or title. A `group` run follows the same rule with the
  coordinator's project. A `capcom` run always gets the count line: the service has no project
  (5.2).
- **Not replayed.** A change made while gff-hab was stopped raises no event, as for every hab
  event. A round that must see every change sweeps on `cron` with `GFF_ROUND_LAST_OK` (5.1).
- **Batches.** Debounce, cooldown and coalescing are in 4.5.
- **No loops.** An event caused by the round's own run session never triggers that round. In
  `capcom` and `group` mode the destination counts as the run session (5.2): Capcom's own
  writes never trigger a `capcom` round, and the coordinator's never trigger a `group` round.
- **No webhooks.** The hab listens on no TCP port (`hab-logic-api.md` 4). An outside system
  triggers a round with `gff-hab api call round.run` over SSH, or a watcher dials out to it
  (4.4).

Decision: events in a later stage, with this fixed list. Reason: Paseo's event triggers live in its
paid Hub; cron covers the bar. The list uses events the hab already sends, so each is cheap.
`hab.woke` is the one new event, and the scheduler already detects the wake.

Decision: the Logbook event filters on the ops, not on the record as it is now. Reason: "a task
became `done`" is a change, and only the ops say what changed; the event alone names no field.

### 4.4 Watch

A `watch` trigger names a **watcher**: a long-lived program that gff-hab keeps running. Each JSON
line the watcher prints is an event. Events make runs through the same batch rules as hab events
(4.5). The watcher reaches outside systems by dialling out: IMAP IDLE for mail, a feed, a status
page. The hab still listens on no port.

| Field | Required | Meaning |
|---|---|---|
| `watch.argv` | Yes | The program and its arguments. No shell |
| `watch.cwd` | No | An absolute folder. Default: `<state>/rounds/watchers/<round id>/`, made 0700 |
| `watch.kinds` | No | Event kinds that count; others are dropped quietly. Default: every kind |
| `secrets` | No | The name of a secret set in the hab's config file (below). Never a value |

**How it runs.**

- **In a pane, visible.** The watcher runs in a holder pane, as every run does, under a wrapper:
  `gff-hab watch-pane`, as `gff-hab opencode-pane` wraps opencode. The wrapper starts the
  program with its stdout on a pipe, reads the event lines, and sends them to gff-hab through
  the pane's `$GFF_HAB` endpoint (`round.watch_event`, 13). The program's stderr and one line
  per event ("event: <title>") show in the pane. Reason: the holder keeps the pane through a
  gff-hab restart, so an update never drops a mail connection, and the user can open the pane
  and see what the watcher does.
- **Its session.** Each `watch` round has one watcher session, named `<round name>-watch` (with
  the usual `-2` suffix rule), apart from its runs' sessions. It carries `Session.round` with the
  round id, no run id, and `watcher: true`. It is not a run. It holds no place under the host
  limit (7.2). Badges and the run-session filter treat it as a run session. Each start, after a
  backoff or a wake, is a new pane in the same session, as `session.resume` makes. The resume
  book does not record it, and a client's Resume on it is refused: gff-hab starts watchers.
  Deleting the round removes the session once its pane has exited. The secret set's values go
  into the pane's environment only, never into the session record.
- **Typing into it** takes nothing over. Closing the pane counts as an exit; to stop the
  watcher, pause the round.
- **Environment.** The runner hab's environment, as for any session. Then the secret set, then
  `GFF_ROUND` (the round id), `GFF_ROUND_LAST_OK` (5.1) and `GFF_WATCH_CURSOR` (below).
- **Start.** gff-hab starts the watcher when the round is created or unpaused, and at its own
  start when no watcher pane of the round survives. At start it first waits until the link has
  reached every registered holder, as run adoption does (8.4), so a watcher in a slow holder is
  adopted, never started twice. An edit to `watch` or `secrets` restarts
  it; an edit to any other field does not.
- **Stop.** A pause, a delete or a completed round hangs up the watcher, as `pane.close` does.
- **gff-hab's own hang-ups** (a wake, an edit of `watch` or `secrets`) count neither toward the
  backoff nor toward the exit pause: the next start is at once, or after the settle.
- **Restart with backoff.** Any other exit restarts the watcher: after 1 s, doubling to 5 minutes. A
  watcher that ran 10 minutes resets the backoff. Exit status 0 is not special: a watcher runs at
  all times. A watcher that cannot start (the program is missing) backs off the same way.
- **Pause after repeated exits.** 5 exits in a row, each less than 60 s after its start, pause
  the round with the `paused` notice (10.2), whose body names the last exit and whose action
  opens the watcher session. It is a self-pause, so `notify = never` keeps it. Unpause clears
  the exit count and the backoff. Reason: the same
  cheap guard as the failure pause (10.1), with the count of systemd's default start limit
  (5 starts).
- **Sleep and wake.** Nothing can run before the host sleeps, and gff-hab has no sleep hook
  without cgo (8.1). The watcher freezes with the host. At a detected wake gff-hab hangs it up
  and starts it again after the wake settle (8.3). The first reading after gff-hab starts is not
  such a wake, though 8.1 counts it as one for slots: a gff-hab restart leaves the watcher
  running (below). Reason: its connections died in the sleep,
  and a blocked read may take many minutes to notice. A host reboot is the same: the watcher
  starts after gff-hab's start settle.
- **A gff-hab restart** leaves the watcher running. While `$GFF_HAB` does not answer, the
  wrapper keeps up to 1,000 lines or 1 MiB, drops the oldest past that, counts the drops, and
  sends the rest when gff-hab answers again.
- **One per round, 16 per hab.** `rounds.max_watchers` in the hab's config sets the cap. A
  create, an unpause or an update that makes a round a `watch` round past it is refused with
  `conflict`.

**Event lines.** One JSON object per line on stdout:

| Field | Required | Meaning |
|---|---|---|
| `title` | Yes | One line, at most 200 characters |
| `kind` | No | A word the watcher picks, such as `mail`. Matched by `watch.kinds` |
| `key` | No | A dedup key. A key the round saw in the last 24 hours is dropped quietly. The round keeps its last 1,000 keys |
| `level` | No | `passive`, `active` or `time-sensitive`. A hint for the run and for Capcom |
| `at` | No | When it happened, RFC 3339. Default: when gff-hab read the line |
| `data` | No | A JSON object, at most 16 KiB encoded |

- **The cursor.** A line `{"cursor": "<text>"}` is no event. gff-hab stores the text (at most
  1 KiB) with the round and passes the latest one as `GFF_WATCH_CURSOR` at each start. A mail
  watcher stores the last message it reported, so after a sleep it reports what came in the
  meantime and starts IDLE from there. gff-hab saves the cursor at most once per 10 s and with
  each new run. Reason: the rounds store writes its whole file (7.7); a crash then repeats at
  most 10 s of events, and the `key` drops the repeats.
- **Bounds.** A line is at most 64 KiB. At most 60 events per minute per watcher, in a bucket
  that allows a burst of 60. A line that is not a JSON object, lacks `title`, breaks a bound or
  comes over the rate is dropped. The wrapper writes the reason in the pane, at most once per
  10 s with a count. The round shows its drop count since the watcher started.
- **Untrusted.** An event's text comes from outside: anyone can write a mail's subject. Runs
  get it as marked data (4.5), never as the prompt.

**Secrets.** A round holds no secrets (3). A watcher that needs one names a set from the hab's
config file:

```toml
[rounds.secrets.mail]
vars = { IMAP_HOST = "imap.example.com", IMAP_USER = "me@example.com" }
command = ["/usr/local/bin/print-mail-secrets"]
```

- `vars` are plain values. `command` is an argv, no shell. gff-hab runs it at each watcher start,
  with stdin `/dev/null` and a 30 s timeout, as it runs the push `key_command`. Each output line
  `NAME=value` sets one variable. Its output is never logged and never stored. A failing
  command counts as a watcher exit and backs off.
- `round.create` and `round.update` refuse a `secrets` name the hab's config lacks (`invalid`).
  The export carries the name only, so a file in git holds no secret.

**Module watchers (stage 12).** A module (`modules.md`) may serve as a watcher instead of a
program. It suits a source that a poll covers: a feed, a status page, an API with a cursor. It
cannot hold a connection: a module call has a 30 s limit (`modules.md` 8.1). Owner decision,
2026-11-06 (W3, section 12): module watchers get a stage of their own.

The `watch` trigger takes `module` in place of `argv`:

| Field | Required | Meaning |
|---|---|---|
| `watch.module` | Yes, instead of `argv` | The id of a module installed on the runner hab (`modules.md` 10.6) |
| `watch.every` | No | Time between calls: 30 s to 24 hours, default 5 minutes |
| `watch.args` | No | A JSON object, at most 4 KiB, passed to the module as `ctx.args`, such as a feed URL |
| `watch.kinds`, `secrets` | No | As for a program watcher |

`argv` and `module` are exclusive; `cwd` is for `argv` only.

**What the module provides.** `main.js` exports `watch(ctx)` beside `render` (`modules.md` 5),
and the manifest says so with `watch = true`.

- **The manifest field.** `watch` is a new manifest key, so it comes with the next bridge version
  (`modules.md` 5: an unknown key is refused; 9). A module that sets it names that bridge. The
  install and every update show "can serve as a round's watcher, and then receives the round's
  secret set" beside the powers; an update that adds `watch` counts as a change of powers and
  waits for the user's accept (`modules.md` 7.2). `round.create` checks the manifest field and
  the export.
- `ctx` holds the usual fields (`modules.md` 7.3) with `place` absent, since nothing is drawn,
  and `call` set to `watch`, plus `round` (the
  round id and name), `cursor` (the round's stored cursor, or absent), `args` (`watch.args`) and
  `secrets` (the secret set's variables as a frozen object of strings, or absent).
- It returns `{events: [...], cursor?: "<text>"}`, or a promise of it (`modules.md` 7.3). Each
  event has the fields of a program watcher's event line (title, kind, key, level, at, data),
  with the same bounds. At most 60 events per call; the rest are dropped and counted.
- Its time limit is `refresh`'s, 30 s. It may use the powers the manifest declares, except `act`:
  in `watch`, `fd.hab.act` throws `PowerError`. A watcher reports; it never acts. `fd.hab.get`
  keeps its read list. `fd.state.set` works as in `refresh`, in the module's one state store.
- The cursor belongs to the round, not the module, so one module can serve several rounds. Two
  rounds that use one module with different `watch.args` keep apart.
- A module with no `watch` export, or without `watch = true`, is refused at `round.create` and
  `round.update` (`invalid`, "module <id> is not a watcher"). So is a module not installed on
  the runner.

**How it runs.**

- **In the module host,** not in a pane. gff-hab asks the module host (`modules.md` 4) to call
  `watch` every `watch.every`, counted from the end of the last call. Calls on one module run one
  at a time with its card's calls (`modules.md` 7.3), so a slow card refresh can delay a watch
  call. The module host's restart, and gff-hab's, lose nothing: the cursor is on the round, and
  the next call starts at the interval from gff-hab's start.
- **No session.** A module watcher has no session and no pane. What it does shows on the round's
  page: the watcher's state, the last call's time, result (`ok` with an event count, or the
  error) and duration, and the last 20 lines its calls wrote with `fd.log`, which the module log
  (`modules.md` 8.3) also keeps. Reason: a module owns no PTY; the page shows what a pane would.
- **States.** `Round.watcher` reads `waiting` (with the next call's time), `calling`, `backoff`
  or `stopped`, in place of a program watcher's `running`.
- **Events** take the same path as a program watcher's lines: `kinds`, the `key` dedup, the rate
  bucket, then the batch (4.5). The cursor follows the same save rule (at most once per 10 s, and
  with each new run).
- **Failures.** A call that throws, returns a value of the wrong shape, or passes its limit is a
  failure; its events are dropped and its cursor is not kept. The next call follows the backoff
  of a program watcher (1 s doubling to 5 minutes, but never sooner than `watch.every`). Five
  failures in a row pause the round with the `paused` notice, its body naming the last error. A
  success clears the count. These counts are the round's; the module's own failure pause for its
  card (`modules.md` 8.3) is apart, and a paused card does not stop the watcher.
- **Sleep and wake.** A call due during sleep runs once after the wake settle (8.3), not once per
  missed interval. A call in flight at sleep ends by its limit at wake and counts as gff-hab's own
  hang-up: no failure.
- **A module removed, disabled or failing to load** stops the watcher: state `stopped`, with the
  reason. The round pauses with the `paused` notice at once. Reinstalling the module and
  unpausing the round starts it again. An update of the module takes effect at the next call; a
  change of its powers waits for the user's accept, as every module update does (`modules.md`
  7.2), and the watcher keeps calling the version it has until then.
- **Caps.** A module watcher counts toward `rounds.max_watchers` (16).
- **Secrets.** The set's command runs once per call, as for a program watcher's start; its
  output reaches `ctx.secrets` and nothing else. Reason: a module with `fetch` can send a secret
  anywhere, as a program can; the user picked both the module and the set.
- **Move.** `move` takes a module watcher round when the new runner has the module installed with
  the same id and a `watch` export; the create is refused otherwise and the move rolls back (9).

Decision: a module watcher is polled by the module host on the round's interval, with the cursor on
the round. Reason: it reuses the module runtime's limits and powers unchanged, and the program
watcher's event path, cursor and pause rules; only the calling loop is new. The round, not the
manifest, sets the interval, since the user tunes a watcher per source.

Decision: a module watcher has no pane. Reason: modules own no PTY (`modules.md` 4), and the
round's page shows what a pane would show for a program watcher: each call and its log lines.

One deviation from `swarm-study.md` 5 remains, for the owner to see: its "stops it on sleep"
becomes "restarts it at wake", since no sleep signal reaches gff-hab without cgo.

Decision: a watcher is a plain program in a pane, under a wrapper. Reason: the owner's rules want
every run visible and sessions that outlive updates; a pane gives both. A program can hold IMAP
IDLE, which a module call cannot. Reading a pipe keeps the terminal's line discipline out of
the event lines.

Decision: restart the watcher at every wake. Reason: there is no cheap sleep signal, and a stale
connection is the common failure after a sleep. The cursor makes a restart lose nothing.

### 4.5 Event batches

Hab events (4.3) and watcher events (4.4) reach a run the same way.

- **Debounce.** A first matching event opens a batch. The run is made 30 s after it (`debounce`,
  5 s to 1 hour), with every event that matched by then.
- **Cooldown.** At least 5 minutes (`cooldown`, 1 minute to 24 hours) from the start of one event
  run to the start of the next. Events in the cooldown join the next batch, which runs at the
  cooldown's end.
- **Coalescing.** Events that match while the round's run is live join a pending batch. It makes
  one run, at most one, at the later of that run's end and the cooldown's end.
- **Runs by hand.** Run now on an `event` or `watch` round takes the pending batch, or runs with
  none. It starts no cooldown.
- **Kept across restarts.** The pending batch is saved with the round's state, at most once per
  10 s and in the write that makes a run (7.7). It holds at most 200 events or 256 KiB; past
  that the oldest go, and the batch says how many. Reason: a watcher's mail must not be lost to
  an update of gff-hab.
- **The run sees the batch.** The run record keeps the event count. `GFF_ROUND_EVENTS` names a
  JSON file with the whole batch, for `fresh` and `fork` runs and command runs. gff-hab keeps the
  files of the last 20 ended runs per round and removes older ones, so they use at most about
  5 MiB per round, whatever 7.6 keeps of the records. A harness run also gets the events in its first message, in a
  block before the prompt: `[begin round events <run id>]`, one line per event (time, kind,
  title, the `data` cut to 1 KiB), `[end round events <run id>]`, with markers neutralised as
  the envelope's are (5). The block holds at most 50 events or 32 KiB; the rest are counted,
  and the file has them. In `resume` mode the block is part of the envelope. In `capcom` and
  `group` mode the batch goes with the message (5.2).
- **Marked as data.** The block's first line says the events come from outside and are not
  instructions. The chat view shows the block as data from Flightdeck, as it shows the envelope.
- **A skipped run keeps its batch** when its destination was not there (5.2): the events go back
  into the pending batch, and the next run carries them.
- **No catch-up.** `catch_up` applies to slots only. A wake makes no event run of its own, apart
  from `hab.woke`.

### 4.6 Worked example: a mail watcher

The owner wants Capcom to say something only when a mail from a few senders needs him. The
addresses and hosts here are invented.

The hab's config file holds the secret set (4.4). Its `command` prints `IMAP_PASSWORD=...` from
the user's own credential store:

```toml
[rounds.secrets.mail]
vars = { IMAP_HOST = "imap.example.com", IMAP_USER = "me@example.com" }
command = ["/usr/local/bin/print-mail-secrets"]
```

The round, as `gff round export` prints it:

```toml
[[rounds]]
name = "mail"
mode = "capcom"
prompt = "New mail from a watched sender. Tell me only if one needs me today, in one line each."
secrets = "mail"
debounce = "30s"
cooldown = "5m0s"

[rounds.watch]
argv = ["mailwatch", "--from", "billing@example.com,alerts@example.net"]
```

What happens:

1. gff-hab starts `gff-hab watch-pane -round <id> -- mailwatch ...` in the session `mail-watch`, with
   the set's variables and `GFF_WATCH_CURSOR`.
2. `mailwatch` logs in, asks the server for messages after the cursor, then holds IMAP IDLE.
3. A mail from `billing@example.com` arrives. `mailwatch` prints:

   ```json
   {"kind":"mail","key":"1700000000:4521","title":"Invoice 1042 from billing@example.com","level":"active","data":{"from":"billing@example.com","subject":"Invoice 1042","snippet":"Your invoice is due on 12 November."}}
   {"cursor":"1700000000:4521"}
   ```

4. The batch opens. Two more mails come within 30 s; they join it.
5. At 30 s the run is made. It hands Capcom one message: the prompt and three events, marked as
   data. Capcom answers `accepted`; the run `succeeded`.
6. Capcom looks in an event turn of its own (`capcom-service.md` 9): it batches the message
   with other events, and the small model takes the first look. Two mails are routine; it says
   nothing about them. The invoice is due soon; it writes one line to the owner, unless quiet
   hours hold it (`capcom-service.md` 9.2).
7. The laptop sleeps for the night. At wake gff-hab hangs `mailwatch` up and starts it again
   after 30 s. It reads the cursor, reports the night's four mails in one burst, and goes back
   to IDLE. One run hands Capcom the four.

What the example never does: open a port, store the password in the round, put a mail's text in
the prompt, or type into a session the owner uses.

---

## 5. Session modes

| Mode | What a run does | Native call |
|---|---|---|
| `fresh` | Starts a new session in the project with the harness, the model and the prompt. Or runs `command` | `session.create` |
| `resume` | Sends the prompt into the target session. A live target gets it under the lock (6). An exited target is resumed first, then gets it | `session.send`; `session.resume` when exited |
| `fork` | Starts a new session that branches from the target's conversation, then sends the prompt. The target is untouched | Claude Code `--resume <id> --fork-session`; Codex `codex fork <id>`; opencode *(unverified)* |
| `capcom` | Hands the prompt and the event batch to the Capcom service as a round message (5.2) | The round message of `capcom-service.md` 9.5 |
| `group` | Sends the prompt and the event batch to a group's coordinator as a channel message (5.2) | Channels (`mesh.md` 6), the coordinator from `groups.md` 12.5 |

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
| `GFF_ROUND_EVENTS` | An event run's batch: the path of a JSON file (4.5); absent for other runs |
| `GFF_WATCH_CURSOR` | The watcher only: the last cursor it printed (4.4); absent before the first |

`GFF_ROUND_LAST_OK` marks where the last good run left off. A prompt can say "triage what changed
since `$GFF_ROUND_LAST_OK`", so a run after a long sleep covers the whole gap and a repeated run
does no harm. In `resume` mode the variables are in the envelope, because a live session's
environment cannot change.

Decision: give the agent the last success time, not a stored cursor of its own. Reason: a time
covers most sweeps, needs no new storage, and makes catch-up once safe: one run sees every missed
change. A watcher is the exception (4.4): only its program knows its source's positions, such as
a mail server's message ids, so gff-hab stores the cursor the watcher prints, as opaque text.

### 5.2 Capcom and group modes

These two modes start no session of their own. A run hands one message to a destination: the
Capcom service (Part 14) or a group's coordinator (Part 15). The run is visible where the message
lands: in Capcom's thread, or in the coordinator's session. This section says only what rounds
send and what they expect back. The other side belongs to those parts' specs; the interface is
shared with them, and a change to it changes both specs.

**What the message holds**, in both modes:

| Field | Meaning |
|---|---|
| `round`, `name`, `run` | The round id and name, and the run id |
| `trigger` | `cron`, `once`, `event`, `watch` or `manual`, with the slot for a slot run |
| `prompt` | The round's prompt: what to do with the events, such as "tell me only if a mail needs me today" |
| `events` | The batch (4.5), at most 50 events or 32 KiB, with the count of the rest. Marked as data from outside |
| `level` | The highest `level` in the batch, or absent. A hint, never a push of its own |
| `last_ok` | `GFF_ROUND_LAST_OK` (5.1) |
| `actions` | `capcom` only, from stage A9: the round's `actions` (3), or absent. The home hab caps them by its level (`capcom-service.md` 9.6) |

- **Size.** The whole message fits the channel limit of 64 KiB (`mesh.md` 6.3); the event block is
  cut first, with the count of what it left out.
- **One message per run.** Its channel message id is derived from the run id (a hash of it), not
  drawn at random as `mesh.md` 6.2 does for an agent's send. The runner never sends a run's
  message twice under another id, so the receiver's dedup by message id (`mesh.md` 6.3) drops
  any repeat.
- **The sender.** The sender's name is `round:<round name>` on the runner's device id. It names
  no session. A reply to it is refused, "a round takes no replies"; the envelope gives no reply
  command.
- **The actor mark.** For the no-loop rule (4.3) the destination must be recognisable in what it
  writes. A coordinator is a session, so its session id serves. The Capcom service has a session
  record with no pane (`capcom-service.md` 2.1), and its id is the service's actor id: its
  Logbook ops carry it as the actor's session, its notices carry it as `session`, and the home
  hab's catalog `capcom` section carries it (`capcom-service.md` 9.5). That mark is part of this
  interface.

**`capcom` mode.**

- **Is there a service?** The Capcom home host publishes a `capcom-service` capability in its
  catalog entry while it holds the Capcom mark with the service set up (`mesh.md` 5,
  `capcom-service.md` 9.5). The runner reads it from its catalog
  copies. A `capcom` round is refused at creation (`unavailable`) while no catalog copy shows
  it. A run that finds no home host, or no `capcom-service` on it, is `skipped`, "no Capcom
  service", and its batch goes back to the pending batch (4.5).
- **The call.** The run hands the message to the service as a round message
  (`capcom-service.md` 9.5). The service answers once: `accepted` (stored; Capcom looks at it in
  an event turn of its own), `duplicate` (that message id was already accepted), or `declined`
  with a reason word from an open set (today `off`: the user turned Capcom's round messages off).
  Quiet hours and the daily turn budget never decline: the message is accepted and waits. The
  run waits for no turn.
- **The second answer** (owner, 2026-11-06; W1 in 12). After the turn that took the message,
  Capcom answers once more with an `outcome`: `told` (it wrote to the user), `quiet` (it did
  not), `acted` (from A9, it ran an action the round allows) or `unseen` (no turn took it)
  (`capcom-service.md` 9.5). It arrives as a status item after the final state, maybe hours
  later, through the outbox while the runner sleeps. The runner records it on the run as
  `outcome` with its time, and emits `round.run_updated`. The run's state never changes: an
  ended run gains this one field, once. A run with no outcome after 7 days shows "outcome
  unknown". The round's page shows it beside the state: "accepted by Capcom · told 07:02".
- **Where Capcom runs.** The service runs on the Capcom home host (`capcom.md` 7.3). On the
  runner the call is local. On a peer, the message travels as a channel message to the service
  (`mesh.md` 6), so the outbox holds it while that host sleeps. The home host's hab hands it to
  the service, and the service's answer goes back as the message's status item (`mesh.md` 6.3,
  control items), with three new final states for a message to the service: `accepted`,
  `duplicate` and `declined` (with its reason).
- **Run states.**

  | What happened | Run state | Batch |
  |---|---|---|
  | `accepted` or `duplicate` | `succeeded`, "accepted by Capcom" | Sent |
  | `queued` in the outbox for a sleeping home host | `succeeded`, "queued for Capcom on <label>". The outbox delivers it later (7 days at most) | Sent |
  | `handed`, then no answer by `timeout` | `timed_out` | Back to pending |
  | `declined` | `skipped`, with the reason | Dropped: Capcom chose not to look |
  | No home host, or no `capcom-service` | `skipped`, "no Capcom service" | Back to pending |
  | Refused by channels: no peer entry for the home host (`mesh.md` 6.3), outbox full | `skipped`, with channels' reason | Back to pending |
  | `refused` by the host: it no longer holds the Capcom mark ("Capcom moved to <label>") | `skipped`, with that reason | Back to pending |
  | `expired` after 7 days in the outbox | Nothing: the run ended long ago. The runner logs it | Lost |

- **What rounds leave to Capcom.** Quiet hours, the daily turn budget, batching several items into
  one turn, the small model for a first look, and whether to write to the user at all. Owner
  decision 2 (`swarm-study.md` 8): Capcom may write first about anything useful, with quiet hours.
  The round only delivers.
- **No pane fallback.** Owner decision 1 replaces the pane Capcom with the service outright, so a
  `capcom` round never targets the pane Capcom. Until the service runs, a `resume` round on the
  pane Capcom's session does the job under the lock.
- **No lock.** The service never counts as busy: messages queue (`swarm-study.md` 2.7).

**`group` mode.**

- **The coordinator.** The run reads the group's coordinator from the catalog `groups` section:
  its address (`label/name`) and the group's state (`groups.md` 5.1, 12.5). Any state but
  `ended` and `dropped` counts as running. Rounds read only those two facts, at the start of
  each run. A new coordinator
  (after a hand-off) gets the next run.
- **The message.** A channel message from the round to that address, in the round envelope (5)
  with the event block. It carries a new `round` mark (13): the round and run ids, and a
  deadline, the run's start plus `wait`, in UTC.
- **The lock on the far side.** A hab that delivers a message with the `round` mark applies the
  round lock (6.1) instead of channels' two-second rule. Reason: a coordinator may be the user's
  own session, and a round must never type into a session in use. A message still undelivered
  at its deadline, by the receiving hab's clock, ends `expired`, "in use: <reason>" when the lock
  held it, and is never delivered later. Two hosts' clocks may differ by a few seconds; at worst
  the coordinator then gets a message the run already gave up on, and the next run repeats its
  events.
- **Only to a hab that knows the mark.** The runner sends a `group` message only to a hab whose
  catalog entry lists the `round-lock` capability. An older hab would deliver under the two-second
  rule. The run is `skipped`, "the coordinator's hab is too old", and keeps its batch.
- **Run states.**

  | Message state (`mesh.md` 6.2) | Run state | Batch |
  |---|---|---|
  | `delivered` | `succeeded` | Sent |
  | `expired` at the deadline, the lock held it | `skipped`, "in use: <reason>" | Back to pending |
  | `queued` or `handed` at `wait` | `skipped`, "not delivered in <wait>: <state>". The far side drops it at the deadline | Back to pending |
  | `refused`: the coordinator is not running | `skipped`, "coordinator not running" | Back to pending |
  | `not_delivered`: written, with no sign the agent took it | `failed` | Dropped: it may have been read |
  | Refused by channels at send (no peer entry, outbox full) | `skipped`, with channels' reason | Back to pending |

- **Group ended.** A group that no longer runs completes the round, as a gone target does
  (10.1), with the `target_gone` notice, body "Its group ended."
- **The coordinator's turn** belongs to the group. The run does not wait for it.
- **Capcom as coordinator.** When the coordinator is the Capcom service (`groups.md` 12.1), the
  run hands its message over as a round message, with the `capcom` answers and run states
  above, and no lock (`capcom-service.md` 9.5).

Decision: a `capcom` run ends at Capcom's answer, not at its turn. Reason: quiet hours and the
budget can hold an item for hours, longer than any run timeout; the run's job is a durable
hand-over. What Capcom did shows in its thread.

Decision: both modes travel over channels, with the round lock on the receiving hab for a
`group` message. Reason: channels already cross hosts, queue for a sleeping host and deduplicate;
the lock is the one rule channels lack for a session the user may be typing in.

Interface shared with Part 14 (A1) and Part 15 (B1), in one place:

| Item | Owner | Rounds use |
|---|---|---|
| The round message and its answers `accepted`, `duplicate`, `declined` | Part 14, `capcom-service.md` 9.5 | Hand over a `capcom` message; map the answer (above) |
| The second answer `outcome`: `told`, `quiet`, `acted`, `unseen` | Part 14, `capcom-service.md` 9.5 | Record it on the run (above) |
| The `actions` of a Capcom job, and the level that caps them | Part 14, `capcom-service.md` 9.6 | Store the field (3); send it in the message |
| The `capcom-service` catalog capability | Part 14, `capcom-service.md` 9.5 | Refuse at create; skip a run |
| The service's actor id (its session id) on its notices and Logbook ops | Part 14, `capcom-service.md` 9.5 | The no-loop rule (4.3) |
| The group's coordinator address and state in the catalog `groups` section | Part 15, `groups.md` 5.1, 12.5 | Address a `group` message; complete the round |
| The `round` mark and the `round-lock` capability | Rounds (stage 9), in channels | Deliver under the lock |
| The `watch(ctx)` export, the manifest's `watch = true` with its bridge version, `ctx.call`, `act` refused in `watch`, watch calls going on while the module's card is paused after errors, the secret set in `ctx.secrets` and its line at install | Part 7, `modules.md` 5, 7.1, 7.2, 7.3, 8.3, 9; written here (4.4), and stage 12 adds them to `modules.md` | Module watchers (4.4) |

---

## 6. The lock: never type into a session in use

The lock matters in `resume` mode, the only mode that writes into an existing live session on the
runner. A `group` message gets the same lock on the coordinator's hab (5.2).

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
| `waiting` | `resume`: waiting for the lock (6.3). `group`: waiting for the message's delivery (5.2) |
| `running` | The session is live and the agent works on the run's turn. `capcom`: the hand-over is under way |
| `blocked` | The agent waits for the user: a permission, a question or a sign-in |
| `succeeded` | The turn ended `done`, or `command` exited 0. `capcom`: accepted, a duplicate, or queued for the home host; `group`: delivered (5.2) |
| `failed` | Spawn failed, the turn ended `error`, the harness exited before the turn ended, or `command` exited non-zero |
| `timed_out` | The run passed `timeout` |
| `skipped` | The lock never cleared, the project folder is missing for one run, or a `capcom` or `group` destination was not there or declined (5.2) |
| `taken_over` | The user typed into the run session (7.5) |

### 7.2 Start

- **One run per round at a time.** A slot that comes due while a run is live is coalesced into
  the next run after it ends, at most one.
- **Host limit.** At most 2 runs start at once per host (`rounds.max_running` in the hab's
  config). Others wait as `queued`, oldest slot first; equal slots go by when the run was made.
  Built rule: every live run that is not `queued` holds a place, `waiting` and `blocked`
  included. Reason: a blocked run still holds a session and an agent.
- **What holds no place.** A `capcom` or `group` run and a watcher hold no place under the host
  limit, and do not wait for one. Reason: they start no harness on the runner; the limit guards
  the host's agents. A `group` run's delivery is still one at a time per pane on the far side.
- **Event runs.** An event run is made when its batch is due (4.5): it carries no slot, its
  trigger kind is `event` or `watch`, and it takes the batch. It then waits for the host limit
  and the wake settle as a slot run does; a `capcom` or `group` run waits for the settle only.
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
- **A `capcom` or `group` run ends** at its destination's answer (5.2). A `capcom` run that has
  no answer at its `timeout` ends `timed_out`, and its batch goes back to the pending batch. A
  `group` run ends at `wait` or earlier (5.2), so its `timeout` matters only when set shorter.
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
  slot, its failure and skip counts, and its run records. For `event` and `watch` rounds it
  also holds the pending batch (4.5), the watcher's cursor and its last 1,000 dedup keys (4.4).
  The batch files of `GFF_ROUND_EVENTS` live beside it in `<state>/rounds/events/`. It is saved whole through a synced
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
  final agent message. Logbook gets the same fields. An event run adds its event count; a
  `capcom` or `group` run adds the destination and, for `group`, the channel message id.
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
- **Watchers and destinations.** A `watch` round's watcher runs on its runner, and an `event`
  round hears only its runner's events (4.3). A `capcom` or `group` round may run on any hab: its
  message reaches Capcom's home host or the coordinator's hab over channels (5.2). A mail watcher
  on a laptop works: its cursor reports at wake what came during the sleep (4.4). The app's
  always-on line applies to `watch` rounds too, since a sleeping runner hears nothing.
- **Moving these rounds.** `move` takes `capcom`, `group` and `event` rounds, and `watch` rounds
  whose `secrets` set the new runner's config holds and, for a module watcher, whose module it
  has installed with a `watch` export (4.4); the new runner refuses the create
  otherwise, and the move rolls back. A `capcom` or `group` round names no project, so the
  remote rule does not apply to it. An event filter that names a session cannot move (the
  session lives on one host); one that names a project names it by remote in the move, as the
  round's project is. The move carries the cursor and the pending batch in its create, as it
  carries the pause mark; the dedup keys stay behind.

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
- **Event and watch rounds** follow the same counts. A watcher's exits are not runs: they count
  only toward the watcher's own pause (4.4). A `capcom` or `group` destination that is not there
  is a skip, so 5 in a row raise the skips notice ("Capcom's service is not running").
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
| Round completed because its target is gone, or its group ended | Yes | Passive |
| Round paused because its watcher keeps exiting (4.4) | Yes, the `paused` row with the last exit | Active |

- `notify = never` silences every row except the self-pause.
- Each round's notifications collapse by round id, so a failing hourly round shows one banner,
  updated.
- An event run's notices are the same rows. The run's batch is not in the banner; the kept
  notice's body gives the event count and the first event's title, under the read scope rule of
  stage 6 (13).
- "Ask an agent" on a round notification works as for any notification (`capcom.md` 8.2): the
  event, with the run's result, is attached to the Capcom conversation.

### 10.3 The status board

- A **Rounds** list per host: name, mode, trigger, next run in the viewer's zone, last run state,
  paused or completed.
- Run sessions carry a round badge on their cards. A filter hides or shows them.
- A round's page shows its run history: slot, state, `missed`, duration, the first line of the
  result, and a link to the session.
- Actions: Run now, Pause or Unpause, Edit, Delete. Delete asks first.
- An `event` or `watch` round shows its trigger in plain words (`on logbook.updated`, `watch
  mailwatch`), its pending event count, and for a watcher its state (`running`, `backoff` until
  <time>, `stopped`), restarts, the drop count and the last exit. A link opens the watcher's pane.
  A module watcher shows `watch module <id>`, its state (`waiting` until <time>, `calling`,
  `backoff`, `stopped`), the last call's result and its log lines, with no pane link (4.4).
  Run now on an event round runs with the pending batch, or with none (4.5).
- A `capcom` run shows Capcom's second answer beside its state ("accepted by Capcom · told
  07:02", 5.2). A filter shows only `capcom` rounds: the Capcom jobs list with their `actions`
  (`capcom-service.md` 9.6, stage A9).

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
| A watcher prints a thousand lines in a second | 60 make the batch, the rest are dropped and counted; one run follows the debounce (4.4, 4.5) |
| A watcher prints its password to stderr | It shows in the watcher's pane, as any program's output does. gff-hab never logs the secret set's values (4.4); what the program prints is the program's business |
| gff-hab restarts while a watcher holds IMAP IDLE | The watcher keeps running in its pane; the wrapper holds its lines until gff-hab answers (4.4) |
| The laptop sleeps for a night with a mail watcher | At wake the watcher is restarted after the settle, reads `GFF_WATCH_CURSOR`, and reports the night's mails in one burst: one run (4.4, 4.5) |
| A `capcom` round fires while Capcom's home host sleeps | The message waits in the outbox; the run succeeds, "queued for Capcom on <label>" (5.2) |
| A `capcom` run times out, but Capcom had accepted it | The batch goes back to the pending batch, so the next run may repeat events Capcom already has. Capcom's own dedup by event `key` is its call |
| A `group` round's coordinator is the user's own session, and the user is typing | The coordinator's hab holds the message under the round lock; past `wait` the run is `skipped`, "in use" (5.2) |
| A Logbook round on `created` tasks, whose own run files a task | The run session's op never triggers the round (4.3). A task filed by another agent in reaction does; the cooldown and the failure pause cap such a chain |
| A notice round whose run raises a notice | A notice whose `round` field names this round never triggers it, nor one whose `session` is the run session (4.3). A notice the run raises with no session does trigger it once; the cooldown spaces such a chain and the failure pause caps a broken one |
| An event names a record in a private project, and the run is in another project | The batch says "1 record in a private project", with no id or title (4.3) |
| A watcher round is moved to another host | The watcher starts there with the cursor and the pending batch the move carried; the dedup keys stay behind, so the `key` cannot drop a repeat of an event from just before the move (9) |

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

**Owner decisions on watchers** (2026-11-06, plan-xagejg31 (q)). These were W1 to W3 of the
watcher study; the owner answered them.

| # | Question | Decision | Where |
|---|---|---|---|
| W1 | Should Capcom report back what it did with a round's message, so the round's history shows it? | Yes (owner, 2026-11-06). Capcom answers each round message a second time, `told` or `quiet` (and `acted` or `unseen`), recorded on the run | 5.2; `capcom-service.md` 9.5 |
| W2 | May an event with `level = time-sensitive` let Capcom write during quiet hours? | Yes (owner, 2026-11-06). Such a round message runs at once in quiet hours. Rounds pass the level either way | `capcom-service.md` 9.2 |
| W3 | Are module watchers worth a stage of their own? | Yes (owner, 2026-11-06): stage 12 | 4.4, 14, 14.1 |

The study's options and recommendations for Q1 to Q3 follow, for the record.

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
| `RoundSpec.trigger` | Field values | Kind `event` with `event` (an event name of 4.3) and `filter` (an object of that event's filter fields); kind `watch` with `watch` (`argv`, `cwd`, `kinds`). Stage 9, stage 11 |
| `RoundSpec` | New fields | `group`, `debounce_ms`, `cooldown_ms`, `secrets`; modes `capcom` and `group` |
| `Round.pending`, `Round.watcher` | Read-only fields | The pending event count; the watcher's state (`starting`, `running`, `backoff` with its next start, `stopped`), restarts, drops since start, last exit, its session id |
| `RoundRun.events`, `RoundRun.destination`, `RoundRun.message` | Read-only fields | An event run's count; a `capcom` or `group` run's destination and channel message id |
| `RoundRun.outcome` | Read-only field | A `capcom` run's second answer: `told`, `quiet`, `acted` or `unseen`, with its time; set once, after the run ended (5.2). Stage 9 |
| `RoundSpec.actions` | New field | `capcom` rounds only: the action words of `capcom-service.md` 9.6. Set only by a person (3). Stage A9 |
| `RoundSpec.trigger` `watch.module`, `watch.every`, `watch.args` | Field values | A module watcher (4.4), in place of `argv`. Stage 12 |
| `Round.watcher` states `waiting`, `calling` | Words | A module watcher's states, beside `backoff` and `stopped` (4.4). Stage 12 |
| Message status item `outcome` | Word (`mesh.md` 6.3) | Capcom's second answer to a round message (5.2). Stage 9 |
| `Session.round.watcher` | Optional field | True on a watcher's session (4.4) |
| `round.watch_event` | Method, pane route only | The wrapper's call: a page of event and cursor lines from the watcher pane that names the round. Answers which were taken and which dropped, with reasons. Any other pane is refused with `denied` |
| `hab.woke` | Event | A detected wake (4.3), with `slept_s`. Host scope |
| Channel message `round` | Optional field | Round id, run id and the deadline. The target hab delivers it under the round lock and expires it at the deadline (5.2) |
| `round-lock` | Capability | The hab honours the channel `round` mark (5.2) |
| `capcom-service` | Capability (Part 14) | The Capcom service runs on this hab (5.2) |
| Message states `accepted`, `duplicate`, `declined` | Words | Final states of a channel message to the Capcom service (5.2) |
| `logbook.history` `folded_after` | Optional parameter | Ops this replica folded after a local fold number (4.3) |
| `[rounds] max_watchers`, `[rounds.secrets.<name>]` | Hab config | The watcher cap (default 16) and the secret sets (4.4) |
| notice `round.kind` `paused` | Body | Gains the watcher's last exit when the watcher's pause raised it (10.2) |

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
| `gff round move <name> --host <label>` | Move a `fresh`, `capcom` or `group` round, or an event round, to another runner (9) |
| `gff round export`, `gff round apply <file>` | Print as TOML, or make the hab match a file (3) |
| `gff round add <name> --mode capcom\|group [--group <id>] ... <prompt>` | Create a round whose runs message Capcom or a group's coordinator (5.2) |
| `gff round add <name> ... --on <event> [--where key=value]... [--debounce d] [--cooldown d]` | Create an event round (4.3). Each `--where` is one filter field; a list is comma-separated (`--where status=done,dropped`) |
| `gff round add <name> ... --watch [--secrets <set>] [--kinds k,...] [--watch-cwd dir] -- <program> [args...]` | Create a watch round (4.4). The words after `--` are the watcher's argv |
| `gff round add <name> ... --watch-module <id> [--every <d>] [--watch-args <json>] [--secrets <set>] [--kinds k,...]` | Create a round with a module watcher (4.4). Stage 12 |
| `gff round add <name> --mode capcom ... [--actions w,...]`, `gff round edit <name> --actions w,...` | Set the action words of a Capcom job (3; `capcom-service.md` 9.6). Refused from a pane when it grants a word. Stage A9 |
| `gff-hab watch-pane -round <id> -- <program> [args...]` | What a watcher's pane runs (4.4). Not for people: gff-hab starts it |

Export and apply carry the new fields: `[rounds.trigger]` holds `on` and its `where` table, or a
`[rounds.watch]` table holds `argv`, `cwd` and `kinds`, or `module`, `every`, `args` and `kinds`;
`secrets`, `debounce`, `cooldown`, `group` and `actions` are plain keys. `apply` of a file that
grants `actions` is a person's write only, as `round.update` is (3). A group named by id cannot move between meshes; apply refuses a `group`
that no catalog copy lists (`groups.md` 5.1).

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
  (the detection manifest format; the vendored Claude manifest has no rule for the dialog). The
  rule lives in `internal/harness`, shared with the trust card (`app.md` 7.10): the pane also
  shows `blocked:trust`, and the owner may answer from the card with `trust.answer`, which is
  not a take over.
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
- **Exited target.** gff-hab starts the target's harness again itself, from what the round
  remembers of its target, as Capcom's resume does (`session.resume` was built for Capcom only
  when rounds came; it now serves every session from the resume book, `gff-client.md` 6.2): the exited pane is released, then
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

Built (stage 7, task-xhpsgzkr): rounds in the Flightdeck app, with no hab change. The app's
choices are in `app.md` 21: the Rounds list per hab and its order, the round page, the form
and the always-on line, the badge and the filter, Open on a round banner, and the app-side
filter that keeps focus reports, mouse motion and the wheel out of a live run's pane. The
catalog's other hosts are not listed yet (`app.md` 21.1); the `gff` client is stage 8.

Built (stage 8, task-cpazgcsj): rounds in the `gff` client, with no hab change. The client's
structure copy (`internal/habconn`) keeps the snapshot's rounds and follows `round.updated`
and `round.run_updated`. Run sessions carry `↻`, and `R` (session mode, sidebar, board) hides
those that do not need the owner. The board lists every host's rounds in one list in
next-run order, with Run now (`r`) and Pause or Unpause (`p`). A host that is not online
shows its last rounds, stale: its last snapshot's, or a newer copy from another hab's
`catalog.list`. The trigger words moved to `rounds.TriggerWords`, so the catalog section, `ls
-all` and the board share them. The calls are in `gff-client.md` 7.5.

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
| 9 | Event triggers, `capcom` and `group` modes (the swarm study's D2) | The six events in 4.3 with their filters, `hab.woke`, the event batch (4.5), no self-loops; the `capcom` and `group` modes (5.2); the channel `round` mark and the lock on the receiving hab | task-awkgh5h1 |
| 10 | Agent kit | `gff round` verbs and MCP tools; a round on another host under `confirm.start_elsewhere` | task-ybb4c3ed |
| 11 | Watchers (the swarm study's D3) | The `watch` trigger: `gff-hab watch-pane`, supervision, backoff, the exit pause, restart at wake, event lines, the cursor, dedup keys, secret sets; `--watch`, export and apply; the watcher's state on the round; a mail watcher example | task-qhdhg0x2 |
| 12 | Module watchers (owner, 2026-11-06, W3) | `watch.module`, `every`, `args`; the manifest's `watch = true` (a new bridge version) and its install line; the `watch(ctx)` export and `ctx.call` in the module runtime and the module host's call loop; the `modules.md` changes of the 5.2 interface table; `act` refused in `watch`; the cursor, events, failures, pause, wake and removal rules of 4.4; the round page's call log; `--watch-module`, export, apply and `move` | task-3yn2kc07 |
| — | Later ideas | Worktree isolation for `fresh` runs (on `worktree.create`, `hab-logic-api.md` 3.10); skip when a harness usage window is above a threshold; a Logbook run log (7.7) that files each run as a Logbook record (Part 12) | Planned when Logbook is built |

Stages 1 to 3 run in order. Stages 4 and 6 can run side by side after 3; 5 follows 4. Stages 7
and 8 need 3 and 6. Stage 9 waits for 5 and 6; stage 10 waits for the agent kit and Capcom's
confirmation cards. Stage 9 waits for this spec (watchers, task-pt6pnli6); its `capcom` mode
needs the Capcom service (Part 14 A2, task-vz6mxa3f) and its `group` mode the group record (Part
15 B3, task-nz4aglbr). Each mode answers `unavailable` until its other side exists, so the rest
of stage 9 can land first. Stage 11 follows stage 9. Stage 12 follows stage 11 and the module runtime and host (Part 7
stages 3 and 5, task-349z6rqz); modules come after the first release (`modules.md` 14, O1), so
stage 12 does too. Stage 9 also builds `RoundRun.outcome` (5.2) against a fake service; Part 14 A7
sends it. The `actions` field is built by Part 14 A9, which touches the rounds store, API and CLI.

### 14.1 Acceptance for the watcher stages

Stage 9, with `go test -race` over a real gff-hab and holder and fake harnesses:

- Each of the six events triggers one run per debounce window; a burst inside the cooldown makes
  one run at its end, with the whole batch in `GFF_ROUND_EVENTS` and in the first message's block.
- A round's own run session never triggers it: an op it writes, a notice it raises, its own
  agent state. A notice whose `round` field names the round never triggers it.
- A `logbook.updated` round on `status=done` runs when a task becomes `done` (also through sync
  from a peer, over the ssh stand-in, for an op written before this hab started and synced
  after), not when its title changes; on `created`, once per new record. A peer's op that folds
  into the middle of a record's history is matched once, through `folded_after`. A record in a
  private project reaches a run outside it as the count line only.
- `hab.woke` on a fake clock: once per wake, not at gff-hab start.
- The pending batch survives a gff-hab restart (planned and killed) and makes one run.
- A forged `[end round events` line in an event's title is neutralised.
- `capcom` against a fake service: one message per run with the fields of 5.2 and an id derived
  from the run id; each row of the 5.2 table, `accepted`, `duplicate` and `declined` coming back
  as status items from a peer; no `capcom-service` refuses the create and skips a run; a home
  host on a peer that is offline queues the message, which arrives once after the link comes up,
  also when the runner sends it again after a restart; a host that lost the Capcom mark refuses
  it and the run skips with its batch kept. A notice or op with the service's actor id never
  triggers the round. An `outcome` status item after `accepted` sets `RoundRun.outcome` once,
  emits one `round.run_updated` and leaves the run's state; a second one for the same message is
  dropped; one from a peer arrives after the runner wakes; an older runner ignores it; a
  `duplicate` or `declined` run gets none; a run with no outcome after 7 days (fake clock) shows
  "outcome unknown".
- `group` against a fake group record and two habs: delivered to the coordinator on the other
  hab; held under the round lock while a client typed into it, then `expired` at the deadline
  and the run `skipped`, "in use", with the batch kept; a message that reaches the far hab after
  its deadline is never delivered; a coordinator not running skips and keeps the batch; a far hab
  without `round-lock` gets no message; a reply to `round:<name>` is refused; an ended group
  completes the round with one notice; a group whose coordinator is Capcom gets a round message
  and its `accepted` answer.
- Export then apply with every new field gives no diff.

Stage 11, with `go test -race` over a real gff-hab and holder and fake watcher programs:

- A watcher runs in the session `<round>-watch` with `Session.round.watcher`; its stderr shows
  there; the session holds no place under the host limit; each restart is a new pane in that
  session; Resume on it is refused.
- A crash restarts it after 1 s, then 2 s, 4 s; 5 quick exits pause the round with one notice
  naming the last exit; a watcher that ran past the reset time starts again at 1 s.
- A fake clock wake hangs the watcher up and starts it once after the settle.
- A gff-hab restart leaves the watcher's process (same pid) running, with a holder that connects
  late too; lines printed meanwhile arrive once after the restart. gff-hab's own hang-ups (wake,
  edit) start no backoff and count toward no pause.
- A burst of lines makes one run; a malformed, untitled, oversize or over-rate line is dropped,
  shown in the pane and counted; a repeated `key` is dropped quietly.
- The cursor reaches the next start as `GFF_WATCH_CURSOR`, also after a gff-hab restart.
- A secret set's `vars` and `command` output reach the watcher's environment; neither appears in
  gff-hab's log, the rounds store, the export, the catalog, the session record or the resume
  book. A missing set is refused at create.
- Pause, delete and an edit of `watch` stop or restart the watcher; an edit of the prompt does not.
- The mail watcher example against a local fake IMAP server (no real mail service): one new mail
  from a listed sender makes one event line; a mail from another sender makes none; a restart with
  the cursor reports a mail that came while it was stopped, once.
- Live check, after Part 14 A7 (task-hj8cnr5v): a mail reaches Capcom's thread as one round
  message.

Stage 12, with `go test -race` over a real gff-hab, its module host and hand-written module
bundles (in the style of `internal/modhost/testdata/bundles/`), on a fake clock where it says so:

- A round with `watch.module` calls the module's `watch` every `every` (fake clock), counted from
  each call's end; a call's events make one run through the batch rules; `ctx` carries the round,
  `args`, the stored cursor and the secret set's variables.
- The returned cursor reaches the next call, also after a gff-hab restart and a module host
  kill; a call that fails keeps the old cursor and drops its events.
- A module with no `watch` export, without `watch = true`, or not installed, is refused at
  create (`invalid`); `argv` with `module` is refused. An update that adds `watch = true` waits
  for the user's accept, and the install shows the watcher line.
- `fd.hab.act` in `watch` throws `PowerError` even with the `act` power; `fetch` and `exec` work
  when declared; a declared power reaches its backend.
- A call that throws, returns the wrong shape or loops past 30 s is a failure: the backoff
  doubles from 1 s but never comes sooner than `every`; five in a row pause the round with one
  `paused` notice naming the error; a success clears the count. The module's card pause does
  not stop the watcher.
- More than 60 events in one call: 60 taken, the rest counted; a bad event (no title, too long,
  oversize `data`) dropped and counted; a repeated `key` dropped quietly.
- Fake clock: a sleep over ten intervals makes one call after the wake settle; a call in flight
  at sleep ends at its limit and counts no failure.
- Removing the module stops the watcher and pauses the round with one notice; reinstalling and
  unpausing starts it again. A module update with a new power keeps the old version calling
  until the user accepts.
- Two rounds on one module with different `args` keep separate cursors and batches.
- The round page's data (`round.get`) shows the state, the last call's result and its `fd.log`
  lines; the secret set's values appear in no log, store, export, catalog or call log.
- `move` to a hab without the module is refused and rolls back; export then apply gives no diff.

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
- Watchers (2026-11-06): `swarm-study.md` 2.6, 2.7, 5 and 8 (owner decisions 1 and 2);
  `logbook.md` 2.2, 3.3 and 10.5 (the `logbook.updated` fields, private projects); `mesh.md` 6.2
  and 6.3 (delivery, outbox, dedup, the 64 KiB limit); `capcom.md` 7.3 and 8.5;
  `hab-logic-api.md` 3.9 (`notice.raise`); `modules.md` 7 and 8.1 (the call limits that rule out
  a module holding a connection). systemd: `systemd.service(5)` `Restart=`, `RestartSec=`, and
  `systemd.unit(5)` `StartLimitBurst=` (default 5). launchd: `launchd.plist(5)` `KeepAlive`,
  `ThrottleInterval`. IMAP IDLE: RFC 2177.

---

## 16. Open questions: watchers

None open. The owner answered W1 to W3 on 2026-11-06; they are in section 12.
