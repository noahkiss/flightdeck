# Harness adapters: behaviour spec

Status: draft for review. Part 4, study stage.

This spec says how `gff-hab` detects, observes and drives three harnesses: Claude Code, Codex
and opencode. It covers detection, the state channel, the conversation channel, the input path,
the permission answer path and the native resume id. It also fixes the environment contract and
one state event model for every harness. Implementation stages work from this file.

Versions studied: Claude Code 2.1.3xx, Codex 0.166, opencode as documented on 2026-10-31.
Every claim about a harness names its source in [Sources](#sources).

## Contents

1. [Terms](#terms)
2. [Rules for every adapter](#rules-for-every-adapter)
3. [Detection](#detection)
4. [Environment contract](#environment-contract)
5. [Hook endpoint](#hook-endpoint)
6. [State event model](#state-event-model)
7. [Claude Code](#claude-code)
8. [Codex](#codex)
9. [opencode](#opencode)
10. [Input delivery](#input-delivery)
11. [Permission answers](#permission-answers)
12. [Edge cases](#edge-cases)
13. [Fixtures](#fixtures)
14. [Open decisions](#open-decisions)
15. [Sources](#sources)

## Terms

| Term | Meaning here |
|---|---|
| pane | One PTY that `gff-hab-holder` owns. Its stable id is the pane uuid. |
| harness | The agent program in a pane: `claude`, `codex` or `opencode`. |
| session | The harness's own conversation, named by its native id. |
| adapter | The per-harness code in `gff-hab`. It is thin: it maps native signals to our model. |
| state channel | Where the adapter learns what the agent is doing. |
| conversation channel | Where the adapter reads messages for the chat view. |
| input path | How the adapter sends the owner's text to the agent. |
| structured client | A second program that speaks a harness protocol (ACP, stream-json, `--mode rpc`, app-server). |

## Rules for every adapter

1. **One owner per session.** The TUI in the pane owns the session. Never point a structured
   client at a live TUI's session id. ACP, `-p --output-format stream-json` and `--mode rpc`
   start a second process. That process forks the conversation. The only exception is a
   protocol the TUI itself is a client of (opencode's server, Codex's app-server). Even then the
   adapter attaches to the server the TUI already uses; it never starts a second one for the
   same session.
2. **Never answer a permission from screen text.** Answer only when the structured state says
   `blocked` with kind `permission` for that pane, and the request id matches. See
   [Permission answers](#permission-answers).
3. **Deliver input only when the state allows it.** See [Input delivery](#input-delivery).
4. **Native data stays native.** Read transcripts where the harness writes them. Never move,
   rewrite or copy them. `claude --resume <id>`, `codex resume <id>` and
   `opencode --session <id>` must keep working from a plain terminal.
5. **Hooks never slow a turn.** A hook command exits fast and prints nothing on stdout, except
   the one opt-in blocking permission hook. On Claude's `UserPromptSubmit`, hook stdout lands in
   the prompt.
6. **Fail visibly.** An unknown record type, a parse error or a missing hook shows as a degraded
   adapter in the status board. It never shows as a wrong state.
7. **The server answers terminal queries once.** `gff-hab` answers terminal queries, including
   the OSC 7501 `?` query, and strips them from the byte stream sent to clients. A client that
   also answered would inject bytes into the agent's input.

## Detection

Detection answers one question per pane: which harness runs in the foreground, if any.

Signals, strongest first:

| Signal | What it proves | Source |
|---|---|---|
| `gff-hab` spawned the harness itself | Harness kind, and for Claude the session id (see below) | Our own spawn record |
| A hook arrived with this pane's token | Harness kind and native session id | [Hook endpoint](#hook-endpoint) |
| OSC 7501 report with `app=` | Harness kind and state | Program status protocol |
| Foreground process name | Harness kind only | herdr detection, process part |
| Screen manifest match | A coarse state (`working`, `blocked`, `unknown`) | herdr detection manifests |

Behaviour:

- **Process name.** Take the foreground process group of the PTY. Match the basename of the
  group leader first, then other members, against each manifest's id and aliases. herdr does the
  same and scores candidates when several match.
- **Screen manifests.** herdr publishes one TOML manifest per agent plus an `index.toml`. Each
  manifest has a `version`, a `min_engine_version` and an ordered list of rules. A rule names a
  state, a priority, a screen region (for example the OSC title, or the bottom N non-empty lines)
  and a match (substring, regex, `any`/`all`/`not` groups). We vendor these files as data and
  write our own matcher from their documented shape. We do not port herdr's matcher.
- **Licence.** herdr is Apache-2.0. Vendored manifests keep their licence text and a NOTICE
  entry that names the source and the commit or catalogue version.
- **Refresh.** herdr serves a catalogue index over HTTPS. Its updater refuses a manifest older
  than the cached one. Ours does the same: compare `version`, never downgrade, skip a manifest
  whose `min_engine_version` is above our matcher's engine version, and keep the last good copy.
  gff-hab runs the refresh itself, on a schedule and by command, and reports drift: a manifest
  it cannot use, or a harness version newer than any manifest knows. A manual refresh from a
  downloaded catalogue works offline. The vendored copies in the repo are the starting point.
- **Screen state is a fallback.** A manifest match may set state only when no hook, OSC 7501 or
  server signal arrived for that pane in the current turn. It never sets `blocked:permission`
  for the purpose of answering. It may raise a "looks blocked" hint in the UI.

Built choices (`internal/detect`, `gff-hab manifests`):

- **Vendored set.** `internal/detect/manifests/` holds every herdr manifest and `index.toml`,
  copied unchanged from one pinned herdr commit. `SOURCE` names the commit; `LICENSE` is the
  Apache-2.0 text; the repo `NOTICE` names both. gff-hab embeds the set.
- **Engine version 3.** The matcher reads every region and gate of manifest engine 3. A
  manifest with a higher `min_engine_version`, an unknown key, an unknown state or region, a
  rule with no matcher, or a regex Go cannot compile is unusable. An unusable manifest is a
  problem (drift), never a crash; the previous copy stays in force.
- **Regex dialect.** The manifests use a regex dialect close to Go's. The loader rewrites
  `\uXXXX` and `\u{...}` to `\x{...}`, and `\p{Alphabetic}` to letters plus letter numbers.
  `\d`, `\w`, `\s` and `\b` are ASCII in Go; the vendored rules use them around ASCII text.
- **Matching.** `contains` needles match the lower-cased region; every needle must appear.
  Every `regex` must match the region; every `line_regex` must match at least one line. `all`,
  `any` and `not` nest. The matching rule with the highest priority wins; the first in the file
  wins a tie. No match gives `unknown`.
- **Process names.** Each foreground process offers its executable name, the base of argv[0],
  and, for a script interpreter (node, bun, deno, python), the base of its script with the
  extension removed. argv[0] catches a harness installed as a versioned binary behind a
  symlink. The leader is tried first, then the other members in order.
- **`AGENT_KIND`.** A detection's kind is the manifest id: `claude`, `codex`, `opencode`, or
  another manifest's id. Aliases resolve to the id.
- **Data directory.** `gff-hab manifests refresh` writes into `manifests/` under the state
  directory. A manifest id must match `^[a-z0-9][a-z0-9_-]*$`, since it names the file, and each
  write is checked to land in that directory. The catalogue must be an https URL or a local
  path; a redirect that leaves https is refused. A date-shaped version (first segment 1000 or
  more) from after next year is refused by refresh and ignored by load, so one absurd version
  cannot outrank every later update. A copy there replaces the vendored one only when it is valid and not older, so a
  gff-hab upgrade with newer vendored copies wins. `-from` takes a URL, a local `index.toml` or a
  directory, so a refresh works offline. `-dry-run` prints the diff and writes nothing.
  `gff-hab manifests list` prints the active set and exits 1 on drift. `gff-hab serve` logs
  drift at start. A scheduled refresh inside gff-hab is later work; it calls the same code.
- **Tested versions.** `detect.Tested` records the newest harness version each adapter was
  tested on, as a prefix (`claude` 2.1, `codex` 0.166; opencode has none yet). A reported
  version above it, or one with no number in it, gives a version warning. The warning rides on
  the pane's `agent.state` (see [State event model](#state-event-model)); it does not stop the
  adapter.

**Claude session id at spawn.** When `gff-hab` starts Claude, it may pass
`--session-id <uuid>`. The adapter then knows the transcript before the first hook fires. Codex
and opencode have no such flag; their id arrives with the first hook or server event.

## Environment contract

`gff-hab-holder` sets these variables when it spawns a pane. Each harness passes them on to
its hooks and tool commands, so anything inside the pane can find `gff-hab`.

| Variable | Set by | Value | Who reads it |
|---|---|---|---|
| `GFF_HAB` | gff-hab, at spawn | The hook endpoint URL for this pane, with a per-pane token (see below) | `gff-hab hook`, the agent kit, any agent that wants to name itself |
| `AGENT_KIND` | holder when it starts a known harness; else the harness's own start hook | `claude`, `codex` or `opencode` | hooks, tools, the agent kit |
| `AGENT_SESSION_ID` | the harness, through its start hook, where the harness allows it | The harness's native session id | tools and child processes that must name their session |

`GFF_HAB` is the one hab address variable. One variable must carry two facts: where `gff-hab`
listens, and which pane is calling. So its value is per pane:
`http+unix://<socket path>/hooks/<pane token>` locally. The token is per pane, known only to
the pane and `gff-hab`, and changes when the pane is respawned (see Hook endpoint, Token). This gives
attribution and a light capability check in one value. A hook with no `GFF_HAB` is outside any
pane we own; it exits 0 and sends nothing.

Per harness:

| Harness | `AGENT_SESSION_ID` reaches tool commands? | How |
|---|---|---|
| Claude Code | Yes | A `SessionStart` hook appends exports to the file named by `CLAUDE_ENV_FILE`. Claude also exports its own `CLAUDE_CODE_SESSION_ID`. |
| Codex | No | Codex has no way for a hook to export a variable to later commands. The id comes from hook payloads, and readers find it through `gff-hab`. |
| opencode | Yes, through a plugin | A plugin can set environment for shell tools (`shell.env` event). The server API also returns the id. |

Inheritance rules:

- A nested agent inherits all three variables from its parent pane. Its hooks must not rebind
  the pane. Each adapter carries a nested-run guard (see [Edge cases](#edge-cases)).
- When the holder restores a pane after a host restart, it drops `AGENT_SESSION_ID` and the
  harness's own session variables from the respawn environment. A resumed harness sets fresh
  ones.

## Hook endpoint

All three adapters post hook events to one endpoint in `gff-hab`. This matches the interim GFF
endpoint, which takes an optional `harness` field that defaults to `claude`.

- **Transport.** HTTP over the hab's local Unix socket, at the path in `GFF_HAB`. The pane
  token in the path names the pane.
- **Caller.** One small command, `gff-hab hook <harness> <event>`, reads the harness's hook JSON on
  stdin once, adds our envelope, posts it, and exits. The post is synchronous, with a cap under
  one second, so a dead `gff-hab` costs the turn nothing. It never writes to stdout, except in
  blocking permission mode.
- **Envelope.** `harness`, `event` (the native event name), `t` (client clock, ms since epoch),
  `n` (a per-process counter from the hook command, see ordering), `payload` (the native JSON,
  untouched).
- **Server side.** `gff-hab` validates the envelope, resolves the pane from the token, and hands
  the payload to that harness's adapter. Unknown harness values are logged and dropped. Unknown
  native events are logged at debug level and dropped.
- **Hook config.** `gff-hab` prints the hook config for each harness. Writing it into a
  harness's settings is a separate, approved step (plan decision, owner).

Built choices (`internal/hook`, `internal/harness`, `gff-hab hook`):

- **Socket.** The endpoint is HTTP on its own Unix socket, `hook.sock` beside the control
  socket (`-hook-socket` or `$GFF_HAB_HOOK_SOCKET` override it). The control socket speaks
  JSON-RPC lines, so the hook endpoint does not share it. The socket is mode 0600 in the same
  private directory, and a caller running as another user is refused. No TCP port.
- **Address.** `GFF_HAB` is `http+unix://<absolute socket path>/hooks/<token>`. gff-hab puts
  it into the environment of each pane it spawns, replacing any `GFF_HAB` the caller passed.
- **Token.** 96 lower-case hex digits: the pane uuid (32), then
  HMAC-SHA256(secret, `"gff-hab hook token v1\0"` + uuid) (64). The secret is 32 random bytes in
  `hook.key` in the state directory. gff-hab creates it on first start with mode 0600, as a
  complete temp file hard-linked into place, so racing starts agree on one secret. It refuses to
  start when the file is a symlink, not a regular file, owned by another user, open to group or
  others, or not 32 bytes; it never replaces such a file. No registry is stored: the same secret
  gives the same token after a gff-hab restart, so a pane that outlives gff-hab keeps posting. A
  respawned pane has a new uuid, so its token changes.
- **Checks.** gff-hab accepts a token only when its hash matches (a constant-time compare) and
  the holder link follows the pane: listed, not lost with its holder, and not released. A forged
  token, one spliced from two panes, one made under another secret, and the token of a pane that
  is gone are all unknown. When the link reports a pane gone, gff-hab also drops its agent state.
- The welcome lists the `agent-state` capability whenever the holder link is on.
- **Answers.** `POST /hooks/<token>` answers 204 when accepted, 400 for a bad envelope, 404
  for an unknown token (logged once per token) and 413 above 8 MiB. A post with no `t` gets the
  server's clock.
- **The command.** `gff-hab hook <harness> [event]` reads stdin, wraps it and posts it, then
  exits 0, always, and never writes to stdout. With no event argument it takes
  `hook_event_name` (Claude Code, Codex) or `type` (opencode) from the payload. The post is
  synchronous with a 900 ms budget for the whole run, enforced by a timer that exits 0: a
  stopped gff-hab fails at dial in a few milliseconds, and a hung one costs at most the budget.
  This keeps posts in order, which a background post would not. `n` is always 1, since each
  hook is its own process; ordering uses `t`.
- **Entrypoint guard.** For Claude Code the command sends nothing when
  `CLAUDE_CODE_ENTRYPOINT` is set and is not `cli`. An unset variable is allowed, so a Claude
  build that does not export it still reports.
- **`gff-hab hook -print <harness>`.** Claude Code and Codex get a JSON object in their shared
  hooks shape (`{"hooks": {"<Event>": [{"hooks": [{"type": "command", ...}]}]}}`), one entry per
  event the adapter uses, with a 5 second timeout (2 seconds for Codex's `SessionEnd` and
  `Interrupt`, which Codex caps at three). opencode gets a JavaScript plugin that spawns
  `gff-hab hook -t <ms> opencode <event type>` with the event JSON on stdin. The plugin stamps
  `Date.now()` itself, because each event spawns its own process and those may run out of
  order. `-t` sets the envelope's `t`; without it the command stamps its own clock. `-command`
  sets how the harness runs gff-hab.
- **Permission input.** A permission request copies the tool input only up to 64 KiB. A larger
  input (a whole file being written) is left out and `input_truncated` is set; `command` and
  `files` stay exact. This keeps one request from filling a client's 8 MiB event backlog.
- **opencode events.** The plugin posts `session.created` (binds, `idle`), `session.deleted`
  (unbinds), `session.status` (`busy` or `retry` → `working`; `idle` is left to
  `session.idle`), `session.idle` (→ `done`), `session.error` (→ `error`), `permission.asked`
  and `permission.updated` (→ `blocked:permission`, request from the event's id, permission and
  metadata), and `permission.replied` (resolves the request, → `working`).

Why a client clock: hook payloads carry no timestamp or sequence. A harness may run its hooks
in parallel, and the opencode plugin spawns one process per event, so posts can land out of
order. Without `t`, the server cannot tell a late event from a new one.

## State event model

### States

The model uses the OSC 7501 state set, so every source maps onto it without loss.

| State | Meaning |
|---|---|
| `idle` | At rest, waiting for input. Also set after the owner interrupts. |
| `working` | A turn, tool call or compaction is running. |
| `blocked` | Waiting for the owner. Carries `kind`: `permission`, `question` or `auth`. |
| `done` | The turn finished and the owner has not looked yet. |
| `error` | The turn failed and stopped. |

Two further values describe the pane, not the agent:

| Pane value | Meaning |
|---|---|
| `none` | No harness detected in the foreground. |
| `exited` | The harness process ended. The last `done` or `error` stays visible; `working` and `blocked` are dropped, as OSC 7501 does on exit. |

`done` becomes `idle` when a client focuses the pane or the owner sends input.

### Fields

Each `agent.state` event carries (built as `api.AgentState`; the event's id is the pane id):

| Field | Type | Notes |
|---|---|---|
| `pane` | uuid | Stable pane id. The key of the delta. |
| `harness` | string | `claude`, `codex`, `opencode`, or another manifest id. |
| `session` | string, optional | Native session id once known. |
| `state` | enum | Above. |
| `kind` | enum, optional | Only with `blocked`. |
| `request` | object, optional | With `blocked:permission`: request id, tool name, exact command or file list, cwd. Copied from the structured payload, never summarised. |
| `source` | enum | `server`, `hook`, `osc`, `screen`, `spawn`. |
| `turn` | string, optional | Native turn or prompt id where the harness gives one (Claude `prompt_id`, Codex `turn_id`). |
| `at` | time | When `gff-hab` accepted the event. |
| `revision` | integer | Per pane, raised on every accepted change. |
| `last` | enum, optional | With `exited`: the last `done` or `error` before the exit. |
| `version` | string, optional | The harness version, once reported (envelope `version`, or a `version` or `cli_version` field in the payload). |
| `warning` | object, optional | Set while `version` is above the adapter's tested range, or unreadable (see Detection). |

Built choices:

- `state` takes the agent states and the pane values in one word set: `idle`, `working`,
  `blocked`, `done`, `error`, `none`, `exited`. On `exited`, `last` keeps the last `done` or
  `error`; `working` and `blocked` are dropped.
- A change is accepted only when a visible field changes, so a run of `PreToolUse` hooks emits
  one `working`. The source alone changing is not a change.
- `revision` lives in memory and restarts with each gff-hab epoch, like the state itself, which
  is rebuilt after a restart.
- The snapshot carries the states as the optional `agents` array of the structure. A scoped
  subscription gets the states of the panes in its scope.
- While a session lives in the pane, a hook from another harness or another session is dropped.
  A session start binds a free pane, and a session start of the same harness rebinds it, so a
  session that died with no end hook never holds the pane. A subagent hook never binds.
- Known gap until the holder link reports foreground-process changes: a nested run of the
  same harness (Codex inside Codex) rebinds the pane at its session start, and a harness that
  died with no end hook keeps the pane from another harness until that harness's process exit
  is seen. Claude's entrypoint guard keeps nested `claude -p` runs out already.
- A second `blocked` of the same kind with no request keeps the open request. Claude fires both
  `PermissionRequest` and `Notification` (`permission_prompt`) for one prompt, in either order.
- A subagent's hook never moves the pane out of `blocked`; only a resolution of the open
  request or the main agent does.
- `done` becomes `idle` through `Store.Seen`, which the input path and focus will call once
  they exist.

### Source precedence

When sources disagree inside one turn, the stronger one wins:

`server` > `hook` > `osc` > `screen`.

- A weaker source may not overwrite a state a stronger source set in the same turn.
- A new turn (a new `turn` id, or a prompt submit event) resets precedence.
- `screen` may only fill a gap: it applies when no other source spoke for the current turn.

### Ordering

- Each hook command stamps `t`. `gff-hab` keeps the last accepted `t` per pane and source.
- An event with an older `t` than the last accepted one for the same pane and source is a late
  arrival. It is recorded in the pane's history but does not change current state.
- Exception: `blocked` and its matching resolution carry request ids. A resolution for request
  X clears `blocked` for X regardless of `t`. A late `blocked` for X after its resolution is
  dropped.
- Clocks are local: the hook command and `gff-hab` run on the same host.
- Built: a session start or a new turn (a prompt submit, or a new turn id) is never late, and it
  resets precedence. An event more than 5 seconds behind the last accepted `t` is taken as a
  wall-clock step back, not a late arrival, so a stepped clock cannot freeze the pane. There is
  no per-pane history yet; a late arrival is dropped.

### How events reach the stream

- The adapter produces state changes. `gff-hab` writes each accepted change as an
  `agent.state` event on Part 2's event stream, with the stream's `epoch` and `seq`.
- The Part 2 snapshot includes the current `agent.state` for every pane. A client that sees a
  new `epoch`, or a `missed` notice, takes a fresh snapshot.
- The adapter never writes escape codes into the pane. The pane's own OSC 7501 reports are
  input to the adapter, parsed from the byte stream.

## Claude Code

| Channel | Behaviour |
|---|---|
| Detection | Process name `claude` (manifest id `claude`, alias `claude-code`). `--session-id` when hab spawns it. |
| State | Two sources. (1) Native OSC 7501 reports, sent only after the terminal answers the `?` query; hab answers it. (2) Hooks: `UserPromptSubmit` → `working`; `PreToolUse`/`PostToolUse` → `working`; `PermissionRequest` → `blocked:permission`; `Notification` with `permission_prompt` → `blocked:permission`, `elicitation_dialog` → `blocked:question`, `idle_prompt` → `idle`; `Stop` → `done`; `StopFailure` → `error`; `SessionStart`/`SessionEnd` bind and unbind the session. |
| Conversation | Tail the session JSONL named by `transcript_path` in every hook payload (under the Claude config dir, `projects/<encoded cwd>/<session id>.jsonl`). Read records of type `user` and `assistant` for the chat view, linked by `uuid` and `parentUuid`. Use `attachment` records for the context ledger. Skip every other record type. |
| Input | PTY: bracketed paste of the text, then Enter. |
| Permission answer | Default: the TUI dialog, answered by the owner in the terminal, or by PTY keys from the app after the checks in [Permission answers](#permission-answers). Opt-in: a blocking `PermissionRequest` hook (see open decisions). |
| Resume id | `session_id` in every hook payload; `claude --resume <id>`. |

Notes:

- The transcript format is undocumented. Records carry a `version` field: the reader keys its
  parser on the Claude version and fails visibly on an unknown shape.
- `transcript_path` may lag the current turn. The reader tails the file; it never treats a short
  read as the end of a turn. `Stop` marks the end.
- Subagent hooks carry `agent_id` and the parent's `transcript_path`. The adapter ignores hooks
  with `agent_id` for pane binding. Subagent transcripts are a later chat-view feature.
- A nested `claude -p` inherits the pane's variables but runs with a different entrypoint value
  (`CLAUDE_CODE_ENTRYPOINT` is `cli` only for the interactive TUI). The hook command sends
  nothing unless the entrypoint is `cli`.

## Codex

| Channel | Behaviour |
|---|---|
| Detection | Process name `codex` (manifest id `codex`). The manifest also marks Codex's own startup dialogs (trust this folder, update available) as `blocked`. |
| State | Hooks. Codex does not emit OSC 7501 itself. `SessionStart` binds; `UserPromptSubmit`, `PreToolUse`, `PostToolUse` → `working`; `PermissionRequest` → `blocked:permission`; `Stop` → `done`; `Interrupt` → `idle`; `SessionEnd` unbinds. With the app-server path (option B or C below), server notifications replace hooks as the state source. |
| Conversation | Depends on the daemon decision. Without the app-server: tail the rollout JSONL named by `transcript_path` (under the Codex home, `sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl`). Each line has `timestamp`, `type` and `payload`; use `session_meta`, `turn_context`, `response_item` and `event_msg`, and skip the rest. With the app-server: `thread/resume` with `excludeTurns`, then `item/*` notifications. |
| Input | PTY: bracketed paste, then Enter. With the app-server: `turn/start` when idle, `turn/steer` with `expectedTurnId` when working. |
| Permission answer | Without the app-server: TUI dialog or PTY keys, after the checks below; or a blocking `PermissionRequest` hook. With the app-server: answer the server request (`item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval`) with `accept`, `acceptForSession`, `decline` or `cancel`; `serverRequest/resolved` confirms. |
| Resume id | `session_id` in hook payloads, `thread.sessionId` on the app-server; `codex resume <id>`. |

Notes:

- **Hook trust.** Codex runs a non-managed hook only after the owner trusts it, keyed by the
  hook's hash. A new or changed hook is skipped with no message. Health checks must prove
  delivery (a `SessionStart` event arrived for this pane), not config presence.
- **Short end hooks.** `SessionEnd` and `Interrupt` hooks default to one second and cap at three.
  The hook command must post within that budget; `gff-hab` also infers end from process exit.
- `AGENT_SESSION_ID` cannot reach Codex tool commands (see the environment contract).
- The rollout format is undocumented and changes between versions. `session_meta` carries
  `cli_version`; the reader keys on it.

## opencode

| Channel | Behaviour |
|---|---|
| Detection | Process name `opencode` (manifest id `opencode`, aliases `open-code`). |
| State | The server's SSE stream `/event`: `session.status`, `session.idle` (→ `idle` or `done`), `permission.asked` (→ `blocked:permission`), `permission.replied`, `session.error` (→ `error`). A plugin can post the same events to our hook endpoint when the server is not reachable. |
| Conversation | The server API: `GET /session/:id/message`, plus `message.updated` and `message.part.updated` events. opencode stores sessions in its own database; the adapter never opens the database file. |
| Input | Prefer the TUI control endpoints (`/tui/append-prompt`, `/tui/submit-prompt`), so the text shows in the TUI as typed. Fallback: PTY paste. Do not use `prompt_async` against a session a TUI is showing unless tests prove the TUI renders it as its own turn. |
| Permission answer | `POST /session/:id/permissions/:permissionID` with the reply. The request id comes from `permission.asked`. |
| Resume id | Session id from the server; `opencode --session <id>`. |

Notes:

- Running `opencode` starts a TUI and a server; the TUI is a client of that server. So the
  adapter may talk to that same server without forking anything. This is the one harness where
  a structured client is safe by design.
- To reach the TUI's server, hab spawns opencode with `--hostname 127.0.0.1 --port <port>`,
  allocated per pane, and sets `OPENCODE_SERVER_PASSWORD` from a per-pane secret so other local
  users cannot drive it. A pane started by hand has no known port; the adapter falls back to
  plugin hooks and the PTY.
- The OpenAPI document lives at `/doc` on that server. The adapter records the document's
  version and fails visibly on an unknown one.
- `opencode serve` or `opencode attach` would start or attach a second front end. The adapter
  never starts a second server for a session a TUI already owns.

## Input delivery

The owner's text reaches the agent only through the paths in each harness table.

| Pane state | What `gff-hab` does with new input |
|---|---|
| `idle`, `done` | Deliver now. |
| `working` | Queue it in hab and deliver on the next `idle` or `done`. Codex on the app-server may use `turn/steer` instead, because steering is a native, safe operation. |
| `blocked` | Refuse with a clear reason. Only a permission answer that matches the open request goes through. |
| `none`, `exited`, unknown | Refuse. Offer to start or resume the harness. |

Further rules:

- **Paste, then Enter.** Wrap the text in bracketed paste, then send Enter as its own write. A
  multi-line message stays one message.
- **The person comes first.** If any client typed into the pane in the last two seconds, hold
  the delivery until typing pauses. herdr does the same.
- **Agent messages are marked.** Text from another agent (channels) arrives wrapped in a short
  envelope naming the sender, so the receiving agent knows it is not from the owner.
- **Read back.** After delivery, the adapter expects a `working` signal or a new user record in
  the conversation within a few seconds. If neither comes, it shows "not delivered" and does
  not retry on its own.

## Permission answers

- **Preconditions.** All must hold: the pane is `blocked:permission` from a `server` or `hook`
  source; the app's answer names the same request id; the request is still open.
- **Prefer structured replies.** opencode's permission endpoint, Codex app-server responses, or
  the opt-in blocking Claude hook. Keystrokes are a fallback.
- **Keystroke fallback.** Send only the keys for the chosen option. Then check, within a short
  window, that a resolution signal arrives (`PostToolUse`, `serverRequest/resolved`, a state
  leave). If it does not, show the pane as needing the owner in the terminal.
- **Show the exact request.** The chat view shows the tool, the exact command or file list, and
  the cwd from the payload. It never shows a summary in place of the command.
- **Screen text never decides.** A manifest that says "looks blocked" may surface a hint. It can
  never enable an answer button.

## Edge cases

| Case | Behaviour |
|---|---|
| Untrusted Codex hooks are skipped silently | The status board shows "no hook seen" for a Codex pane that has run a turn with no `SessionStart` event. It links to the trust step. |
| A hook fires with no pane | `GFF_HAB` is absent or its token is unknown. The hook command exits 0 and sends nothing; `gff-hab` logs an unknown token once per token. |
| A hook fires with no terminal | Claude may start hooks without a controlling terminal. Our hooks never need one: they post to `GFF_HAB` and never write escape codes to a TTY. |
| Nested agent in a pane | Claude: entrypoint guard. Codex and opencode: the adapter binds the pane to the first session per process and ignores hooks whose session id differs while that session lives, unless the foreground process changed. |
| Subagents | Claude hooks with `agent_id` never rebind the pane. |
| Transcript format drift | Each reader pins the harness versions it was tested on. An unknown version reads in a tolerant mode that skips unknown records and flags the adapter as degraded. gff-hab reports the degraded adapter as drift. A CI job runs new harness versions against our fixtures. |
| Transcript not yet written | Claude may create the file after the first hook. The reader waits for the file; it does not fail the pane. |
| `gff-hab` restarts | Hook posts during the gap are lost. On start, the adapter rebuilds state from the transcript tail, the last OSC 7501 report in the holder's byte ring, and the process table. It marks state as `source: screen` until the next hook. |
| Harness upgrade mid-session | The running process keeps its version; only new panes see the new one. The reader picks its parser per session from the version in the transcript, not from the installed binary. |
| Out-of-order hooks | See [Ordering](#ordering). |
| Codex daemon update | With the shared daemon, `codex app-server daemon update` may interrupt running work in every pane. This is one reason for the open decision below. |

## Fixtures

Each adapter ships fixtures written by hand. A fixture is a hook event sequence plus the
matching transcript lines or server events, and the expected `agent.state` stream. No fixture
comes from a real session. Every fixture states the harness version its shape follows.

## Open decisions

### Codex: shared daemon or `--no-daemon`

Decision: open (owner). Blocks the Codex stage.

Codex 0.157 and later start a shared, per-user app-server daemon by default. The TUI is a
client of it, and the agent loop and its tool commands run in the daemon, not under the pane.
`codex --no-daemon` runs without it.

| | A. `--no-daemon` | B. Shared daemon | C. Private app-server per pane |
|---|---|---|---|
| How | hab starts `codex --no-daemon` in the pane | hab starts plain `codex`; the adapter attaches through `codex app-server proxy` | hab starts one `codex app-server` per pane on a Unix socket, as a child of the holder, and the TUI with `--remote unix://<path>`; the adapter connects to the same socket |
| Who owns tool processes | The holder (they are the TUI's children) | The daemon, outside our holder | The holder |
| Survives a `gff-hab` restart | Yes | Yes | Yes |
| Survives a Codex daemon update | Not affected | No: an update may interrupt work in every pane | Not affected |
| Pane env (`GFF_HAB`) reaches tools | Yes | No: tools inherit the daemon's env | Yes, if hab passes it to the app-server |
| Chat view | Rollout JSONL tail | Live `item/*` events | Live `item/*` events |
| Permission answers | PTY keys or blocking hook | Structured | Structured |
| Steering while working | No (queue in hab) | `turn/steer` | `turn/steer` |
| `codex agents` sees the session | No | Yes | No |
| Unknowns | None | Which client receives approval requests when two are attached; docs are silent | Same as B, plus whether the TUI and a second client can share one thread cleanly |
| Effort | Lowest; same shape as Claude | Medium | Highest |

Recommendation: **A now, C as a later spike.** A keeps every process under the holder, so
"sessions outlive updates" holds and the pane's env reaches tools. It reuses the Claude
pattern (hooks + transcript + PTY), so it needs no new protocol code. B breaks two of our rules
at once: tools escape the holder, and a Codex update can stop work in every pane. C has B's
benefits without those costs, but depends on undocumented multi-client behaviour. A short
spike with two clients on one private app-server would settle it. A `/consensus` run can
inform the choice.

### Claude: blocking `PermissionRequest` hook

Decision: open (owner). Blocks only the approval bullet of the Claude stage; it ships disabled.

| | A. Fire-and-forget (today) | B. Blocking hook with timeout |
|---|---|---|
| How | Hook posts `blocked:permission` and exits; the owner answers in the TUI or the app sends keys | Hook posts and waits for the app's answer; it returns allow or deny as hook output, or exits with no decision after a timeout so the TUI dialog appears |
| Answer channel | Keystrokes | Structured |
| Risk | Keystrokes into a changed dialog | A stuck hook delays every permission prompt by the timeout |

Recommendation: B, with a short timeout (about 30 seconds, well under Claude's 600-second
default), a per-host switch that is off by default, and an immediate no-decision exit when no
client is connected. Codex's `PermissionRequest` hook supports the same allow and deny shape
and can share the code.

### Who writes hook config into harness settings

Decision: open (owner), already recorded in the Part 4 plan. `gff-hab` prints the config for
each harness. Writing it into settings files that the owner's dotfiles track needs approval.
This spec assumes print-only until then.

## Sources

Opened while writing this spec, 2026-10-31.

- Claude Code hooks reference: https://code.claude.com/docs/en/hooks
- Claude Code CLI flags (`--session-id`, `--resume`): `claude --help`, version 2.1.3xx
- Codex hooks: https://learn.chatgpt.com/docs/hooks (redirected from
  https://developers.openai.com/codex/hooks)
- Codex app-server protocol: https://learn.chatgpt.com/docs/app-server
- Codex CLI flags (`--no-daemon`, `--remote`, `app-server daemon`, `queue`): `codex --help`,
  version 0.166.0
- opencode server: https://opencode.ai/docs/server/
- opencode plugins: https://opencode.ai/docs/plugins/
- opencode CLI: https://opencode.ai/docs/cli/
- Program status protocol (OSC 7501): https://superlogical.com/rex/docs/build/program-status
- herdr detection manifests and updater: https://github.com/herdrdev/herdr
  (`distribution/agent-detection/`, `src/detect/`), licence Apache-2.0
- herdr catalogue index: https://herdr.dev/agent-detection/index.toml
- herdr-core (send refusal while blocked, sequenced events): https://github.com/herdrdev/herdr-core
- Daemon default since Codex 0.157 (secondary source):
  https://codex.danielvaughan.com/2026/09/25/codex-cli-daemon-auto-start-stable-default-v0157/

Internal inputs: the project's planning notes and research reports on harnesses and sessions.
None of their private detail is reproduced here.
