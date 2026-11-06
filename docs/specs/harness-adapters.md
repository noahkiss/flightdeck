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
10. [Usage](#usage)
11. [Input delivery](#input-delivery)
12. [Permission answers](#permission-answers)
13. [Edge cases](#edge-cases)
14. [Fixtures](#fixtures)
15. [Open decisions](#open-decisions)
16. [Sources](#sources)

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
  tested on, as a prefix (`claude` 2.1, `codex` 0.166, `opencode` 2.2). A reported
  version above it, or one with no number in it, gives a version warning. The warning rides on
  the pane's `agent.state` (see [State event model](#state-event-model)); it does not stop the
  adapter.
- **Explain** (`detect.explain`, `gff-hab detect explain <pane|session>`; herdr's
  `agent explain`, `landscape.md` idea 4.7). It answers "why does this pane show this state?"
  in one call and changes nothing. It returns:
  - the pane's agent state with the source that set it, the strongest source this turn (empty
    after a reset), an open request and the owner's interrupt flag;
  - the process part over the PTY's foreground group, read now: every process in the order it
    is tried, the names each offers, and the match;
  - the manifest whose screen rules ran, and what chose it;
  - every rule of that manifest in file order, with the region text it read, whether it
    matched, and the winner.

  Choices:
  - **Manifest.** The process match picks it; with none, the harness in the agent state. With
    neither, there is no screen part.
  - **Fresh manifests, parsed once.** gff-hab parses the vendored set once and keeps the set it
    loaded from the state directory. It loads that set again when a `*.toml` file there
    changes (its name, size or modification time) or the year turns, so a refresh shows at the
    next call. A call with no change parses nothing. gff-hab loads the set once at start.
  - **Foreground group.** The holder reads it from the PTY (`foreground-query`, holder
    protocol). gff-hab then lists the group's processes with no cgo and, on Linux and macOS,
    with no program run: `/proc` on Linux; sysctl on macOS (`kern.proc.pgrp` for the members
    and their names, `kern.procargs2` for each argv); `ps` elsewhere. `ps` prints argv as one
    line, so where `ps` runs an argument with a space reads as two. A process offers each name
    once.
  - **Exact argv on macOS.** A `kern.procargs2` buffer holds argc, the executable path and its
    NUL, NUL padding to the next multiple of 8, argc NUL-ended arguments, then the environment.
    The parse skips NULs only inside that padding. An empty argument is a NUL of its own, so
    skipping every NUL would eat an empty argv[0], shift each later argument by one, and read
    the first environment string (perhaps a secret) as the last argument. The parse reads at
    most argc strings, so if the padding rule is ever wrong, an extra NUL reads as an empty
    argument and the parse stops earlier, never in the environment.
  - **Only a whole copy.** A read must not race an exec. The kernel copies the whole args area
    when the buffer holds it, and replies with its length. When the buffer is too small, it
    copies only the tail (the environment) after the current argc, and fills the buffer. A
    process that runs exec between the size query and the read, into a larger args area, gets
    that tail. So gff-hab calls sysctl itself (x/sys sizes the buffer exactly) with a buffer
    64 bytes larger than the reported size, capped at `kern.argmax` less 4. Only a reply
    shorter than the buffer counts. A full buffer may be a tail: gff-hab asks the size again
    and retries once with 4096 bytes spare, then the process has no Args. No name is
    compared: the kernel keeps the exec path as the caller gave it (a link, a relative path),
    while `p_comm` is the resolved file's name.
  - **Bounded listing.** A listing returns at most 256 processes and stops when the call's
    context ends. Every listing runs on its own goroutine, so a read that hangs (a `/proc` file,
    a sysctl, a `ps` that a kill cannot reach) cannot hold the reply. It never reads `cmdline`
    of a process in state `D`: `cmdline` takes the process's memory lock, which a process stuck
    on a hung mount can hold for good. On macOS it never reads the argv of a zombie, and
    `kern.procargs2` takes the target's memory lock the same way. Each `ps` run is bounded to
    5 s, and the wait for a killed `ps` and its pipes to 500 ms.
  - **One listing per group.** A read stuck in the kernel holds its goroutine and an OS thread
    until it returns, so a client that polls a stuck pane must not start a listing per call.
    gff-hab runs at most one listing per group at a time. A call that finds one running waits
    for it with the others. When every waiter has given up, the listing is abandoned. While an
    abandoned listing has not returned, a call for that group answers at once with
    `processes of group N: an earlier listing has not returned`. While 8 abandoned listings
    have not returned, no new listing starts: `too many earlier listings have not returned`.
  - **Process budget.** The process part (the foreground query and the listing) gets 5 s of the
    call's 15 s. When the budget runs out, the process part's error names the step that timed
    out (`foreground group: timed out after 5s`, or `processes of group N: timed out after
    5s`). The call still answers the rest: the agent state, and the screen part with the
    manifest the agent state picks. If the call itself passes 15 s, the reply is an
    `unavailable` error 500 ms later, never silence. A client waits longer than 15 s:
    `gff-hab detect explain` waits 20 s by default (`-timeout`).
  - **No reused pids.** Where `ps` runs, it runs three times: all groups, then names, then argv
    for the pids found. The second and third runs print the group again, and a row of another
    group is dropped. Such a row is a pid that exited and was reused between runs. `/proc` and
    sysctl read a member's name and group first, then its argv, and accept the same race: a pid
    reused between the two reads shows the new program's argv. The window is one read wide, a
    reuse needs the pid space to wrap in it, and the result is only shown, never acted on.
  - **Screen text.** The rules read the active screen only: its visible rows, read from the
    parser's cells, never scrollback. A stale dialog in scrollback must not match as if it
    were on screen. The read takes the pane lock for one screen of cells, not the whole
    scrollback. The blank rows below the last printed row are dropped, so a fresh prompt at the
    top of a cleared screen stays inside `bottom_lines(N)` regions.
  - **Region text.** Each rule carries its region's last 2048 bytes, cut at a line where it can.
    The CLI prints the winner's region; `-all` prints every rule with its region.
  - **OSC regions.** gff-hab does not track the OSC title or OSC 9;4 progress yet, so
    `osc_title` and `osc_progress` read empty. Rules on them never match.
  - **Not applied.** This build runs the screen rules only for explain. It never writes a screen
    state into `agent.state`. When the screen disagrees, the verdict line says so, and says that
    the screen is shown, not applied. The fallback that applies it is later work, under the
    precedence rules below.
  - **Names.** The argument is a pane id, a session id or a session name. A session must have a
    live pane.
  - **Revision 1, additive.** A new method and the capability `detect-explain`, sent with the
    holder link on.

**Claude session id at spawn.** When `gff-hab` starts Claude, it may pass
`--session-id <uuid>`. The adapter then knows the transcript before the first hook fires. Codex
and opencode have no such flag; their id arrives with the first hook or server event.

## Environment contract

`gff-hab-holder` sets these variables when it spawns a pane. Each harness passes them on to
its hooks and tool commands, so anything inside the pane can find `gff-hab`.

| Variable | Set by | Value | Who reads it |
|---|---|---|---|
| `GFF_HAB` | gff-hab, at spawn | The hook endpoint URL for this pane, with a per-pane token (see below) | `gff-hab hook`, `gff-hab send` and `gff-hab wait` (`mesh.md` 6.5), the agent kit, any agent that wants to name itself |
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
- **Hook config.** `gff-hab` prints the hook config for each harness. A later opt-in install
  command writes it after showing the diff (owner decision, see [Open decisions](#open-decisions)).

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
  complete temp file hard-linked into place, so racing starts agree on one secret. On a
  filesystem without hard links it creates the final name with O_EXCL, then writes and syncs it:
  the first start still wins and no file is replaced, but a racing start may read the file
  before it is complete and refuse to start. It refuses to
  start when the file is a symlink, not a regular file, owned by another user, open to group or
  others, or not 32 bytes; it never replaces such a file. No registry is stored: the same secret
  gives the same token after a gff-hab restart, so a pane that outlives gff-hab keeps posting. A
  respawned pane has a new uuid, so its token changes: the link refuses a spawn whose uuid names
  a pane it follows, has lost or has dropped (a retry of a spawn on the same holder excepted), so
  a leftover process of an old pane can never post as a new one.
- **Key rotation.** A new `hook.key` (deleted, or a reset state directory) detaches every live
  pane: their tokens were made under the old secret, so their hooks get 404 and they show no
  agent state until respawned. gff-hab logs one warning line with the live pane count when it
  makes a key while the link already follows panes.
- **Checks.** gff-hab accepts a token only when its hash matches (a constant-time compare) and
  the holder link follows the pane: listed, not lost with its holder, and not released. A pane
  whose spawn is in flight counts as followed: the holder starts the program before it answers,
  and the link follows the pane only once it has that answer, so a program that posts at once
  is not refused. session.create files the session before the spawn, so that post also finds
  its session (`hab-logic-api.md` 3.9). A forged
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
  sets how the harness runs gff-hab. Claude Code and Codex commands exit quietly when
  that executable is missing. Claude's `PermissionRequest` entry runs
  `gff-hab hook -wait claude PermissionRequest` with a 130 second timeout; see the blocking
  permission hook under [Open decisions](#claude-blocking-permissionrequest-hook).
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
| `blocked` | Waiting for the owner. Carries `kind`: `permission`, `question`, `auth` or `trust` (a folder trust prompt, read from the screen; `app.md` 7.10). |
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
| `trust` | object, optional | With `blocked:trust`: `folder`, the folder the session started in. `trust.answer` answers it (`app.md` 7.10). |
| `source` | enum | `server`, `hook`, `osc`, `screen`, `spawn`. |
| `turn` | string, optional | Native turn or prompt id where the harness gives one (Claude `prompt_id`, Codex `turn_id`). |
| `at` | time | When `gff-hab` accepted the event. |
| `revision` | integer | Per pane, raised on every accepted change. |
| `last` | enum, optional | With `exited`: the last `done` or `error` before the exit. |
| `version` | string, optional | The harness version, once reported (envelope `version`, or a `version` or `cli_version` field in the payload). |
| `warning` | object, optional | Set while `version` is above the adapter's tested range, or unreadable (see Detection). |
| `hooks` | enum, optional | `missing`: the harness finished a turn and none of its hooks reached `gff-hab` (see [Codex](#codex), built choices). Absent once a hook arrives. |
| `error` | string, optional | With `error`: the harness's own kind of failure, where it gives one. Claude Code: the `error` of `StopFailure` (`rate_limit`, `authentication_failed`, `server_error` and the like). Codex and opencode give none yet. A round tries a run that failed with `authentication_failed`, `cloud_credential_error`, `server_error` or `unknown` once more after a wake (`rounds.md` 8.3; `harness.NetworkFailure`). |

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
- Built: while the bound session has a permission request open, a session start from another
  session is dropped as a nested agent, so it cannot close the request.
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
- Built exception, for an interrupt: an `osc` `idle` ends a `working` or `blocked` state that
  a stronger source set, once 1 second has passed since that source last spoke. Claude Code
  fires no hook when the owner interrupts a turn (its `Stop` hook does not run then), so its
  OSC 7501 `idle` is the only signal. Inside the second, the `idle` is taken as a stale report
  from before the turn began and is dropped.
- The exception never ends a `blocked:permission` whose request a hook or server opened. Only
  a hook, a server event, a new session or the program's exit closes that request; no OSC 7501
  report does, `clear` included. Any program in the pane can write OSC 7501, and Enter from a
  `session.send` on a live dialog would approve it. Cost: when the owner leaves such a dialog
  with Esc in the terminal (no hook fires), the pane shows `blocked` until the session's next
  hook.
- Built exception, for a request Flightdeck answered: once `approval.answer` sent its answer
  (keys written, or the waiting hook took the decision), the next OSC `idle` or shell prompt
  mark closes the request. Claude fires no hook after a deny by Esc; it returns to its prompt
  and reports `idle`. Untrusted bytes still never close a request nobody answered. An answer
  that did not take counts as no answer here: the hook went away before it took the decision,
  the keys could not be written, or the read-back gave up with the request still open.
- Built reset, for the owner's interrupt: Esc or Ctrl-C typed through `pane.input` into a
  `working` Claude pane, as one write and with no request open, shows `idle` with source
  `server` (Paseo does the same). It resets precedence, so the harness's next report of any
  source wins again: a key that interrupted nothing is corrected at once. Plain bytes and the
  kitty keyboard forms count; a paste never does. The reset is display only: `session.send`
  stays refused until the harness itself reports again (Claude sends OSC `idle` once it
  stops). The key may have gone to a shell whose Claude was killed, and a send would then
  type into the shell.
- Built reset, for a shell prompt (Rex spec): an OSC 133 `A` mark in live output after the
  pane's last OSC 7501 report, with no command started after it, means the harness no longer
  runs in the foreground. A `working` or `blocked` state that terminal output set (`osc` or
  `screen`) then shows `exited`, as an OSC `clear` does, so `session.send` never types into
  the shell. The binding stays, and the session's next hook takes the pane back. A state a hook
  set stays, and so does a request nobody answered. Claude Code 2.1.316 writes no OSC 133
  marks of its own, so the mark comes from the shell around it.
- Built reset, for a session's end: `SessionEnd`, or the exit of the pane's program, resets
  precedence. The next program in the pane may report over OSC 7501 alone (an untrusted Codex
  after a Claude session); without the reset, the ended session's hook turn would drop every
  report it sends.

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
| Conversation | Tail the session JSONL named by `transcript_path` in every hook payload (under the Claude config dir, `projects/<encoded cwd>/<session id>.jsonl`). Read records of type `user` and `assistant` for the chat view, linked by `uuid` and `parentUuid`. Use `attachment` records for the context ledger. Skip the other known record types; an unknown type becomes a placeholder message. |
| Input | PTY: bracketed paste of the text, then Enter. |
| Permission answer | Default: the TUI dialog, answered by the owner in the terminal, or by PTY keys from the app after the checks in [Permission answers](#permission-answers). Opt-in: a blocking `PermissionRequest` hook, a per-host setting, off by default (see [Open decisions](#open-decisions)). |
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
- `PermissionRequest` payloads carry no `tool_use_id` (hooks reference). gff-hab names such a
  request itself (see below); `PostToolUse` then resolves it by leaving `blocked`.

Built choices (`internal/progstatus`, `internal/termq`, `internal/link`, `internal/harness`):

- **The `?` query.** `ESC ] 7501 ; ? ST` is a terminal query in `internal/termq`. The holder
  link answers it once, in live bytes, with
  `ESC ] 7501 ; ?:states=idle,working,done,blocked,error:kinds=permission,question,auth`
  and the query's own terminator (BEL or `ESC \`). Data streams leave the query out of the bytes
  clients get, like every other query (rule 7).
- **Reports.** `internal/progstatus` follows each pane's output for `ESC ] 7501 ; <pairs> ST`
  reports, cut at any chunk boundary. It applies the protocol's grammar and limits: a malformed
  pair is skipped, a report with no known `state`, bad base64, a control character in decoded
  text, invalid UTF-8 in decoded text, a bad `id` or more than 4096 bytes is discarded whole.
  Reports stay in the bytes clients get; a terminal ignores an OSC it does not know. The link
  hands each live one to its watchers as a `PaneReport` notice. It also keeps the pane's newest
  root report and saves it in the pane's checkpoint, since a restart replays only the bytes
  after the checkpoint. After a gff-hab restart it hands on that report, marked not live, when
  the replay ends: the newest one in the replayed bytes, else the checkpoint's. gff-hab applies
  it with source `screen` (see Edge cases). A shell prompt after the report (an OSC 133 `A`
  mark, no command started after it) drops it, and in live output it reaches the watchers as
  a `PanePrompt` notice.
- **Mapping.** A report applies with source `osc`. Only the root record counts: a report with
  an `id` never moves the pane. `app` must name a detection manifest (or alias), so another
  program that reports status, such as a package manager, does not become an agent; a report
  with no `app` counts for the harness the pane already has. `clear` from the pane's own
  harness means it exited: the pane shows `exited`, so a lost `SessionEnd` never leaves a shell
  that looks like an idle agent. Like every report, it closes no permission request a hook
  opened, and the session keeps the pane: its next hook takes it back.
  `kind` rides along with `blocked`; `title`, `msg` and `progress` are parsed but not yet
  shown.
- **One state.** Hooks and reports go through the same store, under the precedence and ordering
  rules above, so the hook's `session_id` stays the resume id and its request stays the open
  one. A report's `t` is gff-hab's clock when it parsed the bytes.
- **Request ids.** A permission request with no id from the payload gets one from gff-hab:
  a random prefix per gff-hab run plus a counter, so an id from an earlier run never names a
  new request.
- **Exit.** When the holder reports the pane's program reaped, the pane's agent state becomes
  `exited`: `last` keeps a `done` or `error`, and the session no longer holds the pane.

Built choices for the transcript reader (`internal/transcript`, `cmd/gff-hab/chat.go`):

- **Finding the file.** `chat.page` names a gff-hab session. gff-hab takes the session's pane,
  then the pane's agent state: harness `claude` and the native `session` the hooks reported (the
  resume id). The file is `projects/*/<session id>.jsonl` under `$CLAUDE_CONFIG_DIR`, else
  `~/.claude`; when several project folders hold one, the most recently written wins. A
  session id that could name another path is refused. A file not written yet reads as an empty
  page with an empty `cursor`; gff-hab then looks for the file once a second, and when it
  appears sends each of its messages as a `chat.message` event, with no new page needed. When
  the agent leaves the pane, the last conversation stays readable while its reader is open.
- **Tailing.** The reader opens the file, reads only the bytes appended since its last read,
  and closes it. It hands out complete lines only: a last line with no newline (the writer is
  mid-append) waits for its newline, so it never makes a broken message. A line over 32 MiB is
  skipped as a placeholder. A read of a file that has not grown costs one open and one stat.
  A `chat.page` reads on its own goroutine, never on the connection's read loop, and stops
  between 1 MiB chunks when its call times out or its connection ends; the next read goes on
  from there.
- **Replacement.** A file that shrinks or is replaced (another inode) is read again from the
  start. So is a file truncated and written again past the old size: each time the file grows,
  the reader checks that its first 4 KiB are unchanged and that the byte before the next line
  is still a newline. A rewrite that keeps both is not seen. Each replacement is logged with a
  count. A subscriber gets one `chat.message` event of kind `event`, event `reset`, id
  `reset-<count>`, and none for the messages read again: its messages may be gone or moved, so
  it takes the tail again. A `reset` message is never in a page. A client that does not know
  the word shows it as a plain event line that says the transcript was replaced.
- **Pinned versions.** The parser follows Claude Code 2.1.296 to 2.1.319. A record whose
  `version` is outside that range still reads; the reader logs the drift once per version.
  Drift follows the newest record that names a version: a later record inside the range clears
  it, and so does a replacement.
- **Records to messages.**

  | Record | Chat message |
  |---|---|
  | `user`, text (string or text blocks; images as `[image]`) | kind `user` |
  | `user`, text that is one whole Flightdeck agent-message envelope (`mesh.md` 6.5), bare or in one `<pasted_content id="..">` wrapper with nothing around it | kind `user`, `sender` the envelope's sender, text only what is between the begin and end lines. A paste with the user's own words around it, any other paste, and an envelope whose begin and end ids differ stay as written |
  | `user`, `<command-name>` or `<local-command-stdout>` text | kind `event`, event `command` |
  | `user` with no `message` | kind `event`, event `unknown`, plus one warning |
  | `user`, `[Request interrupted by user…` | kind `event`, event `interrupt` |
  | `user` with `isMeta` or `isCompactSummary`, `<local-command-caveat>` | left out: context for the model |
  | `user`, `tool_result` block | changes its call in place: status `done`, `error` (`is_error`) or `denied` (`permissionDecision.decision` is `reject`); the first 4 KiB of the output |
  | `user`, `tool_result` whose `permissionDecision.source` starts with `user` | also kind `event`, event `permission`: the owner answered a permission prompt ("Allowed Edit (once)", "Denied Bash") |
  | `assistant`, `text` block | kind `agent` |
  | `assistant`, `tool_use` block | kind `tool`, status `running`; target from `file_path`, `path`, `command`, `pattern`, `url`, `query` or `description`; input when at most 64 KiB; a larger input is left out with no flag (the target still shows) |
  | `assistant`, `thinking` and `redacted_thinking` | left out |
  | `system` `compact_boundary` | kind `event`, event `compaction` |
  | `system` `informational`, `api_error` | kind `event`, event `notice` |
  | `system` `local_command` | kind `event`, event `command`; left out when it printed nothing or is the caveat |
  | `system` `informational`, `api_error`, `local_command` whose content is not text; `informational` or `api_error` with no text | kind `event`, event `unknown`, plus one warning per subtype |
  | `system` `turn_duration`, `stop_hook_summary`, `scheduled_task_fire` | left out |
  | `attachment`, titles, modes, request and file-history records, `session-owner` (account and organization ids, 2.1.319) (the list in `claude.go`) | left out |
  | records with `isSidechain` | left out (subagent transcripts are a later feature) |
  | any other type, system subtype or content block; a line that is not JSON | kind `event`, event `unknown`, text naming it, plus one warning per kind in the log |

- **Ids.** A message id is the record's `uuid`; a later content block of the same record adds
  `-<index>`. A permission line is `<uuid>-permission-<tool use id>`. A record with no uuid uses
  `line-<byte offset>`. Ids are stable across gff-hab restarts.
- **Cursor.** `chat.page` returns `cursor`: a hash of the file's first line and a byte offset.
  `after: <cursor>` returns the messages added or changed since, oldest first, a new cursor, and
  `more` when further changes wait. A message made before the cursor and changed after it (a
  call that got its result) comes again with its new content; a client replaces by id and
  appends new ids. A cursor from other file contents, or past the end, answers `conflict`: the
  client takes the tail again. Byte offsets do not change when gff-hab restarts, so a cursor
  survives a restart, and a reader closed for idleness. An empty `cursor` means the file holds
  no complete line yet: there is nothing to resume from, and the client takes a page again.
- **Events.** gff-hab reads each open transcript once a second and sends one `chat.message`
  event per added or changed message. The first read of a file that existed at the first page
  sends none: the client takes a page. A `chat.message` carries no revision on the wire.
- **Closing readers.** A `chat.page` opens a reader; it holds the whole conversation in memory.
  The poll closes it when its session record is removed, or when no `chat.page` came for 10
  minutes and either no subscription gets the session's events, or the session's agent is gone
  and the file has not grown for 10 minutes. A reader waiting for a file not written yet
  follows the same rule. The next `chat.page` opens a new one.

## Codex

| Channel | Behaviour |
|---|---|
| Detection | Process name `codex` (manifest id `codex`). The manifest also marks Codex's own startup dialogs (trust this folder, update available) as `blocked`. |
| State | Two sources, as for Claude Code. (1) Native OSC 7501 reports: Codex 0.166 sends `state=<state>:app=codex`, with `kind=permission` on `blocked`, and `state=clear` on exit. (2) Hooks: `SessionStart` binds; `UserPromptSubmit`, `PreToolUse`, `PostToolUse` → `working`; `PermissionRequest` → `blocked:permission`; `Stop` → `done`; `Interrupt` → `idle`; `SessionEnd` unbinds. With the app-server path (option B or C below), server notifications replace hooks as the state source. |
| Conversation | With `--no-daemon` (decided), there is no app-server: tail the rollout JSONL under the Codex home, `sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl`. Each line has `timestamp`, `type` and `payload`. The chat view reads `session_meta` (for the version), `compacted`, and the `event_msg` lines, chiefly `item_completed`; it skips `response_item`. With the app-server (the later option C spike): `thread/resume` with `excludeTurns`, then `item/*` notifications. |
| Input | PTY: bracketed paste, then Enter. With the app-server: `turn/start` when idle, `turn/steer` with `expectedTurnId` when working. |
| Permission answer | Without the app-server: TUI dialog or PTY keys (`1` allows, Esc denies), after the checks below; or the blocking `PermissionRequest` hook, which Codex shares with Claude Code. With the app-server: answer the server request (`item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval`) with `accept`, `acceptForSession`, `decline` or `cancel`; `serverRequest/resolved` confirms. |
| Resume id | `session_id` in hook payloads, `thread.sessionId` on the app-server; `codex resume <id>`. |

Notes:

- **Hook trust.** Codex runs a non-managed hook only after the owner trusts it, keyed by the
  hook's hash. A new or changed hook needs review: Codex 0.166 says "Hooks need review" at
  start, and "continue without trusting" runs no hook for the whole session. Health checks must
  prove delivery, not config presence (built: the `hooks` field, below).
- **`SessionStart` comes late.** Codex 0.166 fires `SessionStart` with the first prompt, not
  when the TUI opens. Before it, OSC 7501 reports `working`, then `idle`, while Codex starts.
- **No request id.** `PermissionRequest` payloads carry no `tool_use_id` (hooks reference; seen
  live). gff-hab names the request itself, as for Claude Code.
- **No version in hooks.** Hook payloads carry no version field, so `version` stays empty for a
  Codex pane. The rollout's `session_meta` has `cli_version`; the transcript reader checks it.
- **Short end hooks.** `SessionEnd` and `Interrupt` hooks default to one second and cap at three.
  The hook command must post within that budget; `gff-hab` also infers end from process exit.
- `AGENT_SESSION_ID` cannot reach Codex tool commands (see the environment contract).
- The rollout format is undocumented and changes between versions.

Built choices (`cmd/gff-hab/actions.go`, `internal/harness`, `internal/hook`):

- **Start.** `session.create` with `harness: codex` runs `argv`, else `codex`, with
  `--no-daemon` inserted after the program name when `argv` lacks it, and adds
  `AGENT_KIND=codex` to the pane's environment. `harness: claude` starts `claude` (argv when
  given) with `AGENT_KIND=claude`, and `harness: opencode` its wrapper (task-ph00sdci). Any other
  `harness` answers `unavailable`.
- **State.** Codex's OSC 7501 reports and hooks go through the same store and rules as Claude
  Code's (see Claude Code, built choices). gff-hab answers the `?` query for every pane; whether
  Codex waits for that answer before it reports was not checked.
- **Hook health.** A pane's `hooks` becomes `missing` when an OSC 7501 report from its harness
  moves it from `working` to `done` or `error`, and no hook from that harness reached this
  gff-hab run for the pane. gff-hab logs one error line with the fix: install the hook config
  (`gff-hab hook -print <harness>`), and for Codex, trust it with `/hooks`. Any hook clears the
  field. `idle` does not count as a turn end, because Codex reports `idle` at start, before its
  first hook. The field is for display and health only: it never gates `session.send` or an
  answer. Which hooks arrived counts from the session's start: `SessionEnd` or the exit of the
  pane's program forgets them, so a hookless program started next in the pane is marked. A
  gff-hab restart forgets them too, so the field can show again after a restart until the next
  hook.
- **Input.** `session.send` writes the same bytes as for Claude Code: bracketed paste, then CR
  as its own write, under the same send rules (see [Input delivery](#input-delivery)).
- **Permission keys.** Codex's dialog lists "1. Yes, proceed" first: `1` allows. Esc chooses
  "No, and tell Codex what to do differently", which stops the turn as Claude's Esc does. Allow
  by `1` was checked live on 0.166; deny by Esc was not.
- **Blocking hook.** The printed Codex config runs `gff-hab hook -wait codex PermissionRequest`
  with a 130 second timeout. Codex's hook output has Claude's shape, so the wait, the switch
  and the output are the ones in [Open decisions](#open-decisions), Claude: blocking
  `PermissionRequest` hook. One switch covers both harnesses; it is off by default.
  Known limit: the output shape comes from the Codex hooks documentation. Nobody has run it
  live with the switch on, nor checked that Codex accepts a 130 second hook timeout. If Codex
  reads the output differently, the hook takes the app's answer, Codex shows its own dialog
  anyway, and the call fails after the 3 second read-back. Check it live before the switch
  goes on for Codex.

Built choices for the rollout reader (`internal/transcript/codex.go`, `cmd/gff-hab/chat.go`):

- **Finding the file.** As for Claude Code, `chat.page` takes the session's pane, then the
  pane's agent state: harness `codex` and the native `session`. The file is
  `sessions/*/*/*/rollout-*-<session id>.jsonl` under `$CODEX_HOME`, else `~/.codex`; when
  several match, the most recently written wins. A session id that could name another path is
  refused. Known limit: gff-hab reads its own `$CODEX_HOME`, not the pane's, so a pane started
  with another `CODEX_HOME` finds no file. Tailing, replacement, cursors, events and closing
  follow the Claude Code reader.
- **Items, not response items.** The chat view reads `item_completed` events, which match the
  app-server's thread items, and skips `response_item` lines (what the model saw, with context
  and tool plumbing). So a call shows only once it has completed; there is no `running` state
  for Codex tools.
- **Pinned versions.** The parser follows Codex 0.159.0 to 0.166.0, the range that writes
  `item_completed`. A `session_meta` whose `cli_version` is outside it still reads; the reader
  logs the drift once per version, as for Claude Code.
- **Lines and events to messages.**

  | Rollout | Chat message |
  |---|---|
  | `session_meta` | none; `cli_version` sets drift |
  | `compacted` | kind `event`, event `compaction` |
  | `turn_context`, `response_item`, `world_state`, `token_usage_record`, `inter_agent_communication_metadata` | left out |
  | `event_msg` `turn_aborted` | kind `event`, event `interrupt`; with a reason other than `interrupted`, event `notice`, "Turn aborted: <reason>" |
  | `event_msg` `error`, `warning`, `stream_error` | kind `event`, event `notice`, the message |
  | `event_msg` bookkeeping and second copies (`token_count`, `task_started`, `agent_message`, `exec_command_end` and the rest of the list in `codex.go`) | left out |
  | `item_completed` `UserMessage`, `AgentMessage` | kind `user` or `agent`; text blocks joined, images as `[image]`; an empty one left out |
  | `item_completed` `CommandExecution` | kind `tool`, name `exec`; target the command's first line (`sh -lc <script>` shows the script); status `error` on a non-zero exit; the first 4 KiB of the output |
  | `item_completed` `FileChange` | kind `tool`, name `apply_patch`; target the first path, "and N more" |
  | `item_completed` `McpToolCall` | kind `tool`, name `mcp__<server>__<tool>`; text and `[image]` output; status `error` on `isError` |
  | `item_completed` `Extension` | kind `tool`, name `web_search`, `image_gen` or the kind; target the query or saved path; image bytes never read |
  | `item_completed` `ImageView` | kind `tool`, name `view_image`, target the path |
  | `item_completed` `SubAgentActivity` `started`, `completed` | kind `event`, event `notice`; `interacted` left out |
  | `item_completed` `CollabAgentToolCall` | kind `tool`, name `agent_<tool>` |
  | `item_completed` `Reasoning`, `ContextCompaction` | left out (the `compacted` line marks a compaction) |
  | Item status `declined`, `failed`, `inProgress` | tool status `denied`, `error`, `running` |
  | any other line type or item type; a line that is not JSON; an item with no type | kind `event`, event `unknown`, plus one warning per kind |
  | any other `event_msg` type | left out, plus one log line per type |

- **Unknown events are logged, not shown.** An unknown `event_msg` type gets a log line, not a
  placeholder. Codex has many UI events, and a placeholder for each new one would bury the
  conversation. An unknown line or item type still gets a placeholder.
- **Ids.** A message id is the item's `id`; a line with none uses `line-<byte offset>`.

## opencode

The table and notes below follow the opencode 1.x documentation the study read. opencode 2.x,
which the adapter is built on, differs; see the 2.x notes and built choices after them.

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
- The OpenAPI document lives at `/doc` on that server (opencode 1.x; 2.x serves it at `/openapi.json`, below). The adapter records the document's
  version and fails visibly on an unknown one.
- `opencode serve` or `opencode attach` would start or attach a second front end. The adapter
  never starts a second server for a session a TUI already owns.

## Usage

Each adapter reports its account's rate-limit windows: name, used percentage, reset time, when it
was read, and the window's length and start when its data defines them (`app.md` 14.1). gff-hab keeps the latest reading per account and serves it as
`usage.list` and `usage.updated` (`hab-logic-api.md` 3.9). The adapter reads only what the
harness already hands over. It never reads a credential file and never calls a usage endpoint.

| Harness | Source | Version caveat |
|---|---|---|
| Claude Code | The status-line input: `rate_limits.five_hour`, `seven_day`, `spend_limit` and any other key, each with `used_percentage` and `resets_at` in Unix seconds. Claude Code documents this input for `statusLine` commands. Hooks carry no rate limits. | Recent versions only; tested against 2.1.316. An older version sends no `rate_limits`. Claude Code sends the field only for a subscription account (or a gateway spend limit), only after the first answer, and drops a window once its reset time passes. |
| Codex | The rollout's `event_msg` `token_count` events: `rate_limits` with `limit_id`, `limit_name`, `primary` and `secondary` (`used_percent`, `window_minutes`, `resets_at` in Unix seconds) and `plan_type`. Hook payloads carry no rate limits. | The rollout is not a documented format. Tested against 0.159.0 to 0.166.0. Older versions have no `limit_id` and give `resets_in_seconds`; both still read. |
| opencode | None. opencode reports no account rate limits. | The adapter adds no account: no reading, never zero. |

Not built, by rule: the Claude.ai usage endpoint (the `/usage` screen) and Codex's app-server
`account/rateLimits/read` would give the same windows outside a turn. Each needs the user's token
or a second server process, so neither is used.

Built choices (`internal/usage`, `cmd/gff-hab/usage.go`, `cmd/gff-hab/statusline.go`):

- **Claude Code: a status-line wrapper.** Claude Code runs one `statusLine` command. That command
  is `gff-hab statusline claude [-timeout d] [-- <command>...]`. It posts the input to the hook
  endpoint as event `StatusLine`, the same way a hook does. Beside the post it runs the owner's own
  status-line command, if one follows `--`, with the same input. One word after `--` is a shell
  string, run with `/bin/sh -c` as Claude Code runs its own; several words are the argv, run as
  is. That command's output, errors and exit code are the status line's (127 when it cannot
  start). With none, the status line prints nothing. Outside a gff-hab pane it posts nothing.
  `gff-hab statusline -print claude [-wrap <command>]` prints the setting; writing it into
  settings is the owner's step, as for hooks. If gff-hab is missing, the printed command runs
  the wrapped command directly, or exits quietly when there is none. A `StatusLine` envelope is usage only: it never
  changes the agent state.
- **The status line never waits on gff-hab.** Claude Code refreshes the status line many times a
  turn and waits for each run. The post is capped at 200 ms, dial and answer, from the start of
  the run: a hung gff-hab costs the status line at most that, and the next refresh sends the same
  snapshot again.
- **The wrapped command leaves nothing behind.** It runs in its own process group. SIGTERM,
  SIGINT and SIGHUP to `gff-hab statusline` go to the whole group, and the group is killed one
  second later if it still runs. A command that runs past `-timeout` (default 10 s) is killed with
  its group.
- **Codex: the end of the rollout, after a hook.** Each Codex hook names its session. Once a
  session's hooks pause for 2 s, and at most 10 s after the first hook of a burst, gff-hab reads
  the last 256 KiB of that session's rollout (found as for the chat view) and skips the first, cut
  line. A burst of hooks is one read. An unchanged file is not read again. A read that ends
  mid-line does not count as read: the last `token_count` may still be written, so gff-hab reads
  again 2 s later, up to 3 times, with no hook. The latest `token_count` of each `limit_id` wins,
  by its timestamp. `read_at` is the timestamp of the line, not the time of the read, so an old
  rollout never looks fresh. At least one Codex hook must arrive after a gff-hab start before
  Codex has a reading.
- **Window names.** Claude Code: `5h`, `7d`, and `spend` plus the period (`spend monthly`), when
  the period is short lower-case words. Any other `rate_limits` key whose value parses is a window
  too, named by its words with a leading length shortened: `seven_day_opus` is `7d opus`. A key
  that is not lower-case words joined by `_` (40 bytes at most) is drift.
  Codex: the length from `window_minutes` (`5h`, `7d`). A Codex limit id other than `codex` (a
  model with its own limit) puts its `limit_name`, else its id, first: `model-y 7d`. A limit id
  with no windows adds none.
- **Window length and start.** `length_s` comes only from what the harness's data states.
  Codex: `window_minutes` (up to ten years). Claude Code: the key, `five_hour` 5 hours,
  `seven_day` 7 days, and a key that starts with a number and a unit (`seven_day_opus` 7 days,
  `ten_minutes`); minute, hour, day and week units. A spend limit's `period` of `daily`, `weekly`
  or `monthly` gives the calendar period that ends at `resets_at` (UTC): a monthly limit that
  resets on 1 March started on 1 February and is 28 days long. A monthly reset on day 29 to 31
  starts on the same day of the month before, clamped to that month's last day: a reset on
  31 March started on 28 February (29 in a leap year). Any other key or period gives no
  length; a name is never parsed for one. `starts_at` is `resets_at` less the length, and absent
  with no reset time. A client computes pace from these two, so pace works for a model window
  or a spend window as for `5h`.
- **Numbers.** `used_pct` is whole percent, rounded down: 99.6 is not yet full. A value above 100
  stays (a spend limit can pass its limit); the client caps it. Reset times are RFC 3339 in UTC.
- **Account key.** No source gives an account id. The key is the harness plus 12 hex digits of a
  hash of the hab's host id, the harness and the harness's data directory: the directory above
  `projects/` in Claude Code's `transcript_path`, and gff-hab's `$CODEX_HOME` for Codex. Two
  accounts on one hab with separate data directories get two keys. The key names none of them.
- **Label.** Codex `plan_type` with a capital first letter (`Plus`, `Pro`). Claude Code's
  status-line input has no plan, so its label is empty.
- **Merging.** Windows merge by name. A window read earlier never replaces one read later, so
  two sessions on one account, or an old rollout, cannot undo a newer reading. A window that a
  reading lacks stays with its reset time; the client shows it as reset with no reading since
  (`app.md` 14.5). A reading with no windows creates no account. The store keeps at most 64
  accounts of 16 windows.
- **Events.** `usage.updated` goes out when a name, percentage, reset time or label changes, and
  at most every 5 minutes for an unchanged account, so a client sees the reading's age move.
  Every subscription gets it, scoped or not: an account belongs to no session.
- **Drift.** A window whose numbers do not parse (a percentage that is not a number, a reset time
  that is not Unix seconds, a window key that is not lower-case words, a Codex window with no
  length) is left out and logged once, up to 256 distinct lines per gff-hab run. A harness version outside the tested range is logged once. A wrong number is never
  shown; the last good reading ages instead. A Claude Code `rate_limits` that is not an object
  is drift too, on the account the input names.
- **Degraded.** Some drift sets the account's `degraded` field: the first such drift line, plus
  "(and N more)". Drift sets it when:
  - the reading has no windows: all its drift counts;
  - a window the adapter knows fails to parse: Claude Code's `five_hour`, `seven_day` or
    `spend_limit`, or a Codex `primary` or `secondary` window;
  - for Codex, a `token_count` line at or after the newest line that gave a window is bad. An
    older bad line still in the 256 KiB tail is logged only, so it cannot hold the mark.

  Drift in a key the adapter does not know (a future `overage_enabled`, a misnamed key) is logged
  only while the known windows read: their numbers are right, so the card keeps them. A drifted
  reading with no windows still creates the account, so the client shows "usage unavailable"
  (`app.md` 14.5), not "No limits reported". The exception is a Claude Code input with no
  `transcript_path`: it names no data directory and so no real account, and its drift is logged
  only. A reading with windows and no such drift clears the mark. A reading with neither (a
  rollout with no `token_count` yet, a status line before the first answer) leaves it: it says
  nothing about the format.
- **The mark follows report time, as windows do.** The store keeps the time of the report that
  last set or cleared the mark: the status line's time, or the newest `token_count` timestamp.
  An older report neither sets nor clears it, so a re-read of an old rollout cannot move it. A
  report with no time (every `token_count` line lacked one) counts as new. Two Codex sessions of
  one account, one drifted and one clean, leave the mark as the newer report says. A change of
  the mark is sent as `usage.updated`.
- **Why per account.** The mark is per account, not per window: 14.5 puts "usage unavailable" on
  the card, some drift (a key that is not a window name, `rate_limits` of another shape) names no
  window, and each account has one source (the status line of one data directory, or the
  rollouts of one `$CODEX_HOME`), so per account is per source. A harness version outside the
  tested range alone does not set it. A status-line input that is not JSON names no account, so
  it is logged only.

opencode 2 changed the ground under the notes above (seen live on 2.2.5, 2026-11-01):

- By default the TUI attaches to a shared per-user background service on a fixed port, like
  the Codex daemon: the agent loop and its tools run outside the pane, with the service's
  environment. `--standalone` starts a private child server with a password it makes up itself.
  `--server <url>` attaches the TUI to a given server. The TUI and `opencode serve` both read
  the password from `OPENCODE_PASSWORD` (HTTP basic auth, user `opencode`). There is no
  `--hostname`/`--port` on the TUI any more.
- The API lives under `/api/`. The OpenAPI document is at `/openapi.json`; `/doc` now serves the
  web app. The document's own version says `0.0.1`; `/api/info` gives the real one. The document
  does not type the event stream's payloads.
- There are no `/tui/*` control endpoints. A message sent with `POST /api/session/:id/prompt`
  shows in the TUI as the owner's own turn (checked live), so the prompt API is the input path.
- A server's event stream carries only its own process's events, even when other opencode
  processes share its database.

Built choices (`internal/opencode`, `internal/harness/opencode.go`,
`internal/transcript/opencode.go`, `cmd/gff-hab/opencode.go`, `cmd/gff-hab/opencodepane.go`):

- **Start.** `session.create` with `harness: opencode` runs `argv`, else `opencode`, through
  `gff-hab opencode-pane -report <state>/opencode/<pane uuid>.server.json -- <argv>`. The
  wrapper starts a private `opencode serve --stdio --hostname 127.0.0.1 --port 0` in its own
  process group, with a pipe as its stdin. The server binds a port the system picks and prints
  `{"url": ...}` on stdout; the wrapper reads that line (30 s at most), so no other program can
  take the port between a pick and a bind. It then asks `/api/info` at that URL and goes on
  only when the `pid` there is its child, or a process in the child's group (a launcher may run
  the real server as its child); a server that reports no pid is taken on the URL alone. It
  writes the server file (below), then runs the TUI as `opencode --server <url> <argv[1:]>` in
  the foreground. Both stay under the holder, so tools see the pane's environment. When the TUI
  exits, the wrapper closes the pipe, sends SIGTERM to the server's group, and SIGKILL after
  3 s; it removes the server file and exits with the TUI's status. When the wrapper dies, the
  closed pipe ends the server (live, 2.2.5 exits about 1.5 s after stdin closes). `argv` that
  names `--server` or `--server=...`, or has an empty `argv[0]`, is refused.
- **Password.** HMAC-SHA256(hook secret, `"gff-hab opencode password v1\0"` + pane uuid) in
  hex, so it needs no storage and survives a gff-hab restart. It reaches the wrapper as
  `GFF_OPENCODE_PASSWORD`; the wrapper removes that variable from its own environment for its
  children and gives opencode `OPENCODE_PASSWORD`. It never appears in argv, logs or the state
  files. It is not secret from the same user: the wrapper's initial environment block
  (`/proc/<pid>/environ` on Linux) still holds `GFF_OPENCODE_PASSWORD`, since Go does not
  rewrite it, and the TUI and server hold `OPENCODE_PASSWORD`. Whether opencode strips it from
  its tools' environment was not checked. Any process of the same user can read it, as it can
  read the hook secret; the password keeps out other users only.
- **Server file and record.** Two files per pane in `<state>/opencode` (directory mode 0700,
  files 0600, each written through a temporary file and a rename; `MkdirAll` does not tighten
  a directory that already exists). `<pane uuid>.server.json` is the wrapper's: the server's
  URL and pid, and `ended: true` once the server exited. `<pane uuid>.json` is gff-hab's: the
  session last bound. gff-hab reads the server file before each stream connection, each send,
  answer and chat fetch, and talks to the URL only while the file names a server that has not
  ended and whose pid exists. After connecting it checks that `/api/info` names the same pid;
  another pid stops following for good, with an error in the log. Known limit: between the
  server's death and the wrapper reaping it and writing `ended` (microseconds; the pid of an
  unreaped child still exists), a reconnect can still reach the old port. gff-hab stops
  following a pane on its exit or removal, and removes both files. At start it removes the
  records of servers that ended (or whose pid is gone, or whose wrapper never wrote a server
  file within a minute), follows the panes the link follows, and keeps the rest pending: a
  holder the link reaches late reports them with `PaneAdded`, and they are followed then.
- **Client.** Typed by hand for the eight operations in `opencode.Operations`, pinned to 2.2.5.
  A generator buys little, since the document does not type events. On its first connection to
  a server gff-hab checks the document has each operation under the same method and path, and
  logs an error naming each missing one; a document it cannot fetch is logged as an error once.
  A server version other than the tested one is logged; the adapter goes on. `detect.Tested`
  has opencode 2.2. A 404 counts as a missing session or request only with the tag
  `SessionNotFoundError` or `PermissionNotFoundError`; a 404 with another tag or none (2.2.5
  answers an unknown route with an empty 404) is a moved route: `unavailable`, named as API
  drift.
- **State.** One event stream per pane (`GET /api/event`, filtered by `types`), source
  `server`, stamped with the event's `created`. It reconnects after 250 ms, doubling to 10 s.
  On each connect it reads `/api/permission/request`, then `/api/session/active`, and stamps
  what it applies with the time taken before the subscription was sent, so every event the
  stream then delivers is newer and wins (a turn that ends during the reads still shows `done`).
  A running top-level session binds the pane as `working`, else the recorded session binds as
  `idle`. `working` is not applied while a request is pending, since it would end the request;
  each pending request the pane does not already show opens as `blocked:permission`, and a
  request the pane shows that the server no longer lists ends. When the bound session stopped
  running while the stream was down, its newest message gives the end: an `idle` message with
  outcome `succeeded` is `done`, `failed` is `error`, anything else `idle`. A session lookup
  (`GET /api/session/:id`) is bounded to 5 s; when it fails, a session other than the bound one
  is taken as a child, so a failed lookup never moves the pane. The adapter remembers the
  session of at most 256 open requests, drops one on `permission.replied`, and drops the
  oldest first past the bound.

  | Server event | Agent state |
  |---|---|
  | `session.created` | binds, `idle`, sets `version` |
  | `session.viewed` | binds, `idle` (`working` if it runs); ignored for the session already bound, since the TUI posts it after every turn |
  | `session.execution.started` | binds, new turn, `working` |
  | `session.retry.scheduled` | `working` |
  | `session.execution.succeeded`, `failed`, `interrupted` | `done`, `error`, `idle` |
  | `permission.asked` | `blocked:permission`; request id, tool from `action`, `resources` as the exact command (`shell`, `bash`) or files (`edit`, `write`, `read`, `patch`, `apply_patch`, `external_directory`), cwd from the event's folder, the data as input |
  | `permission.replied` | resolves `requestID`, `working` |
  | `session.deleted` | unbinds |

  A session with a parent (a subagent from opencode's task tool) never binds: only its
  permission requests and replies apply, as a subagent's. Parents come from `session.created`,
  else from `GET /api/session/:id`.
- **Input.** `session.send` posts the text to `/api/session/:id/prompt` under the same gates
  as the other harnesses (idle, done or error; no open request; one send at a time). No session
  yet (a fresh TUI) answers `unavailable`: type the first message in the terminal. A
  `SessionNotFoundError` 404 is `conflict`. A call the server may have taken (no answer within
  5 s, a dropped connection, a 5xx) is a partial `unavailable`, so a retry under the same key
  gets that answer and never sends the message twice. A call never written (no connection, or
  a context that ended before the dial) or refused (another 4xx) is a plain `unavailable`.
  "Written" means the request's headers reached the transport's writer (`httptrace`). A refusal
  no retry can fix (a 4xx other than 408, 409 and 429, API drift, a gone session) keeps its kind
  for `session.send`, and channel delivery refuses the message rather than hold it. Only a pane whose server
  gff-hab follows (one it started) takes messages; an opencode in a plain pane answers
  `unavailable`.
- **Permission answers.** `approval.answer` posts `once` (allow) or `reject` (deny, with a
  message) to `/api/session/:id/permission/:request/reply`, under the request's own session for
  a child's request. A `PermissionNotFoundError` or `SessionNotFoundError` 404 means it was
  answered in the terminal or ended: `conflict`. The usual read-back then waits for
  `permission.replied`. A deny differs from Claude's: a Claude deny by keys interrupts the turn,
  while opencode's `reject` with a message likely hands the message to the agent, which goes
  on with the turn (not checked live).
- **Chat view.** `chat.page` reads `GET /api/session/:id/message` through the pane's server;
  gff-hab never opens opencode's database. The first read loads the newest 200 messages, in
  pages of 100; a page that reaches past the oldest one held fetches older pages then. A later
  read fetches newest first, back to the first message held in a form that no longer changes,
  once every message that may still change was listed again. It fetches only after a message
  event marked the session changed, or 15 s after the last fetch. After a revert event
  (`session.revert.staged`, `.committed`, `.cleared`), and at most once a minute anyway, a read
  lists every loaded message again; when the server no longer lists a message the reader holds
  (undo, revert), it starts over: one `reset` event, a new cursor name, and the conversation as
  listed now. One read, every page included, ends after 30 s. The reader keeps a hash of each
  message, not its JSON. Cursors count the changes this reader saw under a name random per
  reader, so a cursor from before a gff-hab restart is stale. Version range 2.2.0 to 2.2.5.

  | Message | Chat message |
  |---|---|
  | `user` | kind `user`; files as `[file <name>]` |
  | `assistant` text block | kind `agent`, id `<message id>.<index>` |
  | `assistant` tool block | kind `tool`, id the call id; target from `filePath`, `path`, `command`, `pattern`, `url`, `query` or `description`; status `running`, `done` or `error` (`denied` when the error says rejected); first 4 KiB of the text output |
  | `assistant` reasoning block | left out |
  | `assistant` error | kind `event`, event `notice` |
  | `idle` | `failed`: event `notice`; `interrupted`: event `interrupt`; `succeeded`: left out |
  | `compaction` | `completed`: event `compaction`; `failed`: event `notice` |
  | `shell` (the owner's `!` command) | kind `tool`, name `shell` |
  | `skill`, `agent-switched`, `model-switched` | event `notice` |
  | `synthetic`, `system`, `location-switched` | left out |
  | any other type or block, an unreadable message | event `unknown`, one warning per kind; an unreadable message's id comes from a hash of its JSON, so a read that lists it again adds nothing |
- **Not checked.** Whether the server's own children (persistent PTYs, LSP and MCP servers)
  end with it: a child that calls `setsid` leaves the server's process group, so the wrapper's
  group signals miss it. If only the wrapper dies, the TUI is left talking to a dead server.

- **Panes opencode was not started in by gff-hab** have no server gff-hab knows: chat, sends
  and answers are `unavailable`. The plugin hook path (above) still gives state where the
  plugin runs with the pane's environment; with opencode 2's shared service it does not, and its
  event shapes were not checked on 2.x.

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

Built choices (`session.send`, `internal/harness`, `cmd/gff-hab`):

- `idle`, `done` and `error` deliver now; `error` means the turn stopped and the harness is at
  its prompt again.
- `working` and `blocked` refuse with `conflict` and a hint that names the state. The hab does
  not queue yet; the client keeps the text and sends again on `idle` or `done`.
- An open permission request refuses with `conflict` whatever the state says, because Enter
  on a live dialog approves it.
- One send at a time per pane. A delivered message holds the pane until the agent leaves
  `idle`, `done` and `error`, or 3 seconds pass; a second `session.send` meanwhile is refused
  with `conflict`, never queued, so two messages never land in one turn.
- No agent state, `none` or `exited` refuse with `unavailable`. An exited session refuses with
  `conflict`.
- Claude Code: `ESC [200~` + text + `ESC [201~` as one write, then CR as its own write, queued
  back to back behind the pane's other input: no other client's `pane.input` lands between the
  paste and its Enter. A write whose caller gives up while it is still queued takes the writes
  queued after it with it, so the Enter never goes out without its paste. The text loses every
  control character except
  newline and tab, so it can neither end the paste early nor press keys; CR LF and CR become
  LF. Codex gets the same bytes. opencode takes the text through its server (see opencode). Other harnesses answer `unavailable` until their stage.
- After delivery, `done` becomes `idle` (`Store.Seen`).
- Agent messages (channels, `mesh.md` 6.5) take this same path, wrapped in their envelope.
  Unlike `session.send`, the hab queues them: a busy agent holds a message until it is idle,
  done or stopped on an error. To opencode they go through its server, as one prompt.
- Attachments (`app.md` 4.4). `session.send` with attachment ids sends each image the way the
  harness's prompt takes one, and every other file as an absolute path after the text, one
  per line (`harness.ImageMessageWrites`):
  - Claude Code: one paste of the text, a blank line, then one image path per line. Its paste
    handler (verified in the 2.1.316 bundle) turns each line that is one absolute image path
    into an image and keeps the rest as text.
  - Codex: each image path as its own paste followed by a typed space, then the text as one
    more paste, then Enter. The space keeps a path Codex did not attach apart from the next
    paste. Codex
    attaches a paste that is one image path and nothing else (inferred from the 0.166 binary
    and its open-source TUI; not run live).
  - opencode: one prompt through its server with every path in the text; its read tool takes
    images. Its server's file parts are not used yet.
  Only the stored file name is free of spaces and shell characters; the session folder part
  of a path may hold spaces, which a pasted path needs no quoting for. A message may be files
  alone. After delivery the hab records the send (text and ids), so `chat.page` and
  `chat.message` can give a user message its ids: from trailing paths left as text, or by
  matching a message whose paths became images to its send (`app.md` 4.4).
- Not built yet: the two-second typing pause, the read back and its "not delivered" event.

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

Built choices (`approval.answer`, Claude Code and Codex):

- The checks run in the state store: `blocked:permission`, last set by a `hook` or `server`
  source, a request with an id that is still open, and the same id in the answer. Any miss is
  `conflict`; an OSC 7501 `blocked` alone never enables an answer.
- `allow` and `deny` only; `answer` (a question) is `unavailable` for now.
- One answer per request. While one is on its way, a second gets `conflict`, never keys. Once
  an answer reached the harness (keys written, or the hook took the decision), the request
  takes no other. An answer that delivered nothing (the hook went away first) frees it.
- A blocking permission hook that waits on this request takes the answer as Claude's decision.
  If the hook could not write it (the hook went away at that instant), the call answers
  `unavailable`; Claude then shows its own dialog, and the owner answers again. It never
  retries as keys on its own.
- Otherwise the dialog gets keys: `1` for allow (the dialog's first option, Yes), Esc for deny.
  A Claude Code plan (`ExitPlanMode`) is the exception for allow. Its first option turns on
  auto-accept: by default the dialog has three rows ("Yes, auto-accept edits", "Yes, manually
  approve edits", "No, keep planning"). With Claude's clear-context setting on, a fourth row comes
  first: "Yes, clear context and auto-accept edits". The hab reads the visible rows and sends the
  number of the one option labelled "Yes, manually approve edits". With no such row, or two, it
  sends nothing and answers `unavailable`, with a hint to approve in the terminal. Deny stays Esc
  ("No, keep planning"). Fixtures, written by hand: `claude-plan-dialog-default-2.1.316.txt`
  (three rows, key 2) and `claude-plan-dialog-2.1.316.txt` (four rows, key 3) in
  `internal/harness/testdata/`.
- Through the blocking hook, a plan's allow is a bare `{"behavior":"allow"}`. Claude then
  restores the mode from before plan mode (its `prePlanMode`), else the default mode. It never
  clears the context (verified against 2.1.316).
  Keys go only while the pane shows that request as a hook reported it; after an OSC `clear`
  the answer is `conflict`, so keys never land in a shell. A waiting hook still takes it.
  When a blocking hook for the request gave up with no decision less than 1 second ago, the
  keys wait out that second, so they do not land in the prompt before the dialog is drawn.
- **Stacked requests.** A request that opened while another was still open and unanswered
  takes no keys: the answer is `conflict`, with a hint to answer in the terminal. Codex may ask
  for parallel tool calls one after the other, and the store keeps only the newest request,
  while the harness may still show the first dialog; `1` would allow that one. A waiting hook
  still takes the answer, since it is keyed by request id. A request that opens after the
  previous one was answered, or with none open, takes keys as usual. Known limit: a hook past
  the first request (its `PostToolUse`) closes the open one, so the pane can show `working`
  while the second dialog waits. Nobody has seen two Codex requests at once live yet.
- **Known limit: an answer in the terminal.** When the owner answers the dialog in the
  terminal, the request stays open until the next hook (`PostToolUse` once the tool ends), and
  the app still offers allow and deny. Esc then interrupts the running tool, and `1` lands in
  the prompt. The harness reports `working` over OSC 7501 once the dialog closes; refusing keys
  after such a report is the planned fix. This applies to Claude Code and Codex alike.
- Either way the call then reads back for up to 3 seconds and succeeds only on a real
  resolution: a hook or server closes the request (`PostToolUse`, a new prompt, the turn's end),
  Claude reports `working` over OSC 7501 (the tool runs), or, for a deny, an OSC `idle` after
  the answer (Esc interrupts the turn). That `idle` also closes the request, so the pane
  returns to `idle` and takes a `session.send` (Source precedence). An OSC `working` only shapes
  the call's result; the request stays open until a hook closes it. An exit or a session change answers `unavailable`, and so does
  a request still open after 3 seconds, with a hint to answer in the terminal.

## Edge cases

| Case | Behaviour |
|---|---|
| Untrusted Codex hooks are skipped silently | Built: when the pane's OSC 7501 reports end a turn (`done` or `error`) and no Codex hook arrived, `agent.state` gets `hooks: missing` and gff-hab logs an error that names the trust step. The status board will show it as "no hook seen". |
| A hook fires with no pane | `GFF_HAB` is absent or its token is unknown. The hook command exits 0 and sends nothing; `gff-hab` logs an unknown token once per token. |
| A hook fires with no terminal | Claude may start hooks without a controlling terminal. Our hooks never need one: they post to `GFF_HAB` and never write escape codes to a TTY. |
| Nested agent in a pane | Claude: entrypoint guard. Codex and opencode: the adapter binds the pane to the first session per process and ignores hooks whose session id differs while that session lives, unless the foreground process changed. Built: a session start from another session is dropped only while a permission request is open. Known limit until the foreground-process check: an interactive nested session (entrypoint `cli`, such as one a background job starts) that starts at any other time takes the pane. |
| Subagents | Claude hooks with `agent_id` never rebind the pane. |
| Transcript format drift | Each reader pins the harness versions it was tested on. An unknown version reads in a tolerant mode that shows unknown records as placeholders and flags the adapter as degraded. gff-hab reports the degraded adapter as drift (built so far: a log line once per version; the status board shows it once it exists). A CI job runs new harness versions against our fixtures. |
| Transcript not yet written | Claude may create the file after the first hook. The reader waits for the file; it does not fail the pane. |
| `gff-hab` restarts | Hook posts during the gap are lost. On start, the adapter rebuilds state from the transcript tail, the last OSC 7501 report in the holder's byte ring, and the process table. It marks state as `source: screen` until the next hook. Built: the last OSC 7501 report only, as `screen`: from the replayed ring, else from the pane's checkpoint (the replay starts there). The transcript tail and process table are not read yet. A permission request open before the restart is not rebuilt: the pane shows the report's `blocked`, sends stay refused, and an answer waits for the session's next hook or goes through the terminal. |
| Esc that is not an interrupt | In Claude Code, Esc while it works may only close an autocomplete menu or leave the editor opened for a queued message. The pane then shows `idle` (source `server`) until Claude's next report, which may take minutes in a long tool run. Sends stay refused meanwhile (Source precedence). |
| A Claude starts while `gff-hab` is down | Nobody answers its OSC 7501 `?` query, and replayed bytes are never answered, so this Claude sends no reports for its whole run. Its state comes from hooks only. |
| A program in the pane forges OSC 7501 | gff-hab trusts OSC 7501 for the displayed state and, as herdr does, for the send gate: `session.send` goes when the pane shows `idle`, `done` or `error`. So a program that forges reports can mislead the gate. Hooks narrow this: a report never opens or closes a permission request, so it cannot end a block a hook reported, and it never enables an answer. A pane with no hooks has only OSC 7501, so there a forged `idle` over Claude's dialog would let a send's Enter approve it. A forged `clear` shows the pane exited until the next hook. |
| Harness upgrade mid-session | The running process keeps its version; only new panes see the new one. The reader picks its parser per session from the version in the transcript, not from the installed binary. |
| Out-of-order hooks | See [Ordering](#ordering). |
| Codex daemon update | With the shared daemon, `codex app-server daemon update` may interrupt running work in every pane. This is one reason the hab starts Codex with `--no-daemon` (decision below). |

## Fixtures

Each adapter ships fixtures written by hand. A fixture is a hook event sequence plus the
matching transcript lines or server events, and the expected `agent.state` stream. No fixture
comes from a real session. Every fixture states the harness version its shape follows.

Built: the Claude Code transcript fixtures are `internal/transcript/testdata/claude-<version>.jsonl`,
with the expected chat messages beside each as `.chat.json`. `claude-2.1.310.jsonl` covers each
record type above, unknown ones included; `claude-drift.jsonl` covers a newer version and an
unreadable line.

Built: the Codex rollout fixtures are `internal/transcript/testdata/codex-0.166.0.jsonl` and
`codex-drift.jsonl`, with `.chat.json` beside each. The hook fixtures are
`internal/harness/testdata/<harness>-*.json`: `codex-turn.json` covers a turn, a late duplicate
request, a nested agent, an interrupt and the end; `codex-permission-no-id.json` covers
`PermissionRequest` with no `tool_use_id`, as Codex sends it. In a hook fixture, a request id of
`*1`, `*2` and so on stands for each distinct one gff-hab made, in order of appearance.

Built: the usage fixtures are `internal/usage/testdata/`: Claude Code status-line inputs
(`claude-2.1.316.json`, `claude-spend.json`, `claude-none.json`, `claude-drift.json`) and Codex
rollout ends (`codex-0.166.0.jsonl`, `codex-older.jsonl`, `codex-drift.jsonl`), with the expected
reading, version and drift beside each as `.usage.json`.

Built: the opencode server fixtures are `internal/harness/testdata/opencode-server/*.json`
(server events and the expected `agent.state` stream: a turn with requests, a failure and an
interrupt; a subagent's request), and `internal/transcript/testdata/opencode-2.2.5.json` (the
message list) with `.chat.json` beside it. `cmd/gff-hab/testdata/fakeopencode` stands in for
opencode in the end-to-end test.

## Open decisions

All three are now decided by the owner (2026-11-01). The section keeps its name so links hold.

### Codex: shared daemon or `--no-daemon`

Decision: A, `--no-daemon` in the holder now; C, a private app-server per pane, is a later spike
(owner, 2026-11-01).

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

Decision: B, keep the blocking hook as a per-host setting, off by default, with about a 30-second
wait (owner, 2026-11-01).
Built: option B, off by default. The setting is `hab.permission_wait`, a preference with no level
that the app writes to one hab (`app.md` 20.4); today only `-permission-wait` and
`$GFF_HAB_PERMISSION_WAIT` set it.

| | A. Fire-and-forget (today) | B. Blocking hook with timeout |
|---|---|---|
| How | Hook posts `blocked:permission` and exits; the owner answers in the TUI or the app sends keys | Hook posts and waits for the app's answer; it returns allow or deny as hook output, or exits with no decision after a timeout so the TUI dialog appears |
| Answer channel | Keystrokes | Structured |
| Risk | Keystrokes into a changed dialog | A stuck hook delays every permission prompt by the timeout |

Recommendation: B, with a short timeout (about 30 seconds, well under Claude's 600-second
default), a per-host switch that is off by default, and an immediate no-decision exit when no
client is connected. Codex's `PermissionRequest` hook supports the same allow and deny shape
and can share the code.

Built (option B, off until the owner turns it on):

- **Switch.** `gff-hab serve -permission-wait <duration>`, else `$GFF_HAB_PERMISSION_WAIT` (a Go
  duration or whole seconds). 0, the default, is off. 1 second to 120 seconds when on.
- **Hook.** The printed Claude config runs `gff-hab hook -wait claude PermissionRequest` with a
  130 second timeout. The command posts to `/hooks/<token>?wait=1`. gff-hab applies the event as
  usual, then answers 204 at once unless the switch is on, a client that can answer is
  connected, and this very envelope opened the pane's open request. A client can answer when it
  said hello as `app` or `terminal` and subscribed to events whose scope holds the pane; a
  `cli` or `script` client, such as `gff-hab events`, does not count. A `PermissionRequest`
  that binding dropped (a nested session) opened nothing, so its hook never waits and can never
  take another request's answer. Then it answers 200
  at once and sends the body later: `{"behavior":"allow"}`, `{"behavior":"deny","message":...}`
  or `{}` when the wait ends with no decision. The answer's header must come within the hook's
  900 ms budget, so a hung gff-hab still costs a turn no more than that.
- **Output.** With a decision, the command prints the harness's hook output (Claude Code and
  Codex share one shape),
  `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{...}}}`. With none it
  prints nothing, and Claude shows its own dialog. The command never prints for any other event.
- **Answer.** `approval.answer` hands the decision to the waiting hook when one waits on that
  request; otherwise it falls back to keys.
- **Wait first.** The hook's state change and its wait are one step, done before the 200, so an
  answer that comes the moment the request is published finds the wait, never the keys path.
- **No approver, no wait.** When the last client that can answer the pane closes or narrows its
  subscription, the pane's waiting hooks end with no decision at once, and Claude shows its own
  dialog.
- **Codex.** Codex's printed config runs the same `-wait` hook; the switch covers both
  harnesses.

### Who writes hook config into harness settings

Decision: print first; a later opt-in install command writes the config after it shows the diff
(owner, 2026-11-01). This is the install prompt that `landscape.md` section 6 suggests; Paseo,
herdr and Rex install hooks themselves. Today `gff-hab hook -print` prints only.

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
