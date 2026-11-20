# Diagnostics: behaviour spec

Status: stages 1 to 3 built; stages 4 to 6 proposed. Owner decisions
are open in section 13.

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
16. [Later, not planned](#16-later-not-planned)

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
| 1 | A panic in a gff-hab goroutine kills gff-hab; no top-level `recover` exists. The trace lands in the launchd file or the journal, and nothing records the crash. One exception: the hook server is `net/http` with no `ErrorLog`, so `net/http` recovers a handler panic and prints it to the standard logger's stderr. gff-hab keeps running and nothing counts it | Recovers: `internal/holder/holder.go:395-406`, `internal/modhost/module.go:358`, `internal/modhost/host.go:569`, `internal/control/attachments.go:114`, `internal/apigen/schema.go:118` (build time). Hook server: `internal/hook/server.go:91` | A short disconnect, or a hook that silently fails; no cause |
| 2 | A recovered holder panic logs the panic value without a stack | `internal/holder/holder.go:401` | One log line with no place to start |
| 3 | A hook never fails. It exits 0 at its budget plus 50 ms, and drops its error unless `GFF_HOOK_DEBUG` is set. A hab that answers with an error status is dropped too | `cmd/gff-hab/hook.go:64-77`, `internal/hook/client.go:22,108` | Agent state stops updating; nothing says why |
| 4 | The harness store's "dropped event" and "none of its hooks reached gff-hab" lines go to a no-op unless `GFF_HAB_DEBUG` is set | `internal/harness/store.go:136,176,182,218,261`, `cmd/gff-hab/main.go:528-531,566` | Same as 3 |
| 5 | When the hook server fails, gff-hab logs one line and keeps running with no hook endpoint | `cmd/gff-hab/main.go:2048-2052` | Every agent status goes stale |
| 6 | A previous gff-hab's end is classed as handover or crash by the holder, and logged once at link ready. Nothing persists it | `internal/holder/holder.go:575-585`, `internal/link/link.go:374,569-578` | Nothing |
| 7 | A failed handover write on close is dropped, so the holder records a crash that was a clean stop | `internal/holderclient/holderclient.go:512` | A false crash in the holder log |
| 8 | A channel message that expires after 7 days in the outbox gets no log line. Delivery does | `internal/channels/channels.go:1819-1834` against `:1186` | A send that never arrives, with no trace |
| 9 | Peer online and offline changes are not logged. Each reconnect attempt is, so a flapping link floods the log with errors and no state lines | `internal/peers/manager.go:293-330`, `internal/habconn/conn.go:540` | Noise without a timeline |
| 10 | The control server logs accepts and refusals (`internal/control/server.go:337,344`) and event encoding errors (`:437,454`, `internal/control/conn.go:292`). No request, error answer or duration is logged | as cited | Nothing to grep for a failed call |
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

These lines do write free text that a program or a user produced. The structured stage (14,
stage 5) moves each to a key of class `text` (8.3); until then the files stay on the machine:

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
- `docs/push.md` 84-89: the push key is never logged; only a cut first stderr line is.
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
| tallies | Local counts of API calls and features (7). Not "usage", which means harness plan limits |
| bundle | One archive of redacted diagnostics a user can attach to an issue (8) |
| friction note | A user's own note that Flightdeck fell short at this moment (9) |
| `<state>` | gff-hab's state directory: `control.DefaultState` (`internal/control/state.go:13`). `$GFF_HAB_STATE`, else macOS `~/Library/Application Support/gff/hab`, else `$XDG_STATE_HOME/gff/hab` or `~/.local/state/gff/hab` |

**Decision: "tallies", not "usage".** "Usage" already names plan limits in four specs and the
`usage.*` methods. One word per concept.

---

## 4. Logs

Logs change in two steps. Stage 1 caps and gathers the logs that exist. It changes none of the
about 530 `logf`-style call sites in 115 files. Stage 5 makes lines structured, because
redaction by key (8.3) needs it.

### 4.1 Stage 1: capped text logs

- gff-hab keeps its standard `log.Logger` and every `Logf` call. Only its output changes: a
  size-capped file writer (4.3) replaces stderr.
- gff-hab opens `hab.log` once it holds the state-directory lock, so one gff-hab writes it. A
  line written before that goes to stderr at once and to `hab.log` after.
- Run by hand (`-init none`), gff-hab also copies each line to stderr, as before. Run by a
  service manager (`-init launchd` or `systemd`), it does not: `hab.stderr` then holds only what
  reaches stderr directly, such as a crash's traceback.
- The logger's flags become date, time with microseconds, and UTC. One line in `main.go`; no call
  site changes. `gff-hab logs` (4.8) parses that time to merge files.
- The prefix stays `gff-hab: `. The holder keeps `gff-hab-holder: `.
- Every start line names the version and the build: gff-hab's (`cmd/gff-hab/main.go:2054`) and,
  with the next holder build, the holder's (`cmd/gff-hab-holder/main.go:99`). After a drain, the
  holder files then say which build wrote each line.
- `gff` writes its own file only (`<gff state>/logs/gff.log`). It never copies a line to stderr,
  which would paint over its screen.

### 4.2 Stage 5: structured lines

Every Go process writes JSON lines through the standard `log/slog` package, behind a small package
of our own, `internal/diaglog` (10.2). One line is one object:

| Field | Always | Meaning |
|---|---|---|
| `t` | yes | Time, RFC 3339 in UTC with milliseconds |
| `lvl` | yes | `debug`, `info`, `warn` or `error` |
| `proc` | yes | `hab`, `holder`, `gff`, `hook`, `capcom`, `speech` |
| `epoch` | hab only | The hab process's epoch, as in the welcome |
| `comp` | yes | The subsystem: `link`, `control`, `peers`, `channels`, and so on |
| `msg` | yes | A constant phrase. It never holds a variable value |
| `trace` | when known | 5 |
| other keys | no | Values made only by `diaglog` constructors, each with a class (8.3) |

- `msg` is a constant, so lines group and count by message, and redaction never parses prose.
- An old `Logf` call becomes a line with `msg` `"legacy"` and its formatted text under the key
  `text`, of class `text`. Call sites move to typed lines one package at a time, starting with
  those of 1.5.
- A line is at most 8 KiB. A longer value is cut and marked `…(cut)`.
- `warn` and `error` lines also go to stderr, so the service manager's capture holds them.

**Decision: JSON lines on disk, from stage 5.** JSON lets the bundle redact by key and lets
`gff-hab logs` filter by trace and level. Every reference tool writes text (2), and text is
enough until the bundle exists. `gff-hab logs` reads both forms.

### 4.3 Files and rotation

All files are in `<state>/logs/`, mode 0600, in a directory of mode 0700.

| File | Writer | Rotated by | Cap |
|---|---|---|---|
| `hab.log` | gff-hab's logger | gff-hab, in-process | 10 MiB, 3 old files |
| `hab.stderr` | The service manager (launchd file; on Linux the unit's shell, `>>`) | gff-hab, copy-truncate at start and in the hourly sweep | 1 MiB, 1 old file |
| `gff-hab-holder.<instance>.log` | The service manager, from the holder's stderr | gff-hab, copy-truncate at start and in the hourly sweep | 5 MiB, 2 old files |
| `hook.log` | Failed hook runs, append only (4.4) | gff-hab, rename in the hourly sweep | 1 MiB, 1 old file |
| `capcom.log` (stage 5) | The Capcom engine | The engine, in-process | 5 MiB, 2 old files |
| `crash/` | Crash output and stack dumps (6, 4.7) | gff-hab's sweep, oldest first | 20 crash files, and apart from them 5 stack dumps |

`gff.log` sits in `gff`'s own state directory: 1 MiB, 1 old file, rotated by `gff` itself.
`<state>/capcom/engine/engine.log` keeps the engine's raw stdout and stderr; gff-hab
copy-truncates it at each engine start and in the sweep, to 1 MiB.

Rules:

- **One writer per file.** gff-hab is the only writer of `hab.log`: the state-directory lock
  allows one gff-hab (`hab-holder.md` 6.8). Its writer renames and reopens.
- **An inode check.** At each rotation check (every 1,000 lines or 60 seconds) the writer
  compares the open file with the path. When the path is gone or is another file, it reopens.
- **Copy-truncate where another process writes.** The service manager holds the holder's file
  open. gff-hab copies it to `.1` and truncates it in place, so the holder needs no change. Lines
  written during the copy may be lost; that is accepted. launchd and the units' shell (`>>`) open the
  file for append, so writes after a truncate land at the new end; stage 1's test confirms it on
  both systems.
- **Never block.** The writer is buffered. When its queue (1,000 lines) is full, it drops lines
  and counts them; the next line written carries the count. A full disk drops lines the same way.
- **Units on Linux.** Both units start their program through `/bin/sh`, which makes the log
  folder and appends the program's stdout and stderr to the same file launchd writes (Q2). It
  is not `StandardOutput=append:`: systemd opens that file for each command of the unit,
  `ExecStartPre` too, so no command could make a missing folder first, and the user may remove
  `<state>/logs/` (12). The shell
  execs the program, so the unit's main pid is the program's.
- **Names.** A holder's file keeps the name launchd's holder jobs already write. A new name would
  need a migration across the holders that run while gff-hab updates, for no gain.
- **Gone holders.** launchd's sweep removes a gone holder's file (`internal/service`). gff-hab's
  sweep removes old files whose current file is gone, and a holder's file that no registered
  holder owns and that has not changed in 7 days.
- **Uninstall.** `gff-hab uninstall` removes the job output files, `hab.log`, `hook.log` and
  their old files with the jobs. `crash/` and `health.jsonl` are gff-hab's state and stay.

**Decision: rotate inside gff-hab, change no holder code.** macOS has no logrotate, and
`newsyslog` needs root. The holder stays tiny (`hab-holder.md` 6.6); its file is handled from
outside.

### 4.4 Hooks

A hook run is short and called often (900 ms budget, `internal/hook/client.go:22`). It logs
nothing when it works.

- When it fails (no hab, a timeout, an error status), it appends one line to `hook.log`: time,
  harness, event, the failure kind and the duration. It never writes the payload.
- A hook only appends. Each run opens the file fresh, so gff-hab's rename in the sweep is safe.
- The exit code stays 0. A hook must never break the harness.
- gff-hab logs "no hook reached gff-hab" for a session as a plain line, not under
  `GFF_HAB_DEBUG` (1.2 #4).

### 4.5 Levels and debug

Stage 1 keeps two levels: every line, and the `debugf` lines that `GFF_HAB_DEBUG` turns on
(`cmd/gff-hab/main.go:528-531`). Stage 5 brings `debug`, `info`, `warn`, `error`.

| Level (stage 5) | Use | Examples |
|---|---|---|
| `error` | Something failed that the user will notice, and no retry is left | Hook server down, a holder lost, a send expired |
| `warn` | Something failed and will be retried, or a fallback ran | Peer offline, a hook answered an error |
| `info` | A state change worth one line, and each failed request (4.6) | Start, stop, peer online, holder handover |
| `debug` | Detail for one investigation | Each hook envelope, each request |

- `gff-hab debug 30m` turns debug on for a time, through the method `diag.debug` `{until}`.
  `until` is an RFC 3339 time at most 24 hours ahead; a later one is refused with `invalid`, so
  every window ends by itself. An empty or past `until` turns debug off. Capability `debug`. Not
  over a peer link.
- gff-hab writes `debug_until` to `<state>/run/debug.json`. A start honours it until it
  expires. A crash loop then keeps its debug lines across restarts. When the window ends,
  gff-hab logs "debug off: the window ended" and removes the file. A file whose `until` is more
  than 24 hours and a minute ahead is no window `diag.debug` wrote: the start removes it and
  logs one line.
- A window ends by the wall clock. Go's timers stop while the machine sleeps (12), so a debug
  line is written only while the wall time is before `until`, and an end timer that fires early
  waits for the rest. `gff-hab status` reads a window whose end has passed as off.
- `gff-hab debug off` clears it. `GFF_HAB_DEBUG` stays, with no time limit, for development.
- Each change logs one line, and the run marker holds `debug` and `debug_until` for
  `gff-hab status` (6.1).
- The level floor is set in the hab's config file, `[diagnostics] log_level`. It is an operator
  limit, so it has no app row (`app.md` 20.1).
- A repeating line with the same message and subsystem logs at most once per 10 seconds; the next
  one carries the count suppressed. This fixes the flood of 1.2 #9.

### 4.6 Lines stage 1 adds

| Line | Where | Fixes |
|---|---|---|
| A failed request: method, error kind, client kind, duration (and `trace` from stage 4) | Control server, on each error answer | 1.2 #10 |
| Hook server panic, through its `ErrorLog` | `internal/hook/server.go:91` | 1.2 #1 |
| Hook failures, store drops, "no hook reached gff-hab" | `internal/harness/store.go`, no longer debug-only | 1.2 #3, #4 |
| Hook server down: the line repeats every 10 minutes while it stays down, and `status` shows it | `cmd/gff-hab/main.go:2048-2052` | 1.2 #5 |
| Previous gff-hab ended by handover or crash, at each link ready | Already logged at link ready (`internal/link/link.go`, "previous handover" or "crash"). gff-hab's own record comes from its run marker (6.3), not from the holder's view, so no second record | 1.2 #6 |
| Handover write failed on close | `internal/holderclient/holderclient.go:512` | 1.2 #7 |
| A channel message expired unsent | `internal/channels/channels.go:1819-1834` | 1.2 #8 |
| Peer online and offline, once per change, named by device id, never by its label (10.1). The older peers lines that print a label move to `name` keys in stage 5 | `internal/peers/manager.go` | 1.2 #9 |
| A hab line that does not decode: method if known, decode error, never the line | `RPCClient.swift:170` | 1.2 #11 |

### 4.7 Hang diagnosis

A deadlocked gff-hab never crashes, so a crash record never sees it.

- **Stack dump on demand.** On `SIGUSR1`, gff-hab writes every goroutine's stack to
  `crash/hab-<epoch>-stacks-<time>.txt` (UTC, to the millisecond), and logs one line naming the
  file. A signal works while the control loop is stuck. gff-hab catches `SIGUSR1` from its first
  line, since Go's default for it ends the process.
- `gff-hab stacks` sends it to the pid in the run marker, waits for the new file, then prints its
  path. It needs no answer on the socket. The marker's `stacks` flag says the running gff-hab
  catches `SIGUSR1`; without it, as from an older build, the command sends nothing.
- **Stall watchdog.** The long-lived points that serialise state are the structure store's lock
  and the holder link's event loop (one loop for every holder). The watchdog sends a no-op
  through each every 5 seconds: it takes the store's lock, and it runs an empty task on the
  link's loop. The probe is the work that waits, so no loop carries watchdog code.
- A probe unanswered for 10 seconds is a stall. gff-hab logs one line "loop stalled", with the
  loop's name and the wait, and writes one stack dump. A stall that lasts logs nothing more. When
  the probe returns, one line "loop recovered" gives the total wait.
- **Excluded.** The link attaches a holder's panes in one task on its loop: it restores each
  pane's checkpoint through the terminal parser, up to the scrollback budget in all. That runs at
  a holder's handover and a reconnect, and may take seconds. While it runs, the link's probe
  wait restarts at each check, so it is never a stall. Replay output reaches the loop frame by
  frame, so a probe passes between frames. The store's longest hold is a state file write with
  an fsync, a small file; one past 10 seconds blocks every client, and is reported.
- A loop reports at most one stall per hour. The watchdog writes at most one dump per hour across
  both loops: one dump shows every goroutine, and a stuck store also stalls the link.
- **Privacy.** A dump holds goroutine stacks, which can show argument values. It lives in
  `crash/` only, mode 0600, and keeps the newest 5 dumps apart from the crash files, so repeated
  dumps never push out a crash trace (4.3). A log line names the dump's file, never
  its content. A bundle gives it pass 2 (8.2).
- The holder gets neither until a holder build needs other changes; its loops are few, and a
  hung holder shows as a stalled holder link in gff-hab.

### 4.8 Reading logs

`gff-hab logs` reads the files directly, so it works while gff-hab is down. From stage 1 it merges
`hab.log`, every `gff-hab-holder.*.log`, `hook.log` and, from stage 5, `capcom.log`, ordered by
time. Each file's old files come first. A line with no time stamp, such as a stack trace, stays
with the line before it. Until the holder's next build, its lines carry local time to the second.

| Flag | Stage | Meaning |
|---|---|---|
| `-f` | 1 | Follow, across rotation |
| `--since 2h`, `--until` | 1 | Time window |
| `--proc holder` | 1 | One process kind |
| `--grep <text>` | 1 | Lines that hold the text |
| `--trace <id>` | 4 | One action (5) |
| `--level warn`, `--comp peers`, `--json` | 5 | Need structured lines |

- Output is local time, one line per entry, with the file's process kind in front.
- `gff-hab status` (stage 1): build and version of gff-hab and each holder, uptime, crashes and
  unclean ends in the last 7 days (6), hook endpoint up or down, peers online, Capcom state (a
  word, never its last stderr line), debug, log folder size. It is the first command a user
  runs, as `herdr status` is. It reads the run marker and the holder registry, so it works while
  gff-hab is down; peers and Capcom need gff-hab to answer. Debug reads "on until <time>" for a
  window (4.5), "on" for `GFF_HAB_DEBUG`, else "off"; the holder's version is its protocol version
  from the registry.

### 4.9 The Capcom engine and the speech helper (stage 5)

- The engine writes the same JSON line format to `capcom.log`, at a path gff-hab gives it.
- It adds handlers for an uncaught exception and an unhandled rejection. Each writes one `error`
  line with the error's class and its stack frames' function names, then exits 1.
- **SDK stderr is never copied into `capcom.log`.** The engine counts its lines and bytes and
  logs the counts once per turn. The raw lines stay only in `engine.log`, which no bundle includes.
  The first line of a provider error (`capcom-engine/src/codex.ts:666,669,681,789`) is logged as
  an error class (`auth`, `rate_limit`, `network`, `provider`, `other`), never its text.
- The handler exception of 1.2 #12 is logged with its class, and the message is not acknowledged,
  so it is resent.
- The speech helper's stderr lines stay in `hab.log`, at `debug`, as `text` class.

### 4.10 The app

- The app keeps `os.Logger`. The unified log is cheap and rotates by itself.
- **Privacy (stage 2).** Each interpolated value is `.private` unless it is an enum word, a
  count, a duration, an id Flightdeck minted (trace, session, pane, client) or an error kind. Host
  names, labels, paths and error descriptions are `.private`.
- Every line goes through one wrapper, `AppLog` (`FlightdeckKit/Diagnostics/AppLog.swift`). Its
  interpolation makes any value private. Integers, Booleans and Doubles are public, as counts and
  durations. Three labels make a value public: `\(word: e)` for an enum word or an API method,
  `\(id: pane)` for a minted id, `\(kind: error)` for an error's kind, never its description.
- An error's kind is its own `logKind` when it has one (`RPCError` gives its API error kind,
  `CallTimedOut` "timeout" and its method); else `Type.case` for an enum; else the domain and
  code of the NSError it bridges to, when that domain is not just its type's name (`URLError`,
  `POSIXError`); else the type's name.
- The unified log gets the line with each private value as `<private>`, as public text, and the
  whole line as one `.private` value.
- **`app.log` (stage 2).** The app writes `Flightdeck/diagnostics/app.log` under its Application
  Support folder: JSON lines (`t`, `lvl`, `cat`, `msg`), `notice` and above, 1 MiB with one old
  file, mode 0600. `msg` is the redacted line: a private value is `<private>` and never reaches
  the file. The unified log on iOS lets an app read only its current run, so this file keeps a
  previous run's errors. The app opens it at launch; tests and extensions write none.
- Export (stage 6) reads the current run through `OSLogStore(scope: .currentProcessIdentifier)`
  and adds `app.log` (8.5).

---

## 5. Correlation

### 5.1 The ids

| Id | Made by | Scope | Use |
|---|---|---|---|
| `trace` | The client at a user action; else gff-hab per request | One action | `gff-hab logs --trace` |
| `epoch` | gff-hab at start | One gff-hab process | Joins log lines, health records and the welcome |
| session id, pane id | gff-hab | A session's life | `--grep` on the id |
| holder instance | gff-hab | One holder | The holder file's name |

- A trace is 12 characters of Crockford base32. The first says who made it: `a` the app, `g`
  `gff`, `h` gff-hab. The other 11 are random (55 bits). It holds no content and no time.

### 5.2 Client to gff-hab

- A request may carry a top-level member `"trace"` beside `id` and `method`.
- **gff-hab checks it.** A value that is not 12 Crockford characters with a known first letter is
  replaced by a new `h` trace, and the failed-request line notes `trace_replaced`. A peer or a
  module can then never write free text into a `safe` key.
- A request without one gets a new `h` trace.
- The response carries the trace. Events do not, for now: `structure.Event` carries no trace, and
  `Publish` (`internal/control/server.go:431`) has no request context.
- gff-hab keeps the trace in the request's `context.Context`. In stage 4 it appears on the failed
  request line and on peer call lines. From stage 5, `diaglog` adds it to every line the request
  writes.
- Old peers and clients ignore the member: `internal/control/wire.go:20-27` with the decode at
  `internal/control/conn.go:424`, Go's `encoding/json` in the Go clients, and the Swift decoder
  (`RPCClient.swift:201`). The change needs no revision. The `trace` capability says
  the hab honours it.

**Decision: a top-level JSON-RPC member, on requests and responses.** Params are typed per method
and generated; a member on the envelope reaches every method with one change in `wire.go` and the
clients. Event echo waits until a need shows (16).

### 5.3 gff-hab to holder

There is no join in this spec. The holder logs no requests (input is a hot path), and its token
restarts at 1 on each connection (`internal/holderclient/holder.go:106`). A join would need the
holder instance, the hab epoch and the token, and would find almost nothing. Holder lines are
found by time and pane id in the merged `gff-hab logs` (4.8).

### 5.4 gff-hab to a peer hab

- A call to a peer carries the current trace in the same member. The peer logs it on its
  failed-request and peer lines.
- A bundle from each machine then holds the same trace. Reading across machines is by hand: one
  grep per machine.

### 5.5 Hooks

A hook envelope cannot know which action caused it. gff-hab logs a hook line with its session and
pane. A reader joins by session id and time.

---

## 6. Health and crash records

### 6.1 Run markers

| Process | Marker | Run id |
|---|---|---|
| gff-hab | `<state>/run/hab.json` | Its epoch |
| Each holder | `<state>/run/holder.<instance>.json` | pid and start time; the holder has no epoch (`api/types.go:62` is gff-hab's) |

Fields: `proc`, `instance`, the run id, `pid`, `build`, `version`, `started`, `last_seen`,
`clean`. gff-hab's marker also holds `crash_file` (6.2), `hook` (`up` or `down`, 4.6), `debug`
and `debug_until` (4.5), and `stacks` (4.7), for `gff-hab status` and `gff-hab stacks`.

- `clean` is `false` until a clean exit sets it `true`.
- `last_seen` changes only when the process flushes a log line, and at a clean exit. An idle
  process writes nothing (11).
- gff-hab writes its marker. The holder's marker, and its crash output, come with the next holder
  build (14, stage 1). Until then gff-hab writes the holder's health records from the link's view
  (6.3).

### 6.2 Crash output

- At start, gff-hab calls `runtime/debug.SetCrashOutput` with a new file
  `crash/hab-<epoch>.txt`. A fatal panic or runtime error writes its full traceback there as
  well as to stderr. A clean exit deletes the file when it is empty. The holder does the same with
  its next build.
- The holder keeps recovering goroutine panics (`hab-holder.md` 6.6), and with its next build
  logs the stack with the value (1.2 #2).
- The hook server's `ErrorLog` goes to gff-hab's logger. A line that reports a recovered handler
  panic also writes a health record of kind `recovered`.
- gff-hab keeps no top-level recover. A panic ends it, the service manager restarts it, and the
  crash file holds the cause (Q4).

### 6.3 The next start

At start, before it writes its own marker, a process reads the previous one:

| Previous marker | Crash file | Health record `kind` | `cause` |
|---|---|---|---|
| `clean` true | n/a | none | n/a |
| `clean` false | has a `panic:` or `fatal error:` line | `crash` | That first line, with the value after `panic:` cut to 120 bytes |
| `clean` false | empty or missing | `unclean` | `signal or kill` |

- The record goes to `<state>/health.jsonl`; the last 500 records stay. Fields: `proc`,
  `instance`, `kind`, `cause`, the previous run id, `build`, `version`, `started`, `last_seen`,
  and the crash file's name.
- The process logs one line: "previous run ended unclean", with the same fields and its own
  version and build.
- The unit's exit status is not read: once systemd restarts the unit, `systemctl --user show`
  reports the new run, not the one that ended.
- gff-hab writes the record for a lost holder from the link's view: holder instance, panes lost,
  the holder's last build, kind `unclean`, `cause` `holder lost`. A broken connection and a
  crash look the same from the link, so a lost holder raises no notice.
- **Crash notice.** gff-hab raises a notice (`notice.raise`, `api/methods.go:105`) for a `crash`
  of gff-hab or a holder, at most one per hour: "gff-hab restarted after a crash". It names no
  file and no host. An `unclean` end raises none: a reboot or a kill is not a bug.
- **Capcom engine.** The supervisor's crash count stays (`capcom-service.md` 218). Each engine
  crash also writes a record. Its `cause` is a class: the exit code or signal, `start_check`,
  `crash_limit`. Never the last stderr line, which can hold a prompt fragment (1.5).

### 6.4 The app (stage 6)

- **MetricKit.** The app subscribes to `MXMetricManager` at launch. iOS 17 and macOS 14 (the
  deployment targets, `apple/project.yml:8-10`) deliver diagnostic payloads (crash, hang, CPU
  exception, disk-write exception) at the next launch. The app stores each payload's JSON in
  `diagnostics/metrickit/`, the last 20 files. Daily metric payloads are not kept.
- A payload holds call stacks of addresses with binary images, not symbols. Symbolication needs the
  build's dSYM, which CI keeps (`ci-release.md`). The app does not symbolicate.
- `MXCrashDiagnostic` is deprecated in the 27.x SDK in favour of `MetricManager` and
  `CrashDiagnostic` [mk2]. The app moves when its deployment target allows.
- **Run marker.** The app keeps the same marker in its defaults. On iOS an unclean end is common
  (the system ends background apps), so it is recorded only with a MetricKit payload.
- Payloads are always kept, within the cap. There is no setting: a capped local file has one
  right answer (`app.md` 20.1).

**Decision: MetricKit, no crash SDK.** It needs no server and no third party, and it covers
crashes a signal handler cannot catch.

---

## 7. Tallies: local usage figures

### 7.1 What gff-hab counts

| Tally | Key | Values |
|---|---|---|
| API calls | method, client kind (the hello's `client.kind`: `app`, `terminal`, `cli`, `script`, `hab`; `route` for a call on the pane route) | count, error count per error `kind` (`protocol` for a JSON-RPC error with no kind) |
| Events | event type | count of events sent to clients, once per event however many clients get it |
| Hook envelopes | harness, event | count, refused count (unknown token, bad envelope, over the size cap) |
| Features | a fixed word list (7.2) | count |
| Settings | setting id from the settings registry (`internal/settings/registry.go`) | `set` count (rows `settings.set` wrote), and `in_config` (the hab's config or a flag locks the id at a flush) |
| Health | process kind: `hab`, `holder` | starts, crashes, unclean ends: gff-hab counts its own start, the previous run's end at the next start, and a lost holder (6.3) |

- A call on the pane route (`POST /channel/<token>`, `mesh.md` 6.5) has no hello. It counts
  under client kind `route`, by its method, with the error kind when it failed.
- The method set is the API table and the route-only methods (`hab-logic-api.md` 3.9), so
  `agent.compact`, `agent.name` and the `personal.*` tools count by name too.
- The key sets are closed and built at start: the method table, the event types, the hook events
  each harness's hook config posts (`internal/hook/config.go`, and `StatusLine`), the features
  of 7.2, the settings registry, the client kinds and error kinds of the API, the process kinds.
  A key outside its set counts under one bucket, `other`. It is never stored by name.
- Each count is one atomic add into that table: no lock, no allocation, no disk write. The code
  is `internal/tallies`.
- Capabilities have no tally. A capability is a word in the welcome, not something a client
  calls; its methods and events carry its use.

### 7.2 Features

| Word | Counted at |
|---|---|
| `send` | A `session.send` that succeeded: input sent through the chat or composer path |
| `session.create`, `session.restart`, `session.resume` | Each method, when it succeeded |
| `round.run` | A round run starts |
| `capcom.job` | A run of a round in Capcom mode starts |
| `approval.answer` | An `approval.answer` that succeeded |
| `channel.send`, `channel.expired` | A channel message this hab sent is queued, or expires unsent |
| `friction` | A `diag.friction` that succeeded (9) |

### 7.3 Storage

- One file per local calendar day: `<state>/tallies/<yyyy-mm-dd>.json`. gff-hab flushes every 5
  minutes when a count changed, and at a clean exit. Each file is written to a temporary file
  and renamed, so `gff-hab tallies` never reads half a file.
- A crash or a kill loses the counts since the last flush: at most 5 minutes. A flush that
  fails keeps its counts for the next one.
- `<state>/tallies/seen.json` keeps per method, event, hook event, feature and setting the first
  and last day seen and a total. It also records `since`: the day tallying started.
- Day files older than 90 days are deleted. `seen.json` stays.
- Tallies hold names and numbers only: method names, feature words, setting ids, error kinds.
  Never a host name, path, title or argument value.
- `[diagnostics] tallies = false` in the hab's config turns them off; `gff-hab tallies --clear`
  deletes the files. It is a config line, not an app row (`app.md` 20.1).

### 7.4 Reading tallies

`gff-hab tallies`:

| Flag | Meaning |
|---|---|
| `--days 7` | Window: calls and errors per method and client kind, events, hook events, features, settings, health |
| `--unused 30` | Methods, events, hook events, features and settings with no use in the last N days, or never since `since` |
| `--json` | The raw figures |
| `--state` | The state directory |
| `--clear` | Delete the tallies |

- `--unused` lists against this build's key sets (7.1): the method table `rpc.discover` serves,
  the event types, the hook events, the features and the settings registry. A name that leaves
  the build is never listed, though `seen.json` keeps it. A name absent from `seen.json` prints
  as "never since `<since>`".
- The output states the window and the client kinds it saw. A method unused on one hab may be
  used against another; the reader runs it on each hab.

**Decision: tallies on by default, local only, counts only.** Names and numbers never leave the
machine, and the planned prune needs them from the first week. Latencies and app view counts wait
(16).

---

## 8. The diagnostics bundle

### 8.1 The command

`gff-hab bundle [-o <file>] [--since 48h] [--no-friction]` writes one `.tar.gz`. By default it
goes in the current directory as `flightdeck-diagnostics-<yyyymmdd-hhmm>.tar.gz`. It works while
gff-hab is down. At the end it prints the archive path, its size, the count of replacements per
rule, and one line: "Review the files before you share them."

### 8.2 Contents

The bundle holds only these files.

| Path in bundle | Source | Treatment |
|---|---|---|
| `manifest.json` | Made by the command | Bundle format version, time, window, file list, replacement counts per rule |
| `versions.json` | `internal/version`, Go version, OS name and version, CPU arch | Kept |
| `status.txt` | `gff-hab status` (4.8) | Pass 2 |
| `service.txt` | Job state from launchd or systemd: loaded, running, last exit, restart count | Pass 2 |
| `logs/` | `hab.log`, `holder.*.log`, `hook.log`, `capcom.log` within the window | Pass 1, then pass 2 |
| `crash/` | Crash files and stack dumps within the window | Pass 2 |
| `health.jsonl` | 6.3 | Pass 1, then pass 2 |
| `tallies/` | Day files within the window and `seen.json` | Kept: names and numbers only |
| `config/hab.toml`, `config/peers.toml` | The config files | Keys kept; values replaced unless allowlisted (8.3) |
| `friction/` | Friction notes within the window (9) | Text: pass 2. Log snapshots: pass 1, then pass 2 |
| `detect.json` | `gff-hab detect explain --json --no-text` for each live pane | Pass 2 |

Never in a bundle: `engine.log`, `hab.stderr`, transcripts, terminal rings and snapshots,
logbook contents, module state, the hab key, device keys, push keys, the Capcom credential,
environment files, secret sets, any file not in this table.

**Rule: a command's output enters the bundle only through a mode that prints no content.**
`gff-hab detect explain` prints the winning rule's screen region (`cmd/gff-hab/detect.go:343-344`,
`writeRegion` at 356-367): terminal output that no scrub can recognise. The bundle uses a new
`--no-text` mode: rule ids, states, priorities and region coordinates, never the text. Test 11's
canary is on screen in a live pane at bundle time, and must not appear in `detect.json`.

### 8.3 Redaction

Pass 1 works by key on structured lines. Pass 2 scrubs every remaining string.

**Pass 1, by key.** Every value in a structured line comes from a `diaglog` constructor, and each
constructor has a class (10.2):

| Class | Constructors (examples) | In a bundle |
|---|---|---|
| `safe` | `Session(id)`, `Pane(id)`, `Method(m)`, `Kind(k)`, `Trace(t)`, `Count(n)`, `Dur(d)`, `Harness(h)`, `Event(e)`, `ClientKind(k)` | Kept |
| `name` | `Host(h)`, `Label(l)`, `Device(id)`, `Project(p)`, `Branch(b)`, `Title(t)` | Replaced by `n:` plus 8 hex digits of HMAC-SHA256 of the value, keyed with a random salt for this bundle |
| `path` | `Path(p)` | The home directory becomes `~`; each component is replaced as a name, except the fixed layout under `<state>` |
| `text` | `Err(err)`, `Text(s)`, and every `legacy` line's text | Pass 2 only |

The same value hashes to the same token within one bundle, so "the same peer failed three times"
still reads. A new salt per bundle means two bundles cannot be joined by token.

**Decision: keep the salted name tokens.** A multi-host issue is the common case: a peer link, a
group move. Fixed placeholders would make two peers look the same. The cost is a few lines in the
bundle command.

**Pass 2, scrub.** On every string that remains, in this order:

| Rule | Replaced by |
|---|---|
| This machine's host name, user name and home path; every host, label and device id in `peers.toml` and `hab.toml`; the names of projects gff-hab knows. Exact, any case | The `n:` token |
| The hygiene scan's built-in rules (`ci-release.md` 9.4): private IPv4, CGNAT IPv4, tailnet IPv6, `<label>.ts.net` names, home paths | `<ip>`, `<ip>`, `<ip>`, `<host>`, `~/<path>` |
| Any other IPv4 or IPv6 address | `<ip>` |
| Email addresses | `<email>` |
| A URL's user info, query and fragment | Removed |
| PEM blocks; `ssh-` and `ecdsa-` key lines; `SHA256:` fingerprints | `<key>` |
| `op://` references; the value after `token`, `key`, `secret`, `password`, `authorization` or `bearer` | `<secret>` |
| Runs of 32 or more base64, base64url or hex characters | `<blob>` |

Pass 2 removes names and secrets. It cannot remove content such as a prompt fragment. Content is
kept out at the source (10.2); the bundle is the second guard, not the first.

The manifest records each rule's hit count, never the matched text, as the hygiene scan does
(`ci-release.md` 9.6).

**Config allowlist.** A config value stays only when its key is a setting whose value is an enum,
a number or a boolean in the settings registry. Every other value becomes `<value>`.

### 8.4 Review

- `gff-hab bundle --dir <folder>` writes the files unpacked, for review, and makes no archive.
- The manifest lists each file and its size.
- The bundle is never uploaded. The user attaches it to an issue by hand.

### 8.5 The app (stage 6)

- Settings → Diagnostics shows: app version and build, counts of MetricKit payloads and recent app
  errors, and each connected hab's `status`.
- **Export app diagnostics** builds the same layout from the app's side: `manifest.json`,
  `versions.json`, `app.log`, the current run's unified log lines (`notice` and above), the
  MetricKit payloads. The same passes run in the app. The share sheet offers the archive.

---

## 9. Friction notes

A friction note records "Flightdeck fell short here; I went back to my old tool". It costs the
user one line of text.

### 9.1 How a user makes one

| Client | How |
|---|---|
| `gff` | `gff friction [-session name] [-blocker] text` (it runs `gff-hab friction`). In the workspace, session mode then `F` (Ctrl+O F by default) opens a one-line prompt that names the focused session; Tab marks the note a blocker. The bottom bar shows the trace |
| App | "Note friction" in the session's More menu on a phone, and an icon in the session header on iPad and Mac; ⌥⌘F with a hardware keyboard. A sheet with a multi-line field, a Blocker switch, Cancel and Send; after Send it shows the trace. It shows only when the hab lists the `friction` capability |
| Any client | The method `diag.friction` `{text, session?, pane?, blocker?}`, sent to the session's own hab. Capability `friction`. Not over a peer link |

- **Blocker or preference.** A note is a blocker when the work could not be done in Flightdeck
  and the user went back to another tool; otherwise it is a preference. The project counts
  blocker fallbacks apart from preferences, so the client asks once per note, and preference is
  the default.

### 9.2 What gff-hab stores

`<state>/friction/<time>-<trace>.json`:

| Field | Meaning |
|---|---|
| `t`, `trace` | Time, and a new trace (an `h` trace: gff-hab makes it) |
| `client` | The client kind from the hello: `app`, `terminal`, `cli`, `script` |
| `session`, `pane` | When given |
| `blocker` | `true` for a blocker (9.1); absent for a preference |
| `text` | The user's own text, at most 2,000 characters |
| `build`, `version` | gff-hab's |
| `log` | The merged log tail (4.8) of the last 10 minutes, at most 500 entries: each with its time, its process (`hab`, `holder <instance>`, `hook`) and its text as the file holds it |
| `log_cut` | Older tail entries dropped so the file stays within 256 KiB (11) |

- gff-hab answers with the note's trace. The client shows it.
- Surrounding white space is cut. An empty text, or one over 2,000 characters, is refused with
  `invalid`, not cut. A session or pane id over 128 bytes is refused the same way.
- gff-hab flushes its log before it reads the tail, so the tail holds the lines up to the note.
  An entry before the first time stamp in its file is left out: its time is unknown.
- gff-hab logs one line per note: its trace, blocker or preference, the client kind and the
  count of tail entries. Never the text, the session or the pane, which a client sends.
- `gff friction list [-days 30]` prints time, trace, session, blocker or preference, and the
  text on one line. `gff friction show <trace>` prints one note with its log tail. Both read the
  files of this host's hab, so they work while gff-hab is down; a note on another host is read
  there (Q6).
- The newest 200 notes stay. Log tails of notes older than 90 days are dropped; the text stays.
  gff-hab prunes after each note and in its hourly sweep.
- The tail is a copy of local log lines. In a bundle it gets the same passes as the log files.

**Decision: text plus a log tail, no rating, no form.** The first period of use needs the moment
and its context. A form costs more than the note is worth.

---

## 10. Privacy and consent

### 10.1 Rules

- Nothing in Flightdeck sends diagnostics anywhere. No diagnostics code opens a network
  connection.
- No log line, health record, tally or crash record holds: transcript text, prompt text, chat
  text, terminal input or output, keystrokes, clipboard contents, file contents, environment
  values, secret values, or a module's state.
- Relayed stderr from a program that sees content (the Capcom SDK, a provider) is counted, never
  copied into a log a bundle reads (4.9).
- The one place a user's text is kept is a friction note's `text`, which the user wrote for this.
- Ids Flightdeck mints (trace, session, pane) carry no content. A trace from a client is checked
  (5.2).

### 10.2 How the rules hold

From stage 5, every Go log line goes through `internal/diaglog`:

- `diaglog.Logger` has `Debug`, `Info`, `Warn`, `Error`, each taking a message and
  `...diaglog.Attr`. `Attr` is an opaque type; only the package's class constructors make one.
- Safe constructors take typed values where the type exists (`time.Duration`, `int`, the API's
  kind words). Those that take a string check its form: an id shape, a method from the table, a
  known word. A value of the wrong form is logged as `text` class instead.
- The `legacy` shim takes the old `Logf` text under `Text`, class `text`.

A source check (in the style of `make check-pure`) walks every Go file outside `internal/diaglog`
and fails on:

| Banned | Why |
|---|---|
| A `diaglog` message that is not a string constant | Prose with values would bypass the key classes |
| Any use of `log/slog` outside `internal/diaglog` (`slog.Any`, `slog.Group`, `slog.String`, `Logger.With`, a `LogValuer`) | Each lets a value skip its class; `slog.Any` marshals a struct whole |
| A call into `fmt` or `strings` as the argument of a `safe` constructor | It would build a safe-looking value from content |
| A `[]byte` or a content type (`api.ChatMessage`, transcript records; a listed set) reaching any `diaglog` call | Content by type |
| A `diaglog` call inside the listed hot-path functions (holder pump and ring, data stream writer, input path) | Cost (11) |
| An import of `net`, `net/http` or `crypto/tls` in the diagnostics packages | 10.1 |

The checker has its own test: a fixture package with one case of each ban must fail it.

Other guards:

| Guard | What it does |
|---|---|
| Canary (test 11) | One random string goes through every content path, then is found in no diagnostics file and no bundle |
| Bundle sources | Only the files of 8.2; command output only through a no-content mode (8.2) |
| Capcom | SDK stderr counted only; provider errors and crash causes as classes (4.9, 6.3) |
| App | A unit test on the app's logging wrapper checks that a string defaults to `.private`. A source check (`AppLogSourceTests`) fails, outside the wrapper, on `privacy:`, `Logger(` or `Logger.init(`, `os_log(`, `NSLog(`, `debugPrint(`, `print(` (except two listed files' DEBUG lines), and `stringLiteral:`; and on a `\(id: …)` or `\(word: …)`, any spacing or raw string, whose expression is not listed for that file. Its fixture has one case per ban |

### 10.3 Consent

- Nothing is sent, so nothing needs consent to send. The README and the app's Diagnostics screen
  say what is kept locally and where.
- If a future feature sends anything, it is off by default, asks once, shows the exact report
  before the first send, and stores consent as the report version accepted, as Syncthing does
  [s2][s3]. That feature needs its own spec and owner approval.

### 10.4 Defaults

| Item | Default | Where it changes |
|---|---|---|
| Logs | On | `[diagnostics] log_level` in the hab's config (from stage 5); no way to turn them off |
| Debug lines | Off | `gff-hab debug <duration>` |
| Crash and health records, crash notice | On | Fixed |
| Tallies | On | `[diagnostics] tallies` in the hab's config |
| MetricKit payloads kept | On | Fixed |
| Friction notes | Made only when the user asks | n/a |

None of these is an app row: each is an operator limit or has one right answer (`app.md` 20.1).

---

## 11. Cost limits

| Resource | Limit |
|---|---|
| `<state>/logs/` | About 75 MiB with one holder: hab 40, holder 15, Capcom 15, hook 2, stderr 1, plus 20 crash files and 5 stack dumps. The hourly sweep deletes the oldest old files over 100 MiB |
| `<state>/tallies/` | 90 day files of a few KiB each, under 5 MiB |
| `<state>/health.jsonl` | 500 lines |
| `<state>/friction/` | 200 notes, each at most 256 KiB |
| App container | `app.log` 2 MiB, MetricKit 20 files |
| CPU per request | One atomic add per tally. A failed request adds one line |
| Hot paths | No logging per PTY read, frame, ring write, keystroke, data stream chunk or terminal query |
| Disk wake-ups | An idle hab writes nothing: no marker update, no tally flush when nothing changed |
| Watchdog | Nothing on a loop's own path. One no-op probe per loop every 5 seconds, and one check |

- A full disk stops log writes. The writer counts dropped lines and never blocks or crashes the
  process.
- The bundle streams files and writes its archive in one pass. It never loads a log file whole.

---

## 12. Edge cases

| Case | Behaviour |
|---|---|
| The laptop sleeps for hours | Nothing is written. The watchdog measures a probe's wait on the monotonic clock, which stops while the machine sleeps on macOS and Linux. A check whose wall-clock gap runs more than 2 seconds past its monotonic gap spans a sleep: it reports nothing and restarts the wait of each probe in flight. A day boundary passed in sleep closes the old tally day at the next flush |
| The clock jumps | Lines keep their wall time. Rotation is by size. Tally days follow local date at flush |
| Two gff-hab processes by accident (`hab-holder.md` 6.8) | The second fails at the state-directory lock before it opens `hab.log`, so one writer holds the file; its lines go to stderr |
| A crash in the middle of a line | `gff-hab logs` and the bundle skip a line that does not parse, and count it |
| The user deletes `<state>/logs/` | The inode check reopens the file at the next rotation check |
| A panic value that holds user text | The crash file stays local. In a bundle, pass 2 runs on it, and `cause` in a health record is cut to 120 bytes. The code rule: a panic message is a constant plus ids, never content |
| A hook with no hab installed | It writes `hook.log` only when `<state>/logs/` exists |
| A drained holder's file | Kept until the sweep that removes a gone instance (`internal/service/launchd.go:388-397`), which also removes its old files |
| A peer on an older build | It drops the trace member. The caller's lines still show the call |
| `debug_until` in the past at start | Ignored and removed |
| A crash loop | One crash notice per hour; one health record per crash; debug stays on while `debug_until` holds |
| Tallies off, then on | A gap in day files. `seen.json` keeps `since`; `--unused` prints how many day files its window holds |
| A method added in a new build | Absent from `seen.json`, so "never since `<since>`" |
| A friction note with no hab running | `gff friction` fails with a hint; the note is not queued |

---

## 13. Open questions

The review settled four earlier questions: JSON lines (from stage 5), the top-level trace member,
the crash notice, and no app-to-hab tally sync for now. Each question below has a recommendation.
The owner decides.

| # | Question | Recommendation | Why |
|---|---|---|---|
| Q1 | Name the figures "tallies"? | Yes | "Usage" means plan limits across four specs (3) |
| Q2 | Should the Linux units write to files (`append:`) instead of journald? | Yes | One reader, one bundle and one rotation rule on every OS. `journalctl` users lose the journal copy; the stage 5 `warn` copy to stderr goes to the file, not the journal |
| Q3 | Stack dump through `SIGUSR1`, or through a control method? | `SIGUSR1`, wrapped by `gff-hab stacks` | A hung control loop cannot answer a method; a signal handler runs on its own goroutine |
| Q4 | Should gff-hab recover panics per request instead of crashing? | No. Crash, restart, record | A restart is cheap and leaves no half-changed state. Revisit if health records show one handler failing often |
| Q5 | Ship the holder's crash output, run marker, start-line version and panic stacks with its next build, not alone? | Yes, with the next holder build | A holder build needs a drain. gff-hab's link view covers holder loss until then |
| Q6 | Add remote log reading and bundles (`diag.logs`, `gff logs <host>`)? | Not now. Add only `diag.friction` and `diag.debug` | The API is about to be pruned; SSH to the host and `gff-hab logs` cover the first period |
| Q7 | Retention: tallies 90 days, health 500 lines, friction 200 notes? | Yes | Enough for a prune decision and a first quarter, well under the caps |
| Q8 | Watchdog threshold: 10 seconds silent while work waits? | Yes, fixed | Long enough to skip a slow git read off the loop, short enough to catch a deadlock in one session |
| Q9 | Add a canary UI test for the app's logs? | After stage 6 | The Go canary covers the content paths first; the app logs little content |

---

## 14. Stages

Smallest useful first. Stage 1 is what one person needs in the first week of daily use.

| # | Stage | Acceptance |
|---|---|---|
| 1 | **Week one.** (a) The size-capped writer behind the existing `log.Logger`, UTC microsecond time, no call-site change; `hab.stderr`, holder files and `engine.log` copy-truncated by gff-hab; Linux units `append:` to files; version and build on gff-hab's start line. (b) `SetCrashOutput`, the run marker, `health.jsonl`, the next-start line, the crash notice; hook server `ErrorLog` with `recovered` records; holder-loss records from the link. (c) The lines of 4.6, including one per failed request. (d) `gff-hab status`, and `gff-hab logs` merged by time with `-f`, `--since`, `--proc`, `--grep`. (e) Friction notes: `diag.friction`, `gff friction` with `list` and `show`, the app's menu item; text plus the merged log tail | A killed gff-hab leaves a health record of kind `unclean`, and a forced panic one of kind `crash`, both read at the next start; a crash raises one notice. Files rotate at the cap in a test with a small cap, and a copy-truncated file keeps receiving lines on macOS and Linux. A failed request, a hook server failure and a peer going offline each produce one line. A friction note from `gff` and one from the app each land with text, session and a log tail |
| 2 | **Hangs, debug and the app's errors.** `SIGUSR1` stack dumps and `gff-hab stacks`; the stall watchdog; `diag.debug`, `gff-hab debug` and `debug_until` across restarts; the app's `.private` fix, its source check, and `app.log` | A test seam that holds the structure store's lock yields one "loop stalled" line and one stack file. Debug set before a forced crash is still on after restart. The app source check passes |
| 3 | **Tallies.** Calls and errors per method and client kind; hook envelopes; features; settings; health counts; day files and `seen.json`; `gff-hab tallies` with `--days` and `--unused` | After an e2e run, `--unused 1` lists the methods the run did not call. The counter path allocates nothing (an allocation test) |
| 4 | **Trace.** The member on requests and responses, its check, context; `gff` and the app send one; peer calls carry it; it appears on failed-request and peer lines; `gff-hab logs --trace` | A failing call from `gff` to a session on a peer hab yields one line on each hab with one trace; a malformed trace is replaced |
| 5 | **Structured logs and the bundle.** `internal/diaglog`, slog JSON, levels, the legacy shim, the source check and its fixture test; the free text of 1.5 moved to `text`; Capcom `capcom.log`, crash handlers, SDK stderr counted only, error classes, the 1.2 #12 fix; `hook.log`; `detect explain --no-text`; `gff-hab bundle` with both passes, `--dir`, the manifest, the config allowlist; the canary e2e test | The source check fails its fixture and passes the tree. The canary test passes, including an engine that echoes the canary on stderr and the canary on screen at bundle time. A fixture state folder seeded with host names, paths, IPs, keys and tokens yields a bundle with none of them, and the manifest counts each rule |
| 6 | **App diagnostics.** MetricKit capture, the Diagnostics screen, export | A forced crash in a debug build yields a stored payload at the next launch, and export holds it |

---

## 15. Tests

Each test guards a promise in this spec, at the lowest layer that proves it (`docs/testing.md`).

| # | Test | Layer | Stage | Proves |
|---|---|---|---|---|
| 1 | The writer rotates at its cap, keeps N old files, reopens after the file is removed, and never blocks when its queue is full | Unit | 1 | 4.3, 11 |
| 2 | Copy-truncate of a file another process appends to: later lines land in the truncated file | Integration (macOS and Linux) | 1 | 4.3 |
| 3 | A process killed with SIGKILL, then started, writes one `unclean` record; a forced panic writes one `crash` record and one notice | Integration | 1 | 6.3 |
| 4 | A hook handler panic is a `recovered` record and gff-hab keeps serving | Integration | 1 | 6.2 |
| 5 | `gff-hab logs` merges hab and holder files in time order and follows across rotation | Unit | 1 | 4.8 |
| 6 | A friction note from `gff friction` and one from an app client store text, session, blocker and a tail that holds that session's lines; the text is in no `hab.log` line; Ctrl+O F sends the focused session; the app's sheet sends to the session's hab | Integration | 1 | 9.1, 9.2 |
| 7 | A blocked loop (test seam) yields one stall line and one stack file; `SIGUSR1` yields a stack file | Integration | 2 | 4.7 |
| 8 | Tallies: calls and errors per method and client kind; `--unused` against the method table; a key outside the surface stored as `other` only; the counter path allocates nothing | Unit and integration | 3 | 7 |
| 9 | A request trace reaches the response and a peer's line; a malformed one is replaced | Integration and end to end | 4 | 5.2, 5.4 |
| 10 | The source check fails its fixture (one case per ban of 10.2) and passes the tree | Unit (source) | 5 | 10.2 |
| 11 | **Canary.** One random string goes through pane input, terminal output (on screen at bundle time), a chat send, a hook payload, a Capcom turn on a test engine that also echoes it on stderr and in a provider error, a config label and an environment value. Afterwards no file under `<state>/logs`, `<state>/tallies`, `<state>/health.jsonl`, the friction log tails, `gff-hab status` output or the bundle (including `detect.json`) contains it | End to end | 5 | 8.2, 10.1, 10.2 |
| 12 | Bundle redaction: the seeded fixture leaks nothing; a value hashes alike within one bundle and differently across two | Unit | 5 | 8.3 |
| 13 | A failed hook writes one `hook.log` line, exits 0, and stays within its budget | Integration | 5 | 4.4 |
| 14 | The app's logging wrapper defaults to `.private`, and export holds `app.log` and payloads | Swift unit | 2, 6 | 4.10, 6.4 |

Test 11 joins the must-have list in `docs/testing.md` when stage 5 lands.

---

## 16. Later, not planned

The review judged these over-built for one user in a first period of use. They stay here so a
later stage can pick them up.

| Item | Why it waits |
|---|---|
| Latency histograms per method and client kind, and key-path latencies | The failed-request line carries a duration; counts answer the prune |
| App view and gesture counts, and app-to-hab tally sync | The prune needs API and setting figures first; the app is one client |
| A trace on events a request caused | Needs a trace in `structure.Event` and a context in `Publish` across 14+ stores |
| A hook `cause` link to the input before it | A 30-second guess; session id and time do the same by hand |
| Traces for Capcom turns and rounds | One more id per subsystem before the first one proves useful |
| A hab-to-holder join on the holder token | The holder logs no requests (5.3) |
| `--unused` against released schemas, to mark methods newer than `since` | A reader can see a new method by its name |
| Remote logs and bundles through the API (Q6) | SSH covers it now |
