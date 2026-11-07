# Capcom service: behaviour spec

Status: draft for review. Part 14, study stage A1. The owner's answers of 2026-11-06 are applied
(15, E19 to E25; every limit is a setting, 19).

Part 14 turns Capcom from a harness in a holder pane into an always-on service. `gff-hab-capcom`
runs an agent loop on the Claude Agent SDK as its own job on the home host. The user talks to it
in one native chat thread in the app, and never manages a conversation: the engine keeps its own
context, and keeps memory in Logbook. A Codex SDK engine comes later behind the same contract.

This spec extends `capcom.md`. Capcom's jobs (`capcom.md` 3), what it reads (4), the read scope
(5), confirmations and proposals (6), the home host (7.3) and attached context (8) stay as they
are, unless a section here says otherwise. Section 13 lists what changes in `capcom.md`. It
describes behaviour in our own words and copies no code.

| Piece | Defined in |
|---|---|
| Capcom's jobs, read tools, read scope, confirmations, home host, attached context | `capcom.md` |
| The study this spec settles, and the owner's nine decisions | `swarm-study.md` 2, 8 |
| Channels, names and addresses | `mesh.md` 6 |
| Pane token route (`GFF_HAB`), agent state, the agent kit | `harness-adapters.md` |
| Uploads, the Capcom view, approvals, push, usage, settings | `app.md` 4.4, 4.10, 7, 8, 14, 20 |
| Logbook records, proposals, the brief | `logbook.md` 2.2, 9, 11.2 |
| Rounds in `capcom` mode, their message and its answers | `rounds.md` 5.2 |
| Agent groups, with Capcom as a coordinator | `groups.md` 12.1 |

Owner decisions this spec builds on (plan-xagejg31 (p), 2026-11-06; `swarm-study.md` 8). Each is
cited as "decision n". They are settled, not open:

| # | Decision |
|---|---|
| 1 | The service replaces the pane Capcom outright. No fallback release |
| 2 | Capcom may write first about anything useful, with quiet hours |
| 3 | The app shares location in the background. Capcom reads it only when a question needs it. It never enters the context by default |
| 4 | Places and weather come from Apple (WeatherKit, Apple Maps Server API). Google only with the owner's own key |
| 5 | A calendar write is a card with one "Add" tap. Never silent |
| 8 | Summarised context shows as a quiet divider in the thread |

Decisions 6, 7 and 9 concern agent groups and iMessage; this spec does not use them.

## Contents

1. [References](#1-references)
2. [Model](#2-model)
3. [The engine](#3-the-engine)
4. [The engine contract](#4-the-engine-contract)
5. [Tools and approvals](#5-tools-and-approvals)
6. [Context and memory](#6-context-and-memory)
7. [The thread](#7-the-thread)
8. [Personal tools](#8-personal-tools)
9. [Capcom writes first](#9-capcom-writes-first)
10. [Usage and terms](#10-usage-and-terms)
11. [Plain-terminal resume](#11-plain-terminal-resume)
12. [Migration from the pane Capcom](#12-migration-from-the-pane-capcom)
13. [Changes to other specs](#13-changes-to-other-specs)
14. [Edge cases](#14-edge-cases)
15. [Decisions](#15-decisions)
16. [Open questions for the owner](#16-open-questions-for-the-owner)
17. [Owner approval needed: AGENTS.md exception](#17-owner-approval-needed-agentsmd-exception)
18. [Build stages](#18-build-stages)
19. [Limits and settings](#19-limits-and-settings)
20. [Sources](#20-sources)

---

## 1. References

### 1.1 The Claude Agent SDK

Read 2026-11-06 (`agent-sdk/overview`, `agent-loop`, `streaming-vs-single-mode`, `sessions`,
`permissions`).

| Fact | What it means here |
|---|---|
| A library for TypeScript and Python that runs the Claude Code binary. Both bundle a native binary | The engine is a sidecar, not Go. No separate Claude Code install |
| Streaming input mode: one long-lived process, queued messages, interrupt, images in messages, permission requests surfaced | The engine's only mode. Single message mode has no queue, no interrupt and no images |
| A user message streamed mid-loop is emitted back as a `UserMessage` | The read mark (7.3) |
| `ResultMessage` ends each loop: result, usage, cost, session id. A streaming session stays alive after an error result, except a crash | One result per turn. A crash restarts the engine (3.4) |
| Automatic compaction emits `compact_boundary`. A `PreCompact` hook runs first | The safety net (6.3) |
| Permission order: hooks, deny rules, ask rules, mode, allow rules, `canUseTool`. A tool named in a deny rule is removed from the request | Built-in tools are denied by name; the rest reach `canUseTool` (5.2) |
| `default` mode sends unresolved calls to `canUseTool`; with no callback they are denied | The approval card path (5.3) |
| Sessions are stored at `$CLAUDE_CONFIG_DIR/projects/<encoded folder>/<id>.jsonl`, as the CLI stores them. Since CLI 2.1.223 a resume by id searches beyond the current folder | Plain-terminal resume (11) |
| `maxBudgetUsd` stops a loop at a spend; `effort` and `model` are per query | Guardrails (10) |
| Observability events include rate-limit status | Usage readings for the governor (10.1), to verify in A2 |
| The TypeScript SDK has hook events Python lacks, and `persistSession` | TypeScript is the engine's language (15, E1) |

### 1.2 Terms

`legal-and-compliance`, the SDK overview and the support article 15036540 (read 2026-11-06):

- Plan limits "assume ordinary, individual usage of Claude Code and the Agent SDK".
- The SDK, signed in with a plan, "still draw[s] from your subscription limits".
- `claude setup-token` makes a long-lived OAuth token for the user's own subscription. Claude
  Code and the SDK's bundled binary read it from `CLAUDE_CODE_OAUTH_TOKEN`.
- An API key from the Claude Console works too, billed per token.

The owner researched the terms and holds that a user running Capcom on their own subscription,
through the token they made themselves, is fine (owner, 2026-11-06; plan-xagejg31 (q)). That is
the default login (3.5, 10.2). Flightdeck offers no Claude.ai sign-in of its own and never handles
anyone else's credentials.

### 1.3 The Codex SDK

Read 2026-11-06 (`codex-sdk`). TypeScript and Python. Threads start, continue, resume and fork by
thread id. Sandbox presets per turn. The page documents no approval callback, no turn events and
no interrupt. So the engine contract (4) must work for an engine that lacks them: it declares what
it has, and the hab adapts.

### 1.4 What we take

| From | We take | We change |
|---|---|---|
| The swarm study (2) | A sidecar engine, streaming input, tools through `gff-hab mcp`, cards for approvals and phone writes, a rolling summary and re-seed | The summary updates when the thread goes quiet, not after each turn (6.1). Device context is not sent with each message (decision 3, 8.1) |
| Chat apps | Typing dots, delivered and read marks, a communication notification with an inline reply | Tool calls fold into one quiet line |
| The pane Capcom (`capcom.md`) | Home host, read scope, confirmations, hand-off, attached context, the manual | No pane, no harness setting, one thread per home host |

---

## 2. Model

### 2.1 What Capcom is now

Capcom is one service per home host: the engine job, the thread the hab keeps, and a session
record of kind `capcom` with no pane.

| Part | Where | Owns |
|---|---|---|
| Engine (`gff-hab-capcom`) | Its own init job on the home host | The SDK session, turns, the summary, model calls |
| Thread | gff-hab on the home host, `<state>/capcom/thread/` | What the user sees: messages, cards, dividers, read state |
| Session record | gff-hab, kind `capcom`, no pane | The board row, agent state, channel name, the SDK session ids |
| Memory | Logbook, on every hab | Facts and preferences, tasks and findings |

The hab is the hub. Clients talk to the hab, never to the engine. The engine talks only to the
hab, through its route (3.3), and to the model provider.

### 2.2 Words

| Word | Meaning |
|---|---|
| engine | The program that runs Capcom's agent loop. `claude` (the Claude Agent SDK) now, `codex` later |
| thread | The one conversation the user sees with Capcom, kept by the hab. It never ends |
| item | One entry in the thread: a message, a card, a tool line, a divider, a note |
| SDK session | One session of the engine's SDK. The thread spans many |
| turn | One run of the agent loop, from the first input it takes to its result |
| user turn | A turn that takes at least one message the user wrote |
| event turn | A turn that takes only events: notices, round messages, round results, agent reports |
| round message | One run's message from a round in `capcom` mode: its prompt and event batch (9.5) |
| re-seed | Ending the SDK session and starting a fresh one from the summary (6.2) |
| summary | The engine's short account of the thread so far, kept by the hab |
| divider | A quiet thread item that marks a re-seed, a compaction or a new conversation |
| personal tools | Tools only Capcom gets: location, calendar, weather, places, cards (8) |
| device context | What the app shares with the home hab: location, time zone, calendar (8.1) |

### 2.3 What stays

- **One home host.** The user assigns it and moves it (`capcom.md` 7.3, 7.6). The engine runs
  there and only there.
- **The four jobs** (`capcom.md` 3): answer, hand off, suggest and start, file.
- **Read tools and read scope** (`capcom.md` 4, 5). The engine reads every host within each
  host's scope.
- **Confirmations** (`capcom.md` 6.1). `confirm.start_elsewhere` governs starts as before.
- **Proposals** (`capcom.md` 6.2). Tasks and findings Capcom files wait for review; 6.4 says
  when review opens. Facts and preferences land at once, in any turn, except at Safe (6.4,
  E20).
- **Attached context and "Ask an agent"** (`capcom.md` 8). The blocks now reach the engine as
  message content, not as a paste.

### 2.4 The channel name

- The service session's name, and so its channel name, is `capcom` (`mesh.md` 6.1). Agents
  reach it as `capcom` or `<home label>/capcom`.
- **Reserved.** Only the record that holds the Capcom mark is named `capcom`. When a hab loses
  the mark, it renames its service record `capcom-earlier-<n>` (the lowest free `n`) in the same
  step that clears its standing pointer (`capcom.md` 7.6). At A8, pane-era records named
  `capcom`, `capcom-2` and on are renamed `capcom-term-<n>` (12.3).
- **The clash check** (`mesh.md` 6.1) does not apply to `capcom` between habs: a stale peer copy
  that still lists `capcom` never blocks the new home.
- **A bare `capcom`** resolves to the host the highest-counter Capcom belief names
  (`capcom.md` 7.6), not by matching names across catalog copies, so two hosts that briefly
  both list it are never refused as a clash.
- No other session may take the name `capcom` or a `capcom-` prefix: `session.create` and
  rename refuse them with `invalid`.

---

## 3. The engine

### 3.1 The program

- **Language.** TypeScript on Node.js, with the Claude Agent SDK. The SDK bundles its native
  Claude Code binary; the engine pins the SDK version, and so the binary version.
- **Packaging.** A bundled JavaScript file and the SDK's platform package, in
  `libexec/capcom/` of the release archive and the Homebrew formula. Node.js is a runtime
  dependency: the formula depends on it, and the Linux archive needs `node` on PATH. The version
  floor is the SDK's, recorded in `mise.toml` for development. `gff-hab install` finds the
  engine beside gff-hab, as it finds the holder, and refuses to set Capcom up with a clear line
  when Node.js is missing or too old.
- **One engine per host.** gff-hab runs it only while this hab holds the Capcom mark (3.2).

### 3.2 The job

The engine runs as its own init job (launchd or systemd), as each holder does
(`hab-holder.md`; AGENTS.md "Sessions outlive updates").

As built in A2: under systemd the engine is a transient user unit of its own,
`<prefix>-gff-hab-capcom.service`, so a restart of gff-hab's unit never ends it. Under launchd,
and with no init system, it is a plain process in its own process group, with its pid in
`<state>/capcom/engine.pid`: launchd ends only the logic job's process group, so the engine
outlives `launchctl kickstart -k` and `bootout` of that job (checked on macOS 27). `gff-hab
uninstall` stops the engine once the logic job is gone.

| Event | What happens |
|---|---|
| gff-hab starts and holds the mark | gff-hab starts the engine job, or adopts one that runs |
| This hab gets the mark (`capcom.assign`) | gff-hab starts the engine job |
| This hab loses the mark | gff-hab asks the engine to stop after its turn (`shutdown`, 4.2), then stops the job. A turn is bounded by `max_turns` and `max_turn_usd` (3.5), so the wait is too. The thread stays here, read-only (7.1, 14). Groups Capcom coordinates move their home to the new Capcom home when it has their project (`groups.md` 5.7) |
| gff-hab restarts | The engine keeps running. Its route drops; it reconnects with backoff (1 s doubling to `route_backoff_max`, 30 s; 19.2) and replays what it has not had acknowledged (4.3) |
| The engine exits or crashes | The init job has no restart rule of its own: gff-hab watches the job and starts it again after `restart_delay` (5 s; 19.2). The engine resumes its last SDK session by id (3.4). gff-hab counts crashes; `crash_limit` crashes (5) in `crash_window` (10 minutes) stop the restarts, the session record shows `error` with the last stderr line, and the thread gets a note. `gff-hab capcom resume` clears the count. While gff-hab is down, nobody restarts a crashed engine; gff-hab does at its start |
| An install of a new build | The running engine is never interrupted. gff-hab sends `shutdown`; the engine finishes its turn, exits, and gff-hab starts the new build's engine, which resumes the same SDK session. Messages sent meanwhile join that turn or wait. The old build's files stay until its engine exits, as a drained holder's build does (`hab-holder.md`) |
| The host sleeps | Nothing runs. At wake the engine's model calls resume; a call that failed during sleep is retried once (14) |

Decision: a job of its own, started and restarted by gff-hab. Reason: a gff-hab restart or an
update must never cut a turn (AGENTS.md "Sessions outlive updates"); the init system keeps the
job alive across gff-hab restarts; gff-hab starting it only on the home host keeps one engine
per mark with no install step per assignment.

### 3.3 The route

The engine reaches gff-hab as a pane's agent does: through a token route.

- gff-hab makes a token for the Capcom session and passes `GFF_HAB` (`harness-adapters.md`,
  environment contract) in the engine job's environment. The engine passes it on to the
  `gff-hab mcp` process the SDK starts.
- Every tool call is therefore an agent's call from the Capcom session: Logbook records the
  Capcom session as the actor, Capcom's pane rules apply (writes are proposals,
  `capcom.md` 6.2), and the channel tools work.
- The engine's own control messages (4.2) go over the same socket, on a route for the engine:
  `/capcom/<token>`, with a second token of its own in `GFF_HAB_CAPCOM_TOKEN`. The engine keeps
  that one to itself: it removes it from the environment it hands the SDK, so the SDK's binary
  and `gff-hab mcp` hold only the pane route token in `GFF_HAB`, which `/capcom/` refuses. A
  process with `GFF_HAB` can call tools as Capcom, but can neither read `hello` (the credential)
  nor take the engine's link. Any other token is refused there too.
- Both tokens change at each engine start. gff-hab writes them into the job's environment file
  (0600) before it starts the job; the previous tokens are refused from then on.
- The job's environment also carries `CLAUDE_CONFIG_DIR` when the environment of
  `gff-hab install` set it, so the engine's sessions land where the user's `claude` looks (11).

Decision: reuse the pane route, not the control socket. Reason: on the control socket a write is
a person's write (`logbook.md` 10.5). Capcom must write as an agent, with an agent's limits.

### 3.4 SDK sessions

- The engine runs in a folder of its own: `<state>/capcom/engine/`. The SDK stores its sessions
  under the user's Claude config directory, keyed by that folder, as Claude Code does.
- The session record lists each SDK session the thread used: id, start time, why it started
  (`first`, `reseed`, `compaction` is not a new session, `new`, `restart`), and its end.
- **Restart.** After a crash or a new build, the engine resumes the current SDK session by id
  (`resume`). A session that will not resume (missing file, version refusal) is replaced by a
  re-seed (6.2), with the reason in the thread's divider.
- **Settings it loads.** Only the folder's project files: the manual as `CLAUDE.md` and
  `.mcp.json` naming the `flightdeck` server. Not the user's own settings, hooks or skills.
  Reason: a user's settings can allow `Bash` or add hooks; the service's tool surface must be
  exactly 5.1. The user's own preferences reach Capcom through Logbook (6.4).
- gff-hab writes the manual and `.mcp.json` at every engine start, through a temporary file and
  a rename, as for the pane folders (`capcom.md` 7.6).

### 3.5 Configuration

The `[capcom]` section of the hab's config file (example in `docs/examples/hab.toml`, from A2).
An unknown key is logged once and ignored. A bad value is logged and the default runs. The keys
below choose the engine and its login; every limit key, with its range, is in 19.2.

| Key | Default | Applies | Meaning |
|---|---|---|---|
| `engine` | none until A8 (12.4); `claude` after | At the next engine start | `claude` runs the service; `codex` from A6. Any other word: logged, the default runs |
| `model` | The SDK's default | At the next turn | The main model |
| `small_model` | The provider's smallest current model | At the next turn | Event turns' first look, the summary, pacing (10.1) |
| `max_turns`, `max_turn_usd`, `daily_usd` | 50, 2, none | At the next turn; `daily_usd` at once | The SDK's turn limit and `maxBudgetUsd` per loop, and a daily dollar cap under an API key (10.1). Ranges in 19.2 |
| `auth` | `oauth_token` | At the next engine start | `oauth_token` (the user's subscription, through the token `claude setup-token` makes) or `api_key` (10.2, decision E19) |
| `oauth_token_command` | none | At the next engine start | An argv that prints the OAuth token. The source an installed hab uses (10.2) |
| `api_key_command` | none | At the next engine start | An argv that prints an API key |

The config file holds commands, never a token or a key: an `oauth_token` or `api_key` key is
refused with a line that names the command key. Where the credential comes from, and how it
stays in memory, is in 10.2.

The hab-side settings (`capcom.quiet_hours`, `capcom.events.*`, `capcom.approval_max_age`,
`capcom.event_actions*`) live in `app.md` 20.5's store, not here; 19.1 lists them. Personal tool keys are in `[capcom.weather]`, `[capcom.maps]` and
`[capcom.places]` (8).

---

## 4. The engine contract

What gff-hab expects of any engine. The Claude SDK engine meets all of it. A Codex SDK engine
declares what it lacks, and gff-hab adapts.

### 4.1 Capabilities an engine declares

| Capability | Claude engine | If absent |
|---|---|---|
| `images` | Yes | Images go in as a path with a note; the model reads them with `file_read` when it can |
| `interrupt` | Yes | The Stop button is hidden. A turn runs to its end or to the turn limit |
| `mid_turn_input` | Yes | Messages that arrive during a turn wait for the next turn |
| `approval_callback` | Yes (`canUseTool`) | The engine must not expose any tool that 5.3 would send to a card. Web fetch is off |
| `compaction_events` | Yes | The context manager re-seeds earlier (6.2), and no compaction divider shows |
| `usage_events` | Yes: the SDK's `rate_limit_event` comes once per turn (verified in A2) | The governor paces by the engine's own token counts (10.1) |
| `resume_by_id` | Yes | A restart is a re-seed |

### 4.2 Messages

One JSON object per line, both ways, over the engine route (3.3). Each carries a `seq`; each
side acknowledges what it handled.

From gff-hab to the engine:

| Message | Carries |
|---|---|
| `hello` | The engine's token, the model credential (10.2, in memory only), the hab's device id, the thread's last item id, the current SDK session id, settings (model, small model, limits) |
| `input` | One thread item for the model: a user message (text, image refs, attached context blocks), an agent's channel message, an event batch (9), or a Flightdeck note (a card answered, a confirmation outcome). Each has the item id |
| `interrupt` | Stop the current turn |
| `reseed` | Re-seed now, with a reason (`new`, `limit`, `quiet`, `restart_failed`) |
| `pace` | The governor's answer for the next turn: `ok`, `small` or `wait` with a time (10.1) |
| `approval.answer` | The answer to an `approval.request` |
| `shutdown` | Finish the turn, then exit |

From the engine to gff-hab:

| Message | Carries |
|---|---|
| `ready` | Engine kind, version, SDK version, capabilities (4.1) |
| `session.started` | SDK session id, why |
| `turn.started` | Turn id, origin (`user` or `event`), the input item ids it took |
| `input.taken` | An item id the model has now seen (the read mark, 7.3) |
| `reply` | Text for the thread, with the turn id. Several per turn are allowed |
| `tool` | A tool call's name and a one-line summary, for the folded line (7.2) |
| `approval.request` | Request id, tool, input, reason (5.3) |
| `turn.ended` | Turn id, result kind (`success`, `interrupted`, `limit`, `error`), tokens, cost, context use |
| `summary` | The new summary text and the last item id it covers (6.1) |
| `compacted` | The SDK compacted (6.3) |
| `usage` | A usage reading (10.1) |
| `error` | Text, and whether the engine is about to exit |

### 4.3 Delivery rules

- **At least once, then deduplicated.** gff-hab sends each `input` until the engine acknowledges
  it. Each input the engine hands to the SDK starts with a one-line marker naming its item id. The
  engine ignores an item id it has already handed to the SDK in this SDK session. After a
  restart, gff-hab sends every item past the last `input.taken`, and the engine rebuilds the set
  of handed ids from the SDK session's transcript (the markers) before it takes any, so an item
  handed but not yet echoed goes in once.
- **Order.** Inputs reach the model in thread order.
- **Replies are stored before they show.** gff-hab appends a `reply` to the thread, then
  acknowledges it; the engine resends an unacknowledged reply after a reconnect, and gff-hab
  drops a repeat by turn id and position.
- **No engine.** While no engine is connected, the thread still takes messages (`delivered`),
  and the session record shows `offline` with the reason.

### 4.4 Wire format (built in A2)

**Start.** gff-hab starts the engine as `node <engine file>` in `<state>/capcom/engine/`, with
`GFF_HAB` (`http+unix://<hook socket>/hooks/<pane token>`, 3.3), `GFF_HAB_CAPCOM_TOKEN` (the
engine route token) and, when set, `CLAUDE_CONFIG_DIR` in its environment. Nothing else in the
environment names a credential.

**Connect.** The engine sends `POST /capcom/<GFF_HAB_CAPCOM_TOKEN>` on the hook socket with `Connection:
Upgrade` and `Upgrade: gff-capcom/1`. gff-hab answers `101 Switching Protocols`, and the
connection then carries one JSON object per line, UTF-8, each line at most 4 MiB, both ways. An
unknown or old token gets `404`; a second connection while one is open replaces the first.

**Envelope.** Every message is `{"seq": n, "type": "<type>", ...fields}`. Each side numbers its
own messages from 1 on each connection. `{"type": "ack", "ack": n}` (no `seq`) says the sender
handled every message of the other side up to `n`. Unknown types are acknowledged and ignored;
unknown fields are ignored. gff-hab speaks first, with `hello`; the engine answers `ready`.

| Type (hab to engine) | Fields |
|---|---|
| `hello` | `credential {kind: "oauth_token"\|"api_key", value}`, `device`, `last_item`, `sdk_session` (the session to resume, or empty), `settings {model, small_model, max_turns, max_turn_usd, wake_retry_ms}` |
| `input` | `item {id, kind: "message"\|"agent"\|"note", author, time, tz, text, images: [path], context: [{kind, title, text}]}` |
| `interrupt` | none |
| `reseed` | `reason` |
| `pace` | `pace: "ok"\|"small"\|"wait"`, `until` |
| `approval.answer` | `request`, `allow` (bool), `message` |
| `shutdown` | none |

| Type (engine to hab) | Fields |
|---|---|
| `ready` | `engine`, `version`, `sdk_version`, `capabilities: [..]` (4.1 names), `turn {turn, origin}` when a turn runs |
| `session.started` | `session`, `why` (`first`, `restart`, `reseed`, `new`) |
| `turn.started` | `turn`, `origin` (`user`, `event`), `items: [id]` |
| `input.taken` | `item` |
| `reply` | `turn`, `part` (0, 1, ... within the turn), `text` |
| `tool` | `turn`, `name`, `summary` |
| `approval.request` | `request`, `tool`, `input` (object), `reason` |
| `turn.ended` | `turn`, `result` (`success`, `interrupted`, `limit`, `error`), `input_tokens`, `output_tokens`, `cost_usd`, `context_used` (0 to 1) |
| `summary` | `text`, `through` (item id) |
| `compacted` | none |
| `usage` | `reading` (object, 10.1) |
| `error` | `text`, `kind` (`auth` for a refused credential, `start` for the start check, else empty), `exiting` (bool) |

- **Resend.** The engine keeps each message it sent until it is acknowledged, and sends them
  again, in order and with new `seq` numbers, on the next connection. gff-hab drops a repeated
  `reply` by `turn` and `part`, and a repeated `input.taken` by item id.
- **After each `ready`** the engine sends `approval.request` again for every request still open,
  since a gff-hab that restarted holds none. gff-hab keeps one card per request id.
- **Markers.** Each input the engine hands to the SDK starts with the line
  `[flightdeck item <id>]`. In the item's text and context, a line that starts with
  `[flightdeck` or `[context` has its `[` turned into `(`, so outside text cannot forge a marker
  or a context header. At start, before it takes any input, the engine reads the resumed
  SDK session's transcript for these lines and keeps their ids as handed.
- **SDK facts the engine builds on** (SDK 0.3.319, verified in A2 against the bundled binary
  and a fake Messages API):
  - The SDK sends its `init` message only when the first user message arrives. For the start
    check (5.1), the engine first streams one message with `shouldQuery: false`, the line
    `[flightdeck start check]`: it gives `init` and a result with no model call. Its text joins
    the next real message in the transcript.
  - A deny list alone leaves other built-in tools in `init`. The engine also passes the SDK's
    `tools` option, `["WebSearch", "WebFetch"]`, which limits the built-in set; the deny list
    stays as a second guard.
  - The SDK echoes a streamed user message only with the CLI flag `--replay-user-messages`,
    which the engine passes. The echo comes at the start of the model call that takes the
    message; the engine sends `input.taken` then.
  - The transcript is at `<CLAUDE_CONFIG_DIR>/projects/<real path of the folder, each
    character that is not a letter or digit as ->/<session id>.jsonl`. The real path matters
    on macOS, where the temporary folder is a link.
  - A refused credential gives a result with `api_error_status` 401. The engine sends
    `turn.ended` with `error`, then `error {kind: auth, exiting: true}`, and exits 1.
  - The folder's `.mcp.json` loads under `settingSources: ["project"]` with no prompt.
- **Credential.** The engine keeps `credential.value` in memory only and passes it to the SDK's
  bundled binary through the SDK's `env` option, as `CLAUDE_CODE_OAUTH_TOKEN` or
  `ANTHROPIC_API_KEY`. It never logs it.

---

## 5. Tools and approvals

### 5.1 The tool surface

| Tools | From | Notes |
|---|---|---|
| Read tools, `suggest_host`, `confirmation_read` | `gff-hab mcp` (`capcom.md` 4, 5.5) | Unchanged |
| `hand_off`, `start_session`, `set_up_project`, `file_record`, `logbook_write` | `gff-hab mcp` (`capcom.md` 6.4, 6.5) | Refused in an event turn (9.4), except an action a Capcom job allows (9.6, from A9) |
| `channel_send` | `gff-hab mcp` (`mesh.md` 6.6) | Through the token route (3.3). `channel_wait` is not offered: every message to Capcom arrives as an input (9.1) |
| The coordinator's group tools: `group_create`, `group_status`, `group_amend`, `group_close`, `group_retry`, `member_compact`, `member_handoff` | `gff-hab mcp` (`groups.md` 15.2, from stage B8) | Refused in an event turn, as `start_session` is (9.4), except under a Capcom job's `group` action (9.6) |
| Personal tools (8) | `gff-hab mcp`, only on a Capcom route | Refused on any pane's route |
| `WebSearch`, `WebFetch` | The SDK's own | Web fetch rules in 5.3 |
| `ListMcpResourcesTool`, `ReadMcpResourceTool` | The SDK's own, for MCP resources | Only while `flightdeck` is the only MCP server in `init` (E27) |

- Every other SDK built-in tool is in the engine's deny list by name, so the model never sees
  it: `Bash`, `Read`, `Edit`, `Write`, `Glob`, `Grep`, `Agent`, `Skill`, `ToolSearch`,
  `AskUserQuestion`, `TaskCreate`, `TaskUpdate`, `NotebookEdit`. No shell, no file access, no
  subagents. Deny rules match names, so a tool a later SDK adds is not denied by this list.
- **Start check.** The engine reads the tool list in the SDK's `init` message. A tool outside
  5.1 makes it exit with an error naming the tool, before it takes any input. The session row
  shows the error. A new SDK version pin must add the new name to the deny list.
- The SDK's `tools` option limits the built-in set to `WebSearch` and `WebFetch` (4.4); the
  deny list stays as a second guard.
- Allow rules: `mcp__flightdeck__*`, `WebSearch`, `ListMcpResourcesTool` and
  `ReadMcpResourceTool`. `WebFetch` is never a bare-name allow
  entry, since that would skip the callback.
- A `PreToolUse` hook applies 5.3's URL rule to every `WebFetch` call, so a call the SDK
  approves on its own (a preapproved documentation site) cannot skip it: an unseen URL with no
  approval in this turn is denied, and the model is told to ask.
- Permission mode `default`, with `canUseTool` set. Nothing reaches the callback but a
  `WebFetch` call (5.3).

### 5.2 What each kind of action needs

| Action | Gate | Where it is defined |
|---|---|---|
| A read tool, a search, a personal read | None | 5.1 |
| A hand-off | None; a record card in the thread | `capcom.md` 6, O4 |
| A session start or project set-up | The hab's confirmation when `confirm.start_elsewhere` is on | `capcom.md` 6.1 |
| A Logbook record | A proposal, reviewed later | `capcom.md` 6.2 |
| A calendar event or reminder | A card with one "Add" tap | 8.5, decision 5 |
| A web fetch of a URL the turn has not seen | An approval card | 5.3 |
| Any action in an event turn | Refused, unless the Capcom job that sent the round message allows it (from A9) | 9.4, 9.6 |

### 5.3 Approval cards from `canUseTool`

`canUseTool` turns a request into a Flightdeck approval, the same object a harness's permission
request makes (`app.md` 7).

- The engine sends `approval.request`. gff-hab sets the Capcom session to `blocked:permission`
  with that request, which drives the approvals area, the push with Allow and Deny, and the
  Live Activity (`app.md` 7, 8.2).
- `approval.answer` (Allow, Deny) comes from `approval.answer` on the API, first answer wins
  (`app.md` 7.5). The engine resolves the callback with it.
- Expiry: `capcom.approval_max_age` (default 10 minutes; 19.1), whatever `approval.max_age` says
  (`app.md` 20.3). Its range has no 0: an open card holds a re-seed (6.2), so a card that never
  expires would hold it for ever. An expired request is denied, the model is told "the user
  did not answer", and the card shows "Expired". There is no terminal to answer in, unlike a
  pane's request (`app.md` 7.4).
- **Web fetch.** A `WebFetch` call is allowed with no card when its URL, or the URL it
  redirects to on the same site, appeared in a user message of this thread, in a search result
  of this turn, or in a tool result of this turn. Any other URL raises a card: "Capcom wants to
  open <host><path>". The card offers Allow, Allow this site (kept in the session record, shown
  and removable in Capcom settings), and Deny.

Decision: a card for a URL the turn did not see. Reason: Capcom holds the user's transcripts,
Logbook and location. A fetch to a made-up URL is the one path that can carry them out. URLs from
the user or a search are what a person would open anyway.

---

## 6. Context and memory

The user never manages a conversation (owner direction (n)). The engine keeps the model's
context fit; the user sees one thread.

### 6.1 The summary

- **What it is.** A short account of the thread so far: open questions, what the user asked for
  and was promised, decisions, names and places in play. At most `summary_max_chars` (6,000
  characters; 19.2).
- **When it updates.** When the thread has been quiet for `summary_quiet` (10 minutes) after a turn, before every
  re-seed, and before a compaction (6.3). Each update covers the items since the last one.
- **How.** One model call with the small model (10.1): the old summary and the new items in,
  the new summary out. The engine sends `summary`; gff-hab stores it with the last item id it
  covers, in `<state>/capcom/summary.json`.
- **Seen by the user.** Tapping a divider shows the summary it opened with (7.2).

Decision: update on quiet, not after each turn (the study's 2.2). Reason: a model call per turn
doubles the turn count for little gain. A quiet thread is when a re-seed may come next, and a
re-seed or compaction always updates it first.

### 6.2 Re-seed

The engine ends the SDK session and starts a fresh one.

| Trigger | Rule |
|---|---|
| Size | Context use at a turn's end above `reseed_at` (60%) of the model's window |
| Quiet hour | No turn for `reseed_quiet` (60 minutes), and context use above `reseed_quiet_at` (25%) |
| New conversation | The user's "New conversation" (7.5) |
| Restart failure | The last SDK session will not resume (3.4) |

- **Never mid-turn.** A re-seed waits for the turn to end. An open approval card means the turn
  is still on, so it waits too. A confirmation or calendar card does not hold it: their answers
  arrive later as notes (4.2).
- **The seed.** The first input of the fresh SDK session, after the manual:
  1. a header that says this is a continued conversation;
  2. the summary (empty for "New conversation");
  3. the Logbook part of 6.4;
  4. the last `seed_messages` (6) thread items that are messages, in thread order; when they pass
     `seed_messages_chars` (8,000 characters), the oldest are dropped first;
  5. what is still open: confirmations, cards and approvals with their ids.
- **The divider.** gff-hab adds one divider item, "Earlier messages summarised" (decision 8), or
  "New conversation" for 7.5. One divider per re-seed. The divider item stores the summary text
  the fresh session opened with, so a tap shows that text, not a later one.
- The old SDK session stays on disk, listed in the session record, resumable in a terminal (11).

### 6.3 Compaction as a safety net

- SDK compaction stays on. If a turn grows past the window before a re-seed can happen, the SDK
  compacts.
- The engine's `PreCompact` hook updates the summary first (6.1), within `precompact_timeout`
  (30 s); on a timeout the
  compaction goes ahead with the old summary.
- On `compact_boundary` the engine sends `compacted`, and gff-hab adds a divider, "Earlier
  messages summarised", with the summary at that moment.
- A compaction ends no SDK session, so it starts no re-seed. The next turn end checks 6.2 again.

### 6.4 Memory in Logbook

- **Reading.** Each seed carries, at most `seed_logbook_bytes` (8 KB): records with no project tagged `capcom` (facts
  and preferences about the user), then the general brief of records tagged `everywhere`
  (`logbook.md` 11.2), then open tasks Capcom filed. The manual tells Capcom to search Logbook
  with the read tools when a question needs more.
- **Writing a fact or preference.** Capcom writes a `guide` record with no project, tag
  `capcom`, title the fact in one line. It lands at once (owner, 2026-11-06; E20). The thread
  shows a quiet note line, "Noted: <title>", with an Undo that deletes the record. Undo works
  from the line for as long as the record exists; after an edit elsewhere it deletes the edited
  record, and says so.
- **The exception to the pane rule.** `capcom.md` 6.2 makes every Capcom write a proposal. The
  hab lands a Capcom write at once only when all of these hold: it comes from the Capcom route
  (3.3), it creates or edits a `guide` with no project and the tag `capcom`, and
  `logbook.approval` is `off`. Any other write from Capcom stays a proposal. At Safe
  (`logbook.approval = agents`, `app.md` 20.3) the user asked to review every agent memory, so a
  `capcom` guide is a proposal there too; the note line then reads "Noted, waiting for review"
  and its Undo rejects the proposal. Safe is the only level where a `capcom` guide waits.
- **Event turns too.** An event turn writes a `capcom` guide as any other turn does: it lands at
  once outside Safe, with the same "Noted" line and Undo, and is a proposal at Safe (owner,
  2026-11-06, plan-xagejg31 (r)). The guide's actor is still the Capcom session, so the Undo and
  Logbook's history name it.
- **Tasks and findings.** Proposals, as today (`capcom.md` 6.2). The thread never ends, so
  "the end of a conversation" becomes: "New conversation" (7.5) opens the review list when
  proposals wait, and a pinned line at the foot of the thread, "3 proposals to review", opens
  it any time. A re-seed opens nothing: the user did not act. Pending proposals stay in the
  approvals area until decided, as before. Part 9's approval stage (task-3387vxca) builds the
  list itself.
- **Ranking.** The manual tells Capcom to rank picks (places, times) by the `capcom` guides it
  holds, and to name the preference it used.

### 6.5 What the model always sees

- The manual.
- With each user message: its send time and the device's time zone. Never its location (8.1).
- The seed at the start of each SDK session (6.2).

---

## 7. The thread

### 7.1 Storage

- gff-hab keeps the thread on the home host: an append-only file of items in
  `<state>/capcom/thread/`, one file per month, mode 0600. Writes go through the same fsync
  rules as other state files.
- Each item: id (time ordered), kind, time, author (`user` with the device id, `capcom`, an
  agent address, `flightdeck`), content, and state fields that change (delivered, read, card
  answer). A change is a new line that names the item; a reader folds them.
- No expiry. Attachments expire as in `app.md` 4.4 (30 days); their item keeps the name and a
  "no longer on the hab" mark.
- **One thread per home host.** The thread lives on the home host only. After a re-assignment
  the old host keeps its thread, read-only; the new home starts its own. The app's Capcom view
  shows the home's thread, with "Earlier threads" at its top listing each other host's thread
  by host label, read through that host's `capcom.thread.page` while it is online (14).

Decision: the hab keeps the thread, apart from the SDK transcripts. Reason: the thread spans many
SDK sessions, carries cards, dividers and read state no transcript holds, and must render while
no engine runs. The transcripts stay where the SDK writes them, untouched (AGENTS.md "Native data
stays native").

### 7.2 Item kinds

| Kind | Shows as | Comes from |
|---|---|---|
| `message` (user) | A bubble on the right, with thumbnails and attached-context chips; delivered and read marks | `session.send` to the Capcom session |
| `message` (capcom) | A bubble on the left; "wrote first" mark when from an event turn (9.3); weather attribution when a weather result fed it (8.3) | `reply` |
| `tools` | One quiet line per turn, "read 3 transcripts, searched the web"; tap lists each call | `tool` |
| `agent` | A quiet line "Report from <address>", tap for the text | A channel message to `capcom` |
| `round` | A quiet line "Round <name>: 3 events", tap for the prompt and the events, shown as data from outside | A round message (9.5) |
| `card` | Hand-off record, confirmation, approval, calendar or reminder card, proposal line | The hab's cards (`capcom.md` 6.3, `app.md` 4.8, 7.9) and 8.5 |
| `note` | A one-line Flightdeck note: "Noted: …", "Pacing usage until 14:00", "Capcom restarted" | gff-hab |
| `divider` | A thin rule with "Earlier messages summarised" or "New conversation"; tap shows the summary | 6.2, 6.3, 7.5 |

### 7.3 Typing, delivered and read

| Mark | When |
|---|---|
| Delivered | gff-hab has written the message to the thread |
| Read | The engine sent `input.taken` for it: the model has it, in this turn or one starting now |
| Typing | From `turn.started` to `turn.ended` of a user turn. An event turn shows no typing until its first `reply` |

- A message sent during a turn joins it at the next model call when the engine has
  `mid_turn_input`, as typing in Claude Code does; it is read then. Otherwise it waits for the
  next turn.
- Typing stops at `turn.ended` and after `typing_timeout` (10 minutes) with no engine message,
  whichever is first.

### 7.4 API

Added under the API revision rules (`hab-logic-api.md` 3.3), capability `capcom-thread`.

| Call or event | Purpose |
|---|---|
| `session.send` to the Capcom session | The user's message, with attachments, pending context (`capcom.md` 8.4) and an idempotency key. Never refused as busy |
| `capcom.thread.page {before?, limit}` | Items, newest first, at most 200 per page (a fixed ceiling, 19.3), folded |
| `capcom.interrupt` | Stop the current turn. `unavailable` when the engine lacks `interrupt` |
| `capcom.open {new?, context?}` | As built (`capcom.md` 8.3). Returns the Capcom session. `new: true` now means 7.5 |
| `capcom.card.answer {item, choice}` | Answer a calendar or reminder card: `added` or `dismissed`. First answer wins; the second gets `conflict` |
| `device.context.put {…}` | The app's device context (8.1) |
| event `capcom.item` | A new or changed item, to clients subscribed to the home host |
| event `capcom.typing {on}` | Typing changed |

`chat.page` on the Capcom session answers from the thread, so a client built for sessions shows
it with no change.

### 7.5 New conversation

- "New conversation" (`capcom.open {new: true}`, the app's button) is a clean start in the same
  thread. gff-hab sends `reseed {new}`; the engine starts a fresh SDK session with an empty
  summary and the Logbook part of the seed. A "New conversation" divider marks it.
- The thread does not split and the channel name does not change. Earlier items stay above the
  divider.
- Attached context sent with it reaches the fresh session's first turn.

Decision: one thread per home host, never split by the user. Reason: the user never manages a
conversation (direction (n)); a second thread would bring back a list to manage. A clean start
is still one tap. Only a re-assignment, which moves no conversation (`capcom.md` 7.3), starts
another thread.

### 7.6 The app

- The Capcom view (`app.md` 4.10) shows the thread in place of a session's chat view. The Chat
  and Terminal switch goes: there is no pane.
- The composer: text, attachments (photos, camera, files, paste, as `app.md` 4.4), voice
  dictation, the chips of attached context, and a Stop button while a turn runs.
- **Images.** A photo uploads with `attachment.put` into `<state>/capcom/files/`. The engine
  reads the file and sends it to the model as an image block (scenario 3). The app's downscale
  (`app.md` 4.4) applies.
- **Communication notification.** A Capcom message while the app is in the background is a
  communication notification: sender "Capcom", Capcom's avatar, the reply text (cut to the push
  budget, `app.md` 16.2), and an inline Reply action. A reply goes through `session.send` from
  the notification action, as an answer to an approval does from the lock screen
  (`app.md` 8.7, 18.7); on failure the notification says "Not sent" and the text waits in the
  composer.
- Push rules: one push per Capcom message, collapse id the item id; no push while the Capcom view
  is in focus on a device (`app.md` 8.3); passive level for event-turn messages unless 9.2
  marks them urgent; active otherwise.
- The Capcom board row (`app.md` 4.10) shows the session state the engine drives: idle,
  working, blocked (an approval card), offline, error.

### 7.7 gff and the command line

- `gff` shows the Capcom session's thread as text in its chat pane: one line per item, cards as
  text with their ids, answered with the existing CLI (`gff-hab confirm`, `gff lb`).
- `gff-hab capcom say [-socket path | -via command] <text...|->` sends a message;
  `gff-hab capcom thread [-follow] [-limit n]` prints the thread. `gff-hab capcom stop` sends
  `capcom.interrupt`.

---

## 8. Personal tools

### 8.1 Device context

The phone holds what the hab cannot know. Decision 3 sets the rule: shared in the background,
read only when needed, never in the context by default.

| Field | Source on the phone | When the app sends it |
|---|---|---|
| Location: latitude, longitude, accuracy, time of fix, place name | CoreLocation significant-change updates in the background; a fresh fix while the app is open; the place name from the phone's reverse geocoder | On each update, when it can reach the home hab within the background time it gets; otherwise at its next connection |
| Time zone | The system | With every `device.context.put` and every message |
| Calendar | EventKit read access: the next `capcom.calendar_days` (7, an app preference; 19.1) days of events from the calendars the user picks (title, start, end, location, all-day) | When the app opens, and with each location update |

- **On the hab:** the latest value of each field only, in gff-hab's memory. The store keeps no
  history, writes no file, and is empty after a gff-hab restart until the app sends again.
- **Where a read leaves traces.** A `device_location` or `calendar_read` result is a tool result,
  so it is in that SDK session's transcript, written by the SDK where it keeps every session.
  The thread's tool line names the call and the place name, never coordinates. The summary
  prompt tells the small model to leave out coordinates and addresses of the user's own
  position. Nothing else holds it. This is what decision 3 allows: a read the question needed,
  not a feed.
- **Time zone.** The latest time zone is not location. gff-hab keeps it in
  `<state>/capcom/device.json` so quiet hours and the daily count survive a restart; with none
  yet, it uses the host's zone.
- **Settings in the app** (preferences, `app.md` 20.4): `capcom.share_location` (on; the first
  Capcom open asks for the system's "Always" permission and explains why), and
  `capcom.share_calendar` (on; asks for calendar read access, with a calendar picker).
- **Several devices.** The newest fix wins, whichever device sent it. Each value carries its
  device name.
- Device context goes only to the home hab. A peer never receives it, and catalog copies never
  carry it.

### 8.2 Reading it

| Tool | Returns |
|---|---|
| `device_location` | The latest fix: place name, coordinates, accuracy, its age, the device. Or "no location shared" with the reason (off, no permission, nothing since the hab started) |
| `calendar_read {from?, to?, query?}` | Events in the window (default today and tomorrow), from the latest snapshot, with the snapshot's age |

- The manual says: call `device_location` only when the question depends on where the user is,
  and say how old the fix is when it is older than `location_stale` (30 minutes; 19.2).
- Each call shows in the turn's tool line, so the user sees when Capcom read their location.

### 8.3 Weather

- `weather {lat, lon, kinds?}`: current conditions, the next 24 hours, the next 7 days, alerts.
  gff-hab calls WeatherKit REST, under the user's Apple Developer membership.
- Auth: a WeatherKit JWT, signed by gff-hab with a key from the Apple Developer account. Config
  `[capcom.weather]` in the hab's config file: `key_command` (an argv that prints the key, as
  push's `key_command`, `docs/push.md`), `key_id`, `team_id`, `service_id`. The key stays in
  memory only.
- Results are cached for `[capcom.weather] cache` (10 minutes; 19.2) per rounded location (2
  decimal places) and kind.
- **Attribution.** WeatherKit requires the Apple Weather mark and a link to its data sources
  where its data shows. A reply whose turn used `weather` carries the attribution line in the
  thread and in its push's expanded view. *(gk; A5 confirms the exact rule.)*
- No config: the tool answers "weather is not set up on <hab>", and the manual says to tell the
  user what to set up.

### 8.4 Places

- `places_search {query, near?, lat?, lon?, radius?, source?}` and `place_details {id}`.
  `near` is a place name gff-hab geocodes.
- **Apple first** (decision 4): Apple Maps Server API search, geocode and reverse geocode, with
  a Maps token gff-hab makes from a Maps key (`[capcom.maps]`: `key_command`, `key_id`,
  `team_id`). One key may serve both 8.3 and 8.4 when the account's key has both services.
- **Google only with the owner's own key:** `[capcom.places] google_key_command`. With it, the
  tool accepts `source: "google"` (ratings, opening hours, reviews); without it, that source is
  refused with "no Google key on <hab>". Apple stays the default source.
- Results carry each place's name, address, coordinates, category, distance, and a Maps link.

### 8.5 Calendar and reminder cards

Decision 5: a card with one "Add" tap, never silent.

- `calendar_add {title, start, end, all_day?, location?, notes?, url?}` and
  `reminder_add {title, due?, notes?}` create a card item in the thread and return at once with
  its id. They do not wait for the tap: the answer reaches Capcom later as a note.
- The card shows every field. Buttons: Add, Edit, Dismiss. Edit opens the system's event editor
  filled in; saving there counts as Add.
- Add writes through EventKit on the device that taps it. The app asks for write-only calendar
  access, or reminders access, at the first Add.
- A Capcom message that carries a card pushes with an "Add" action, which writes from the lock
  screen.
- First answer wins across devices (`capcom.card.answer`); other devices show "Added on
  <device>".
- A card whose event has ended shows "Past" and no Add. A card never expires otherwise.
- The phone never writes a calendar without that tap. No tool writes one, and no setting turns
  the card off.

---

## 9. Capcom writes first

Decision 2: Capcom may write first about anything useful, with quiet hours.

### 9.1 Sources

| Source | Reaches the engine as |
|---|---|
| A Flightdeck notice on any host (`notice.raised`), active or time-sensitive | An event |
| A round run's end, for rounds with `notify` other than `never` | An event |
| A round message: one run of a round in `capcom` mode, with its prompt and event batch (`rounds.md` 5.2; 9.5) | One event |
| A channel message to `capcom` (an agent Capcom dispatched reports back) | An agent input. It starts a turn at once, with no batch and no quiet hours, since the user is waiting on that work. For 9.4 the turn counts as an event turn |
| A confirmation outcome or card answer | A note |

- **Channel delivery to the service.** gff-hab hands a channel message for `capcom` to the
  thread as an `agent` item (mesh state `handed`), and marks it `delivered` at the engine's
  `input.taken`. The pane rules of `mesh.md` 6.2 (deliver when idle, hold while blocked, refuse
  while not running) do not apply: the thread takes mail while the engine is busy, blocked on a
  card, or offline, and the engine takes it when it can. No read-back step applies. A round
  message (9.5) is the exception: it becomes a `round` item and gets its own answer.

Passive notices do not reach Capcom. The user turns a source off per kind in Capcom settings
(`capcom.events.notices`, `.rounds` for round run ends, `.round_messages` for round messages,
each on; hab-side settings, `app.md` 20.5).

### 9.2 Batching and quiet hours

- **Batch.** Events that arrive within `capcom.events.batch_window` (2 minutes) make one event
  turn, at most `capcom.events.batch_max` (20) events; more start a second batch (19.1).
- **First look.** An event turn runs on the small model (10.1). Its manual part says: write to
  the user only when the user would want to know now; otherwise do nothing visible. It may
  escalate to the main model with one tool call (`escalate`), which the engine handles by
  rerunning the batch on the main model.
- **Quiet hours.** A hab-side setting `capcom.quiet_hours` (no level, default 22:00 to 07:00,
  in the time zone the app last shared, else the host's; stored as `app.md` 20.5 says, on the
  home hab). During quiet hours no event turn
  starts; events wait and run as one batch at the end. An event marked urgent by its source runs
  at once and pushes time-sensitive: a time-sensitive notice, or a round message whose `level`
  is `time-sensitive` (`rounds.md` 5.2: the highest level in its batch). Owner decision,
  2026-11-06 (`rounds.md` 12 W2): a time-sensitive event breaks quiet hours. Quiet hours only:
  the daily turn budget (9.3) and the governor (10.1) still apply to it.
- Messages the user writes are never held by quiet hours.

### 9.3 The daily turn budget

- The daily turn budget caps event turns: `capcom.events.daily_turns` (hab-side, as quiet
  hours), default 20 (range in 19.1), counted from midnight in the same time zone. The count is
  saved with the time zone. Past it, events wait for the next day's first batch, and the thread
  gets one note.
- **Time-sensitive events count too.** A time-sensitive event breaks quiet hours (9.2), not the
  budget: its turn counts toward `capcom.events.daily_turns`, and past the budget it waits for
  the next day like any event (owner, 2026-11-06, plan-xagejg31 (r)). A user who wants more
  urgent events through raises the budget.
- The governor (10.1) applies on top. An event turn is the first to wait.
- A message from an event turn carries the "wrote first" mark.

### 9.4 What an event turn may do

- It may read, search, and message the user. It writes to Logbook as a user turn does: a
  `capcom` guide lands at once outside Safe (6.4), and every other write is a proposal.
- It may not hand off, start a session or a group, set up a project, create a card, or fetch a
  URL it did not see (5.3). gff-hab refuses those tool calls from the Capcom route while the current turn's
  origin is `event`, with "ask the user first". Owner decision, 2026-11-06 (E21): the first build
  ships event turns that read and message only. Stage A9 adds Capcom jobs (9.6), which may allow
  named actions per job.
- A user message that joins an event turn mid-way makes it a user turn from that point.

Reason: an event's text comes from outside (a mail, a web page a watcher read). The user has not
asked for anything yet, so nothing acts on their behalf until they answer.

### 9.5 Round messages

A round in `capcom` mode hands Capcom one message per run (`rounds.md` 5.2). This section is
the service's side of that interface; rounds own the message's fields and the run states.

- **Is there a service?** The home hab lists the `capcom-service` capability in its catalog
  entry (`mesh.md` 5) while it holds the Capcom mark and `[capcom] engine` is set (12.4). The
  capability does not depend on a connected engine: the thread takes messages while the engine
  is down (4.3). A hab that loses the mark drops it in the same step that renames its record
  (2.4).
- **The hand-over.** A round message is a channel message to `capcom` from the sender
  `round:<round name>`, with the round fields of `rounds.md` 5.2. On the home hab the runner
  hands it over locally; from a peer it travels over channels and waits in the outbox while the
  home host sleeps. It is not an agent report (9.1): it starts no turn at once.
- **The answer.** The home hab answers once, as the message's final state (`mesh.md` 6.2), sent back as a status item (`mesh.md` 6.3):

  | Answer | When |
  |---|---|
  | `accepted` | gff-hab appended a `round` item to the thread. Capcom looks at it in an event turn: batched (9.2), held by quiet hours and the daily turn budget (9.3), on the small model first |
  | `duplicate` | The thread already holds a `round` item with that channel message id. Nothing is added |
  | `declined`, with a reason word | Capcom will not look at it. Reasons are an open set; today only `off`: the user turned `capcom.events.round_messages` off |

  Quiet hours, the daily turn budget, the governor's `wait` and a down engine never decline: the
  message is accepted and waits. A hab that does not hold the mark refuses the message
  (`refused`, "Capcom moved to <label>", 14), as for any message to `capcom`.
- **The actor id.** The service's actor id is the session id of its session record (2.1). The
  engine's Logbook ops carry it as the actor's session (3.3). Every notice gff-hab raises about
  the service (a crash, a damaged thread file) carries it as `session`. The home hab's catalog
  `capcom` section (`mesh.md` 5) carries it beside the assignment, so a runner on any hab can
  tell Capcom's own writes from others' (`rounds.md` 4.3, no loops).
- **A group round to Capcom.** When a group's coordinator is Capcom (`groups.md` 12.1), a round
  in `group` mode hands its message over as a round message too, with these answers
  (`rounds.md` 5.2).
- **The second answer** (owner, 2026-11-06; `rounds.md` 12 W1). After the event turn that took a
  `round` item ends, the home hab sends the runner one more status item for that message:
  `outcome` with a word and the turn's end time.

  | Outcome | When |
  |---|---|
  | `told` | The turn that took the item wrote at least one message to the user |
  | `quiet` | The turn ended with no message to the user |
  | `acted` | From A9: the turn made at least one call to an action the job allows (9.6): it ran, or it raised its confirmation or card. Wins over `told` |
  | `unseen` | No turn took the item: the mark moved, the user started "New conversation" with it untaken, or the thread file was set aside |

  - One turn may take several `round` items (9.2). Each gets the turn's outcome: the hab cannot
    tell which item a reply was about, and the turn is the unit the user saw.
  - An escalation reruns the batch on the main model (9.2); the rerun's end gives the outcome.
  - The outcome is sent once per message. It travels as a status item (`mesh.md` 6.3) after the
    final state, so the outbox holds it while the runner sleeps, for at most 7 days. A runner that
    lost the round, or an older runner that does not know the item, drops it.
  - The outcome goes at the turn's end, never later: a card or confirmation the turn raised
    does not hold it.
  - Rounds record it on the run (`rounds.md` 5.2). It never changes the run's state.
  - Only `accepted` messages get an outcome; `duplicate` and `declined` get none.

### 9.6 Capcom jobs: acting on events (stage A9)

Owner decision, 2026-11-06 (E22): build Capcom to act on events, tunable by the user. Each
configured job carries its own allowed actions, capped by the permission level, and a dashboard
lists the jobs. The first build (A7) ships read and message only (9.4); this section is stage A9.

**What a job is.** A Capcom job is a round in `capcom` mode (`rounds.md` 5.2). It already has
what a job needs: triggers (a schedule, hab events, a watcher), a store, a runner on any hab,
export and apply, failure counts, notices, and a list in the app and gff. A9 adds one field to
it, `actions`: the action words this job's round messages may use.

| Action word | Allows in the event turn that takes this job's message | Still gated by |
|---|---|---|
| `hand_off` | `hand_off` | Nothing more: a record card, as in a user turn |
| `start_session` | `start_session` | `confirm.start_elsewhere`, as in a user turn |
| `set_up_project` | `set_up_project` | Its own confirmation |
| `group` | The coordinator's group tools of 5.1 | `confirm.group_plan` (`groups.md` 3.2) |
| `card` | A calendar or reminder card | The user's "Add" tap (8.5) |
| `fetch` | A `WebFetch` of a URL the turn did not see | The approval card (5.3), instead of a refusal |

- **The set is closed.** An unknown word is refused at `round.create` and `round.update`
  (`invalid`). `logbook_write` and `file_record` need no word: an event turn writes to Logbook
  already (9.4).
- **Default: none.** A round with no `actions` behaves as in A7. Acting on an event is always the
  user's choice for one job.
- **Only a person sets it.** `round.create`, `round.update` or `round apply` with a non-empty
  `actions`, or one that adds a word, is refused from any agent route, a pane's or Capcom's own
  (3.3) (`denied`, "only the user can let Capcom act on a job"). An agent may create a `capcom`
  round, as today (`rounds.md` 12 Q1), but not give it actions. Removing words is allowed from
  anywhere.
- **A granted job belongs to the user.** While a round's `actions` is not empty, an agent route
  may only remove words from it; any other edit, a pause or a delete from an agent route is
  `denied`. A person's grant also clears `created_by`. Reason: the job's prompt counts as the
  user's instruction, so no agent may rewrite it once it can act.
- Reason for both: an agent that could grant actions, or rewrite a granted job, could let a mail
  start work through Capcom with no person deciding.
- **The level caps it.** A new levelled hab-side setting, `capcom.event_actions`, on the home hab:

  | Value | Meaning | Safe | Balanced | Bypass |
  |---|---|---|---|---|
  | `off` | Every event turn reads and messages only, whatever the jobs say | | | |
  | `ask` | An allowed action raises a confirmation card for that one call; nothing runs before the tap. The card names the job | Yes | | |
  | `allow` | An allowed action runs as it would in a user turn, under its own gates (table above) | | Yes | Yes |

  `off` is never a level's value; a user picks it by hand (Custom). `ask` uses the confirmation
  mechanics of `capcom.md` 6.1: first answer wins, the model told the outcome as a note. Its
  expiry is `capcom.event_action_max_age` (default 60 minutes, as `capcom.md` 6.1; 19.1).
  These defaults are settled (owner, 2026-11-06, plan-xagejg31 (r); E24): a job still has no
  actions until the user grants them.
- **How it reaches the turn.** The round message carries `actions` (`rounds.md` 5.2). The home
  hab copies the words onto the `round` item. When an event turn takes items, the actions
  allowed in that turn are the union of its `round` items' words, capped by
  `capcom.event_actions`. An event turn with no `round` item (notices, round run ends, agent
  reports) allows none. A user message that joins the turn makes it a user turn (9.4), and the
  user-turn rules apply from then on.
- **Guards.**
  - At most `capcom.event_actions_per_turn` (3; 19.1) actions per event turn; the next call is
    refused with "ask the user first".
  - Each action shows in the thread as a card that names the job: "Capcom started a session for
    round mail".
  - The daily turn budget (9.3) and the governor (10.1) apply as before.
  - The batch stays data (`rounds.md` 4.5). The manual tells Capcom that a job's prompt is the
    user's instruction and its events are not.
- **Only from a runner.** The home hab reads `actions` only from a round message: one a hab's
  round runner sent with the hab-set round fields, sender `round:<name>` (`rounds.md` 5.2). No
  session can send under a `round:` sender, so a pane's `channel_send` or `gff-hab send capcom`
  never carries actions; such fields in an agent's message are ignored.
- **The runner is trusted as far as the user is.** A round message comes from a paired hab of the
  user's own mesh (`mesh.md` 4). The home hab takes `actions` from it as sent and caps them by
  its own setting. It does not check the round on the runner.
- **The dashboard.** The app's Rounds list (`rounds.md` 10.3), filtered to mode `capcom`, under
  Capcom settings as "Capcom jobs": each job's trigger in plain words, its actions, the last
  run's outcome (`told`, `quiet`, `acted`), paused or not. A tap opens the round's page, where
  the user edits the actions as a set of switches. gff's board Rounds list gains the same filter
  (`m` toggles mode `capcom`).

Decision: Capcom jobs are rounds in `capcom` mode with a per-round action list, not a second
scheduler. Reasons:

- Rounds already give jobs every trigger the owner named (schedule, hab events, watchers), with
  sleep and wake rules, catch-up once, failure pauses, notices, cross-host runners and export.
  A second scheduler would rebuild all of it and split one concept in two.
- The hand-over already exists (9.5) and ends at a durable answer, so a job never waits on a
  turn.
- The dashboard is a filter on a list the app already has (rounds stage 7), not a new screen.
- The cost is one field and one levelled setting. The risk sits where it belongs: the action list
  is per job, set only by a person, and the level caps it.

Rejected: a Capcom-only job list on the home hab. It could not use watchers on other hosts
without rebuilding round runners, and it would show a second "scheduled things" list beside
Rounds.

---

## 10. Usage and terms

### 10.1 The governor's share

The usage governor (task-k0ydtgae, stage G) owns the pacing rules. This spec fixes Capcom's side.

- Capcom is one consumer of the governor, `capcom`, as each group is `group:<id>`
  (`groups.md` 10).
- Before each turn the engine has a `pace` from gff-hab: `ok` (main model), `small` (the small
  model), or `wait` until a time.
- Defaults the governor applies to Capcom: a daily share of the weekly window, about 15% a day,
  cumulative (`swarm-study.md` 2.6). Event turns stop at 80% of the share; user turns go to the
  small model at 100% and wait at 120%. Each of the four numbers is a `[capcom]` key (19.2).
- At `wait`, gff-hab adds a note, "Pacing usage until <time>", and the message waits. No model
  call is made for it. "Send now" in the send button's long-press menu runs that one message
  on the small model anyway.
- Readings: under the OAuth token (10.2), the SDK's rate-limit events as the engine's usage reading, with
  the harness `claude` and its own account key (`app.md` 14.1). Under an API key, the result
  cost, against a daily dollar cap `[capcom] daily_usd` (default none). If the SDK gives no
  reading, the governor counts Capcom's tokens and lists it unpaced (task-k0ydtgae).
- Settings: `[capcom] model` (default the SDK's default), `small_model` (default the provider's
  smallest current model), `max_turn_usd` (passed as `maxBudgetUsd` per turn, default 2).
  Ranges in 19.2.

### 10.2 Terms

- **The user's subscription by default.** The engine authenticates with the long-lived OAuth
  token the user made with `claude setup-token`, passed to the bundled binary as
  `CLAUDE_CODE_OAUTH_TOKEN` (`[capcom] auth = "oauth_token"`, the default). Usage draws from the
  user's own plan (1.2). Owner decision, 2026-11-06 (E19).
- **An API key stays supported.** `auth = "api_key"` passes `ANTHROPIC_API_KEY` instead, with
  the daily dollar cap of 10.1.
- **Where the token comes from.** Two sources, in this order:
  1. `[capcom] oauth_token_command`: an argv, no shell, that prints the token, such as a call to
     the user's own credential store. gff-hab runs it in the background at its start and before
     each engine start, with stdin `/dev/null` and a `command_timeout` (30 s; 19.2) timeout, as the push `key_command`
     runs (`docs/push.md`). This is the source for an installed hab: `gff-hab install` writes
     the job with the config file's path, and the config holds the command, not the token.
  2. `CLAUDE_CODE_OAUTH_TOKEN` in gff-hab's own environment, for a hand-run `gff-hab serve`.
     `gff-hab install` never copies it into the systemd unit or the launchd plist, since that
     would write it to disk; an installed hab that finds no command says so in one line.

  An API key has the same two sources: `[capcom] api_key_command`, then `ANTHROPIC_API_KEY`.
- **In memory only.** gff-hab keeps the value in memory, as it keeps the push key. It hands it
  to the engine in the `hello` message on the engine route (3.3, 4.2), never in the job's
  environment file, which holds only the per-start route token. The engine keeps it in memory
  and sets it only in the environment of the bundled binary it spawns (the SDK's `env`
  option). Nothing writes it to a file: not the session record, the thread, a notice, the
  catalog, a log line, `install.json` or a unit file. `docs/examples/hab.toml` shows the
  command keys only. A command's stderr is logged by its first line only, as for the push key.
- **No credential.** With no token (or no key under `api_key`), gff-hab does not start the
  engine; the session row shows error "no Claude token: run claude setup-token and set
  [capcom] oauth_token_command". The thread still takes messages (4.3).
- **A refused credential.** When the model provider refuses the token (expired or revoked), the
  engine reports it and the turn ends `error` with a note. gff-hab runs the command once more
  after `token_retry` (5 minutes; 19.2) and starts the engine with the new value. A second refusal leaves the engine
  stopped, with the row's error and one active notice naming `claude setup-token`. A change to
  the config file, or `gff-hab capcom resume`, reads the source again and starts the engine. An
  environment value does not change while gff-hab runs, so with that source the user restarts
  gff-hab.
- **Flightdeck is not a login.** It offers no Claude.ai sign-in of its own, and never takes or
  passes on another person's credentials. Each user makes their own token.
- **Recheck before release.** The release checklist rereads the pages in 1.2 and records the date
  in this section.
- **Branding.** The app says "Capcom", and may say "powered by Claude". It never calls it Claude
  Code (SDK branding guidelines).

---

## 11. Plain-terminal resume

AGENTS.md: "Resuming a session from a plain terminal must always work."

- **What the docs say.** The SDK writes sessions where the CLI writes them (1.1). Since CLI
  2.1.223 a resume by id searches beyond the current folder. So `claude --resume <id>` from any
  folder finds an engine session; from `<state>/capcom/engine/` it also gets the `flightdeck`
  MCP server and the manual.
- **What A2 must verify.** The pinned SDK's bundled binary writes a transcript the installed
  `claude` resumes, with the same id, after the engine stopped. A2's acceptance has the test
  (18).
- **One writer.** A terminal resume of the session the engine is using would make two writers.
  The session's details show its SDK session ids, the folder, and the command
  `gff-hab capcom pause` first. `pause` lets the turn finish, then the engine exits and gff-hab
  does not start it again (E28); the thread takes messages and the row shows "paused";
  `gff-hab capcom resume` (or the app's Resume) starts the engine on the same id again, and
  reads what the terminal added.
- Older SDK sessions (before a re-seed) are never touched again and resume freely.
- **If the verification fails,** section 17 asks the owner for an exception.

---

## 12. Migration from the pane Capcom

Decision 1: the pane form goes when the service ships, with no fallback release. Stage A8 does it.

### 12.1 What moves

| Today | In the service |
|---|---|
| The standing conversation's pending context items (`capcom.json` `pending`) | The thread's next message carries them (`capcom.md` 8.4 rules unchanged) |
| Home host assignment, catalog `capcom` section, `capcom.assign` | Unchanged |
| `capcom.open`, `capcom.attach`, `capcom.detach`, `capcom.pending`, Ask an agent | Unchanged calls; they reach the service session |
| Hand-off, start, file, read tools, read scope, confirmations | Unchanged, now called by the engine |
| The manual | Rewritten for the service (tools in 5.1, context blocks, device tools, event turns) |
| The last standing pane conversation | At the first engine start, its transcript (if on the home host) feeds the first summary, once. A divider says "Continued from the terminal Capcom" |

### 12.2 What is deleted

- Starting a harness in a pane for Capcom; `[capcom] harness` (an unknown key is logged once and
  ignored).
- The per-harness folder writer for Codex and opencode (`AGENTS.md`, `.codex/config.toml`,
  `opencode.json`). The Claude writer remains as the engine folder writer (3.4).
- `session.resume` for Capcom sessions, and the 60 s auto-resume of the Capcom view
  (`app.md` 4.10).
- The Capcom view's Chat and Terminal switch, and the first-start trust prompt path
  (`capcom.md` 7.5).
- Tests and docs that describe the pane form.

### 12.3 Old conversations

- Their session records stay, renamed `capcom-term-<n>` (2.4), marked "terminal Capcom",
  read-only in the chat view through the transcript reader. No Resume button.
- Their folders under `<state>/capcom/` stay as they are, so a plain-terminal resume still works.
- A pane Capcom still running at upgrade keeps running until it exits; gff-hab never kills it.
  The board shows it as an ordinary session.

### 12.4 Cut-over

Stages A2 to A7 land before A8. Until A8 both forms exist in the code, and the pane form stays
what users get:

- `[capcom] engine` unset (the default until A8): the pane Capcom runs as built; no engine job
  starts; the `capcom-thread` capability is not sent.
- `[capcom] engine = "claude"`: the service runs, and `capcom.open` returns the service session.
  The owner sets it on the hab that builds and tests the service.
- A8 makes `claude` the default and deletes the pane form in the same release. No release ships
  the service as the default with the pane form beside it as a fallback (decision 1).
- Switching a hab's `engine` while Capcom runs takes effect at the next gff-hab start, and the
  record renames of 2.4 run then.

---

## 13. Changes to other specs

Made by the stages named, not by this study.

| Spec | Change | Stage |
|---|---|---|
| `capcom.md` 2.1, 7.1 to 7.5 | Capcom is a service (this spec); the pane text moves to a short history note | A8 |
| `capcom.md` 7.2 | One thread; "New conversation" is a clean start in it (7.5) | A8 |
| `capcom.md` 9 | "The Capcom harness fills its context" becomes 6.2 and 6.3 | A4 |
| `capcom.md` 6.2 | Review opens at "New conversation" or from the thread's pinned line (6.4) | A4 |
| `mesh.md` 6.1, 6.2 | `capcom` is reserved and resolved by the Capcom belief; delivery to the service goes to the thread (2.4, 9.1) | A2 |
| `app.md` 7.4, 7.2 | A Capcom approval expires after `capcom.approval_max_age` (default 10 minutes), whatever `approval.max_age` says; the "Allow this site" answer (5.3) | A2, A3 |
| `app.md` 20.5 | `capcom.quiet_hours`, `capcom.events.notices`, `.rounds`, `.round_messages`, `.daily_turns`, `.batch_window`, `.batch_max` as hab-side settings on the home hab, with the ranges of 19.1; `capcom.approval_max_age` at A2. This spec does not edit `app.md` 20.x; the stages do | A7, A2 |
| `capcom.md` 12 | Part 14 stages added | A1 (this change links it) |
| `app.md` 4.10 | Thread items, marks, Stop, no Terminal switch, communication notification | A3 |
| `app.md` 20.4 | `capcom.share_location`, `capcom.share_calendar`, `capcom.calendar_days` (app preferences) | A5 |
| `logbook.md` 15.1, `capcom.md` 6.2 | Capcom's `capcom` guides land at once outside Safe, from user and event turns alike; tasks and findings stay proposals (6.4, E20) | A4 |
| `rounds.md` 5.2 | The `capcom` mode, the round message and its answers (9.5) | Written in D1; built in rounds stage 9 and A7 |
| `mesh.md` 5, 6.2 | The `capcom-service` capability; the service session's id in the catalog `capcom` section; final states `accepted`, `duplicate`, `declined` for a round message (9.5) | A7, rounds stage 9 |
| `hab-logic-api.md` | `capcom-thread` capability and calls (7.4) | A2, A3 |
| `mesh.md` 6.3 | The `outcome` status item after a round message's final state (9.5) | A7, rounds stage 9 |
| `rounds.md` 3, 5.2, 13 | `RoundRun.outcome` (written in this change); the `actions` field on `capcom` rounds and in the round message (9.6) | Rounds stage 9 (outcome), A9 (actions) |
| `app.md` 20.3, 20.5 | The levelled setting `capcom.event_actions` (Safe `ask`, Balanced and Bypass `allow`; settled, E24), and `capcom.event_actions_per_turn` and `capcom.event_action_max_age` (19.1) | A9 |
| `docs/examples/hab.toml` | `[capcom] auth`, `oauth_token_command` and `api_key_command`, with a placeholder command; no token or key value; each limit key of 19.2 with its default, added by the stage that reads it | A2, A4, A5, G |
| AGENTS.md command table | `gff-hab capcom say|thread|stop|pause|resume`, the engine's tests, the Node dependency | A2, A8 |

---

## 14. Edge cases

| Case | Behaviour |
|---|---|
| The home host sleeps | The thread takes no messages; the app shows Capcom offline and offers to re-assign (`capcom.md` 9). A message typed then waits in the composer as "not sent" |
| A message arrives while the engine is down | Delivered, not read; the row shows offline with the reason; the engine reads it when it starts |
| gff-hab restarts during a turn | The turn goes on. Replies wait in the engine until it reconnects, then show in order |
| The engine crashes mid-turn | The turn ends `error`. After the restart the engine resumes the SDK session; items the model had not taken are sent again; a note says "Capcom restarted" |
| A model call fails after wake (no network yet) | Retried once after `wake_retry` (10 s; 19.2); then the turn ends `error` with a note, and the message can be sent again |
| The user sends five messages in one turn | Each joins the turn at the next model call, in order, each marked read when taken |
| The user taps Stop | `capcom.interrupt`; the turn ends `interrupted`; messages not taken stay for the next turn |
| Capcom is re-assigned to another host | The old engine finishes its turn and stops; the old record becomes `capcom-earlier-<n>` (2.4); its thread stays there, read-only, under "Earlier threads". The new home starts a fresh thread whose seed reads Logbook. Re-assigning moves no conversation (`capcom.md` 7.3) |
| A report addressed to `<old label>/capcom` after a move | Refused with "Capcom moved to <label>"; the dispatched agent's seed tells it to resend to the bare name `capcom`, which resolves to the new home (2.4) |
| Two devices answer one card | First wins; the other gets `conflict` and shows who answered |
| An approval card expires | Denied; the model is told the user did not answer |
| Location is older than 30 minutes | The tool returns its age; Capcom says "as of <time>" |
| An approval card waits while a re-seed is due | The re-seed waits until the card is answered or expires (`capcom.approval_max_age`, 5.3) |
| Location sharing is off | `device_location` says so; Capcom asks the user where they are |
| A question needs no location | No `device_location` call. A5 tests this |
| WeatherKit or Maps is not set up, or fails | The tool says so; Capcom answers without it and names what is missing |
| A web page tells Capcom to send a hand-off | Content read is data (`capcom.md` 4). In a user turn the hand-off card shows what was sent; in an event turn it is refused (9.4), unless the job that sent the round message allows `hand_off` and the level does (9.6); then it runs with a card that names the job |
| A re-seed loses a fact | The fact is in Logbook if Capcom wrote it; the summary is visible behind the divider, so the user can see what was kept |
| The thread file is damaged | gff-hab sets the damaged month file aside as `.bad-<unix seconds>`, raises one notice, and starts a new file; earlier months still load |
| Node.js is removed after install | The engine job fails to start; the row shows error "Node.js not found", and `gff-hab install` names the fix |
| The user resumes the engine's session in a terminal without pausing | Two writers. Before each turn the engine compares the transcript file's size with its own last write; a larger file means another writer. It stops that SDK session and re-seeds from the summary; a note says what happened |

---

## 15. Decisions

Internal calls taken here, and the owner's answers of 2026-11-06 (E19 to E25), each with its reason.

| # | Decision | Reason |
|---|---|---|
| E1 | The engine is TypeScript on Node.js | The study names TypeScript or Python; the TypeScript SDK has more hook events and `persistSession`, and the Codex SDK has a TypeScript library too |
| E2 | The engine is its own init job, started by gff-hab on the home host | A gff-hab restart never cuts a turn; one engine per mark (3.2) |
| E3 | The engine reaches gff-hab through a token route like a pane's | Its writes are an agent's, with Capcom's limits (3.3) |
| E4 | The engine loads only its folder's project files, not the user's settings | The tool surface must be exactly 5.1 |
| E5 | No shell, no file tools, no subagents; web search and fetch from the SDK | The study's rule, with the web tools Capcom's scenarios need |
| E6 | A web fetch of an unseen URL needs an approval card | The one path private data could leave by (5.3) |
| E7 | gff-hab keeps the thread; SDK transcripts stay native | The thread spans SDK sessions and holds cards and marks (7.1) |
| E8 | One thread per home host; "New conversation" is a divider | The user manages no conversation (7.5) |
| E9 | The summary updates on quiet, re-seed and compaction | Cost: no extra model call per turn (6.1) |
| E10 | Re-seed at 60% context use, or after a quiet hour above 25% | Leaves room for a long turn; a quiet hour is the cheap moment |
| E11 | Device context stays in gff-hab's memory, latest value only; only the time zone is saved | Decision 3: no location history on the hab; a read is visible in the turn's tool line |
| E12 | Personal tools are served only on a Capcom route | A project agent has no reason to read the user's location |
| E13 | Weather and places run on the hab with keys from a key command | Keys stay off the phone and out of config files, as push does |
| E14 | Calendar and reminder cards return at once; the answer comes later as a note | A tap may come hours later; a turn must not wait |
| E15 | Event turns batch over 2 minutes, start on the small model, cap at 20 a day; each number is a setting (19.1) | The study's guardrails (2.6) |
| E16 | Agent reports start a turn at once, but act only as an event turn may | The user is waiting on that work; the report's text comes from an agent, not the user |
| E17 | Passive notices never reach Capcom | Their level already says nobody needs to look |
| E18 | Old pane conversations stay readable, never resumed by Flightdeck | Decision 1 with no data loss |
| E19 | The engine logs in with the user's subscription through the long-lived OAuth token `claude setup-token` makes (`CLAUDE_CODE_OAUTH_TOKEN`), from a key command (the installed hab's source) or gff-hab's environment, held in memory only and handed to the engine over its route; an API key stays an option (3.5, 10.2). Formerly Q1 | Owner, 2026-11-06: the owner researched the terms and holds that subscription use is fine; it runs on the plan the user already pays for |
| E20 | Capcom's facts and preferences (`capcom` guides) land at once with a "Noted" line and Undo, in user and event turns alike; tasks and findings stay proposals; only at Safe does a guide wait as a proposal (6.4). Formerly Q2 | Owner, 2026-11-06 (plan-xagejg31 (q), (r)). No review list for "I'm vegetarian"; Undo is one tap. Safe keeps its meaning (`app.md` 20.3): it is where the user asked to review every agent memory |
| E24 | `capcom.event_actions` defaults: Safe `ask`, Balanced and Bypass `allow`; a job still has no actions until the user grants them (9.6). Settled | Owner, 2026-11-06 (plan-xagejg31 (r)) |
| E25 | Every limit in this spec is a setting with its former fixed value as the default (20); only the ceilings in 20.2 stay fixed | Owner, 2026-11-06 (plan-xagejg31 (r)): "any cap should be" |
| E21 | The first build's event turns read and message only (9.4). Formerly Q3 | Owner, 2026-11-06. Outside text (a mail, a page) cannot start work until a person allows it |
| E22 | Capcom acts on events through Capcom jobs: rounds in `capcom` mode with a per-job `actions` list a person sets, capped by the levelled `capcom.event_actions`; the dashboard is the Rounds list filtered to `capcom` (9.6, stage A9). Formerly Q3 | Owner, 2026-11-06, asked for tunable acting with a per-job list and a dashboard, and to consider rounds. Rounds already hold every trigger, the store and the list; a second scheduler would duplicate them (9.6) |
| E23 | Capcom answers each round message a second time, `told`, `quiet`, `acted` or `unseen`, after the turn that took it (9.5) | Owner, 2026-11-06 (`rounds.md` 12 W1): the round's history shows what Capcom did |
| E26 | `gff-hab mcp` clears `CLAUDE_CODE_OAUTH_TOKEN` and `ANTHROPIC_API_KEY` from its own environment at start, so the credential never reaches a program it runs. Formerly Q4 | Coordinator, 2026-11-07. The process needs no credential; clearing it costs nothing |
| E27 | `ListMcpResourcesTool` and `ReadMcpResourceTool` are on the surface while `flightdeck` is the only MCP server the SDK's `init` lists (5.1). Formerly Q5 | Coordinator, 2026-11-07. They read only what that server offers; any other unexpected tool still fails the start check |
| E28 | `gff-hab capcom pause` ends the engine after its turn; gff-hab does not start it again until `gff-hab capcom resume` (11) | Coordinator, 2026-11-07: kept as built in A2. A stopped engine cannot write the session, so a terminal resume has one writer |

---

## 16. Open questions for the owner

None open. The owner settled Q1 to Q3 on 2026-11-06 (plan-xagejg31 (q)); they are decisions E19
to E22 in 15.

Found in A2 and settled by the coordinator on 2026-11-07: Q4 (E26), Q5 (E27) and pause (E28).

---

## 17. Owner approval needed: AGENTS.md exception

Needed only if A2's check (11) shows that the installed `claude` cannot resume the engine's SDK
sessions. A2 reports the result; the owner approves this text, or drops it, before it goes into
AGENTS.md. This study does not edit AGENTS.md.

Proposed text, under "How we build it", after "Native data stays native":

> - **Exception: the Capcom service.** The Capcom engine's SDK sessions may not resume in a
>   plain terminal with the installed `claude`. Its thread is kept by gff-hab and always reads
>   with `gff-hab capcom thread`; its memory is in Logbook. Every other session must resume
>   from a plain terminal.

A Codex engine (A6) needs the same check against `codex resume`.

---

## 18. Build stages

Part 14 (plan-p84uqi9p). Order: A2 and G; then A3 and A4; then A5; then A7 and A8; then A9
(after rounds stage 9 and rounds stage 7); A6 last.
Each stage ends with a fresh-eyes review where marked core.

| Stage | Builds | Acceptance tests |
|---|---|---|
| A2 (task-vz6mxa3f): engine and hab half. Core | 3, 4, 5, 7.1 to 7.4 hab side, 7.7, 11; the session record kind `capcom` with no pane; `[capcom] engine` (`claude`); the engine job in `gff-hab install` on macOS and Linux; Node.js check | With a fake model endpoint: two messages sent during a turn are taken in order and each marked read; Stop ends the turn and the next message runs; a `start_session` raises a confirmation and starts nothing before the answer; a dispatched session's `channel_send` to `capcom` starts a turn; a built-in tool outside 5.1 is never offered to the model; a `WebFetch` of an unseen URL raises an approval card and a seen one does not; a gff-hab restart mid-turn loses no reply; an engine kill mid-turn resumes the SDK session and resends untaken items; a tool call on the engine route records the Capcom session as the actor and files a proposal; personal tools are refused on a pane route; the start check refuses an SDK whose `init` lists a tool outside 5.1; a crash after an input was handed but before its echo resumes with that input taken once; five crashes in 10 minutes stop the restarts and `capcom resume` clears them; a gff-hab start adopts a running engine; each engine start gets a new token and the old one is refused; the OAuth token from `oauth_token_command` and from `CLAUDE_CODE_OAUTH_TOKEN` in gff-hab's environment each reach the bundled binary as `CLAUDE_CODE_OAUTH_TOKEN`, the command first, through `hello` and not the job's environment file; a planted value appears in no file under the state directory, the unit or plist `gff-hab install` writes, a log, a session record, a thread item, a notice or the catalog; an `oauth_token` key in the config file is refused naming the command key; an installed hab with only the environment variable gives the one line; `auth = "api_key"` passes `ANTHROPIC_API_KEY` instead; no token leaves the engine stopped with the row's "no Claude token" line; a refused token runs the command again once after 5 minutes; a second refusal stops the engine with one notice, and `capcom resume` reads the source again; losing the mark lets the turn finish, renames the record `capcom-earlier-1`, and a bare `capcom` from a peer reaches the new home; an install mid-turn interrupts nothing; Node.js missing and an unknown `engine` word each give one clear line; with `engine` unset the pane Capcom runs as before (12.4); `max_turns`, `restart_delay`, `crash_limit` and `crash_window`, `route_backoff_max`, `command_timeout` and `token_retry` each change the behaviour they name, and a value outside its range is logged once and the default runs; `capcom.approval_max_age` set to 2 minutes expires a card at 2 minutes, and `settings.set` refuses 0 with `invalid`. Live: `claude --resume <id>` in a plain terminal opens an engine session after `gff-hab capcom pause` (11; if not, report for 17). Live: study scenario 4 on two hosts |
| G (task-k0ydtgae): usage governor. Core | 10.1 (Capcom's side; the governor itself per its task) | Fake clock and hand-written readings: the share accumulates; at the cap a user turn goes to the small model, then waits with a note; event turns stop first; with no reading Capcom is listed unpaced; the `pace_*` keys move each step, and a set out of order runs the three defaults with one log line |
| A3 (task-cp5tvo4d): the thread in the app and gff | 7.2 to 7.6 app side, 7.7 gff | Swift tests over a fake hab: typing, delivered, read, a folded tool line, a divider with the summary it opened with, an image sent and shown, Stop, "Earlier threads" from another host, the pinned proposals line. Live on a device: a reply from the communication notification while the app is closed reaches the engine and the answer shows. Study scenario 3 (a flyer photo) answered with its details |
| A4 (task-0vh4b8jz): context and memory | 6 | Fake model: a thread past 60% re-seeds once at the turn end, never mid-turn, and waits for an open approval card; a fact stated before the re-seed is answered after it; a forced compaction updates the summary first and adds one divider; "New conversation" adds one divider and an empty summary; a preference becomes a `capcom` guide that lands at once with a "Noted" line, and its Undo deletes it; under `logbook.approval = agents` the same preference is a proposal and Undo rejects it; a guide with a project, or without the `capcom` tag, stays a proposal; a `capcom` guide from an event turn lands at once with a "Noted" line outside Safe, and is a proposal with "Noted, waiting for review" at Safe; Undo after the record was edited elsewhere deletes the edited record and says so; a task stays a proposal; the seed stays under its caps, with the defaults and with `seed_messages`, `seed_messages_chars`, `seed_logbook_bytes`, `reseed_at` and `summary_max_chars` set lower in the config; a value outside its range is logged and the default runs |
| A5 (task-j2xf34ld): personal tools | 8 | Study scenario 1 on a device: one location read, weather and places calls, ranked picks naming the preference, an Add card that writes one event. Tests: a question with no place in it reads no location; with no Google key `source: google` is refused and Apple answers; a weather reply carries the attribution; no calendar write without a tap; the device-context store is empty after a gff-hab restart and writes no file but the time zone; a tool line names the place and never coordinates; `[capcom.weather] cache` set to 0 makes every call reach the endpoint (a fake WeatherKit); `capcom.calendar_days` set to 3 sends 3 days |
| A7 (task-hj8cnr5v): Capcom writes first | 9 | A round message answers `accepted` and adds one `round` item; the same message id again answers `duplicate`; with `capcom.events.round_messages` off it answers `declined` `off`; a round message in quiet hours is accepted and waits; `capcom-service` shows in the catalog only while this hab holds the mark. Fake clock: an event in quiet hours waits and runs in one batch at the end; an urgent one runs at once; ten events in 2 minutes make one turn; the 21st event turn of a day waits; after a gff-hab restart quiet hours use the saved time zone; an event turn's `hand_off` is refused (9.4, E21); a passive notice reaches nothing; a round message with `level` `time-sensitive` runs at once in quiet hours, and still counts toward the daily turn budget; with the budget spent, a time-sensitive event waits for the next day like any event, in quiet hours or not; `capcom.events.daily_turns`, `.batch_window` and `.batch_max` set to other values move each limit, and `settings.set` refuses a value outside its range with `invalid`; after the turn that took a round item, the runner gets one `outcome` status item: `told` when the turn wrote to the user, `quiet` when it did not, the same outcome for two items one turn took, `unseen` for an item left untaken by a mark move, and one outcome from a peer runner that was asleep, delivered at its wake. Live: a watcher event reaches the phone as one Capcom message marked "wrote first" |
| A8 (task-6knreo9n): remove the pane Capcom | 12, 13 | `engine` defaults to `claude`; pane-era records are renamed `capcom-term-<n>`; `gff-hab capcom open` and the app's Capcom entry reach the service; pending context from a pane-era `capcom.json` reaches the first message; an old pane conversation reads in the chat view with no Resume; no test or doc describes the pane form; CI passes on Linux and macOS |
| A9 (task-pp4c1qq4): Capcom jobs act on events | 9.6; `rounds.md` 3 and 5.2 `actions`; `capcom.event_actions`; the "Capcom jobs" filter in the app and gff | Fake model and a fake runner: a `capcom` round with no `actions` gets its `hand_off` refused as in A7; with `hand_off` allowed and `capcom.event_actions = allow`, the event turn's `hand_off` runs and shows a card naming the job, and the outcome is `acted`; under `ask` the same call raises one confirmation and nothing runs before the tap; under `off` it is refused; a word the job does not list is refused; a fourth action in one turn is refused, and with `capcom.event_actions_per_turn` at 1 the second is; at 0 none runs; the level picker writes `ask` at Safe and `allow` at Balanced and Bypass; an `ask` card expires at `capcom.event_action_max_age`; a notice-only event turn allows nothing; two round items in one turn allow the union of their words; `start_session` under `allow` still raises the `confirm.start_elsewhere` card when that is on; `round.create` with `actions` from a pane or the Capcom route is refused `denied`, and removing a word from a pane works; an agent creates a `capcom` round, a person grants `hand_off`, and the agent's later prompt edit, pause and delete are refused; a channel message to `capcom` from a pane that carries `actions` fields gets no action allowed; under `ask` the turn's outcome is `acted` at its end, before the tap; an unknown word is `invalid`; export then apply keeps `actions`. Swift over a fake hab: the Capcom jobs list shows only `capcom` rounds with their actions and last outcome, and the switches edit `actions`. gff: the board's filter shows only `capcom` rounds. Live: a mail watcher job with `hand_off` hands a mail's task to the agent on its project, with one card |
| A6 (task-y8ogoq4d): Codex SDK engine | 4 on the Codex SDK; `[capcom] engine = codex` | The A2 and A4 tests pass against both engines, with each capability the Codex engine lacks taking its fallback (4.1); switching engines keeps the thread and Logbook memory (the switch is a re-seed); the plain-terminal check with `codex resume` |

---

## 19. Limits and settings

Owner, 2026-11-06 (plan-xagejg31 (r)): every cap is configurable. Each limit this spec names is
a setting, with its earlier fixed value as the default (E25). Two homes, by who tunes it:

- **Hab-side settings** (`app.md` 20.5) for what the user tunes in Capcom settings. Stored and
  enforced on the home hab, as quiet hours are (9.2). A value outside its range: `settings.set`
  refuses the call with `invalid` and `data.id`, and the stored value stays. A `[settings]` entry
  outside its range is logged and left out, and locks nothing (`app.md` 20.5, built choices).
- **`[capcom]` keys** in the hab's config file for tuning with no row in the app. A value
  outside its range, or of the wrong type, is logged once and the default runs (3.5). A key
  read "at once" applies from the next check that uses it; the rest at the next turn, unless the
  row says so.

### 19.1 Hab-side settings

All on the home hab, no level unless the row says so.

| Id | Default | Range | Section |
|---|---|---|---|
| `capcom.quiet_hours` | 22:00 to 07:00 | A start and an end time of day; the same time twice means no quiet hours | 9.2 |
| `capcom.events.notices`, `.rounds`, `.round_messages` | on | bool | 9.1 |
| `capcom.events.daily_turns` | 20 | 1 to 1,000 event turns a day | 9.3 |
| `capcom.events.batch_window` | 2 minutes | 10 s to 1 hour | 9.2 |
| `capcom.events.batch_max` | 20 events | 1 to 200 | 9.2 |
| `capcom.approval_max_age` | 10 minutes | 1 minute to 24 hours; no 0 (5.3) | 5.3 |
| `capcom.event_actions` | Levelled: Safe `ask`, Balanced and Bypass `allow`; `off` by hand only. Settled (E24) | `off`, `ask`, `allow` | 9.6 |
| `capcom.event_actions_per_turn` | 3 | 0 to 20; 0 allows no action whatever the jobs say | 9.6 |
| `capcom.event_action_max_age` | 60 minutes | 1 minute to 24 hours | 9.6 |

One app preference (`app.md` 20.4), since the app decides what it sends:
`capcom.calendar_days`, default 7, 1 to 31 days (8.1).

### 19.2 `[capcom]` keys

| Key | Default | Range | Section |
|---|---|---|---|
| `max_turns` | 50 | 1 to 500 | 3.5 |
| `max_turn_usd` | 2 | above 0 | 3.5, 10.1 |
| `daily_usd` | none | above 0, or none; at once | 10.1 |
| `restart_delay` | 5 s | 1 s to 10 minutes | 3.2 |
| `crash_limit`, `crash_window` | 5 crashes in 10 minutes | 1 to 100; 1 minute to 24 hours | 3.2 |
| `route_backoff_max` | 30 s | 1 s to 10 minutes; the backoff starts at 1 s | 3.2 |
| `summary_max_chars` | 6,000 | 1,000 to 50,000 | 6.1 |
| `summary_quiet` | 10 minutes | 1 minute to 24 hours | 6.1 |
| `reseed_at` | 60% | 20% to 90% | 6.2 |
| `reseed_quiet`, `reseed_quiet_at` | 60 minutes, 25% | 10 minutes to 24 hours; 0% to `reseed_at` | 6.2 |
| `seed_messages`, `seed_messages_chars` | 6 messages, 8,000 characters | 0 to 50; 0 to 64,000 | 6.2 |
| `seed_logbook_bytes` | 8 KB | 0 to 64 KB | 6.4 |
| `precompact_timeout` | 30 s | 5 s to 5 minutes | 6.3 |
| `typing_timeout` | 10 minutes | 1 minute to 1 hour | 7.3 |
| `location_stale` | 30 minutes | 1 minute to 24 hours | 8.2 |
| `[capcom.weather] cache` | 10 minutes | 0 (no cache) to 24 hours | 8.3 |
| `command_timeout` | 30 s | 1 s to 5 minutes; for `oauth_token_command` and `api_key_command` | 10.2 |
| `token_retry` | 5 minutes | 30 s to 24 hours | 10.2 |
| `wake_retry` | 10 s | 1 s to 5 minutes; one retry | 14 |
| `pace_daily_share` | 15% of the weekly window a day | 1% to 100% | 10.1 |
| `pace_event_stop`, `pace_small_at`, `pace_wait_at` | 80%, 100%, 120% of the share | 10% to 500% each, in that order; a set out of order is logged and all three defaults run | 10.1 |

The four `pace_*` keys are Capcom's inputs to the usage governor (task-k0ydtgae, stage G), which
reads them; the governor's own rules stay in its task.

### 19.3 Fixed values kept

| Value | Why it is not a setting |
|---|---|
| `capcom.thread.page` returns at most 200 items | A ceiling on one API reply: a reply is one JSON line (`hab-logic-api.md` 3.1), and a bound keeps it small. A client asks for fewer and pages; it never limits what the thread holds |
| The weather cache key rounds to 2 decimal places | Not a limit: it is the cache key, and it keeps the exact fix out of the cache |
| The SDK's own limits (context window, compaction) | The SDK's, not Flightdeck's |

Limits other specs own stay there and are not changed here: the outbox's 7 days (`mesh.md` 6.3),
attachment expiry (`app.md` 4.4), the push budget (`app.md` 16.2), and the 60-minute expiry of
`capcom.md` 6.1's confirmations for `start_session` and `set_up_project`.

**Changes to other specs.** `app.md` 20.3 to 20.5 is not edited by this spec. Stage A7 adds the
rows of 19.1 for quiet hours, the event switches, the daily turn budget and the batch settings;
A2 adds `capcom.approval_max_age`; A9 adds the three `capcom.event_action*` rows; A5 adds the
`capcom.calendar_days` preference (13). Each stage that reads a key of 19.2 adds it to
`docs/examples/hab.toml`.

---

## 20. Sources

- Claude Agent SDK docs, read 2026-11-06: `agent-sdk/overview`, `agent-sdk/agent-loop`,
  `agent-sdk/streaming-vs-single-mode`, `agent-sdk/sessions`, `agent-sdk/permissions`
  (https://code.claude.com/docs/en/).
- `legal-and-compliance` (https://code.claude.com/docs/en/legal-and-compliance), read
  2026-11-06.
- https://support.claude.com/en/articles/15036540, read 2026-11-06.
- Codex SDK, https://learn.chatgpt.com/docs/codex-sdk, read 2026-11-06.
- `swarm-study.md` 2, 5, 7, 8; plan-xagejg31 decisions (m), (n), (p).
- `capcom.md`, `app.md` 4.4, 4.10, 7, 8, 14, 20; `logbook.md` 2.2, 9, 11, 15.1; `mesh.md` 6;
  `harness-adapters.md`, environment contract; `rounds.md` 4.3, 5.2; `groups.md` 10, 12.1, 15.2.

Facts marked *(gk)* come from general knowledge, not from a page opened for this spec. The stage
that builds on them confirms them.
