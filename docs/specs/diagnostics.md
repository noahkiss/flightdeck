# Diagnostics: behaviour spec

Status: proposed. Owner decisions are open in section 13.

This spec says how Flightdeck records logs, traces, crashes and local usage figures. The goal is
that early daily use tells the maintainers three things: how the product is used, which bugs
appear, and how one issue moves across processes and machines. It describes behaviour in our own
words. It copies no code from the reference projects.

Rules this spec works within:

- Nothing leaves the machine unless the user exports it. Flightdeck has no telemetry service and
  sends no diagnostics anywhere. No third-party telemetry library is used.
- Diagnostics never record transcript text, prompt text, keystrokes, terminal output or secrets.
- Diagnostics never slow a hot path. Nothing logs per PTY byte, per frame or per keystroke.

Each section that settles a question ends with a **Decision** line, as in `hab-logic-api.md`.

## Contents

1. [Today](#1-today)
2. [References](#2-references)
3. [Words](#3-words)
4. [Logs](#4-logs)
5. [Correlation](#5-correlation)
6. [Health and crash records](#6-health-and-crash-records)
7. [Tallies: local usage figures](#7-tallies-local-usage-figures)
8. [The diagnostics bundle](#8-the-diagnostics-bundle)
9. [Friction notes](#9-friction-notes)
10. [Privacy and consent](#10-privacy-and-consent)
11. [Cost limits](#11-cost-limits)
12. [Edge cases](#12-edge-cases)
13. [Open questions](#13-open-questions)
14. [Stages](#14-stages)
15. [Tests](#15-tests)

---

## 1. Today

What each component records at the commit this spec starts from. Paths are repo-relative.

### 1.1 Per component

| Component | Logging API and format | Where output goes | Levels and switches | Rotation |
|---|---|---|---|---|
| `gff-hab` | Standard `log`, text, prefix `gff-hab: `, `LstdFlags` (`cmd/gff-hab/main.go:394`). Subsystems get `logger.Printf` through a `Logf` config field | stderr. launchd: `<state>/logs/gff-hab.log` (`internal/service/launchd.go:126-132`, `internal/service/service.go:287-291`). systemd: no `StandardOutput` line, so journald (`internal/service/systemd.go:15-47`) | No levels. `GFF_HAB_DEBUG` turns on the harness store and data stream lines (`cmd/gff-hab/main.go:528-531`) | None. Only the logs of holder instances that are gone are swept (`internal/service/launchd.go:388-397`) |
| `gff-hab-holder` | Standard `log`, text (`cmd/gff-hab-holder/main.go:57`) | stderr. launchd: `<state>/logs/gff-hab-holder.<instance>.log`. systemd: journald | None | None |
| `gff` | Passes no `Logf` to its libraries (`cmd/gff/main.go:221,230,234,271`), so they log nothing (`internal/gffclient/client.go:275`, `internal/habconn/conn.go:175`). Prints one error line at exit | The user's terminal | None | n/a |
| Hook (`gff-hab hook`) | Prints its error only when `GFF_HOOK_DEBUG` is set (`cmd/gff-hab/hook.go:74-76`) | The harness's stderr capture, usually nowhere | None | n/a |
| Capcom engine (TypeScript) | One custom function, plain text to stderr, ad hoc prefixes (`capcom-engine/src/engine.ts:172-173`). A redactor masks the credential value only (`capcom-engine/src/contract.ts:176-180`) | gff-hab appends stdout and stderr to `<state>/capcom/engine/engine.log` (`internal/capcomengine/supervisor.go:29,354`, `internal/capcomengine/job.go:81-87`, `internal/capcomengine/systemd.go:44-45`) | None | None |
| Speech helper | Its stderr reaches the gff-hab log one line at a time (`internal/speech/helper.go:82-85`) | gff-hab's log | None | n/a |
| Flightdeck app | `os.Logger`, one subsystem, 14 categories (for example `apple/FlightdeckKit/Sources/FlightdeckKit/Board/Fleet.swift:112`). Privacy markers are mixed: some errors that can name a host are `.public` (`SSHSetup.swift:446`) | The unified log | debug, info, notice, error | The system's |

Existing diagnostic commands: `gff-hab detect explain` (`cmd/gff-hab/detect.go:209`),
`gff-hab manifests list` (`cmd/gff-hab/manifests.go:25`), `gff-hab capcom status`
(`cmd/gff-hab/capcomcli.go:74`), `gff-hab events`, `gff-hab api call`, and `version` on every
binary (`internal/version`, stamped by `-X main.version` and `-X main.build`, `Makefile:251-253`).
There is no status or doctor command for a hab as a whole. No binary reads
`debug.ReadBuildInfo`.

### 1.2 Silent failures

| # | Failure | Evidence | What a user sees |
|---|---|---|---|
| 1 | A panic in any gff-hab goroutine kills gff-hab. No top-level `recover` exists. The trace lands in the launchd log or the journal; nothing records the crash | `rg recover` finds only `internal/holder/holder.go:395-406`, `internal/modhost/module.go:358`, `internal/modhost/host.go:569`, `internal/control/attachments.go:114` | A short disconnect; no cause |
| 2 | A recovered holder panic logs the panic value without a stack | `internal/holder/holder.go:401` | One log line with no place to start |
| 3 | A hook never fails. It exits 0 at its budget plus 50 ms, and drops its error unless `GFF_HOOK_DEBUG` is set. A hab that answers with an error status is dropped too | `cmd/gff-hab/hook.go:64-77`, `internal/hook/client.go:22,108` | Agent state stops updating; nothing says why |
| 4 | The harness store's "dropped event" and "none of its hooks reached gff-hab" lines go to a no-op unless `GFF_HAB_DEBUG` is set | `internal/harness/store.go:136,176,182,218,261`, `cmd/gff-hab/main.go:528-531,566` | Same as 3 |
| 5 | When the hook server fails, gff-hab logs one line and keeps running with no hook endpoint | `cmd/gff-hab/main.go:2048-2052` | Every agent status goes stale |
| 6 | A previous gff-hab's end is classed as handover or crash by the holder, and logged once at link ready. Nothing persists it | `internal/holder/holder.go:575-585`, `internal/link/link.go:374,569-578` | Nothing |
| 7 | A failed handover write on close is dropped, so the holder records a crash that was a clean stop | `internal/holderclient/holderclient.go:512` | A false crash in the holder log |
| 8 | A channel message that expires after 7 days in the outbox gets no log line. Delivery does | `internal/channels/channels.go:1819-1834` against `:1186` | A send that never arrives, with no trace |
| 9 | Peer online and offline changes are not logged. Each reconnect attempt is, so a flapping link floods the log with errors and no state lines | `internal/peers/manager.go:293-330`, `internal/habconn/conn.go:540` | Noise without a timeline |
| 10 | The control server logs accepts and refusals only. No request, error answer or duration is logged | `internal/control/server.go:337,344,437,454`, `internal/control/conn.go:292` | Nothing to grep for a failed call |
| 11 | The app drops a line from the hab that does not decode, with no log line | `apple/FlightdeckKit/Sources/FlightdeckKit/RPC/RPCClient.swift:170` | A protocol mismatch is invisible |
| 12 | The Capcom engine logs a handler exception, then acknowledges the message anyway, so the message is lost | `capcom-engine/src/route.ts:204-206` | A missing Capcom answer |
| 13 | The Capcom engine has no `uncaughtException` or `unhandledRejection` handler | grep | A crash with no recorded cause, though gff-hab counts it |
| 14 | Logs grow without a cap: `gff-hab.log`, the holder logs and `engine.log` | 1.1 | A full disk after months |

The app paths in this table are under `apple/`; some are abbreviated to the file name.

### 1.3 Counters and timing that exist

- `hab.stats`: host CPU, memory, disk, battery and uptime, not process figures
  (`api/methods.go:107`, `api/clients.go:507-517`).
- `usage.list` and `usage.updated`: harness plan limits, not product use (`api/methods.go:106,226`;
  `app.md` 14).
- The usage governor's counters in `<state>/governor.json` (`internal/governor/governor.go:164-173`).
- Capcom engine crash count and window (`internal/capcomengine/supervisor.go:445-471`), cleared by
  `capcom.resume`. Turn tokens and cost in `turn.ended` (`capcom-service.md` 339).
- A round's restart count (`api/rounds.go:60`).
- No `expvar`, `pprof` or Prometheus endpoint. No per-method timing in the control server.

### 1.4 What crosses each boundary today

| Boundary | Wire | Fields usable for correlation | Evidence |
|---|---|---|---|
| Client to gff-hab | JSON-RPC 2.0, one JSON object per line | `id` per connection; client id, kind and label per connection. No trace field; unknown members are dropped | `internal/control/wire.go:20-27`, `internal/control/conn.go:39-53` |
| gff-hab to holder | Binary frames, 14-byte header: kind, pane, a `u32` token echoed on replies | Pane and token | `internal/holderproto/holderproto.go:39-49` |
| gff-hab to peer hab | The same JSON-RPC, as client kind `hab` | As client to gff-hab | `internal/peers/manager.go:122-135` |
| Hook to gff-hab | HTTP POST on the hook Unix socket, `/hooks/<token>` | Envelope: harness, event, time, sequence, version | `internal/hook/server.go:108-201`, `internal/harness/normalise.go:19-26` |
| gff-hab to Capcom engine | JSON lines after an HTTP upgrade on the hook socket, with seq and ack | seq | `capcom-engine/src/route.ts:1-2,58-60` |

### 1.5 Free text in logs today

No Go log line prints PTY bytes, pane input or a hook body. The link logs offsets only
(`internal/link/pane.go:312-436`); the hook server logs an 8-character token prefix, never the
body (`internal/hook/server.go:128,142,150`). Go has no general log redaction.

These lines do write free text that a program or a user produced, and stage 1 moves each to a
keyed value under the scrub of 8.3:

- Queued group move output (`cmd/gff-hab/groupmove.go:758`).
- Rounds notice text (`cmd/gff-hab/rounds.go:1283`).
- The Capcom usage line (`cmd/gff-hab/capcompace.go:241`).
- The engine's start check and report text (`internal/capcomengine/supervisor.go:494,497`).
- Speech helper and module host child stderr (`internal/speech/helper.go:84,117`,
  `internal/modhost/host.go:460`).
- Trust answers, with a folder path (`cmd/gff-hab/trust.go:467`).

The Capcom engine relays raw SDK stderr (`capcom-engine/src/engine.ts:592-594`) and the first
line of provider errors (`capcom-engine/src/codex.ts:666,669,681,789`). Either can echo a prompt
fragment.

### 1.6 Specs this one must agree with

- `hab-holder.md` 5.3: launchd job output goes to `<state>/logs/`.
- `hab-holder.md` 6.6: the holder recovers a panic in every goroutine and logs it.
- `capcom-service.md` 218 and 438-442, 1494-1498: Capcom's crash limit; the credential is never
  logged.
- `push.md` 84-89: the push key is never logged; only a cut first stderr line is.
- `voice.md` 281-283: no audio or speech text is kept or logged.
- `instructions-mcp-flight-rules.md` 498-503 and `rounds.md` 1315, 1915: secret values are never
  logged.
- `app.md` 14, `gff-client.md` 15.6, `groups.md` 10: "usage" means harness plan limits. This spec
  does not reuse the word (3).

No spec covers log rotation, crash records, a bug-report bundle, MetricKit or product-use
counts. This spec defines them.

---

## 2. References

Sources were read in November 2026. Facts not found in a source are marked.

| Tool | Logs | Bundle | Usage figures and consent |
|---|---|---|---|
| zellij | One text file under the temp dir, `zellij-<uid>/zellij-log/zellij.log`, level fixed in code. Rotates at 16 MiB, keeps one old file. File mode 0600 [z1] | None | None |
| tmux | No log by default. `-v` writes one client and one server log per PID in the working directory; `-vv` adds a copy of all terminal output. SIGUSR2 toggles server logging [t1] | None | None |
| herdr | Logs per session under its config directory. `herdr status` summarises server and client [h1]. Levels and rotation: not found | Not found | herdr-app sends telemetry by default (`landscape.md` 247) |
| Tailscale | Local logs through the OS (journal, Console). A file buffer for uploaded logs, capped at 1 MiB on NAS builds so disks can sleep [ts2] | `tailscale bugreport` uploads nothing new: it prints an id and writes a marker into logs already uploaded. A macOS config report is a local archive the docs ask users to review first [ts1] | All-or-nothing opt-out flag [ts3] |
| Syncthing | `--log-file`, `--log-level`, `--log-max-size`, `--log-max-old-files`; per-facility debug through `STTRACE` [s1] | None | Usage reports off by default. The GUI asks once and offers a preview of the exact report. Consent is stored as the report version accepted [s2][s3]. Crash reports are opt-out; panic text leaked paths until it was cut [s4] |
| VS Code | A folder per run under the user data dir, one file per component [v1]. Level per output channel, until restart [v2] | `--status` prints process diagnostics; the troubleshooting guide asks the user to redact logs by hand [v3] | Telemetry opt-out; "Show Telemetry" traces each event to a local log [v4] |
| Moshi | `moshi-hook logs -f`; `moshi-hook doctor` checks the daemon, pairing and hooks [m1] | A support email with version and error | Not found |
| MetricKit | n/a | Crash, hang and resource diagnostics reach the app in-process; on iOS 15 and macOS 12 or later they arrive at the next launch. No server is needed [mk1][mk2] | Delivery did not depend on the user's analytics sharing in one Apple test [mk3]; not documented |

What we take:

- **A small fixed cap with few old files** (zellij, Syncthing). A laptop disk is not a log store.
- **A per-component file** (tmux, VS Code): one grep per process, and a holder log that stays
  small.
- **A doctor-style status command** (herdr, Moshi, VS Code `--status`).
- **No tool ships an automatic redacting bundle.** Tailscale avoids the problem by uploading;
  VS Code leaves it to the user; Syncthing leaked paths in panics. We redact by allowlist, and the
  user still reviews (8).
- **Local-only figures with a preview** (Syncthing's model, without the upload). Since nothing is
  sent, the preview is the whole report.
- **Never log terminal output** (tmux `-vv` is the counter-example).

Sources:

- [z1] https://zellij.dev/documentation/plugin-api-logging.html ;
  https://raw.githubusercontent.com/zellij-org/zellij/main/zellij-utils/src/logging.rs
- [t1] https://raw.githubusercontent.com/tmux/tmux/master/tmux.1
- [h1] https://herdr.dev/agent-guide.md
- [ts1] https://tailscale.com/kb/1227/bugreport
- [ts2] https://raw.githubusercontent.com/tailscale/tailscale/main/logpolicy/logpolicy.go
- [ts3] https://tailscale.com/kb/1011/log-mesh-traffic
- [s1] https://docs.syncthing.net/users/syncthing.html
- [s2] https://docs.syncthing.net/users/security.html
- [s3] https://docs.syncthing.net/users/config.html
- [s4] https://forum.syncthing.net/t/crash-reporting/13538
- [v1] https://skills.sh/microsoft/vscode/code-oss-logs (a skill in the VS Code repo, not product
  docs)
- [v2] https://github.com/microsoft/vscode/issues/205159
- [v3] https://code.visualstudio.com/docs/supporting/troubleshoot-terminal-launch ;
  https://code.visualstudio.com/docs/configure/command-line
- [v4] https://code.visualstudio.com/docs/configure/telemetry
- [m1] https://getmoshi.app/docs/troubleshooting
- [mk1] https://developer.apple.com/documentation/metrickit
- [mk2] https://developer.apple.com/documentation/metrickit/mxcrashdiagnostic
- [mk3] https://developer.apple.com/forums/thread/784399

---

## 3. Words

| Word | Meaning |
|---|---|
| log | The line-per-event record each process writes (4) |
| trace | A random id that follows one action across processes (5) |
| health record | One line per process start that ended badly: a crash, a recovered panic, an unclean exit (6) |
| tallies | Local counts and latencies of API calls and features (7). Not "usage", which means harness plan limits |
| bundle | One archive of redacted diagnostics a user can attach to an issue (8) |
| friction note | A user's own note that Flightdeck fell short at this moment (9) |
| `<state>` | gff-hab's state directory: `control.DefaultState` (`internal/control/state.go:13`). `$GFF_HAB_STATE`, else macOS `~/Library/Application Support/gff/hab`, else `$XDG_STATE_HOME/gff/hab` or `~/.local/state/gff/hab` |

**Decision: "tallies", not "usage".** "Usage" already names plan limits in four specs and the
`usage.*` methods. One word per concept.

---

## 4. Logs

### 4.1 Format

Every Go process writes JSON lines through the standard `log/slog` package. One line is one
object:

| Field | Always | Meaning |
|---|---|---|
| `t` | yes | Time, RFC 3339 in UTC with milliseconds |
| `lvl` | yes | `debug`, `info`, `warn` or `error` |
| `proc` | yes | `hab`, `holder`, `gff`, `hook`, `capcom`, `speech` |
| `epoch` | hab only | The hab process's epoch, as in the welcome |
| `comp` | yes | The subsystem: `link`, `control`, `peers`, `channels`, and so on |
| `msg` | yes | A constant phrase. It never holds a variable value |
| `trace` | when known | 5 |
| other keys | no | Typed values: ids, counts, durations, error kinds, error text |

- `msg` is a constant so lines group and count by message, and so redaction (8.3) never has to
  parse prose. Variable data goes in keys.
- Error text goes in the key `err`. It is scrubbed in a bundle (8.3), because OS errors carry
  paths and host names.
- A line is at most 8 KiB. A longer value is cut and marked `…(cut)`.

**Decision: JSON lines on disk, text for people.** `gff-hab logs` (4.6) prints text. JSON lets
the bundle redact by key, lets `gff-hab logs` filter by trace and level, and needs no parser of
our own. The standard library has it, so no new dependency. Every reference tool writes text
locally (2); none of them correlates across processes, which is the need here.

### 4.2 Levels

| Level | Use | Examples |
|---|---|---|
| `error` | Something failed that the user will notice, and no retry is left | Hook server down, a holder lost, a send expired |
| `warn` | Something failed and will be retried, or a fallback ran | Peer offline, a hook answered an error, a config row ignored |
| `info` | A state change worth one line | Start, stop, peer online, pane created, holder handover |
| `debug` | Detail for one investigation | Each hook envelope, each request with its duration |

- The default level is `info`.
- The hab setting `diagnostics.log_level` sets it. `gff-hab logs --debug 30m` raises a running
  hab to `debug` for a time, then it falls back by itself. A forgotten debug level must not fill a
  disk.
- `GFF_HAB_DEBUG` stays as a synonym for `debug` with no time limit, for development.
- Lines on per-event paths (each hook envelope, each request) are `debug` only.
- A repeating `warn` or `error` with the same `msg` and `comp` logs at most once per 10 seconds.
  The next line carries `suppressed` with the count. This fixes 1.2 #9 without hiding the first
  failure.

### 4.3 Files

One file per process kind, in `<state>/logs/`:

| File | Writer | Cap |
|---|---|---|
| `hab.log` | gff-hab | 10 MiB, 3 old files |
| `holder.<instance>.log` | each holder | 5 MiB, 2 old files |
| `capcom.log` | the Capcom engine (4.5) | 5 MiB, 2 old files |
| `gff.log` | `gff`, in its own state directory (`<gff state>/logs/`) | 1 MiB, 1 old file |
| `hook.log` | hook runs that fail (4.4) | 1 MiB, 1 old file |
| `crash/` | crash output (6.2) | 20 files |

- Each process opens its file with `O_APPEND`, mode 0600, in a directory of mode 0700.
- The writer rotates by size: it renames `hab.log` to `hab.log.1`, shifts older files, and drops
  the oldest. A lock file beside the log keeps two writers from rotating at once.
- The writer is buffered and never blocks its caller. When the queue (1,000 lines) is full, it
  drops lines and counts them; the next written line carries `dropped`. `warn` and `error` lines
  flush at once.
- The process also writes `warn` and `error` lines to stderr. The service manager's capture
  (launchd file, journald) then holds what came before the logger was up, and a fatal runtime
  error.
- The launchd job's own stdout and stderr files become `hab.stderr` and
  `holder.<instance>.stderr`. gff-hab truncates each at start when it is over 1 MiB.

**Decision: each process writes its own file on both systems.** journald alone would leave macOS
and Linux with two ways to read and bundle. The stderr copy keeps journald useful.

**Decision: rotate inside the process.** macOS has no logrotate, and `newsyslog` needs root
configuration. A size cap set by us is the same on every machine.

### 4.4 Hooks

A hook run is short and called often (`internal/hook/client.go:22`, a 900 ms budget). It logs
nothing when it works.

- When it fails (no hab, a timeout, an error status), it appends one line to `hook.log`: harness,
  event, the failure kind and the duration. It never writes the payload.
- Before it writes, it checks the file size. Over the cap, it renames the file to `hook.log.1`.
  Two hooks may race; one rename wins and the other's line still lands. That is accepted.
- gff-hab counts envelopes it receives per harness and event (7), and logs "no hook reached
  gff-hab" for a session at `warn`, not `debug` (1.2 #4).
- The exit code stays 0. A hook must never break the harness.

### 4.5 The Capcom engine and the speech helper

- The engine writes the same JSON line format to its own `capcom.log`, with the same rotation. The
  path comes from gff-hab in its environment. `engine.log` keeps raw stdout and stderr, which then
  hold only a startup failure or a crash trace; gff-hab truncates it at each engine start when it
  is over 1 MiB.
- The engine adds handlers for an uncaught exception and an unhandled rejection. Each writes one
  `error` line, then exits 1. gff-hab's supervisor records the crash (6).
- Raw SDK stderr lines (`capcom-engine/src/engine.ts:592-594`) stay at `debug` and pass the
  scrub of 10.2 before they are written: they can echo prompt fragments.
- The speech helper's stderr stays relayed into `hab.log`, now at `debug`, under `comp` `speech`.

### 4.6 Reading logs

`gff-hab logs` reads the files directly, so it works while gff-hab is down.

| Flag | Meaning |
|---|---|
| `-f` | Follow, across rotation |
| `--since 2h`, `--until` | Time window |
| `--level warn` | This level and above |
| `--proc holder`, `--comp peers` | One process kind or subsystem |
| `--trace <id>` | One action, across every file on this machine (5) |
| `--session <id>` | Lines that name this session or one of its panes |
| `--json` | Raw lines |
| `--debug <duration>` | Raise the running hab to `debug` for this long |

- Text output is local time, one line per entry: time, level, process, subsystem, message, then
  `key=value` pairs.
- `gff-hab status` is new: build and version of gff-hab and each holder, uptime, restarts and
  crashes in the last 7 days (6), peers online, hook endpoint up, Capcom state, log directory size.
  It is the first command a user runs, as `herdr status` is.
- `gff logs <host>` and remote log reading through the API wait for an owner decision (Q9).

### 4.7 The app

- The app keeps `os.Logger`. The unified log is cheap, rotates by itself, and has privacy built
  in.
- Each interpolated value is `.private` unless it is one of: an enum word, a count, a duration,
  an id Flightdeck minted (trace, session, pane, client), an error kind. Host names, labels, paths
  and error descriptions are `.private` (1.1 notes where they are `.public` today).
- The app writes a small file of its own, `diagnostics/app.log` in its container: JSON lines,
  `notice` and above, 1 MiB with one old file. The unified log on iOS lets an app read only its
  current run, so this file is how a previous run's errors survive.
- Export reads the current run through `OSLogStore(scope: .currentProcessIdentifier)` and adds
  `app.log` (8.5).
- The decode failure of 1.2 #11 logs at `error` with the method and the decode error, never the
  line.

---

## 5. Correlation

### 5.1 The ids

| Id | Made by | Scope | Use |
|---|---|---|---|
| `trace` | The client, at a user action; else gff-hab, per request or hook envelope | One action | `gff-hab logs --trace` |
| `epoch` | gff-hab at start | One gff-hab process | Joins log lines, health records and the welcome |
| session id, pane id | gff-hab | A session's life | `gff-hab logs --session` |
| holder token | gff-hab, per holder request | One holder request | Joins hab and holder lines (5.3) |

- A trace is 12 characters from the Crockford base32 alphabet, random (60 bits). It holds no
  content and no time.
- The prefix says who made it: `a` the app, `g` `gff`, `h` gff-hab, `k` a hook envelope, `c`
  Capcom, `r` a round. `a7Q2…` is from the app.

### 5.2 Client to gff-hab

- A request may carry a top-level member `"trace"` beside `id` and `method`.
- gff-hab keeps it in the request's `context.Context`. The log handler adds it to every line the
  request writes.
- A request without one gets a new `h` trace.
- The response carries the same `trace`. An event that a request caused (for example
  `session.updated` after a send) carries it too, so the app can log the end of the action.
- An older hab drops the member (`internal/control/wire.go:20-27`), so the change needs no
  revision. The `trace` capability says the hab honours it.
- `hab-logic-api.md` 3.1 gets one line pointing here.

**Decision: a top-level JSON-RPC member, not `params._meta`.** Params are typed per method and
generated; a member on the envelope reaches every method with one change in `wire.go` and the
clients. JSON-RPC 2.0 names no extension members, but our server and clients already ignore
unknown ones.

### 5.3 gff-hab to holder

- The holder protocol does not change. Each frame already carries a pane and a `u32` token that
  the holder echoes (`internal/holderproto/holderproto.go:39-49`).
- gff-hab logs the token with the trace on every holder request it logs. The holder logs the
  token and pane. `gff-hab logs --trace` follows the token into `holder.*.log`.

**Decision: join on the existing token.** The holder almost never restarts, and a protocol change
needs a new holder build and a drain. A join in the reader costs nothing at runtime.

### 5.4 gff-hab to a peer hab

- A call to a peer sends the current trace in the same member. The peer logs it.
- A bundle from each machine then holds the same trace. Cross-machine reading is by hand: one
  grep per machine's bundle.

### 5.5 Hooks

- A hook envelope cannot know which action caused it. gff-hab gives each envelope a `k` trace and
  logs it with the session and pane.
- When an envelope arrives within 30 seconds of input sent to the same pane, gff-hab logs
  `cause` with that input's trace. One grep then finds "send, then the agent stopped".

### 5.6 gff-hab to the Capcom engine

- Each message on the engine link carries `trace`. The engine logs it on every line for that
  turn. A Capcom job started by the app keeps the app's trace.

### 5.7 Rounds and other background work

- A round run, a peer sync tick and a channel delivery attempt each start an `r` or `h` trace.
  The delivery attempts of one message keep the trace of the send that queued it, so a queued
  send can be followed to its expiry (1.2 #8).

---

## 6. Health and crash records

### 6.1 Run marker

Each long-lived process (gff-hab, each holder, the Capcom engine supervisor in gff-hab) writes
`<state>/run/<proc>.json` at start:

| Field | Meaning |
|---|---|
| `proc`, `instance` | Process kind; holder instance |
| `epoch`, `pid` | This run |
| `build`, `version` | From `internal/version` |
| `started` | Start time |
| `last_seen` | Updated every 60 seconds and at each log flush |
| `clean` | `false` until a clean exit sets it `true` |

### 6.2 Crash output

- At start, each Go process calls `runtime/debug.SetCrashOutput` with a new file
  `<state>/logs/crash/<proc>-<epoch>.txt`. A fatal panic or runtime error then writes its full
  traceback there as well as to stderr. The process deletes the file at a clean exit when it is
  empty.
- The holder keeps recovering goroutine panics (`hab-holder.md` 6.6). It now logs the stack with
  the value, at `error`, and writes a health record of kind `recovered`.
- gff-hab keeps no top-level recover. A panic ends it, the service manager restarts it, and the
  crash file holds the cause. See Q5.

### 6.3 The next start

At start, before it writes its own marker, a process reads the previous one:

| Previous marker | Crash file | Health record `kind` | `cause` |
|---|---|---|---|
| `clean` true | n/a | none | n/a |
| `clean` false | has a `panic:` or `fatal error:` line | `crash` | That first line, cut to 200 bytes |
| `clean` false | empty or missing | `unclean` | `signal or kill`; on Linux the unit's last exit status from `systemctl --user show` when available |

- The record goes to `<state>/health.jsonl`, the last 500 records kept. Fields: `proc`,
  `instance`, `kind`, `cause`, the previous run's `epoch`, `build`, `version`, `started` and
  `last_seen`, and the crash file's name.
- The process logs one `warn` line: "previous run ended unclean", with the same fields.
- gff-hab raises a notice (`notice.raise`, `api/methods.go:105`) for a `crash` of gff-hab or a
  holder, at most one per hour: "gff-hab restarted after a crash". The notice names no file. See
  Q6.
- gff-hab writes the record for a lost holder too, from the link's view: holder instance, panes
  lost, and the holder's last build.
- The Capcom supervisor's existing crash count stays (`capcom-service.md` 218). Each engine crash
  also writes a health record with the last stderr line, scrubbed (10.2).
- The handover write error of 1.2 #7 is logged at `warn`, so a false crash can be told apart.

### 6.4 The app

- **MetricKit.** The app subscribes to `MXMetricManager` at launch. iOS 17 and macOS 14 (the
  deployment targets, `apple/project.yml:8-10`) deliver diagnostic payloads (crash, hang, CPU
  exception, disk-write exception) at the next launch. The app stores each payload's JSON in
  `diagnostics/metrickit/`, the last 20 files. The daily metric payloads are not kept.
- The payload holds call stacks of addresses with binary images, not symbols. Symbolication
  needs the build's dSYM, which CI keeps (`ci-release.md`). The app does not symbolicate.
- `MXCrashDiagnostic` is deprecated in the 27.x SDK in favour of `MetricManager` and
  `CrashDiagnostic` [mk2]. The app moves when its deployment target allows.
- **Run marker.** The app keeps the same marker in its defaults. A launch after an unclean end
  without a MetricKit payload records `unclean`; iOS ends apps in the background often, so this
  is `info`, not `warn`, on iOS.
- Nothing is sent. The Diagnostics screen (8.5) shows the count of payloads and exports them.

**Decision: MetricKit, no crash SDK.** It needs no server and no third party, and it covers
crashes a signal handler cannot catch. A third-party crash reporter would send data away.

---

## 7. Tallies: local usage figures

### 7.1 What gff-hab counts

| Tally | Key | Values |
|---|---|---|
| API calls | method, client kind (`app-ios`, `app-macos`, `gff`, `hab`, `capcom`, `module`, `other`) | count, error count per error `kind`, latency histogram |
| Events delivered | event name | count |
| Hook envelopes | harness, event | count, refused count |
| Features | a fixed word list (7.2) | count |
| Settings | setting id | `set` count (by `settings.set`), and `in_config` (the id is set in `hab.toml`) |
| Peers | — | online changes, reconnect attempts |
| Health | process kind | starts, crashes, unclean ends |
| Diagnostics itself | — | log lines dropped, lines suppressed |

- Latency uses fixed buckets: 1 ms, 5 ms, 20 ms, 100 ms, 500 ms, 2 s, more. A report reads p50
  and p95 off the buckets.
- Counters are atomic adds into a table built at start from the method list. A call costs no
  allocation.

### 7.2 Features and key paths

Features, counted where gff-hab sees them:

| Word | Counted at |
|---|---|
| `send` | Input sent to a session through the chat or composer path |
| `session.create`, `session.restart`, `session.resume` | Each method |
| `round.run` | A round run starts, by trigger kind |
| `capcom.job` | A Capcom job starts, by harness |
| `approval.answer` | An approval answered, by client kind |
| `channel.send`, `channel.expired` | A channel message queued, or expired unsent |
| `friction` | A friction note (9) |

Views the app opens are counted by the app (7.4).

Key paths with a latency tally:

| Path | From | To |
|---|---|---|
| hello | `hab.hello` received | welcome sent |
| attach | data stream open | snapshot sent |
| send | send received | first PTY output after it on that pane |
| hook | envelope received | its state event published |
| peer call | call sent to a peer | answer received |

### 7.3 Storage

- One file per local calendar day: `<state>/tallies/<yyyy-mm-dd>.json`. gff-hab flushes every 5
  minutes and at a clean exit.
- A small index, `<state>/tallies/seen.json`, keeps per method, feature, event and setting the
  first and last day seen and a total count. It also records `since`: the day tallying started.
- Day files older than 90 days are deleted. `seen.json` is kept.
- Tallies hold names and numbers only: method names, feature words, setting ids, error kinds.
  Never a host name, path, session title or argument value.

### 7.4 The app

- The app counts views opened (a fixed word list per screen: board, session, chat, terminal,
  console, approvals, rounds, Capcom, settings, diagnostics) and gestures that never reach a hab
  (a copy, a share).
- It keeps its own day files in its container, and sends each finished day to its home hab with
  `diag.tally`. The hab stores them as client tallies under the app's kind. See Q4.
- The app tallies its own settings (each `UserDefaults` key that differs from its default) by id,
  never by value.

### 7.5 Reading tallies

`gff-hab tallies`:

| Flag | Meaning |
|---|---|
| `--days 7` | Window; prints calls, errors and p50/p95 per method, features, health |
| `--unused 30` | Methods, events, features and settings with no use in the last N days, or never since `since` |
| `--json` | The raw figures |

- `--unused` lists against the full method and event list of this build (the same table
  `rpc.discover` serves) and the settings registry (`internal/settings/registry.go`). A name
  absent from `seen.json` prints as "never since `<since>`".
- The output states the window and the client kinds it saw, so "unused" is read with care: a
  method unused on one hab may be used against another.

**Decision: tallies on by default, local only.** They are names and numbers, they never leave the
machine, and the planned prune needs them from the first day. A setting turns them off (10.4).

---

## 8. The diagnostics bundle

### 8.1 The command

`gff-hab bundle [-o <file>] [--since 48h] [--no-friction]` writes one `.tar.gz`. By default it
goes in the current directory as `flightdeck-diagnostics-<yyyymmdd-hhmm>.tar.gz`. It works while
gff-hab is down.

At the end it prints the archive path, its size, the count of replacements per rule, and one line:
"Review the files before you share them."

### 8.2 Contents

The bundle holds only these files. Anything else is left out.

| Path in bundle | Source | Treatment |
|---|---|---|
| `manifest.json` | Made by the command | Bundle format version, time, window, the file list, replacement counts per rule |
| `versions.json` | `internal/version`, Go version, OS name and version, CPU arch | Kept |
| `status.txt` | `gff-hab status` (4.6) | Scrubbed |
| `service.txt` | The job state from launchd or systemd: loaded, running, last exit, restart count | Scrubbed |
| `logs/` | Each log file within the window, including old files | Redacted by key (8.3), then scrubbed |
| `crash/` | Crash files within the window | Scrubbed |
| `health.jsonl` | 6.3 | Redacted by key |
| `tallies/` | Day files within the window and `seen.json` | Kept: names and numbers only (7.3) |
| `config/hab.toml`, `config/peers.toml` | The config files | Keys kept; values replaced unless the key is on the setting allowlist (8.3) |
| `friction/` | Friction notes within the window (9) | Scrubbed |
| `detect.txt` | `gff-hab detect explain` for each live pane | Scrubbed |

Never in a bundle: transcripts, terminal rings and snapshots, logbook contents, module state,
the hab key, device keys, push keys, the Capcom credential, environment files, secret sets, the
holder registry's sockets, any file not in this table.

### 8.3 Redaction rules

Two passes. The first works by key on JSON lines. The second scrubs every remaining string.

**Pass 1, by key.** Each log key has a class, declared in one table in the logging package:

| Class | Keys (examples) | In a bundle |
|---|---|---|
| safe | `msg`, `lvl`, `comp`, `trace`, `epoch`, session and pane ids, method, error `kind`, counts, durations, sizes, harness name, event name, client kind | Kept |
| name | host id, host label, device id, peer label, user name, project name, branch, session title, worktree name | Replaced by `n:` plus the first 8 hex digits of HMAC-SHA256 of the value, keyed with a random salt made for this bundle |
| path | any file path | The home directory becomes `~`; each component beneath `~` or `/` is replaced as a name, except the fixed Flightdeck layout under `<state>` |
| text | `err`, a relayed stderr line | Pass 2 only |

A key the table does not know is treated as `name`. A new key leaks nothing until someone
declares it `safe`.

The same value hashes to the same token within one bundle, so "the same host failed three times"
still reads. A new salt per bundle means two bundles cannot be joined by token.

**Pass 2, scrub.** On every string that remains, in this order:

| Rule | Replaced by |
|---|---|
| This machine's host name, user name and home path; every host, label and device id in `peers.toml` and `hab.toml`; the names of projects gff-hab knows. Exact, case-insensitive | The `n:` token of pass 1 |
| The hygiene scan's built-in rules (`ci-release.md` 9.4): private IPv4, CGNAT IPv4, tailnet IPv6, `<label>.ts.net` names, home paths | `<ip>`, `<ip>`, `<ip>`, `<host>`, `~/<path>` |
| Any other IPv4 or IPv6 address | `<ip>` |
| Email addresses | `<email>` |
| A URL's user info, query and fragment | Removed; scheme, host token and path kept |
| PEM blocks; `ssh-` and `ecdsa-` key lines; `SHA256:` fingerprints | `<key>` |
| `op://` references; strings after `token`, `key`, `secret`, `password`, `authorization` and `bearer` (any case) | `<secret>` |
| Runs of 32 or more base64, base64url or hex characters | `<blob>` |

The manifest records each rule's hit count, never the matched text, as the hygiene scan does
(`ci-release.md` 9.6).

**Settings allowlist.** A config value stays only when its key is a setting whose value is an
enum, a number or a boolean in the settings registry. Every other value becomes `<value>`.

**Decision: allowlist files, classify keys, then scrub.** The reference tools show that redaction
by pattern alone leaks (Syncthing's panics, 2). Leaving content out of logs (10) is the main
guard; the bundle rules are the second.

### 8.4 Review

- `gff-hab bundle --dir <folder>` writes the files unpacked, for review, and makes no archive.
- The manifest lists each file and its size.
- The bundle is never uploaded. The user attaches it to an issue by hand.

### 8.5 The app

- Settings → Diagnostics shows: app version and build, counts of MetricKit payloads and recent app
  errors, the hab's `status` for each connected hab, and two actions.
- **Export app diagnostics** builds the same layout from the app's side: `manifest.json`,
  `versions.json`, `app.log`, the current run's unified log lines (`notice` and above), the
  MetricKit payloads, and the app tallies. The same passes run in the app. The share sheet offers
  the archive.
- **Bundle from a hab** waits for Q9.

---

## 9. Friction notes

A friction note records "Flightdeck fell short here; I went back to my old tool". It costs the
user one line of text.

### 9.1 How a user makes one

| Client | How |
|---|---|
| `gff` | `gff friction "text"`; inside a session view, the prefix key then `!` opens a one-line prompt. The note names the current session |
| App | A "Note friction" item in each session's menu, and on macOS the shortcut ⌥⌘F. A text field and a Save button |
| Any client | The API method `diag.friction`: `{text, session?, pane?}` |

### 9.2 What gff-hab stores

`<state>/friction/<time>-<trace>.json`:

| Field | Meaning |
|---|---|
| `t`, `trace` | Time and a new trace |
| `client` | Client kind and label |
| `session`, `pane` | When given |
| `text` | The user's own text, at most 2,000 characters |
| `build`, `version` | gff-hab's |
| `log` | The last 10 minutes of `hab.log` at `info` and above, at most 500 lines; those for the named session's panes come first. Includes the holder lines that share a token |
| `tallies` | Today's feature tallies |

- gff-hab answers with the note's trace. The client shows it.
- `gff friction list [--days 30]` prints time, session and text. `gff friction show <trace>`
  prints one note with its log lines.
- The newest 200 notes are kept. Log snapshots of notes older than 90 days are dropped; the text
  stays.

**Decision: a friction note is text plus a snapshot, with no rating.** The first period of use
needs the moment and its context. A form would cost the user more than the note is worth.

---

## 10. Privacy and consent

### 10.1 Rules

- Nothing in Flightdeck sends diagnostics anywhere. No diagnostics code opens a network
  connection.
- No log line, health record, tally or crash record holds: transcript text, prompt text, chat
  text, terminal input or output, keystrokes, clipboard contents, file contents, environment
  values, secret values, or a module's state.
- The one place a user's text is kept is a friction note's `text`, which the user wrote for this.
- Ids Flightdeck mints (trace, session, pane) carry no content.

### 10.2 How the rules hold

| Guard | What it does |
|---|---|
| Constant `msg` | A test walks every `slog` call in the repo with `go/ast`, and fails when `msg` is not a string constant |
| Declared keys | Every log key used anywhere must be in the class table (8.3); a test fails on an unknown key |
| No content types | The logging helpers take no `[]byte` and no type that carries content (`api.ChatMessage`, transcript records). A test lists those types and fails when one reaches a log call |
| Hot paths | The holder's pump and ring, the data stream writer and the input path call no logger. A test lists those functions and fails on a log call inside them |
| No network | A test fails when the diagnostics packages import `net`, `net/http` or `crypto/tls` |
| Canary | An end-to-end test (15) puts one random canary string through every content path, then finds it in no diagnostics file and no bundle |
| Scrub on relay | Relayed stderr (Capcom SDK, speech helper, key commands) passes the pass-2 scrub before it is written, and is cut to one line of 4 KiB |
| App | A unit test on the app's logging wrapper checks the default is `.private`. The app's canary UI test is Q8 |

The guards in the Go tests follow the pattern of `make check-pure`: a source check that fails the
build.

### 10.3 Consent

- Since nothing is sent, nothing needs consent to send. The README and the app's Diagnostics
  screen say what is kept locally and where.
- If a future feature sends anything, it is off by default, asks once, shows the exact report
  before the first send, and stores consent as the report version accepted, as Syncthing does
  [s2][s3]. That feature needs its own spec and owner approval.

### 10.4 Defaults and settings

| Item | Default | Setting |
|---|---|---|
| Logs | On, `info` | `diagnostics.log_level`: `error`, `warn`, `info`, `debug`. Logs cannot be turned off; `error` is the floor |
| Crash and health records | On | None. They are small and needed to fix the product |
| Tallies | On | `diagnostics.tallies`: `on` or `off`. `off` stops counting; `gff-hab tallies --clear` deletes the files |
| App tallies sent to the home hab | On | App setting "Share figures with my hab" (Q4) |
| MetricKit payloads kept | On | App setting "Keep crash reports" |
| Friction notes | Made only when the user asks | n/a |

---

## 11. Cost limits

| Resource | Limit |
|---|---|
| `<state>/logs/` | About 75 MiB at most with one holder: hab 40, holder 15, Capcom 15, hook 2, plus 20 crash files. gff-hab checks the folder at start and every hour, and deletes the oldest old files over 100 MiB |
| `<state>/tallies/` | 90 day files of a few KiB each, under 5 MiB |
| `<state>/health.jsonl` | 500 lines |
| `<state>/friction/` | 200 notes, each at most 256 KiB, under 50 MiB |
| App container | `app.log` 2 MiB, MetricKit 20 files, tallies 90 days |
| CPU per request | One atomic add per tally, one histogram bucket add, no allocation. A `debug` line only when that level is on |
| Hot paths | No logging per PTY read, frame, ring write, keystroke, data stream chunk or terminal query |
| Disk wake-ups | An idle hab writes nothing: no flush when nothing changed |

- A full disk stops log writes. The writer counts dropped lines and never blocks or crashes the
  process.
- The bundle reads files in a stream and writes its archive in one pass. It never loads a log
  file whole.

---

## 12. Edge cases

| Case | Behaviour |
|---|---|
| The laptop sleeps for hours | The run marker's `last_seen` stops. A clean wake changes nothing. A day boundary passed in sleep closes the old tally day at the next flush |
| The clock jumps | Lines keep their wall time. Rotation is by size, not time. Tally days go by local date at flush |
| Two gff-hab processes by accident (`hab-holder.md` 6.8) | Both append to `hab.log` with their own `epoch`. The rotation lock keeps one rotation at a time. Each writes its own run marker under its epoch first, then renames it to `hab.json` |
| A crash in the middle of a line | `gff-hab logs` and the bundle skip a line that does not decode and count it |
| The user deletes `<state>/logs/` | The writer reopens its file at the next rotation check (every 1,000 lines or 60 seconds) |
| A panic value that holds user text | The crash file is local. The bundle's scrub runs on it. The code rule: a panic message is a constant plus ids, never content |
| A hook with no hab at all (hab never installed) | `hook.log` lands under `<state>/logs/` when `<state>` exists; otherwise the hook writes nothing |
| A drained holder's log | Kept until the sweep that removes a gone instance (`internal/service/launchd.go:388-397`), which now also removes its old files |
| A peer on an older build without `trace` | The peer drops the member. The trace stops at the edge; the caller's lines still show the call |
| `diagnostics.tallies` off, then on | A gap in day files. `seen.json` keeps its `since`; `--unused` notes the gap |
| A method added in a new build | It is absent from `seen.json` and shows as "never since `<since>`". The report marks methods newer than `since` by comparing with `api/released/` |
| The app on a device with no hab | App tallies wait in the container, at most 90 days, and are sent on the next connection to the home hab |

---

## 13. Open questions

Each has a recommendation. The owner decides.

| # | Question | Recommendation | Why |
|---|---|---|---|
| Q1 | JSON lines on disk, or text? | JSON lines, read through `gff-hab logs` | Redaction by key, trace filters, counts by message (4.1) |
| Q2 | Name the figures "tallies"? | Yes | "Usage" means plan limits across four specs (3) |
| Q3 | `trace` as a top-level JSON-RPC member? | Yes | One change reaches every method; older peers ignore it (5.2) |
| Q4 | Should the app send its day tallies to its home hab? | Yes, on by default, with an app setting | The prune needs app views and app settings beside the API figures; both devices are the user's |
| Q5 | Should gff-hab recover panics per request instead of crashing? | No. Crash, restart, record | A restart is cheap and leaves no half-changed state. Revisit if crash records show one handler failing often |
| Q6 | Raise a notice after a gff-hab or holder crash? | Yes, at most one per hour | During daily use a silent restart hides bugs. One notice an hour cannot flood |
| Q7 | Write a file on Linux, or rely on journald? | Our own file on both, `warn` and above also to stderr | One reader and one bundle on every OS (4.3) |
| Q8 | Add a canary UI test for the app's logs? | Later, after stage 6 | The Go canary covers the content paths first; the app logs little content today |
| Q9 | Add remote log reading and remote bundles (`diag.logs`, `diag.bundle`, `gff logs <host>`)? | Not now. Add only `diag.friction` and `diag.tally` | The API is about to be pruned; SSH to the host and `gff-hab logs` cover the first period |
| Q10 | Retention: tallies 90 days, health 500 lines, friction 200 notes? | Yes | Enough for a prune decision and a first quarter; all well under the disk caps |

---

## 14. Stages

Smallest useful first. Stages 1 to 3 give the most signal for the first week of daily use.

| # | Stage | Acceptance |
|---|---|---|
| 1 | **Logs and crash records for gff-hab and the holder.** `slog` JSON lines, levels, the size-capped writer, `<state>/logs/` on both OSes, stderr copy of `warn` and above; run markers, `SetCrashOutput`, `health.jsonl`, the next-start `warn` line; holder panic stacks. Promote the silent failures 1.2 #2 to #10 to `warn` or `error` lines, and move the free text of 1.5 into scrubbed keys. `gff-hab logs` with `-f`, `--since`, `--level`, `--proc`, `--json`. `gff-hab status` | A killed gff-hab leaves a health record of kind `unclean`, and a forced panic one of kind `crash` with its first line, both after restart. Files rotate at the cap in a test with a small cap. The hook-server failure and a peer going offline each produce one line. The msg-constant and key-table source checks pass |
| 2 | **Friction notes.** `diag.friction`, `gff friction`, `gff friction list` and `show`, the app's menu item | A note from `gff` and one from the app each land with session, text and a log snapshot; `list` shows both |
| 3 | **Tallies.** Per-method calls, errors and latency by client kind; hook envelopes; feature words; settings; health counts; day files and `seen.json`; `gff-hab tallies` with `--days` and `--unused` | After an e2e run, `--unused 1` lists exactly the methods the run did not call. A call adds no allocation (an allocation test on the counter path) |
| 4 | **Trace.** The envelope member, context propagation, response and event echo, the holder token join, peer calls, hook `k` traces and `cause`, channel deliveries; `gff` and the app send a trace; `gff-hab logs --trace` and `--session` | One send from `gff` to a session on a peer hab yields lines on both habs and the holder with one trace; `--trace` prints them in order |
| 5 | **Capcom engine and hooks.** Engine JSON logs with rotation, crash handlers, the scrub on relayed SDK stderr, health records for engine crashes; `hook.log`; the handler-exception fix of 1.2 #12 | A thrown handler in a test engine leaves an `error` line and a health record, and the message is not acknowledged |
| 6 | **Bundle.** `gff-hab bundle` and `--dir`, both redaction passes, the manifest, the config allowlist; the canary e2e test | The canary test passes. A fixture state folder seeded with host names, paths, IPs, keys and tokens yields a bundle in which none of them appear, and the manifest counts each rule |
| 7 | **App diagnostics.** Privacy defaults in the app's logging, `app.log`, MetricKit capture, the Diagnostics screen and export, app tallies and `diag.tally` | A forced crash in a debug build yields a stored payload at the next launch; export holds it; no `.public` host or path remains (a source check) |

---

## 15. Tests

Each test guards a promise in this spec, at the lowest layer that proves it (`docs/testing.md`).

| # | Test | Layer | Proves |
|---|---|---|---|
| 1 | The log writer rotates at its cap, keeps N old files, and never blocks when its queue is full | Unit | 4.3, 11 |
| 2 | Source check: every `slog` message is a constant; every key is in the class table | Unit (source) | 10.2 |
| 3 | Source check: no log call inside the listed hot-path functions | Unit (source) | 11 |
| 4 | Source check: diagnostics packages import no network package | Unit (source) | 10.1 |
| 5 | A process killed with SIGKILL, then started, writes one `unclean` record; a forced panic writes one `crash` record with the first line | Integration | 6.3 |
| 6 | A holder goroutine panic logs a stack and writes a `recovered` record; the holder keeps serving | Integration | 6.2 |
| 7 | A request with a trace yields hab and holder lines that `gff-hab logs --trace` joins; the response and caused event carry it | Integration | 5.2, 5.3 |
| 8 | A peer call carries the trace to the second hab | End to end | 5.4 |
| 9 | Tallies: calls, errors and buckets per method and client kind; `--unused` against the method table | Unit and integration | 7 |
| 10 | Bundle redaction: the seeded fixture leaks nothing; the same value hashes alike within one bundle and differently across two | Unit | 8.3 |
| 11 | **Canary.** One random string goes through pane input, terminal output, a chat send, a hook payload, a Capcom turn on the test engine, a config label and an environment value. Afterwards no file under `<state>/logs`, `<state>/tallies`, `<state>/health.jsonl`, `<state>/friction` (log snapshots) or the bundle contains it | End to end | 10.1 |
| 12 | A failed hook writes one `hook.log` line, exits 0, and stays within its budget | Integration | 4.4 |
| 13 | A friction note stores text, session and a snapshot that holds the session's lines | Integration | 9.2 |
| 14 | The app's export holds `app.log`, payloads and tallies, and its logging wrapper defaults to `.private` | Swift unit | 4.7, 6.4 |

Test 11 joins the must-have list in `docs/testing.md` when stage 6 lands.
